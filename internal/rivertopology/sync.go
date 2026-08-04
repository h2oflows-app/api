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
	"github.com/jackc/pgx/v5/pgxpool"
)

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
// runs on a tributary of the same-named river. They keep river_sequence NULL
// and fall back to elevation rather than being guessed at.
type RiverResult struct {
	RiverID   string
	RiverName string
	Seed      string
	Headwater string
	Flowlines int
	Members   int
	Sequenced int
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

	n, err := s.assignRuns(ctx, riverID)
	if err != nil {
		res.Err = err
		return res
	}
	res.Sequenced = n
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
	var total int
	if err := s.db.QueryRow(ctx, `
		SELECT count(*) FROM user_reaches
		WHERE river_id = $1 AND deleted_at IS NULL AND river_sequence IS NOT NULL
	`, riverID).Scan(&total); err != nil {
		return 0, err
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
