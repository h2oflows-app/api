// backfill-run-state resolves and writes user_reaches.state_abbr (#356) —
// the per-run US state that migration 000149 added. Every run is recomputed
// from its OWN put-in via nldi.StateAt (TIGERweb Census lookup), the same
// service the live Create/Import/Update handlers use
// (internal/handlers/user_reaches.go's runStateFromCoords) — never borrowed
// from the shared rivers.state_abbr row, since one river spanning multiple
// states (e.g. the Colorado River: Grand Canyon in AZ, Westwater/Cataract/
// Moab reaches in UT) is exactly the bug this column exists to fix.
//
// Processes every non-deleted user_reaches row, not just the runs already
// known to be wrong — no run has ever had an independently verified state,
// so the existing value (river-level, possibly right, possibly wrong) is
// never trusted as a starting point; it's always recomputed from scratch.
//
// Usage:
//
//	go run ./cmd/backfill-run-state              # write changes
//	go run ./cmd/backfill-run-state -dry-run      # show changes, no writes
//	go run ./cmd/backfill-run-state -rate-ms 300  # slower between lookups
//
// Idempotent: re-running recomputes from the same put-in and only writes
// rows whose resolved state actually differs from what's already stored, so
// a repeat run against a healthy TIGERweb endpoint reports updated=0.
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
	"github.com/h2oflow/h2oflow/apps/api/internal/nldi"
	"github.com/jackc/pgx/v5/pgxpool"
)

// runRow is one user_reaches candidate: its identity, current stored state
// ("" means NULL), and put-in coordinate to re-resolve from.
type runRow struct {
	ID       string
	Slug     string
	Name     string
	OldState string
	Lat      float64
	Lng      float64
}

func main() {
	dryRun := flag.Bool("dry-run", false, "show changes without writing to DB")
	rateMs := flag.Int("rate-ms", 250, "min ms between TIGERweb requests")
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

	// Sanity check: put_in is required at Create/Import time, so this should
	// always be zero — surfaced rather than silently excluded from the run
	// below (which requires a real coordinate to resolve against).
	var missingPutIn int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM user_reaches WHERE deleted_at IS NULL AND put_in IS NULL`,
	).Scan(&missingPutIn); err == nil && missingPutIn > 0 {
		log.Printf("WARNING: %d non-deleted run(s) have no put_in at all — skipped, cannot resolve state", missingPutIn)
	}

	todo, err := loadRuns(ctx, pool)
	if err != nil {
		log.Fatalf("query: %v", err)
	}
	if len(todo) == 0 {
		log.Printf("no non-deleted user_reaches rows with a put_in found")
		return
	}
	log.Printf("runs to process: %d", len(todo))
	if *dryRun {
		log.Printf("dry-run mode — no writes")
	}

	type result struct {
		run       runRow
		newState  string
		lookupErr error
	}
	results := make([]result, 0, len(todo))
	for i, r := range todo {
		time.Sleep(time.Duration(*rateMs) * time.Millisecond)
		newState, lookupErr := nldi.StateAt(ctx, r.Lat, r.Lng)
		results = append(results, result{run: r, newState: newState, lookupErr: lookupErr})

		status := fmt.Sprintf("%s → %s", dispState(r.OldState), dispState(newState))
		if lookupErr != nil {
			status = fmt.Sprintf("ERROR: %v (kept %s)", lookupErr, dispState(r.OldState))
		}
		log.Printf("  [%d/%d] %-45s put_in=(%.4f,%.4f)  %s", i+1, len(todo), r.Name, r.Lat, r.Lng, status)
	}

	var updated, unchanged, failed int
	var changes []result
	for _, res := range results {
		switch {
		case res.lookupErr != nil:
			// Network/parse failure — never write; leave whatever is
			// already stored (NULL on first run, a prior good value on a
			// re-run) rather than blank out a possibly-correct value on a
			// transient error.
			failed++
		case res.newState != res.run.OldState:
			changes = append(changes, res)
		default:
			unchanged++
		}
	}

	fmt.Println()
	fmt.Println("── CHANGED (old → new) ──────────────────────────────────────")
	if len(changes) == 0 {
		fmt.Println("  (none)")
	}
	for _, res := range changes {
		fmt.Printf("  %-45s  %-40s  %-4s → %-4s\n",
			res.run.Slug, res.run.Name, dispState(res.run.OldState), dispState(res.newState))
		if *dryRun {
			updated++
			continue
		}
		if _, err := pool.Exec(ctx,
			`UPDATE user_reaches SET state_abbr = NULLIF($1, '') WHERE id = $2`,
			res.newState, res.run.ID,
		); err != nil {
			log.Printf("  WRITE ERROR %s: %v", res.run.Slug, err)
			failed++
			continue
		}
		updated++
	}

	fmt.Println()
	fmt.Printf("summary: total=%d  updated=%d  unchanged=%d  failed=%d\n", len(todo), updated, unchanged, failed)
	if *dryRun {
		fmt.Println("(dry-run — nothing written)")
	}
}

// dispState renders "" (NULL) as a visibly-empty marker so old→new diff
// lines never look like a blank column alignment glitch.
func dispState(s string) string {
	if s == "" {
		return "∅"
	}
	return s
}

// loadRuns returns every non-deleted user_reaches row that has a put-in
// coordinate to resolve state from. Coalescing state_abbr to an empty
// string in SQL sidesteps a nullable-text scan (pgx v5): an empty Go string
// and SQL NULL are interchangeable here since the column only ever holds
// NULL or a real 2-letter code, never an empty string.
func loadRuns(ctx context.Context, pool *pgxpool.Pool) ([]runRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, slug, name, COALESCE(state_abbr, ''),
		       ST_Y(put_in::geometry), ST_X(put_in::geometry)
		FROM user_reaches
		WHERE deleted_at IS NULL AND put_in IS NOT NULL
		ORDER BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []runRow
	for rows.Next() {
		var r runRow
		if err := rows.Scan(&r.ID, &r.Slug, &r.Name, &r.OldState, &r.Lat, &r.Lng); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
