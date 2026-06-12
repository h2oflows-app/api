// backfill-river-gnis resolves and repairs river identity (gnis_id, state_abbr,
// basin, huc8) for rivers that are missing one or more of these fields.
//
// Pass 1 — gnis_id IS NULL: queries NHD by name; fills uniquely-matched rows;
//
//	reports ambiguous and unmatched rivers for manual review.
//
// Pass 2 — gnis_id IS SET but state_abbr or basin IS NULL: re-derives geo-meta
//
//	from the stored gnis_id via NHD → StateAt + BasinAt.
//
// Usage:
//
//	go run ./cmd/backfill-river-gnis              # write changes (both passes)
//	go run ./cmd/backfill-river-gnis -dry-run     # show changes, no writes
//	go run ./cmd/backfill-river-gnis -pass1-only  # only gnis_id IS NULL
//	go run ./cmd/backfill-river-gnis -pass2-only  # only missing meta on known gnis
//
// Reads DATABASE_URL from the environment.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/h2oflow/h2oflow/apps/api/internal/db"
	gauge "github.com/h2oflow/h2oflow/apps/api/internal/gaugecore"
	"github.com/h2oflow/h2oflow/apps/api/internal/nldi"
	"github.com/jackc/pgx/v5/pgxpool"
)

type riverRow struct {
	ID     string
	Name   string
	GnisID string // non-empty only for pass-2 rows
}

func main() {
	dryRun    := flag.Bool("dry-run", false, "show changes without writing to DB")
	rateMs    := flag.Int("rate-ms", 800, "min ms between NHD requests")
	pass1Only := flag.Bool("pass1-only", false, "only run pass 1 (gnis_id IS NULL)")
	pass2Only := flag.Bool("pass2-only", false, "only run pass 2 (missing state/basin on known gnis)")
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

	runPass1 := !*pass2Only
	runPass2 := !*pass1Only

	// ── Pass 1: gnis_id IS NULL ──────────────────────────────────────────────
	if runPass1 {
		fmt.Println()
		fmt.Println("═══ Pass 1: resolve gnis_id for rivers with gnis_id IS NULL ════")
		todo, err := loadNullGNIS(ctx, pool)
		if err != nil {
			log.Fatalf("pass1 query: %v", err)
		}
		if len(todo) == 0 {
			log.Printf("pass1: no rivers need GNIS backfill")
		} else {
			log.Printf("pass1: rivers without gnis_id: %d", len(todo))
			if *dryRun {
				log.Printf("dry-run mode — no writes")
			}
			runPass1Loop(ctx, pool, todo, *dryRun, *rateMs)
		}
	}

	// ── Pass 2: gnis_id set but state_abbr or basin NULL ────────────────────
	if runPass2 {
		fmt.Println()
		fmt.Println("═══ Pass 2: fill missing state/basin for rivers with known gnis_id ═")
		todo2, err := loadMissingMeta(ctx, pool)
		if err != nil {
			log.Fatalf("pass2 query: %v", err)
		}
		if len(todo2) == 0 {
			log.Printf("pass2: no rivers need meta backfill")
		} else {
			log.Printf("pass2: rivers with gnis_id but missing state/basin: %d", len(todo2))
			if *dryRun {
				log.Printf("dry-run mode — no writes")
			}
			runPass2Loop(ctx, pool, todo2, *dryRun, *rateMs)
		}
	}
}

// ── Pass 1 helpers ────────────────────────────────────────────────────────────

