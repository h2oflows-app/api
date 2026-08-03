// backfill-run-elevation resolves and writes user_reaches.put_in_elevation_ft,
// take_out_elevation_ft, and gradient_fpm (mig 000150) — replacing the
// put_in_lng "west=upstream" dashboard sort heuristic (web#386) with a
// direction-agnostic basis: elevation. Modeled on cmd/backfill-run-state
// (#356), the most recent precedent for a per-run, re-runnable backfill in
// this repo.
//
// Every non-deleted run with BOTH a put_in and a take_out is reprocessed
// from scratch on every invocation — like backfill-run-state, no existing
// value is trusted as a starting point, since it may be stale or the
// leftover of a partial write from before this tool existed. Elevation is
// resolved via the USGS Elevation Point Query Service (internal/elevation),
// the same service the live Create/Import/Update handlers use
// (internal/handlers/run_elevation.go). Gradient (feet per mile) =
// (put_in_elevation_ft - take_out_elevation_ft) / centerline river-miles,
// via PostGIS ST_Length on the stored centerline — only when a centerline
// exists; NULL otherwise.
//
// Gradient uses each row's EFFECTIVE elevation: this run's freshly resolved
// value when the lookup succeeds, falling back to whatever is already
// stored when it fails. That means a row whose centerline was only added by
// a LATER backfill-centerline pass — after its elevations were already
// resolved by an earlier run of this tool, or by a live create/update —
// still gets a gradient the next time this tool runs, without needing a
// fresh (and redundant) EPQS hit on an already-correct elevation.
//
// Usage:
//
//	go run ./cmd/backfill-run-elevation              # write changes
//	go run ./cmd/backfill-run-elevation -dry-run      # show changes, no writes
//	go run ./cmd/backfill-run-elevation -rate-ms 500  # slower between rows
//
// Idempotent: every write is gated on the resolved (rounded to the
// NUMERIC(8,1)/(6,1) column precision) value actually differing from what's
// already stored, so a repeat run against unchanged data and a healthy EPQS
// endpoint reports updated=0. A per-point EPQS failure never clobbers a good
// stored value — the effective value silently falls back to it, which is
// exactly what makes it compare equal to the old value and get skipped
// rather than written. "failed" in the summary counts DB write errors only;
// a lookup failure that changes nothing is indistinguishable from a healthy
// re-run that found no drift, and is reported as unchanged like one.
//
// Reads DATABASE_URL from the environment.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"os"
	"time"

	"github.com/h2oflow/h2oflow/apps/api/internal/db"
	"github.com/h2oflow/h2oflow/apps/api/internal/elevation"
	"github.com/jackc/pgx/v5/pgxpool"
)

// metersPerMile matches the conversion constant used elsewhere in this repo
// (internal/handlers/run_elevation.go, MapCommunity's clustering radius).
const metersPerMile = 1609.34

// elevationTimeout bounds each USGS EPQS call. Put-in and take-out are
// queried concurrently per row, so this is the worst case for the PAIR, not
// each; a slow/unresponsive point can't stall the whole batch.
const elevationTimeout = 15 * time.Second

// runRow is one user_reaches candidate: its identity, endpoint coordinates,
// currently stored values (nil = NULL), and centerline (nil = none) to
// compute gradient from.
type runRow struct {
	ID         string
	Slug       string
	Name       string
	PutInLat   float64
	PutInLng   float64
	TakeOutLat float64
	TakeOutLng float64
	OldPutFt   *float64
	OldTakeFt  *float64
	OldGradFpm *float64
	Centerline *string // GeoJSON LineString, nil when the run has none
}

