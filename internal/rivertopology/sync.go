// Package rivertopology derives exact upstream→downstream ordering for the
// runs and gauges on a river, using NHDPlus topology instead of a geographic
// proxy.
//
// Ordering previously leaned on put_in_elevation_ft DESC (mig 000150) with
// longitude as a fallback. Both are proxies for flow direction and each fails
// predictably: elevation on flat rivers (the Green through Canyonlands, the
// Menominee) where the real drop sits inside USGS EPQS noise, and longitude on
// any river not flowing west→east. Topology is not a proxy — NLDI's DM
// (downstream mainstem) navigation returns flowlines in true downstream order,
// so a run's up_comid or a gauge's comid indexes directly into that order.
//
// Cost is per RIVER, not per run: two NLDI calls (UM to find the headwater,
// DM to sweep down from it) sequence every member at once, and the resulting
// order is cached in river_flowline_order so later additions are a lookup.
package rivertopology

import (
	"context"
	"fmt"
	"time"

	"github.com/h2oflow/h2oflow/apps/api/internal/nldi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Execer is the subset of pgx a resequence needs. Declared as an interface so
// Resequence works both on the pool and inside a caller's transaction — the
// upload path (run_upload.go) creates a run inside one, and a sequence written
// outside it would survive a rollback.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Navigation distances in km. NLDI caps distance at ~9999.
//
// Both are deliberately generous, because undershooting FAILS SILENTLY and
// looks exactly like a genuine topology gap. Measured on the Colorado, which
// is the worst case in the dataset: at 1000 km the Grand Canyon run
// (comid 20733845, Lees Ferry) fell outside the sweep and looked like an
// unreachable member; at 2000 km it appears at index 724. The feature count
// plateaus at 1874 by 4000 km — the sweep has reached the river's mouth near
// Yuma — so past that, extra distance costs nothing at all.
//
// Overshooting is close to free in storage terms too: the stored order is
// TRUNCATED at the downstream-most member (see SyncRiver), so a long sweep
// costs bytes on one request, not rows.
//
// headwaterNavKm matters for the same reason in the other direction: the UM
// leg has to reach ABOVE every member, or the DM sweep that follows starts
// mid-river and silently misses everything upstream of the seed. Reaching the
// Colorado's true headwater from Lees Ferry takes 725 flowlines.
const (
	headwaterNavKm  = 3000
	downstreamNavKm = 5000
)

type Syncer struct {
	db   *pgxpool.Pool
	nldi *nldi.Client
}

func New(db *pgxpool.Pool) *Syncer {
	return &Syncer{db: db, nldi: nldi.New()}
}

// RiverResult reports what one river's sync did. Uncovered is the count of
// members whose comid did NOT appear in the mainstem order — almost always
// runs on a tributary of the same-named river, a fork (which carries no comid
// at all), or a put-in that snapped across a confluence.
//
// Uncovered runs get an INFERRED sequence rather than staying NULL (web#406),
// counted separately in Inferred: their position comes from elevation, not
// topology, and the two must stay distinguishable. Sequenced counts only
// surveyed rows.
type RiverResult struct {
	RiverID   string
	RiverName string
	Seed      string
	Headwater string
	Flowlines int
	Members   int
	Sequenced int
	Inferred  int
	Uncovered []string
	Err       error
}

// SyncRiver resolves and stores the flowline order for one river, then
// assigns river_sequence to its runs.
//
// Seed choice is the highest-elevation member when elevation is known. That
// is only a heuristic for picking a STARTING POINT — being wrong costs a
// slightly different headwater, not a wrong order, because the DM sweep that
// follows is what actually determines sequence. On a flat river where
// elevation is unreliable, any member works: UM walks up the same stem
// regardless of which member it starts from.
func (s *Syncer) SyncRiver(ctx context.Context, riverID string) RiverResult {
	res := RiverResult{RiverID: riverID}

	_ = s.db.QueryRow(ctx, `SELECT name FROM rivers WHERE id = $1`, riverID).Scan(&res.RiverName)

	members, err := s.riverMemberComIDs(ctx, riverID)
	if err != nil {
		res.Err = err
		return res
	}
	res.Members = len(members)
	if len(members) < 2 {
		// Nothing to order. Not an error — most rivers have one run.
		return res
	}
	res.Seed = members[0]

	headwater, err := s.findHeadwater(ctx, res.Seed)
	if err != nil {
		res.Err = fmt.Errorf("headwater from %s: %w", res.Seed, err)
		return res
	}
	res.Headwater = headwater

	coll, err := s.nldi.DownstreamFlowlines(ctx, headwater, downstreamNavKm)
	if err != nil {
		res.Err = fmt.Errorf("downstream from %s: %w", headwater, err)
		return res
	}
	order := comidSequence(coll)
	if len(order) == 0 {
		res.Err = fmt.Errorf("downstream from %s returned no flowlines", headwater)
		return res
	}

	// Truncate at the downstream-most member. DM navigation does not stop at
	// a confluence — it keeps going into the receiving river — so an
	// untruncated order would claim flowlines that belong to a DIFFERENT
	// river, and a gauge sitting on one of them would be assigned to the
	// wrong river by AssignGauges. Cutting at the last member bounds each
	// river's order to the span its own members actually occupy.
	idx := make(map[string]int, len(order))
	for i, c := range order {
		idx[c] = i
	}
	last := -1
	for _, m := range members {
		if i, ok := idx[m]; ok && i > last {
			last = i
		} else if !ok {
			res.Uncovered = append(res.Uncovered, m)
		}
	}
	if last < 0 {
		res.Err = fmt.Errorf("no member comid found in the %d-flowline order from %s", len(order), headwater)
		return res
	}
	order = order[:last+1]
	res.Flowlines = len(order)

	if err := s.storeOrder(ctx, riverID, order); err != nil {
		res.Err = err
		return res
	}

	// Clear last sweep's guesses BEFORE assigning, so assignRuns sees a clean
	// surveyed/NULL split and inferRuns interpolates against measured topology
	// only. Re-inferring from a mix of surveyed and previously-inferred rows
	// would let one guess anchor the next.
	if err := clearInferred(ctx, s.db, riverID); err != nil {
		res.Err = err
		return res
	}

	n, err := s.assignRuns(ctx, riverID)
	if err != nil {
		res.Err = err
		return res
	}
	res.Sequenced = n

	inf, err := inferRuns(ctx, s.db, riverID, nil)
	if err != nil {
		res.Err = err
		return res
	}
	res.Inferred = inf
	return res
}

// riverMemberComIDs returns the distinct up_comids of a river's live runs,
// highest put-in elevation first so the seed is likely near the top of the
// river. NULLS LAST keeps elevation-less runs usable as seeds.
func (s *Syncer) riverMemberComIDs(ctx context.Context, riverID string) ([]string, error) {
	// The DISTINCT ON dedupes comids; the OUTER sort is what actually puts the
	// highest-elevation member first. Postgres requires DISTINCT ON's ORDER BY
	// to lead with the distinct key, so sorting by elevation has to happen in
	// the enclosing query or the seed is just the lowest comid string.
	rows, err := s.db.Query(ctx, `
		SELECT t.up_comid FROM (
			SELECT DISTINCT ON (up_comid) up_comid, put_in_elevation_ft
			FROM user_reaches
			WHERE river_id = $1
			  AND deleted_at IS NULL
			  AND up_comid IS NOT NULL
			ORDER BY up_comid, put_in_elevation_ft DESC NULLS LAST
		) t
		ORDER BY t.put_in_elevation_ft DESC NULLS LAST, t.up_comid
	`, riverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// findHeadwater walks UM (upstream mainstem) from seed and returns the last
// flowline reached — the top of the stem the seed sits on. UM is a single
// channel; UpstreamFlowlines uses UT, which fans into every tributary and has
// no meaningful last element.
//
// A seed already at the headwater returns just itself, which is why the empty
// case falls back to seed rather than erroring.
func (s *Syncer) findHeadwater(ctx context.Context, seed string) (string, error) {
	coll, err := s.nldi.UpstreamMainstemFlowlines(ctx, seed, headwaterNavKm)
	if err != nil {
		return "", err
	}
	order := comidSequence(coll)
	if len(order) == 0 {
		return seed, nil
	}
	return order[len(order)-1], nil
}

// comidSequence extracts comids in the order NLDI returned them, which for
// navigation responses IS the topological order. Duplicates are dropped
// keeping first occurrence.
func comidSequence(coll *nldi.Collection) []string {
	seen := make(map[string]bool, len(coll.Features))
	out := make([]string, 0, len(coll.Features))
	for _, f := range coll.Features {
		var c string
		switch {
		case f.Props.NhdplusComID != nil:
			c = string(*f.Props.NhdplusComID)
		case f.Props.ComID != nil:
			c = string(*f.Props.ComID)
		default:
			continue
		}
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// storeOrder replaces a river's cached flowline order in one transaction.
// Delete-then-insert rather than upsert: a re-sync can legitimately SHORTEN
// the order (a run was deleted, moving the downstream-most member upstream),
// and leftover rows past the new end would keep claiming flowlines this river
// no longer occupies.
func (s *Syncer) storeOrder(ctx context.Context, riverID string, order []string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM river_flowline_order WHERE river_id = $1`, riverID); err != nil {
		return err
	}
	rowsSrc := make([][]any, len(order))
	for i, c := range order {
		rowsSrc[i] = []any{riverID, c, i}
	}
	if _, err := tx.CopyFrom(ctx,
		pgx.Identifier{"river_flowline_order"},
		[]string{"river_id", "comid", "seq"},
		pgx.CopyFromRows(rowsSrc),
	); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE rivers SET topology_synced_at = NOW() WHERE id = $1`, riverID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// assignRuns writes river_sequence for every live run on the river whose
// up_comid appears in the stored order. Runs that don't appear are set back to
// NULL so a re-sync can't leave a stale sequence behind after a run moves onto
// a tributary.
func (s *Syncer) assignRuns(ctx context.Context, riverID string) (int, error) {
	if _, err := s.db.Exec(ctx, `
		UPDATE user_reaches ur
		SET river_sequence = o.seq
		FROM river_flowline_order o
		WHERE o.river_id = ur.river_id
		  AND o.comid = ur.up_comid
		  AND ur.river_id = $1
		  AND ur.deleted_at IS NULL
		  AND ur.river_sequence IS DISTINCT FROM o.seq
	`, riverID); err != nil {
		return 0, err
	}
	if _, err := s.db.Exec(ctx, `
		UPDATE user_reaches ur
		SET river_sequence = NULL
		WHERE ur.river_id = $1
		  AND ur.deleted_at IS NULL
		  AND ur.river_sequence IS NOT NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM river_flowline_order o
		      WHERE o.river_id = ur.river_id AND o.comid = ur.up_comid
		  )
	`, riverID); err != nil {
		return 0, err
	}
	// Report how many runs now HAVE a sequence, not how many rows changed.
	// The UPDATE above is guarded by IS DISTINCT FROM so it is idempotent —
	// counting affected rows made a correct re-run log "sequenced=0", which
	// reads as a failure rather than as "already correct".
	//
	// Inferred rows are excluded so this stays a count of MEASURED topology.
	// Callers use it to judge coverage, and folding guesses in would report a
	// river as fully sequenced when half of it was interpolated.
	var total int
	if err := s.db.QueryRow(ctx, `
		SELECT count(*) FROM user_reaches
		WHERE river_id = $1 AND deleted_at IS NULL
		  AND river_sequence IS NOT NULL
		  AND NOT river_sequence_inferred
	`, riverID).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// clearInferred drops a river's interpolated sequences back to NULL. Always
// paired with a following inferRuns — leaving them cleared would reintroduce
// the NULL branch this feature exists to close.
func clearInferred(ctx context.Context, db Execer, riverID string) error {
	_, err := db.Exec(ctx, `
		UPDATE user_reaches
		SET river_sequence = NULL, river_sequence_inferred = FALSE
		WHERE river_id = $1::uuid
		  AND deleted_at IS NULL
		  AND river_sequence_inferred
	`, riverID)
	return err
}

// inferRuns gives every still-unsequenced run on a river a sequence
// interpolated from where its put-in elevation falls among the river's
// SURVEYED runs (web#406). Passing reachID limits it to one run, for the
// create/update path; nil does the whole river.
//
// The point is not to improve on elevation — it is to collapse two ordering
// keys into one. A pairwise comparator is only transitive when each tier is a
// total preorder (i.e. NULLS LAST), so "compare by sequence only when both
// sides have one" cannot be made transitive in the comparator. Moving the
// elevation fallback here, applied once at write time, leaves a single key
// that both the server's ORDER BY and the client's comparator can use
// unconditionally.
//
// The interpolation is a bracket, not a proportional fit: find the
// lowest-positioned surveyed run still ABOVE this one by elevation and the
// highest-positioned one still BELOW it, and take the midpoint. Proportional
// interpolation would imply elevation is linear in flowline index, which it is
// not — only the ordering is meaningful.
//
// Degenerate cases resolve themselves through the next tier rather than
// needing special handling. A run above every surveyed run brackets to
// (min_seq-1, min_seq), whose midpoint is min_seq itself; it ties with the
// topmost run and elevation DESC then puts it first, which is the right
// answer. Same, mirrored, at the bottom. Two inferred runs landing in one gap
// tie and are separated by elevation.
//
// Non-monotonic surveyed data (elevation disagreeing with topology — the flat
// rivers this all exists for) cannot produce a cycle here either: the bracket
// is still a single number, so the result stays a total order. It may place
// the run oddly, but oddly is recoverable and a cyclic comparator is not.
//
// Runs with no elevation are left NULL deliberately. There is nothing to
// interpolate from, and inventing a position would be worse than sorting last.
func inferRuns(ctx context.Context, db Execer, riverID string, reachID *string) (int, error) {
	tag, err := db.Exec(ctx, `
		WITH surveyed AS (
			SELECT ur.river_sequence AS seq, ur.put_in_elevation_ft AS elev
			FROM user_reaches ur
			WHERE ur.river_id = $1::uuid
			  AND ur.deleted_at IS NULL
			  AND ur.river_sequence IS NOT NULL
			  AND NOT ur.river_sequence_inferred
			  AND ur.put_in_elevation_ft IS NOT NULL
		),
		bounds AS (
			SELECT min(seq) AS min_seq, max(seq) AS max_seq FROM surveyed
		),
		target AS (
			SELECT ur.id, ur.put_in_elevation_ft AS elev
			FROM user_reaches ur
			WHERE ur.river_id = $1::uuid
			  AND ur.deleted_at IS NULL
			  AND ur.river_sequence IS NULL
			  AND ur.put_in_elevation_ft IS NOT NULL
			  AND ($2::uuid IS NULL OR ur.id = $2::uuid)
		),
		calc AS (
			SELECT t.id,
			       ( COALESCE((SELECT max(s.seq) FROM surveyed s WHERE s.elev > t.elev), b.min_seq - 1)
			       + COALESCE((SELECT min(s.seq) FROM surveyed s WHERE s.elev < t.elev), b.max_seq + 1)
			       ) / 2 AS seq
			FROM target t CROSS JOIN bounds b
			WHERE b.min_seq IS NOT NULL
		)
		UPDATE user_reaches ur
		SET river_sequence = calc.seq,
		    river_sequence_inferred = TRUE
		FROM calc
		WHERE ur.id = calc.id
	`, riverID, reachID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// Resequence recomputes one run's river_sequence from cached data only — no
// NLDI, no network. Called on every path that creates a run or moves its
// endpoints.
//
// Without this a new run stays NULL until someone runs
// cmd/backfill-river-topology by hand, which is both the wrong ordering and
// invisible until it is noticed. The flowline order for the river is already
// stored (mig 000151 exists to make this a lookup), so there is no reason to
// defer it.
//
// Failure is worth reporting but never worth failing a create over: the run is
// already written, and the backfill re-derives sequences for the whole river
// anyway. Callers log and continue.
func Resequence(ctx context.Context, db Execer, reachID string) error {
	// Surveyed first. The subquery form (rather than UPDATE ... FROM) is what
	// makes this correct on an EDIT: when a moved put-in snaps to a comid that
	// is not in the order, the sequence must go back to NULL so the infer pass
	// below can replace it. A join would simply match no rows and leave the
	// stale value in place, pointing at the run's old position on the river.
	if _, err := db.Exec(ctx, `
		UPDATE user_reaches ur
		SET river_sequence = (
		        SELECT o.seq FROM river_flowline_order o
		        WHERE o.river_id = ur.river_id AND o.comid = ur.up_comid
		    ),
		    river_sequence_inferred = FALSE
		WHERE ur.id = $1::uuid
	`, reachID); err != nil {
		return err
	}

	var riverID *string
	if err := db.QueryRow(ctx,
		`SELECT river_id::text FROM user_reaches WHERE id = $1::uuid`, reachID,
	).Scan(&riverID); err != nil {
		return err
	}
	if riverID == nil {
		// A run with no river has nothing to be sequenced against.
		return nil
	}

	_, err := inferRuns(ctx, db, *riverID, &reachID)
	return err
}

// InferAll re-infers sequences on every river that has at least one surveyed
// run. Network-free — it reads only cached topology — so it is cheap enough to
// re-run routinely, unlike a full SyncRiver sweep.
//
// It exists because the per-run Resequence on the create path can only place a
// run against the surveyed runs that exist AT THAT MOMENT. Sequencing a river
// for the first time, or a later run filling in a gap, changes the brackets
// around every guess made before it. This recomputes them all.
func (s *Syncer) InferAll(ctx context.Context) (int, error) {
	rows, err := s.db.Query(ctx, `
		SELECT DISTINCT ur.river_id::text
		FROM user_reaches ur
		WHERE ur.deleted_at IS NULL
		  AND ur.river_id IS NOT NULL
		  AND ur.river_sequence IS NOT NULL
		  AND NOT ur.river_sequence_inferred
		ORDER BY 1
	`)
	if err != nil {
		return 0, err
	}
	var rivers []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		rivers = append(rivers, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	total := 0
	for _, id := range rivers {
		if err := clearInferred(ctx, s.db, id); err != nil {
			return total, err
		}
		n, err := inferRuns(ctx, s.db, id, nil)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// RiversNeedingSync lists rivers with at least two live runs carrying a
// comid — the only rivers where an order changes anything. force ignores
// topology_synced_at so a full re-sweep is possible.
func (s *Syncer) RiversNeedingSync(ctx context.Context, force bool) ([]string, error) {
	q := `
		SELECT ur.river_id::text
		FROM user_reaches ur
		JOIN rivers rv ON rv.id = ur.river_id
		WHERE ur.deleted_at IS NULL AND ur.up_comid IS NOT NULL
		  AND ($1 OR rv.topology_synced_at IS NULL)
		GROUP BY ur.river_id
		HAVING count(DISTINCT ur.up_comid) > 1
		ORDER BY ur.river_id
	`
	rows, err := s.db.Query(ctx, q, force)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ResolveGaugeComIDs fills gauges.comid by snapping each gauge's stored
// coordinates onto the NHD network. Bounded by limit and restricted to gauges
// someone is actually watching — resolving all ~2100 known gauges would be
// thousands of NLDI calls for rows nobody sorts.
//
// This is what makes gauge-only groups orderable at all: DWR gauges have NO
// elevation (0 of 19 in prod), so they currently fall straight through to the
// longitude tier.
func (s *Syncer) ResolveGaugeComIDs(ctx context.Context, limit int, pause time.Duration) (int, error) {
	rows, err := s.db.Query(ctx, `
		SELECT g.id::text,
		       ST_Y(g.location::geometry),
		       ST_X(g.location::geometry)
		FROM gauges g
		WHERE g.comid IS NULL
		  AND g.location IS NOT NULL
		  AND (EXISTS (SELECT 1 FROM user_watchlists w WHERE w.gauge_id = g.id)
		    OR EXISTS (SELECT 1 FROM user_reaches ur WHERE ur.primary_gauge_id = g.id AND ur.deleted_at IS NULL))
		ORDER BY g.id
		LIMIT $1
	`, limit)
	if err != nil {
		return 0, err
	}
	type target struct {
		id       string
		lat, lng float64
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.lat, &t.lng); err != nil {
			rows.Close()
			return 0, err
		}
		targets = append(targets, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	n := 0
	for _, t := range targets {
		snap, err := s.nldi.SnapToComID(ctx, t.lat, t.lng)
		if err != nil {
			// A gauge off the NHD network (reservoir outlet, canal) is a real
			// condition, not a failure worth aborting a batch over.
			continue
		}
		if _, err := s.db.Exec(ctx, `UPDATE gauges SET comid = $2 WHERE id = $1`, t.id, snap.ComID); err != nil {
			return n, err
		}
		n++
		if pause > 0 {
			time.Sleep(pause)
		}
	}
	return n, nil
}

// AssignGauges writes gauges.river_sequence from the cached flowline orders.
//
// A comid is only accepted when it appears in exactly ONE river's order.
// Orders are truncated at each river's downstream-most member precisely to
// keep that true, but tributary confluences can still legitimately overlap,
// and an ambiguous gauge is better left NULL (falling back to elevation) than
// assigned to a river it may not be on.
func (s *Syncer) AssignGauges(ctx context.Context) (int, error) {
	tag, err := s.db.Exec(ctx, `
		UPDATE gauges g
		SET river_sequence = o.seq
		FROM (
			SELECT comid, min(seq) AS seq
			FROM river_flowline_order
			GROUP BY comid
			HAVING count(DISTINCT river_id) = 1
		) o
		WHERE o.comid = g.comid
		  AND g.river_sequence IS DISTINCT FROM o.seq
	`)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