func runPass1Loop(ctx context.Context, pool *pgxpool.Pool, todo []riverRow, dryRun bool, rateMs int) {
	type result struct {
		river     riverRow
		matches   []nldi.NHDNameResult
		lookupErr error
	}
	results := make([]result, 0, len(todo))
	for i, r := range todo {
		time.Sleep(time.Duration(rateMs) * time.Millisecond)
		matches, err := nldi.NHDLookupByName(ctx, r.Name)
		results = append(results, result{river: r, matches: matches, lookupErr: err})
		status := fmt.Sprintf("%d matches", len(matches))
		if err != nil {
			status = fmt.Sprintf("ERROR: %v", err)
		}
		log.Printf("  [%d/%d] %q: %s", i+1, len(todo), r.Name, status)
	}

	var filled, ambiguous, unmatched, errored []result
	for _, res := range results {
		switch {
		case res.lookupErr != nil:
			errored = append(errored, res)
		case len(res.matches) == 0:
			unmatched = append(unmatched, res)
		case len(res.matches) == 1:
			filled = append(filled, res)
		default:
			ambiguous = append(ambiguous, res)
		}
	}

	fmt.Println()
	fmt.Println("── FILLED (unique match) ────────────────────────────────────")
	sort.Slice(filled, func(i, j int) bool { return filled[i].river.Name < filled[j].river.Name })
	for _, res := range filled {
		m := res.matches[0]
		huc8display := m.HUC8
		if huc8display == "" {
			huc8display = "—"
		}
		fmt.Printf("  %-40s  gnis_id=%-10s  huc8=%s\n", res.river.Name, m.GnisID, huc8display)
		if !dryRun {
			if err := applyGNIS(ctx, pool, res.river.ID, m.GnisID); err != nil {
				log.Printf("  WRITE ERROR %s: %v", res.river.Name, err)
			}
		}
	}

	fmt.Println()
	fmt.Println("── AMBIGUOUS (multiple GNIS IDs) — manual review needed ─────")
	sort.Slice(ambiguous, func(i, j int) bool { return ambiguous[i].river.Name < ambiguous[j].river.Name })
	for _, res := range ambiguous {
		ids := make([]string, len(res.matches))
		for i, m := range res.matches {
			ids[i] = m.GnisID
		}
		fmt.Printf("  %-40s  candidates: %s\n", res.river.Name, strings.Join(ids, ", "))
	}

	fmt.Println()
	fmt.Println("── UNMATCHED — no NHD feature found ────────────────────────")
	sort.Slice(unmatched, func(i, j int) bool { return unmatched[i].river.Name < unmatched[j].river.Name })
	for _, res := range unmatched {
		fmt.Printf("  %s  (id=%s)\n", res.river.Name, res.river.ID)
	}

	if len(errored) > 0 {
		fmt.Println()
		fmt.Println("── ERRORS ───────────────────────────────────────────────────")
		for _, res := range errored {
			fmt.Printf("  %s: %v\n", res.river.Name, res.lookupErr)
		}
	}

	fmt.Println()
	fmt.Printf("pass1 summary: filled=%d  ambiguous=%d  unmatched=%d  errors=%d\n",
		len(filled), len(ambiguous), len(unmatched), len(errored))
}

// ── Pass 2 helpers ────────────────────────────────────────────────────────────

func runPass2Loop(ctx context.Context, pool *pgxpool.Pool, todo []riverRow, dryRun bool, rateMs int) {
	type result struct {
		river     riverRow
		stateAbbr string
		basin     string
		huc8      string
		err       error
	}
	results := make([]result, 0, len(todo))
	for i, r := range todo {
		time.Sleep(time.Duration(rateMs) * time.Millisecond)
		stateAbbr, basin, huc8, err := resolveMetaByGNIS(ctx, r.GnisID)
		results = append(results, result{river: r, stateAbbr: stateAbbr, basin: basin, huc8: huc8, err: err})
		status := fmt.Sprintf("state=%q  basin=%q  huc8=%q", stateAbbr, basin, huc8)
		if err != nil {
			status = fmt.Sprintf("ERROR: %v", err)
		}
		log.Printf("  [%d/%d] %q  gnis=%s: %s", i+1, len(todo), r.Name, r.GnisID, status)
	}

	var okCount, errCount int
	fmt.Println()
	fmt.Println("── BACKFILLED ───────────────────────────────────────────────")
	for _, res := range results {
		if res.err != nil {
			fmt.Printf("  ERROR %-35s  gnis=%-10s: %v\n", res.river.Name, res.river.GnisID, res.err)
			errCount++
			continue
		}
		fmt.Printf("  %-35s  gnis=%-10s  state=%-4s  basin=%q\n",
			res.river.Name, res.river.GnisID, res.stateAbbr, res.basin)
		if !dryRun {
			_, err := pool.Exec(ctx, `
				UPDATE rivers
				SET    state_abbr = COALESCE(state_abbr, NULLIF($1,'')),
				       basin      = COALESCE(basin,      NULLIF($2,'')),
				       huc8       = COALESCE(huc8,       NULLIF($3,''))
				WHERE  id = $4
			`, res.stateAbbr, res.basin, res.huc8, res.river.ID)
			if err != nil {
				log.Printf("  WRITE ERROR %s: %v", res.river.Name, err)
				errCount++
				continue
			}
		}
		okCount++
	}

	fmt.Println()
	fmt.Printf("pass2 summary: backfilled=%d  errors=%d\n", okCount, errCount)
	if dryRun {
		fmt.Println("(dry-run — nothing written)")
	}
}

