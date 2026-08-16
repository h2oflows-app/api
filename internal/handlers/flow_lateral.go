package handlers

import (
	"net/http"
	"strconv"
	"strings"
)

// flowBandLateralSQL is the canonical flow-band resolution block: latest
// reading within 48h (lr), highest cleared threshold (thresh), and the
// GUARDED band columns (fr) — a run with zero user_reach_flow_ranges rows
// resolves band_label/band_color to NULL instead of leaking base_label.
// users.go MapAllByHandle carries an older UNGUARDED variant of this block;
// do not copy that one (migrating the remaining inline copies onto this
// fragment is a follow-up PR).
//
// Splice requirements: the surrounding query must alias user_reaches as `ur`
// and LEFT JOIN custom_gauges as `cg`, and the fragment must sit directly
// before WHERE — it ends in a comma-joined LATERAL, which Postgres accepts
// after a JOIN chain.
//
// Custom-gauge caveat (accepted, web#335): cg.last_value_cfs is a live
// snapshot with no freshness gate — last_value_at is deliberately not
// checked here, matching every existing surface.
const flowBandLateralSQL = `
	LEFT JOIN LATERAL (
		SELECT value FROM gauge_readings
		WHERE gauge_id = ur.primary_gauge_id
		  AND timestamp > NOW() - INTERVAL '48 hours'
		ORDER BY timestamp DESC LIMIT 1
	) lr ON TRUE
	LEFT JOIN LATERAL (
		SELECT label, color FROM user_reach_flow_ranges
		WHERE user_reach_id = ur.id
		  AND COALESCE(lr.value, cg.last_value_cfs) >= value
		ORDER BY value DESC
		LIMIT 1
	) thresh ON TRUE,
	LATERAL (
		SELECT
			CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
				COALESCE(thresh.label, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_label END)
			END AS band_label,
			CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
				COALESCE(thresh.color, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_color END)
			END AS band_color
	) fr
`

// inBandPredicateSQL is the "running now" filter (web#335): a reading exists,
// the run has at least one configured threshold, and the reading clears the
// lowest one — i.e. the resolved label is a real threshold band, not
// base_label. Canonical rule documented at nudges.go (in-band = band non-nil
// AND != base_label). Both consumers bind the flag as $6; renumber here if
// either handler's param order ever changes.
const inBandPredicateSQL = `
	AND ($6::bool IS NOT TRUE OR (fr.band_label IS NOT NULL AND fr.band_label IS DISTINCT FROM ur.base_label))
`

// flowBandLateralCanary exists for cmd/sqlcheck: queries built by
// concatenating flowBandLateralSQL are syntactically incomplete per-literal
// and get skipped, so this single complete literal keeps every column the
// fragment touches under PREPARE coverage. flow_lateral_test.go asserts it
// contains flowBandLateralSQL and inBandPredicateSQL verbatim — the canary
// cannot drift from the fragments.
const flowBandLateralCanary = `
	SELECT
		COALESCE(lr.value, cg.last_value_cfs) AS current_cfs,
		fr.band_label,
		fr.band_color
	FROM user_reaches ur
	LEFT JOIN custom_gauges cg ON cg.id = ur.custom_gauge_id
	LEFT JOIN LATERAL (
		SELECT value FROM gauge_readings
		WHERE gauge_id = ur.primary_gauge_id
		  AND timestamp > NOW() - INTERVAL '48 hours'
		ORDER BY timestamp DESC LIMIT 1
	) lr ON TRUE
	LEFT JOIN LATERAL (
		SELECT label, color FROM user_reach_flow_ranges
		WHERE user_reach_id = ur.id
		  AND COALESCE(lr.value, cg.last_value_cfs) >= value
		ORDER BY value DESC
		LIMIT 1
	) thresh ON TRUE,
	LATERAL (
		SELECT
			CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
				COALESCE(thresh.label, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_label END)
			END AS band_label,
			CASE WHEN EXISTS(SELECT 1 FROM user_reach_flow_ranges WHERE user_reach_id = ur.id) THEN
				COALESCE(thresh.color, CASE WHEN COALESCE(lr.value, cg.last_value_cfs) IS NOT NULL THEN ur.base_color END)
			END AS band_color
	) fr
	WHERE ur.deleted_at IS NULL
	AND ($6::bool IS NOT TRUE OR (fr.band_label IS NOT NULL AND fr.band_label IS DISTINCT FROM ur.base_label))
`

// runFilters is the shared filter set for GET /discover/runs and
// GET /user-runs/map/community. One parser for both handlers so the map's
// pin set and the sidebar list can never drift apart (web#335 pins == list).
type runFilters struct {
	Q        string   // q — matches run name, river name, author handle (ILIKE)
	MinClass *float64 // min_class
	MaxClass *float64 // max_class
	HasGauge bool     // has_gauge, strict "true"
	Handle   string   // handle — EXACT match, case-sensitive (long-standing quirk, kept)
	InBand   bool     // in_band, strict "true" — see inBandPredicateSQL
}

func parseRunFilters(r *http.Request) runFilters {
	f := runFilters{
		Q:        strings.TrimSpace(r.URL.Query().Get("q")),
		HasGauge: r.URL.Query().Get("has_gauge") == "true",
		Handle:   strings.TrimSpace(r.URL.Query().Get("handle")),
		InBand:   r.URL.Query().Get("in_band") == "true",
	}
	if v := r.URL.Query().Get("min_class"); v != "" {
		if fl, err := strconv.ParseFloat(v, 64); err == nil {
			f.MinClass = &fl
		}
	}
	if v := r.URL.Query().Get("max_class"); v != "" {
		if fl, err := strconv.ParseFloat(v, 64); err == nil {
			f.MaxClass = &fl
		}
	}
	return f
}
