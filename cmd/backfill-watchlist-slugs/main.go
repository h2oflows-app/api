// backfill-watchlist-slugs relinks orphaned user_watchlists.reach_slug values
// (#395) — stale bare-name slugs left over from the retired `reaches` table
// (dropped in migs 000122/000123) that no longer resolve to any live
// user_reaches row, causing gauge groups to render as "Unknown River".
//
// Matching strategy, in priority order:
//
//  1. Gauge-anchored: use the watchlist row's gauge_id/custom_gauge_id to
//     find every live user_reaches row sharing that same gauge. This is a
//     structural signal, not a string guess — it can only ever propose runs
//     that are physically on the same monitored stretch of river.
//
//  2. Within that gauge-anchored candidate set, disambiguate with
//     normalized name/slug text comparison (trailing "-<digit>" uniqueness
//     suffixes stripped from the old slug; both the candidate's slug and
//     its slugified name are checked, since a run's current slug and its
//     display name can diverge from the old bare slug in different ways —
//     e.g. old slug "dowd-chute" matches live run name "Dowd Chute" whose
//     slug is "eagle-river-fs-visitor-center-to-river-run"). A candidate
//     owned by the SAME user as the watchlist row is preferred over a
//     higher-scoring curated candidate, provided it has any textual
//     relation at all — a user's own reach sharing the gauge is a stronger
//     signal than an unrelated curated reach with a marginally tighter
//     string match.
//
//  3. Only when a row has no gauge/custom_gauge to anchor on, or that gauge
//     currently has zero live reaches, fall back to an EXACT normalized
//     name/slug match scoped to reaches owned by the SAME user as the
//     watchlist row. Never a global fuzzy match — a coincidental name
//     collision on someone else's reach is exactly the kind of wrong-river
//     relink this tool must refuse to make.
//
// A row is never written unless exactly one candidate wins. Ties, zero-score
// sole candidates, and rows with no candidate at all are reported separately
// for human review rather than guessed.
//
// Usage:
//
//	go run ./cmd/backfill-watchlist-slugs              # write changes
//	go run ./cmd/backfill-watchlist-slugs -dry-run      # show changes, no writes
//
// Idempotent: only rows whose reach_slug fails to resolve to a live
// user_reaches row are considered, so a repeat run after a successful
// relink finds that row already resolved and skips it. Ambiguous/unmatched
// rows are re-reported on every run until fixed out-of-band — this is
// intentional, not a bug: the tool never guesses.
//
// Reads DATABASE_URL from the environment.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/h2oflow/h2oflow/apps/api/internal/db"
	"github.com/h2oflow/h2oflow/apps/api/internal/kmlimport"
	"github.com/jackc/pgx/v5/pgxpool"
)

// orphanRow is one user_watchlists row whose reach_slug does not resolve to
// any live (deleted_at IS NULL) user_reaches row.
type orphanRow struct {
	ID            string
	UserID        string
	ReachSlug     string
	GaugeID       *string
	CustomGaugeID *string
}

// candidate is a live user_reaches row eligible to receive a relink.
type candidate struct {
	ID      string
	Slug    string
	Name    string
	OwnerID string
}

// outcome is the resolved fate of one orphaned row.
type outcome struct {
	row     orphanRow
	status  string // "relink" | "ambiguous" | "unmatched"
	newSlug string
	newID   string
	basis   string
	detail  string // extra context: competing candidates, near-misses, etc.
}

// trailingNumRe strips a trailing "-<digits>" uniqueness suffix that the old
// `reaches` table appended when a base slug collided (e.g. "the-numbers-2",
// "box-canyon-to-creede-2") — an artifact of that table's global slug
// namespace, not necessarily meaningful to what the run actually is.
var trailingNumRe = regexp.MustCompile(`^(.+)-([0-9]+)$`)