func loadNullGNIS(ctx context.Context, pool *pgxpool.Pool) ([]riverRow, error) {
	rows, err := pool.Query(ctx, `SELECT id, name FROM rivers WHERE gnis_id IS NULL ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []riverRow
	for rows.Next() {
		var r riverRow
		if err := rows.Scan(&r.ID, &r.Name); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// loadMissingMeta returns rivers that have a gnis_id but are missing state_abbr
// or basin — the "corrupted" rows that grouping requires.
func loadMissingMeta(ctx context.Context, pool *pgxpool.Pool) ([]riverRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, name, gnis_id
		FROM   rivers
		WHERE  gnis_id IS NOT NULL
		  AND  (state_abbr IS NULL OR basin IS NULL)
		ORDER  BY name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []riverRow
	for rows.Next() {
		var r riverRow
		if err := rows.Scan(&r.ID, &r.Name, &r.GnisID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// resolveMetaByGNIS returns (stateAbbr, basin, huc8) for a known gnis_id.
func resolveMetaByGNIS(ctx context.Context, gnisID string) (stateAbbr, basin, huc8 string, err error) {
	coord, err := nldi.NHDCoordByGNISID(ctx, gnisID)
	if err != nil {
		return
	}

	type stateRes struct {
		val string
		err error
	}
	type basinRes struct {
		info nldi.BasinInfo
		err  error
	}
	sCh := make(chan stateRes, 1)
	bCh := make(chan basinRes, 1)
	go func() { v, e := nldi.StateAt(ctx, coord.Lat, coord.Lng); sCh <- stateRes{v, e} }()
	go func() { v, e := nldi.BasinAt(ctx, coord.Lat, coord.Lng); bCh <- basinRes{v, e} }()
	sr := <-sCh
	br := <-bCh

	stateAbbr = sr.val
	if br.err == nil {
		huc8 = br.info.HUC8
		basin = gauge.CanonicalBasin(huc8)
		if stateAbbr == "" && br.info.States != "" {
			stateAbbr = strings.SplitN(br.info.States, ",", 2)[0]
		}
	}
	return
}

// applyGNIS writes gnis_id to rivers, then updates state_abbr/basin/huc8 via
// NHD coord → StateAt + BasinAt. Used by pass-1 (gnis_id IS NULL rows).
func applyGNIS(ctx context.Context, pool *pgxpool.Pool, riverID, gnisID string) error {
	stateAbbr, basin, huc8, err := resolveMetaByGNIS(ctx, gnisID)
	if err != nil {
		// Write gnis_id alone — meta can be filled on the next run or by admin.
		_, err2 := pool.Exec(ctx, `UPDATE rivers SET gnis_id = $1 WHERE id = $2`, gnisID, riverID)
		return err2
	}

	_, err = pool.Exec(ctx, `
		UPDATE rivers
		SET    gnis_id    = $1,
		       state_abbr = COALESCE(NULLIF($2,''), state_abbr),
		       basin      = COALESCE(NULLIF($3,''), basin),
		       huc8       = COALESCE(NULLIF($4,''), huc8)
		WHERE  id = $5
	`, gnisID, stateAbbr, basin, huc8, riverID)
	return err
}
