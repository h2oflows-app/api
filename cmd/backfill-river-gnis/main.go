// backfill-river-gnis resolves GNIS IDs for rivers that pre-date the gnis_id
// column. For each river with gnis_id IS NULL it queries NHD by name, fills
// uniquely-matched rows, and reports ambiguous / unmatched rivers.
//
// Usage:
//
//	go run ./cmd/backfill-river-gnis              # write changes
//	go run ./cmd/backfill-river-gnis -dry-run     # show changes, no writes
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
	ID   string
	Name string
}

func main() {
	dryRun := flag.Bool("dry-run", false, "show changes without writing to DB")
	rateMs := flag.Int("rate-ms", 800, "min ms between NHD requests")
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

	todo, err := loadNullGNIS(ctx, pool)
	if err != nil {
		log.Fatalf("query: %v", err)
	}
	if len(todo) == 0 {
		log.Printf("no rivers need GNIS backfill")
		return
	}
	log.Printf("rivers without gnis_id: %d", len(todo))
	if *dryRun {
		log.Printf("dry-run mode — no writes")
	}

	type result struct {
		river     riverRow
		matches   []nldi.NHDNameResult
		lookupErr error
	}
	results := make([]result, 0, len(todo))

	for i, r := range todo {
		time.Sleep(time.Duration(*rateMs) * time.Millisecond)
		matches, err := nldi.NHDLookupByName(ctx, r.Name)
		results = append(results, result{river: r, matches: matches, lookupErr: err})
		status := fmt.Sprintf("%d matches", len(matches))
		if err != nil {
			status = fmt.Sprintf("ERROR: %v", err)
		}
		log.Printf("[%d/%d] %q: %s", i+1, len(todo), r.Name, status)
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

	// ── Write unique matches ──────────────────────────────────────────────────
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
		if !*dryRun {
			if err := applyGNIS(ctx, pool, res.river.ID, m.GnisID); err != nil {
				log.Printf("  WRITE ERROR %s: %v", res.river.Name, err)
			}
		}
	}

	// ── Report ambiguous ──────────────────────────────────────────────────────
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

	// ── Report unmatched ──────────────────────────────────────────────────────
	fmt.Println()
	fmt.Println("── UNMATCHED — no NHD feature found ────────────────────────")
	sort.Slice(unmatched, func(i, j int) bool { return unmatched[i].river.Name < unmatched[j].river.Name })
	for _, res := range unmatched {
		fmt.Printf("  %s  (id=%s)\n", res.river.Name, res.river.ID)
	}

	// ── Report errors ─────────────────────────────────────────────────────────
	if len(errored) > 0 {
		fmt.Println()
		fmt.Println("── ERRORS ───────────────────────────────────────────────────")
		for _, res := range errored {
			fmt.Printf("  %s: %v\n", res.river.Name, res.lookupErr)
		}
	}

	fmt.Println()
	fmt.Printf("summary: filled=%d  ambiguous=%d  unmatched=%d  errors=%d\n",
		len(filled), len(ambiguous), len(unmatched), len(errored))
	if *dryRun {
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

// applyGNIS writes gnis_id to rivers, then updates state_abbr/basin/huc8 via
// riverMetaFromGNIS-equivalent logic (NHD coord → StateAt + BasinAt).
func applyGNIS(ctx context.Context, pool *pgxpool.Pool, riverID, gnisID string) error {
	coord, err := nldi.NHDCoordByGNISID(ctx, gnisID)
	if err != nil {
		// Write gnis_id alone — meta can be filled by admin later.
		_, err2 := pool.Exec(ctx, `UPDATE rivers SET gnis_id = $1 WHERE id = $2`, gnisID, riverID)
		return err2
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

	stateAbbr := sr.val
	var basin, huc8 string
	if br.err == nil {
		huc8 = br.info.HUC8
		basin = gauge.CanonicalBasin(huc8)
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
