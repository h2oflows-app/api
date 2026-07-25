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
// and a maximum of 500 rows, most recent first.
//
// #246 A6 repoint (IMPLEMENTATION_PLAN.md §3 PART 2 item 2): reads plan_runs
// (superset of `reports` post-000143 backfill) instead — paddled + public
// only ("keeps the ask-AI corpus live" per the plan; the anon-scoping REVISED
// block flags that this still lets a public paddled plan_run's notes surface
// via the AI corpus even though the calendar domain itself is now auth-only —
// accepted for now, "same text was always public as reports", revisit if it
// stings). `name` has no plan_runs equivalent (the free-text author-name
// snapshot was dropped in the new schema) — reuses the resolved handle, same
// as buildReportsBlock's own `author` fallback-to-name logic below.
func loadReachReports(ctx context.Context, db *pgxpool.Pool, reachID string) ([]reportForContext, error) {
	rows, err := db.Query(ctx, `
		SELECT
			pr.id, pr.slug,
			COALESCE(up.handle, '') AS handle,
			COALESCE(up.handle, '') AS name,
			pr.run_date::TEXT,
			COALESCE(pr.notes, ''),
			pr.paddled, pr.gauge_cfs, pr.flow_band
		FROM plan_runs pr
		JOIN plans p ON p.id = pr.plan_id AND p.deleted_at IS NULL AND p.visibility = 'public'
		LEFT JOIN user_profiles up ON up.owner_id = pr.owner_id
		WHERE pr.user_reach_id = $1 AND pr.paddled AND pr.deleted_at IS NULL
		  AND pr.run_date >= CURRENT_DATE - INTERVAL '24 months'
		ORDER BY pr.run_date DESC, pr.created_at DESC
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
