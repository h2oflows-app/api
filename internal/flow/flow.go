// Package flow holds the report/plan-run flow-stamping logic that used to
// live as unexported helpers on handlers.ReportHandler (reports.go). It is
// intentionally small and free of handler-package dependencies so it can be
// shared by both the legacy reports handlers and the calendar/plan-runs
// handlers landing in a later PR (#246 A3).
package flow

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// Queryer is the subset of *pgxpool.Pool (and *pgx.Tx) this package needs.
// Kept minimal so callers can pass a pool, a tx, or a test double.
type Queryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Threshold is a single CFS threshold in the flexible band model: the band
// applies when cfs >= Value. Deliberately shape-compatible with (but
// independent of) handlers.FlowBandThreshold — this package must not import
// internal/handlers.
type Threshold struct {
	Value float64
	Label string
}

// AllowedReportBands is the set of flow_band values the live `reports` table
// CHECK constraint (reports_flow_band_check) accepts. BandLabelForCFS can
// return arbitrary free-text labels (reach authors set their own base_label
// and user_reach_flow_ranges.label) — anything outside this set will 500 on
// INSERT into `reports`. plan_runs.flow_band (issue #246) drops the CHECK
// entirely, but reports.go is staying live-but-dormant for now, so callers
// writing to `reports` should run the band through ClampReportBand first.
var AllowedReportBands = map[string]bool{"low": true, "running": true, "high": true}

// ClampReportBand returns band unchanged when nil or already a value the
// `reports` table's CHECK constraint accepts; otherwise nil, so the caller
// stores the cfs reading without a band instead of failing the INSERT.
func ClampReportBand(band *string) *string {
	if band == nil || AllowedReportBands[*band] {
		return band
	}
	return nil
}

// BandLabelForCFS applies V9 logic: highest threshold where cfs >= value;
// else baseLabel.
func BandLabelForCFS(cfs float64, baseLabel string, thresholds []Threshold) *string {
	best := ""
	bestVal := -1.0
	for _, t := range thresholds {
		if cfs >= t.Value && t.Value > bestVal {
			best = t.Label
			bestVal = t.Value
		}
	}
	if best == "" {
		best = baseLabel
	}
	s := best
	return &s
}

// StampCFSAndBand queries the gauge reading nearest to `at` for the
// user_reach's primary gauge, falling back to a custom gauge's latest
// snapshot value when the user_reach has no primary_gauge_id (or the
// primary-gauge query comes up empty), then derives the flow band from
// user_reach_flow_ranges. userReachID is a user_reaches.id (sentinel-owned
// twin, runs-unify 5a).
//
// Custom gauges (internal/handlers/custom_gauges.go) have no readings
// history table — only a live custom_gauges.last_value_cfs/last_value_at
// snapshot maintained by the poller — so the fallback has no "nearest to
// `at`" query available; it just takes the current snapshot value.
func StampCFSAndBand(ctx context.Context, db Queryer, userReachID string, at time.Time) (*float64, *string) {
	var cfs float64
	err := db.QueryRow(ctx, `
		SELECT gr.value
		FROM user_reaches ur
		JOIN gauge_readings gr ON gr.gauge_id = ur.primary_gauge_id
		WHERE ur.id = $1 AND ur.primary_gauge_id IS NOT NULL
		ORDER BY ABS(EXTRACT(EPOCH FROM (gr.timestamp - $2::TIMESTAMPTZ))) ASC
		LIMIT 1
	`, userReachID, at).Scan(&cfs)
	if err != nil {
		fbErr := db.QueryRow(ctx, `
			SELECT cg.last_value_cfs
			FROM user_reaches ur
			JOIN custom_gauges cg ON cg.id = ur.custom_gauge_id
			WHERE ur.id = $1 AND ur.custom_gauge_id IS NOT NULL AND cg.last_value_cfs IS NOT NULL
		`, userReachID).Scan(&cfs)
		if fbErr != nil {
			return nil, nil
		}
	}

	var baseLabel string
	_ = db.QueryRow(ctx, `SELECT base_label FROM user_reaches WHERE id = $1`, userReachID).Scan(&baseLabel)
	if baseLabel == "" {
		return &cfs, nil
	}

	frRows, err := db.Query(ctx,
		`SELECT value, label FROM user_reach_flow_ranges WHERE user_reach_id = $1 ORDER BY value ASC`, userReachID,
	)
	if err != nil {
		return &cfs, nil
	}
	defer frRows.Close()

	var thresholds []Threshold
	for frRows.Next() {
		var t Threshold
		if frRows.Scan(&t.Value, &t.Label) == nil {
			thresholds = append(thresholds, t)
		}
	}

	band := BandLabelForCFS(cfs, baseLabel, thresholds)
	return &cfs, band
}

// ObservedAt converts a date + optional time string to UTC. Defaults to noon
// UTC when time is absent.
func ObservedAt(date, timeStr string) time.Time {
	if timeStr != "" {
		t, err := time.Parse("2006-01-02 15:04", date+" "+timeStr)
		if err == nil {
			return t.UTC()
		}
	}
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		return time.Now().UTC()
	}
	return time.Date(d.Year(), d.Month(), d.Day(), 12, 0, 0, 0, time.UTC)
}