func main() {
	dryRun := flag.Bool("dry-run", false, "show changes without writing to DB")
	rateMs := flag.Int("rate-ms", 250, "min ms between lookup rounds (one round = put-in + take-out, resolved in parallel) — this hits a public USGS service, be polite")
	flag.Parse()

	ctx := context.Background()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatalf("DATABASE_URL is required")
	}
	pool, err := db.Connect(ctx, dbURL)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	defer pool.Close()

	// Sanity check, mirroring backfill-run-state: put_in/take_out are
	// required at Create/Import time, so this should always be zero —
	// surfaced rather than silently excluded from the run below (which
	// requires both real coordinates to resolve elevation against).
	var missingEndpoints int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM user_reaches WHERE deleted_at IS NULL AND (put_in IS NULL OR take_out IS NULL)`,
	).Scan(&missingEndpoints); err == nil && missingEndpoints > 0 {
		log.Printf("WARNING: %d non-deleted run(s) are missing put_in or take_out — skipped, cannot resolve elevation", missingEndpoints)
	}

	todo, err := loadRuns(ctx, pool)
	if err != nil {
		log.Fatalf("query: %v", err)
	}
	if len(todo) == 0 {
		log.Printf("no non-deleted user_reaches rows with both put_in and take_out found")
		return
	}
	log.Printf("runs to process: %d", len(todo))
	if *dryRun {
		log.Printf("dry-run mode — no writes")
	}

	type outcome struct {
		run     runRow
		effPut  *float64
		effTake *float64
		grad    *float64
		changed bool
		negGrad bool
	}
	outcomes := make([]outcome, 0, len(todo))

	for i, r := range todo {
		time.Sleep(time.Duration(*rateMs) * time.Millisecond)

		putCh := queryElevationAsync(ctx, r.PutInLng, r.PutInLat)
		takeCh := queryElevationAsync(ctx, r.TakeOutLng, r.TakeOutLat)
		putRes, takeRes := <-putCh, <-takeCh

		// Effective value: this run's fresh reading on success, else
		// whatever is already stored — see the package doc comment. When a
		// lookup fails AND nothing was stored before, this is correctly nil.
		effPut := putRes.ft
		if effPut == nil {
			effPut = r.OldPutFt
		}
		effTake := takeRes.ft
		if effTake == nil {
			effTake = r.OldTakeFt
		}

		var grad *float64
		if r.Centerline != nil && effPut != nil && effTake != nil {
			if miles, ok := centerlineMiles(ctx, pool, *r.Centerline); ok {
				g := (*effPut - *effTake) / miles
				grad = &g
			}
		}

		changed := !ftEqual(roundPtr(effPut), roundPtr(r.OldPutFt)) ||
			!ftEqual(roundPtr(effTake), roundPtr(r.OldTakeFt)) ||
			!ftEqual(roundPtr(grad), roundPtr(r.OldGradFpm))
		negGrad := grad != nil && *grad < 0

		outcomes = append(outcomes, outcome{
			run: r, effPut: effPut, effTake: effTake, grad: grad,
			changed: changed, negGrad: negGrad,
		})

		status := fmt.Sprintf("put=%s take=%s grad=%s", dispFt(effPut), dispFt(effTake), dispFt(grad))
		if putRes.err != nil {
			status += fmt.Sprintf("  [put-in EPQS ERROR: %v]", putRes.err)
		}
		if takeRes.err != nil {
			status += fmt.Sprintf("  [take-out EPQS ERROR: %v]", takeRes.err)
		}
		if negGrad {
			status += "  *** NEGATIVE GRADIENT ***"
		}
		if !changed {
			status = "(unchanged) " + status
		}
		log.Printf("  [%d/%d] %-45s %s", i+1, len(todo), r.Name, status)
	}

	var updated, unchanged, failed int
	var changes, negatives []outcome
	for _, o := range outcomes {
		if o.negGrad {
			negatives = append(negatives, o)
		}
		if !o.changed {
			unchanged++
			continue
		}
		changes = append(changes, o)
	}

	fmt.Println()
	fmt.Println("── CHANGED (old → new) ──────────────────────────────────────")
	if len(changes) == 0 {
		fmt.Println("  (none)")
	}
	for _, o := range changes {
		var parts []string
		if p := diffPart("put", o.run.OldPutFt, o.effPut, "ft"); p != "" {
			parts = append(parts, p)
		}
		if p := diffPart("take", o.run.OldTakeFt, o.effTake, "ft"); p != "" {
			parts = append(parts, p)
		}
		if p := diffPart("grad", o.run.OldGradFpm, o.grad, "ft/mi"); p != "" {
			parts = append(parts, p)
		}
		fmt.Printf("  %-45s  %s\n", o.run.Slug, joinParts(parts))
		if *dryRun {
			updated++
			continue
		}
		if _, err := pool.Exec(ctx,
			`UPDATE user_reaches SET put_in_elevation_ft = $1, take_out_elevation_ft = $2, gradient_fpm = $3 WHERE id = $4`,
			o.effPut, o.effTake, o.grad, o.run.ID,
		); err != nil {
			log.Printf("  WRITE ERROR %s: %v", o.run.Slug, err)
			failed++
			continue
		}
		updated++
	}

	fmt.Println()
	fmt.Println("── NEGATIVE GRADIENTS (take-out higher than put-in — bad data or a reversed centerline) ──")
	if len(negatives) == 0 {
		fmt.Println("  (none)")
	}
	for _, o := range negatives {
		fmt.Printf("  %-45s  %-40s  %.1f ft/mi\n", o.run.Slug, o.run.Name, *o.grad)
	}

	fmt.Println()
	fmt.Printf("summary: total=%d  updated=%d  unchanged=%d  failed=%d  negative_gradients=%d\n",
		len(todo), updated, unchanged, failed, len(negatives))
	if *dryRun {
		fmt.Println("(dry-run — nothing written)")
	}
}

type elevResult struct {
	ft  *float64
	err error
}

// queryElevationAsync resolves one coordinate's elevation in the background.
// Fails soft: ft is nil (err set) on timeout, network error, or unparseable
// response — never blocks the caller past elevationTimeout.
func queryElevationAsync(ctx context.Context, lng, lat float64) <-chan elevResult {
	ch := make(chan elevResult, 1)
	go func() {
		cctx, cancel := context.WithTimeout(ctx, elevationTimeout)
		defer cancel()
		v, err := elevation.QueryElevation(cctx, lng, lat)
		if err != nil {
			ch <- elevResult{nil, err}
			return
		}
		ch <- elevResult{&v, nil}
	}()
	return ch
}

// centerlineMiles returns the length of a GeoJSON LineString in miles, via
// PostGIS ST_Length on a geography cast. ok=false for a non-positive length
// (degenerate geometry — also the gradient divide-by-zero guard) or a query
// error.
func centerlineMiles(ctx context.Context, pool *pgxpool.Pool, geojson string) (miles float64, ok bool) {
	err := pool.QueryRow(ctx,
		`SELECT ST_Length(ST_GeomFromGeoJSON($1)::geography) / $2`,
		geojson, metersPerMile,
	).Scan(&miles)
	if err != nil || miles <= 0 {
		return 0, false
	}
	return miles, true
}

// roundPtr rounds to 1 decimal place (matching the NUMERIC(8,1)/(6,1) column
// precision) so a float64 that only differs from the stored value in noise
// below what the column can even represent doesn't register as a "change".
func roundPtr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	r := math.Round(*v*10) / 10
	return &r
}

// ftEqual compares two possibly-nil rounded values: nil==nil, else by value.
func ftEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// dispFt renders a nullable feet/gradient value, "∅" for NULL — mirrors
// cmd/backfill-run-state's dispState so a diff line never looks like a
// blank-column alignment glitch.
func dispFt(v *float64) string {
	if v == nil {
		return "∅"
	}
	return fmt.Sprintf("%.1f", *v)
}

// diffPart renders one "label: old→new unit" fragment, or "" when old and
// new are equal (post-rounding) — callers skip empty fragments so a row that
// only changed ONE of its three columns doesn't print two redundant
// old==new segments.
func diffPart(label string, oldV, newV *float64, unit string) string {
	if ftEqual(roundPtr(oldV), roundPtr(newV)) {
		return ""
	}
	return fmt.Sprintf("%s: %s→%s%s", label, dispFt(oldV), dispFt(newV), unit)
}

func joinParts(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "  "
		}
		out += p
	}
	return out
}

// loadRuns returns every non-deleted user_reaches row that has BOTH a
// put-in and a take-out coordinate to resolve elevation from.
func loadRuns(ctx context.Context, pool *pgxpool.Pool) ([]runRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, slug, name,
		       ST_Y(put_in::geometry), ST_X(put_in::geometry),
		       ST_Y(take_out::geometry), ST_X(take_out::geometry),
		       put_in_elevation_ft, take_out_elevation_ft, gradient_fpm,
		       CASE WHEN centerline IS NOT NULL THEN ST_AsGeoJSON(centerline::geometry) ELSE NULL END
		FROM user_reaches
		WHERE deleted_at IS NULL AND put_in IS NOT NULL AND take_out IS NOT NULL
		ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []runRow
	for rows.Next() {
		var r runRow
		if err := rows.Scan(
			&r.ID, &r.Slug, &r.Name,
			&r.PutInLat, &r.PutInLng, &r.TakeOutLat, &r.TakeOutLng,
			&r.OldPutFt, &r.OldTakeFt, &r.OldGradFpm,
			&r.Centerline,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
