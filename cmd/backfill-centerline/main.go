// backfill-centerline re-trims stale centerlines (#344): a run's centerline
// is generated once by fetching the NLDI mainstem and trimming it (PostGIS
// ST_LineSubstring) to the run's put_in/take_out. Before the fix in
// internal/handlers/user_reaches.go's Update, the JWT-authenticated in-app
// edit path wrote new put_in/take_out coordinates but never re-trimmed the
// stored centerline, so the line stayed anchored to the OLD endpoints while
// the markers moved on to the new ones — the line visibly overshoots or
// falls short. This command repairs the runs that went stale before that
// fix landed; Update (and the already-correct UploadUpdate) keep new edits
// in sync going forward, so this is a one-time (or occasional, re-runnable)
// repair tool, not a scheduled job.
//
// A run is "stale" when its stored centerline's start or end point is more
// than -threshold-m metres from the run's CURRENT put_in/take_out (measured
// via PostGIS: ST_StartPoint/ST_EndPoint of the centerline vs the
// put_in/take_out geography columns, spheroidal distance in metres). Only
// non-deleted runs with a non-NULL centerline are considered — a NULL
// centerline is not "stale," it's the normal 2-point-straight-line fallback
// state every client already renders correctly, and is out of scope here.
//
// Re-fetch + re-trim mirrors buildUploadCenterline (internal/handlers/
// run_upload.go) — same two exported kmlimport entry points, same PostGIS
// trim query — but reimplemented locally since that method is an unexported
// *UserReachHandler method (not reachable from cmd/main without importing
// the whole HTTP handler surface, which pulls in chi/auth/etc for no
// benefit to a small maintenance tool — the same tradeoff cmd/backfill-run-
// state already makes by calling nldi.StateAt directly instead of importing
// handlers.runStateFromCoords). Keep the trim query below in sync with
// internal/handlers/nldi.go's trimLineGeoJSON if it ever changes.
//
// UNLIKE the in-app Update/UploadUpdate fail-soft-to-NULL convention (an
// HTTP request that can't fully resolve a centerline still returns 200 with
// no line, letting the client's straight-line fallback render — visible
// immediately, correctable by the user on the next edit), a failed
// re-fetch/re-trim here leaves the existing stored centerline COMPLETELY
// UNTOUCHED. That row is already known-stale — worth fixing — but it is not
// this tool's job to make it worse: a batch job has the luxury of simply
// trying again on the next invocation, so "skip and report" is strictly
// safer than "clear a line that's still rendering something, even if it's
// wrong, with nobody watching to notice or re-save."
//
// Usage:
//
//	go run ./cmd/backfill-centerline                    # write changes
//	go run ./cmd/backfill-centerline -dry-run            # show stale runs + gaps, no writes, no NLDI calls
//	go run ./cmd/backfill-centerline -threshold-m 100    # looser gap tolerance
//	go run ./cmd/backfill-centerline -rate-ms 500        # slower between NLDI requests
//
// -dry-run answers entirely from PostGIS (no NLDI round trip is needed to
// REPORT staleness — only to FIX it), so it's cheap and safe to run
// repeatedly, including against prod read replicas / mirrors.
//
// Idempotent: re-running only considers rows still beyond the threshold, so
// a repeat run against already-fixed data (and a healthy NLDI endpoint)
// reports stale=0 / changed=0.
//
// Reads DATABASE_URL from the environment.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/h2oflow/h2oflow/apps/api/internal/db"
	"github.com/h2oflow/h2oflow/apps/api/internal/kmlimport"
	"github.com/jackc/pgx/v5/pgxpool"
)

// nldiFetchTimeout bounds each re-fetch the same way
// UserReachHandler.buildUploadCenterline (internal/handlers/run_upload.go)
// bounds its own NLDI round trip. Kept in sync manually — that method is
// unexported so its timeout can't be imported directly.
const nldiFetchTimeout = 30 * time.Second

// staleRow is one user_reaches candidate whose stored centerline's start or
// end point diverges from the run's current put_in/take_out beyond the
// configured threshold.
type staleRow struct {
	ID         string
	Slug       string
	Name       string
	PutInLat   float64
	PutInLng   float64
	TakeOutLat float64
	TakeOutLng float64
	UpComID    string // "" when NULL — a legitimate hint-absent state
	DownComID  string
	GapStartM  float64
	GapEndM    float64
}

