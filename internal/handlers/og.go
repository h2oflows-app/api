package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/og"
	"github.com/jackc/pgx/v5/pgxpool"
)

// OGHandler renders OpenGraph share-card PNGs for reach, report, and gauge pages.
type OGHandler struct {
	db *pgxpool.Pool
}

func NewOGHandler(db *pgxpool.Pool) *OGHandler { return &OGHandler{db: db} }

// writePNG sets caching headers and writes the PNG body.
func writePNG(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=300, s-maxage=900")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(b)))
	_, _ = w.Write(b)
}

func paramTrimExt(r *http.Request, name string) string {
	v := chi.URLParam(r, name)
	v = strings.TrimSuffix(v, ".png")
	return v
}

// Reach renders the OG image for a reach. Path: /og/reaches/{slug}.png
func (h *OGHandler) Reach(w http.ResponseWriter, r *http.Request) {
	slug := paramTrimExt(r, "slug")
	if slug == "" {
		http.Error(w, "missing slug", http.StatusBadRequest)
		return
	}

	d, err := h.loadReachData(r.Context(), slug)
	if err != nil {
		http.Error(w, "reach not found", http.StatusNotFound)
		return
	}

	png, err := og.RenderReach(d)
	if err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	writePNG(w, png)
}

func (h *OGHandler) loadReachData(ctx context.Context, slug string) (og.ReachData, error) {
	d := og.ReachData{Slug: slug}
	var (
		name       string
		commonName *string
		riverName  *string
		region     *string
		classMax   *float64
		lengthMi   *float64
		cfs        *float64
		bandLabel  *string
	)
	err := h.db.QueryRow(ctx, `
		SELECT
			r.name,
			r.common_name,
			rv.name AS river_name,
			r.region,
			r.class_max,
			r.length_mi,
			lr.value AS current_cfs,
			fr.label AS flow_band_label
		FROM reaches r
		LEFT JOIN rivers rv ON rv.id = r.river_id
		LEFT JOIN gauges g  ON g.id  = r.primary_gauge_id
		LEFT JOIN LATERAL (
			SELECT value FROM gauge_readings
			WHERE gauge_id = g.id
			  AND timestamp > NOW() - INTERVAL '48 hours'
			ORDER BY timestamp DESC LIMIT 1
		) lr ON TRUE
		LEFT JOIN LATERAL (
			SELECT label FROM flow_ranges
			WHERE reach_id = r.id
			  AND (min_value IS NULL OR lr.value >= min_value)
			  AND (max_value IS NULL OR lr.value <  max_value)
			ORDER BY min_value ASC NULLS FIRST
			LIMIT 1
		) fr ON TRUE
		WHERE r.slug = $1
	`, slug).Scan(&name, &commonName, &riverName, &region, &classMax, &lengthMi, &cfs, &bandLabel)
	if err != nil {
		return d, err
	}
	d.Name = name
	if commonName != nil && *commonName != "" {
		d.Name = *commonName
	}
	if riverName != nil {
		d.RiverName = *riverName
	}
	if region != nil {
		d.Region = *region
	}
	if classMax != nil {
		d.ClassLabel = classToRoman(*classMax)
	}
	if lengthMi != nil {
		d.LengthMi = *lengthMi
	}
	if cfs != nil {
		d.CurrentCfs = *cfs
		d.HasCfs = true
	}
	if bandLabel != nil {
		d.FlowStatus = *bandLabel
	}
	return d, nil
}

// Report renders the OG image for a report. Path: /og/reports/{id}.png
func (h *OGHandler) Report(w http.ResponseWriter, r *http.Request) {
	id := paramTrimExt(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	d := og.ReportData{ID: id}
	var (
		name       string
		handle     *string
		reportDate string
		flowCfs    *float64
		flowBand   *string
		paddled    bool
		reachName  *string
		reachCommon *string
	)
	err := h.db.QueryRow(r.Context(), `
		SELECT
			rep.name,
			rep.handle,
			rep.report_date::text,
			rep.flow_cfs,
			rep.flow_band,
			rep.paddled,
			rch.name AS reach_name,
			rch.common_name AS reach_common
		FROM reports rep
		LEFT JOIN reaches rch ON rch.id = rep.reach_id
		WHERE rep.id = $1
	`, id).Scan(&name, &handle, &reportDate, &flowCfs, &flowBand, &paddled, &reachName, &reachCommon)
	if err != nil {
		http.Error(w, "report not found", http.StatusNotFound)
		return
	}
	d.Title = name
	if handle != nil {
		d.Handle = *handle
	}
	d.ReportDate = reportDate
	d.Paddled = paddled
	if reachCommon != nil && *reachCommon != "" {
		d.ReachName = *reachCommon
	} else if reachName != nil {
		d.ReachName = *reachName
	}
	if flowCfs != nil {
		d.FlowCfs = *flowCfs
		d.HasCfs = true
	}
	if flowBand != nil {
		d.FlowBand = *flowBand
	}

	png, err := og.RenderReport(d)
	if err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	writePNG(w, png)
}

// Gauge renders the OG image for a gauge. Path: /og/gauges/{id}.png
func (h *OGHandler) Gauge(w http.ResponseWriter, r *http.Request) {
	id := paramTrimExt(r, "id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}

	d := og.GaugeData{ID: id}
	var (
		name     string
		source   *string
		cfs      *float64
	)
	err := h.db.QueryRow(r.Context(), `
		SELECT
			g.name,
			g.source,
			lr.value
		FROM gauges g
		LEFT JOIN LATERAL (
			SELECT value FROM gauge_readings
			WHERE gauge_id = g.id
			  AND timestamp > NOW() - INTERVAL '48 hours'
			ORDER BY timestamp DESC LIMIT 1
		) lr ON TRUE
		WHERE g.id = $1
	`, id).Scan(&name, &source, &cfs)
	if err != nil {
		http.Error(w, "gauge not found", http.StatusNotFound)
		return
	}
	d.Name = name
	if source != nil {
		d.Source = *source
	}
	if cfs != nil {
		d.CurrentCfs = *cfs
		d.HasCfs = true
	}
	// Flow status not surfaced at gauge level without a reach context — leave blank.

	png, err := og.RenderGauge(d)
	if err != nil {
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	writePNG(w, png)
}

// classToRoman converts a float class (3.5) to Roman numeral display ("III+").
// 5.0 caps at "V"; anything above 5 returns "V+".
func classToRoman(c float64) string {
	whole := int(c)
	frac := c - float64(whole)
	roman := ""
	switch whole {
	case 0:
		roman = "I"
	case 1:
		roman = "I"
	case 2:
		roman = "II"
	case 3:
		roman = "III"
	case 4:
		roman = "IV"
	case 5:
		roman = "V"
	default:
		return "V+"
	}
	if frac >= 0.4 && frac < 0.7 {
		roman += "+"
	} else if frac >= 0.7 {
		// e.g. 3.8 → "IV-"; we display as next class minus
		switch whole {
		case 0, 1:
			roman = "II-"
		case 2:
			roman = "III-"
		case 3:
			roman = "IV-"
		case 4:
			roman = "V-"
		case 5:
			roman = "V+"
		}
	}
	return roman
}