func normalizeSlug(s string) string {
	if m := trailingNumRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

func main() {
	dryRun := flag.Bool("dry-run", false, "show matches without writing to DB")
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

	orphans, err := loadOrphans(ctx, pool)
	if err != nil {
		log.Fatalf("query orphans: %v", err)
	}
	if len(orphans) == 0 {
		log.Printf("no orphaned user_watchlists.reach_slug rows found")
		return
	}
	log.Printf("orphaned watchlist rows to process: %d", len(orphans))
	if *dryRun {
		log.Printf("dry-run mode — no writes")
	}

	var results []outcome
	for _, row := range orphans {
		results = append(results, resolveRow(ctx, pool, row))
	}
	sort.Slice(results, func(i, j int) bool { return results[i].row.ReachSlug < results[j].row.ReachSlug })

	var relinks, ambiguous, unmatched []outcome
	for _, res := range results {
		switch res.status {
		case "relink":
			relinks = append(relinks, res)
		case "ambiguous":
			ambiguous = append(ambiguous, res)
		default:
			unmatched = append(unmatched, res)
		}
	}

	fmt.Println()
	fmt.Println("── RELINK (old → new) ───────────────────────────────────────")
	if len(relinks) == 0 {
		fmt.Println("  (none)")
	}
	var writeFailed int
	for _, res := range relinks {
		fmt.Printf("  %-35s → %-50s [%s]\n", res.row.ReachSlug, res.newSlug, res.basis)
		if res.detail != "" {
			fmt.Printf("      note: %s\n", res.detail)
		}
		if *dryRun {
			continue
		}
		if _, err := pool.Exec(ctx,
			`UPDATE user_watchlists SET reach_slug = $1 WHERE id = $2`,
			res.newSlug, res.row.ID,
		); err != nil {
			log.Printf("  WRITE ERROR row=%s (%s → %s): %v", res.row.ID, res.row.ReachSlug, res.newSlug, err)
			writeFailed++
		}
	}

	fmt.Println()
	fmt.Println("── AMBIGUOUS (human review — nothing written) ──────────────")
	if len(ambiguous) == 0 {
		fmt.Println("  (none)")
	}
	for _, res := range ambiguous {
		fmt.Printf("  %-35s (user=%s)\n", res.row.ReachSlug, res.row.UserID)
		fmt.Printf("      %s\n", res.detail)
	}

	fmt.Println()
	fmt.Println("── UNMATCHED (no live candidate found) ──────────────────────")
	if len(unmatched) == 0 {
		fmt.Println("  (none)")
	}
	for _, res := range unmatched {
		fmt.Printf("  %-35s (user=%s)  %s\n", res.row.ReachSlug, res.row.UserID, res.detail)
	}

	relinkedCount := len(relinks)
	if *dryRun {
		relinkedCount = 0
	}
	fmt.Println()
	fmt.Printf("summary: total=%d  relinked=%d  ambiguous=%d  unmatched=%d  write_failed=%d\n",
		len(orphans), func() int {
			if *dryRun {
				return 0
			}
			return len(relinks) - writeFailed
		}(), len(ambiguous), len(unmatched), writeFailed)
	if *dryRun {
		fmt.Printf("(dry-run — nothing written; %d row(s) matched and would be relinked)\n", len(relinks))
	}
	_ = relinkedCount
}

// loadOrphans returns every user_watchlists row with a non-null reach_slug
// that does not resolve to a live user_reaches row.
func loadOrphans(ctx context.Context, pool *pgxpool.Pool) ([]orphanRow, error) {
	rows, err := pool.Query(ctx, `
		SELECT uw.id, uw.user_id, uw.reach_slug, uw.gauge_id::text, uw.custom_gauge_id::text
		FROM user_watchlists uw
		LEFT JOIN user_reaches ur ON ur.slug = uw.reach_slug AND ur.deleted_at IS NULL
		WHERE uw.reach_slug IS NOT NULL AND ur.id IS NULL
		ORDER BY uw.reach_slug
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []orphanRow
	for rows.Next() {
		var r orphanRow
		if err := rows.Scan(&r.ID, &r.UserID, &r.ReachSlug, &r.GaugeID, &r.CustomGaugeID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// gaugeCandidates returns every live user_reaches row sharing the given
// gauge (standard or custom). Global — not scoped by owner — since the
// correct target is very often a curated (h2oflows-owned) reach rather than
// one owned by the watchlist's own user.
func gaugeCandidates(ctx context.Context, pool *pgxpool.Pool, gaugeID, customGaugeID *string) ([]candidate, error) {
	var (
		rows pgxRows
		err  error
	)
	switch {
	case gaugeID != nil:
		rows, err = pool.Query(ctx,
			`SELECT id, slug, name, owner_id FROM user_reaches WHERE primary_gauge_id = $1::uuid AND deleted_at IS NULL`,
			*gaugeID)
	case customGaugeID != nil:
		rows, err = pool.Query(ctx,
			`SELECT id, slug, name, owner_id FROM user_reaches WHERE custom_gauge_id = $1::uuid AND deleted_at IS NULL`,
			*customGaugeID)
	default:
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return scanCandidates(rows)
}

// ownerCandidates returns every live user_reaches row owned by the given
// user — the fallback pool when no gauge anchor is available.
func ownerCandidates(ctx context.Context, pool *pgxpool.Pool, ownerID string) ([]candidate, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, slug, name, owner_id FROM user_reaches WHERE owner_id = $1 AND deleted_at IS NULL`,
		ownerID)
	if err != nil {
		return nil, err
	}
	return scanCandidates(rows)
}

// pgxRows is the minimal surface used from *pgxpool.Rows — keeps
// scanCandidates usable for either query above without importing pgx.Rows
// directly into the function signature.
type pgxRows interface {
	Next() bool
	Scan(...any) error
	Close()
	Err() error
}

func scanCandidates(rows pgxRows) ([]candidate, error) {
	defer rows.Close()
	var out []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.ID, &c.Slug, &c.Name, &c.OwnerID); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// textScore rates how strongly a candidate's slug or slugified name relates
// to the normalized old slug: 3 = exact match on either field, 2 = one is a
// hyphen-bounded prefix/suffix of the other on either field, 0 = no relation
// found on either field.
func textScore(oldNorm string, c candidate) int {
	nameSlug := kmlimport.Slugify(c.Name)
	best := 0
	for _, field := range []string{c.Slug, nameSlug} {
		if field == "" {
			continue
		}
		if field == oldNorm {
			return 3
		}
		if strings.HasPrefix(field, oldNorm+"-") || strings.HasSuffix(field, "-"+oldNorm) ||
			strings.HasPrefix(oldNorm, field+"-") || strings.HasSuffix(oldNorm, "-"+field) {
			best = 2
		}
	}
	return best
}

func describeCandidates(oldNorm string, cands []candidate, ownerID string) string {
	var parts []string
	for _, c := range cands {
		tag := "curated/other-owner"
		if c.OwnerID == ownerID {
			tag = "same-owner"
		}
		parts = append(parts, fmt.Sprintf("%s (name=%q, %s, score=%d)", c.Slug, c.Name, tag, textScore(oldNorm, c)))
	}
	return "candidates: " + strings.Join(parts, "; ")
}

// resolveRow decides the fate of one orphaned watchlist row: gauge-anchored
// match first, same-owner exact-match fallback second, unmatched otherwise.
func resolveRow(ctx context.Context, pool *pgxpool.Pool, row orphanRow) outcome {
	oldNorm := normalizeSlug(row.ReachSlug)

	if row.GaugeID != nil || row.CustomGaugeID != nil {
		cands, err := gaugeCandidates(ctx, pool, row.GaugeID, row.CustomGaugeID)
		if err != nil {
			return outcome{row: row, status: "unmatched", detail: fmt.Sprintf("gauge candidate query failed: %v", err)}
		}
		if len(cands) > 0 {
			return pickFromGauge(row, oldNorm, cands)
		}
		// Gauge anchor present but resolves to zero live reaches — fall
		// through to the same-owner exact-match fallback below rather than
		// giving up immediately, then report unmatched with a reason that
		// makes the zero-candidate gauge state visible.
	}

	ownerCands, err := ownerCandidates(ctx, pool, row.UserID)
	if err != nil {
		return outcome{row: row, status: "unmatched", detail: fmt.Sprintf("owner candidate query failed: %v", err)}
	}
	if len(ownerCands) == 0 {
		return outcome{row: row, status: "unmatched", detail: "no gauge anchor resolved to a live reach, and this user owns no live reaches to fall back on"}
	}
	return pickFromOwnerFallback(row, oldNorm, ownerCands)
}

// pickFromGauge resolves a match among reaches sharing the watchlist row's
// gauge. A same-owner candidate with any textual relation (score > 0) wins
// over a higher-scoring candidate owned by someone else — see the package
// doc comment for the rationale (elevenmile: same-owner "elevenmile-canyon"
// at score 2 is preferred over curated "south-platte-river-elevenmile-canyon"
// at score 3, an exact name match but not the user's own reach).
func pickFromGauge(row orphanRow, oldNorm string, cands []candidate) outcome {
	var sameOwnerHits []candidate
	maxScore := 0
	for _, c := range cands {
		s := textScore(oldNorm, c)
		if s > maxScore {
			maxScore = s
		}
		if c.OwnerID == row.UserID && s > 0 {
			sameOwnerHits = append(sameOwnerHits, c)
		}
	}

	if len(sameOwnerHits) == 1 {
		note := ""
		if maxScore > textScore(oldNorm, sameOwnerHits[0]) {
			note = fmt.Sprintf("preferred same-owner reach over a higher-scoring candidate — %s", describeCandidates(oldNorm, cands, row.UserID))
		}
		return outcome{
			row: row, status: "relink",
			newSlug: sameOwnerHits[0].Slug, newID: sameOwnerHits[0].ID,
			basis:  fmt.Sprintf("gauge match, same-owner reach (score=%d)", textScore(oldNorm, sameOwnerHits[0])),
			detail: note,
		}
	}
	if len(sameOwnerHits) > 1 {
		return outcome{row: row, status: "ambiguous",
			detail: "multiple same-owner reaches share this gauge and match the old slug — " + describeCandidates(oldNorm, sameOwnerHits, row.UserID)}
	}

	// No same-owner candidate — resolve by highest text score among all
	// gauge-sharing candidates (all curated/other-owner at this point).
	if maxScore == 0 {
		return outcome{row: row, status: "ambiguous",
			detail: fmt.Sprintf("sole/only gauge-sharing candidate(s) have no textual relation to old slug %q — %s", row.ReachSlug, describeCandidates(oldNorm, cands, row.UserID))}
	}
	var top []candidate
	for _, c := range cands {
		if textScore(oldNorm, c) == maxScore {
			top = append(top, c)
		}
	}
	if len(top) == 1 {
		label := "boundary"
		if maxScore == 3 {
			label = "exact"
		}
		return outcome{
			row: row, status: "relink",
			newSlug: top[0].Slug, newID: top[0].ID,
			basis: fmt.Sprintf("gauge match, %s text match (%d candidate(s) shared this gauge)", label, len(cands)),
		}
	}
	return outcome{row: row, status: "ambiguous",
		detail: fmt.Sprintf("%d candidates tie at the top text score — %s", len(top), describeCandidates(oldNorm, top, row.UserID))}
}

// pickFromOwnerFallback is used only when the watchlist row has no
// gauge/custom_gauge anchor, or that anchor has zero live reaches left. It
// requires an EXACT normalized match (score 3) — never a boundary/fuzzy
// one — since there is no physical gauge co-location backing the guess.
func pickFromOwnerFallback(row orphanRow, oldNorm string, cands []candidate) outcome {
	var exact []candidate
	for _, c := range cands {
		if textScore(oldNorm, c) == 3 {
			exact = append(exact, c)
		}
	}
	gaugeNote := ""
	if row.GaugeID != nil || row.CustomGaugeID != nil {
		gaugeNote = "gauge anchor present but has zero live reaches; "
	}
	switch len(exact) {
	case 0:
		return outcome{row: row, status: "unmatched",
			detail: gaugeNote + fmt.Sprintf("no exact name/slug match among this user's %d live reach(es)", len(cands))}
	case 1:
		return outcome{
			row: row, status: "relink",
			newSlug: exact[0].Slug, newID: exact[0].ID,
			basis: "no gauge candidates — same-owner exact name/slug match",
		}
	default:
		return outcome{row: row, status: "ambiguous",
			detail: gaugeNote + "multiple same-owner reaches exactly match the old slug — " + describeCandidates(oldNorm, exact, row.UserID)}
	}
}