func main() {
	dryRun := flag.Bool("dry-run", false, "show stale runs and their gaps without writing to DB or calling NLDI")
	thresholdM := flag.Float64("threshold-m", 50, "gap (metres) beyond which a centerline endpoint is considered stale")
	rateMs := flag.Int("rate-ms", 250, "min ms between NLDI requests")
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

	// Sanity check / FYI: runs with put_in+take_out but NO centerline at all
	// use the normal 2-point fallback and are out of scope for this
	// command — surfaced so a "why didn't run X show up" question has an
	// immediate answer instead of a silent exclusion.
	var noCenterline int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM user_reaches
		WHERE deleted_at IS NULL AND put_in IS NOT NULL AND take_out IS NOT NULL AND centerline IS NULL
	`).Scan(&noCenterline); err == nil && noCenterline > 0 {
		log.Printf("FYI: %d non-deleted run(s) have NO centerline (2-point fallback) — out of scope, not scanned", noCenterline)
	}

	all, err := loadCandidates(ctx, pool)
	if err != nil {
		log.Fatalf("query: %v", err)
	}
	if len(all) == 0 {
		log.Printf("no non-deleted runs with put_in + take_out + centerline found")
		return
	}

	var stale []staleRow
	var unchanged int
	for _, r := range all {
		if r.GapStartM > *thresholdM || r.GapEndM > *thresholdM {
			stale = append(stale, r)
		} else {
			unchanged++
		}
	}
	log.Printf("scanned=%d stale=%d (threshold %.0fm)", len(all), len(stale), *thresholdM)

	fmt.Println()
	fmt.Println("── STALE (slug — gap at put-in / gap at take-out) ───────────────")
	if len(stale) == 0 {
		fmt.Println("  (none)")
	}
	for _, r := range stale {
		fmt.Printf("  %-40s  %-40s  put_in=%8.1fm  take_out=%8.1fm\n", r.Slug, r.Name, r.GapStartM, r.GapEndM)
	}

	if *dryRun {
		fmt.Println()
		fmt.Printf("summary: scanned=%d changed=%d unchanged=%d failed=0\n", len(all), len(stale), unchanged)
		fmt.Println("(dry-run — nothing written, NLDI not queried)")
		return
	}

	var changed, failed int
	for i, r := range stale {
		time.Sleep(time.Duration(*rateMs) * time.Millisecond)

		cl, ok := refetchCenterline(ctx, pool, r)
		if !ok {
			log.Printf("  [%d/%d] FAILED   %-40s — re-fetch/trim unsuccessful, existing centerline left untouched", i+1, len(stale), r.Slug)
			failed++
			continue
		}

		tag, err := pool.Exec(ctx, `
			UPDATE user_reaches
			SET centerline = ST_GeomFromGeoJSON($1)::geography, updated_at = NOW()
			WHERE id = $2 AND deleted_at IS NULL
		`, cl, r.ID)
		if err != nil {
			log.Printf("  [%d/%d] WRITE ERROR  %-40s: %v", i+1, len(stale), r.Slug, err)
			failed++
			continue
		}
		if tag.RowsAffected() == 0 {
			log.Printf("  [%d/%d] SKIPPED  %-40s: no matching row (deleted mid-run?)", i+1, len(stale), r.Slug)
			failed++
			continue
		}
		log.Printf("  [%d/%d] fixed    %-40s", i+1, len(stale), r.Slug)
		changed++
	}

	fmt.Println()
	fmt.Printf("summary: scanned=%d changed=%d unchanged=%d failed=%d\n", len(all), changed, unchanged, failed)
}

// refetchCenterline re-fetches + re-trims one run's centerline from NLDI,
// mirroring buildUploadCenterline (internal/handlers/run_upload.go): the
// ComID-driven path when the run already has both hints stored (fast — no
// point-snap needed), the point-snap path otherwise. ok=false on ANY
// failure — fetch or trim — so the caller never writes a half-resolved
// result. This is intentionally STRICTER than buildUploadCenterline, which
// falls back to the untrimmed raw mainstem line if only the trim step
// fails; for a tool whose entire purpose is producing a correctly TRIMMED
// line, "fetched fine but didn't trim" is not a success worth persisting —
// an untrimmed multi-mile mainstem could be a worse map than the stale line
// already there.
func refetchCenterline(ctx context.Context, pool *pgxpool.Pool, r staleRow) (geojson string, ok bool) {
	nctx, cancel := context.WithTimeout(ctx, nldiFetchTimeout)
	defer cancel()

	var raw string
	var err error
	if r.UpComID != "" && r.DownComID != "" {
		raw, err = kmlimport.FetchCenterlinePreview(nctx, r.UpComID, r.DownComID)
	} else {
		raw, _, _, err = kmlimport.FetchCenterlineFromPoints(nctx, r.PutInLng, r.PutInLat, r.TakeOutLng, r.TakeOutLat)
	}
	if err != nil || raw == "" {
		return "", false
	}

	trimmed, terr := trimLineGeoJSON(nctx, pool, raw, r.PutInLng, r.PutInLat, r.TakeOutLng, r.TakeOutLat)
	if terr != nil || trimmed == "" {
		return "", false
	}
	return trimmed, true
}

// trimLineGeoJSON applies PostGIS ST_LineSubstring to trim the raw GeoJSON
// line to the closest points on the line to putIn/takeOut. A duplicate of
// the unexported internal/handlers/nldi.go function of the same name — see
// the package doc comment for why this isn't imported instead.
func trimLineGeoJSON(ctx context.Context, pool *pgxpool.Pool, geojson string, putInLon, putInLat, takeOutLon, takeOutLat float64) (string, error) {
	var result string
	err := pool.QueryRow(ctx, `
		SELECT ST_AsGeoJSON(
			ST_LineSubstring(
				line,
				LEAST(ST_LineLocatePoint(line, put_pt), ST_LineLocatePoint(line, take_pt)),
				GREATEST(ST_LineLocatePoint(line, put_pt), ST_LineLocatePoint(line, take_pt))
			)
		)
		FROM (
			SELECT
				ST_GeomFromGeoJSON($1)                                      AS line,
				ST_ClosestPoint(ST_GeomFromGeoJSON($1),
				    ST_SetSRID(ST_MakePoint($2, $3), 4326))                 AS put_pt,
				ST_ClosestPoint(ST_GeomFromGeoJSON($1),
				    ST_SetSRID(ST_MakePoint($4, $5), 4326))                 AS take_pt
		) sub
	`, geojson, putInLon, putInLat, takeOutLon, takeOutLat).Scan(&result)
	return result, err
}

// loadCandidates returns every non-deleted user_reaches row that has both a
// put_in/take_out and a stored centerline, with the current gap (metres)
// between the centerline's start/end and put_in/take_out already computed
// in PostGIS: centerline is a geography column, cast to geometry so
// ST_StartPoint/ST_EndPoint (geometry-only functions) can extract its
// endpoints, then cast back to geography so ST_Distance returns metres
// (spheroidal) rather than degrees. Rows with NULL centerline are excluded
// — see the package doc comment.
//
// gapStart/gapEnd are scanned as *float64 (pgx v5: nullable numeric needs a
// pointer target) because ST_StartPoint/ST_EndPoint return NULL for a
// non-LineString geometry (e.g. a degenerate single-point line) — treated
// as "cannot assess, skip" rather than crashing the whole run over one bad
// row.
func loadCandidates(ctx context.Context, pool *pgxpool.Pool) ([]staleRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT
			id, slug, name,
			ST_Y(put_in::geometry)   AS put_in_lat,
			ST_X(put_in::geometry)   AS put_in_lng,
			ST_Y(take_out::geometry) AS take_out_lat,
			ST_X(take_out::geometry) AS take_out_lng,
			COALESCE(up_comid, '')   AS up_comid,
			COALESCE(down_comid, '') AS down_comid,
			ST_Distance(ST_StartPoint(centerline::geometry)::geography, put_in)   AS gap_start_m,
			ST_Distance(ST_EndPoint(centerline::geometry)::geography,   take_out) AS gap_end_m
		FROM user_reaches
		WHERE deleted_at IS NULL
		  AND put_in IS NOT NULL
		  AND take_out IS NOT NULL
		  AND centerline IS NOT NULL
		ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []staleRow
	var skippedUnmeasurable int
	for rows.Next() {
		var r staleRow
		var gapStart, gapEnd *float64
		if err := rows.Scan(
			&r.ID, &r.Slug, &r.Name,
			&r.PutInLat, &r.PutInLng, &r.TakeOutLat, &r.TakeOutLng,
			&r.UpComID, &r.DownComID,
			&gapStart, &gapEnd,
		); err != nil {
			return nil, err
		}
		if gapStart == nil || gapEnd == nil {
			skippedUnmeasurable++
			continue
		}
		r.GapStartM, r.GapEndM = *gapStart, *gapEnd
		out = append(out, r)
	}
	if skippedUnmeasurable > 0 {
		log.Printf("WARNING: %d run(s) have a centerline whose start/end point could not be measured (not a simple LineString?) — skipped", skippedUnmeasurable)
	}
	return out, rows.Err()
}
