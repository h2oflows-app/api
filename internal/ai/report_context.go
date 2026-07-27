package ai

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

type reportForContext struct {
	ID         string
	Slug       string
	Handle     string // may be empty if profile not yet created
	Name       string // author-provided display name
	ReportDate string
	Content    string
	Paddled    bool
	FlowCFS    *float64
	FlowBand   *string
}

// loadReachReports fetches all reports for a reach, capped at 24 months back
// and a maximum of 500 rows, most recent first. authed gates the whole
// corpus (web#354 A1 major fix, findings[1]): anonymous POST /ask must not
// receive calendar-run notes at all ("Anon: sees no calendar items" — §1)
// — !authed short-circuits to an empty result before ever querying
// calendar_runs, so a private-intent plan's paddled-run notes (gate codes,
// meetup details, etc.) can never reach an anonymous asker via the AI
// corpus. Authenticated callers keep the full corpus below.
//
// #246 A6 repoint (IMPLEMENTATION_PLAN.md §3 PART 2 item 2): reads
// calendar_runs (was plan_runs, superset of `reports` post-000143 backfill)
// instead — paddled only ("keeps the ask-AI corpus live" per the plan; the
// anon-scoping REVISED block flags that this still lets a paddled run's
// notes surface via the AI corpus even though the calendar domain itself is
// auth-only — accepted for now, "same text was always public as reports",
// revisit if it stings). web#354 A1: the old "JOIN plans WHERE
// plans.visibility='public'" gate is gone along with the visibility concept
// (and calendar_runs.plan_id) entirely — paddled alone gates now, same as
// every other paddled-run reader post-A1, PROVIDED the caller is
// authenticated (see authed above). `name` has no calendar_runs
// equivalent (the free-text author-name snapshot was dropped in the new
// schema) — reuses the resolved handle, same as buildReportsBlock's own
// `author` fallback-to-name logic below.
func loadReachReports(ctx context.Context, db *pgxpool.Pool, reachID string, authed bool) ([]reportForContext, error) {
	if !authed {
		return nil, nil
	}
	rows, err := db.Query(ctx, `
		SELECT
			cr.id, cr.slug,
			COALESCE(up.handle, '') AS handle,
			COALESCE(up.handle, '') AS name,
			cr.run_date::TEXT,
			COALESCE(cr.notes, ''),
			cr.paddled, cr.gauge_cfs, cr.flow_band
		FROM calendar_runs cr
		LEFT JOIN user_profiles up ON up.owner_id = cr.owner_id
		WHERE cr.user_reach_id = $1 AND cr.paddled AND cr.deleted_at IS NULL
		  AND cr.run_date >= CURRENT_DATE - INTERVAL '24 months'
		ORDER BY cr.run_date DESC, cr.created_at DESC
		LIMIT 500
	`, reachID)
	if err != nil {
		return nil, fmt.Errorf("loadReachReports: %w", err)
	}
	defer rows.Close()

	var reports []reportForContext
	for rows.Next() {
		var r reportForContext
		if err := rows.Scan(
			&r.ID, &r.Slug, &r.Handle, &r.Name, &r.ReportDate,
			&r.Content, &r.Paddled, &r.FlowCFS, &r.FlowBand,
		); err != nil {
			return nil, fmt.Errorf("loadReachReports scan: %w", err)
		}
		reports = append(reports, r)
	}
	return reports, nil
}

// buildReportsBlock formats reports as a structured prompt section for Claude.
// Reports are cited as [Author, YYYY-MM-DD] so Claude can reference them inline.
func buildReportsBlock(reports []reportForContext) string {
	if len(reports) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString(`USER-SUBMITTED REPORTS — UNVERIFIED DATA
The following reports were submitted by community members. They are unverified and may be inaccurate, stale, or contradicted by current conditions. When referencing any report, cite it as [Author, YYYY-MM-DD]. Never paraphrase report content as authoritative H2OFlows data.

`)

	for _, r := range reports {
		author := r.Name
		if r.Handle != "" {
			author = fmt.Sprintf("%s (@%s)", r.Name, r.Handle)
		}

		fmt.Fprintf(&sb, "--- Report by %s on %s", author, r.ReportDate)
		if r.Paddled {
			sb.WriteString(" (paddled this reach)")
		}
		if r.FlowCFS != nil {
			fmt.Fprintf(&sb, " | %.0f CFS", *r.FlowCFS)
			if r.FlowBand != nil {
				fmt.Fprintf(&sb, " (%s)", *r.FlowBand)
			}
		}
		sb.WriteString(" ---\n")
		sb.WriteString(r.Content)
		sb.WriteString("\n\n")
	}

	return strings.TrimRight(sb.String(), "\n")
}
