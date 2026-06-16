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
func loadReachReports(ctx context.Context, db *pgxpool.Pool, reachID string) ([]reportForContext, error) {
	rows, err := db.Query(ctx, `
		SELECT
			rp.id, rp.slug,
			COALESCE(up.handle, '') AS handle,
			rp.name, rp.report_date::TEXT,
			rp.content,
			rp.paddled, rp.flow_cfs, rp.flow_band
		FROM reports rp
		LEFT JOIN user_profiles up ON up.owner_id = rp.owner_id
		WHERE rp.user_reach_id = $1
		  AND rp.report_date >= CURRENT_DATE - INTERVAL '24 months'
		ORDER BY rp.report_date DESC, rp.created_at DESC
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
