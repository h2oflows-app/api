package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// FlowBandOverrideHandler handles per-user flow band overrides for canonical reaches.
type FlowBandOverrideHandler struct {
	db            *pgxpool.Pool
	devFallbackID string
}

func NewFlowBandOverrideHandler(db *pgxpool.Pool, devFallbackID string) *FlowBandOverrideHandler {
	return &FlowBandOverrideHandler{db: db, devFallbackID: devFallbackID}
}

func (h *FlowBandOverrideHandler) callerID(r *http.Request) (string, bool) {
	if id, ok := auth.UserIDFromContext(r.Context()); ok {
		return id, true
	}
	if h.devFallbackID != "" {
		return h.devFallbackID, true
	}
	return "", false
}

type reachFlowBandOverride struct {
	ID         string    `json:"id"`
	UserID     string    `json:"user_id"`
	ReachID    string    `json:"reach_id"`
	LowMax     *float64  `json:"low_max"`
	RunningMin float64   `json:"running_min"`
	RunningMax float64   `json:"running_max"`
	HighMin    *float64  `json:"high_min"`
	Note       *string   `json:"note"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

func (h *FlowBandOverrideHandler) reachIDForSlug(r *http.Request, slug string) (string, error) {
	var id string
	err := h.db.QueryRow(r.Context(), `SELECT id FROM reaches WHERE slug = $1`, slug).Scan(&id)
	return id, err
}

// GetOwn handles GET /api/v1/me/reaches/{slug}/flow-band-override
func (h *FlowBandOverrideHandler) GetOwn(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.callerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")
	reachID, err := h.reachIDForSlug(r, slug)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "reach not found")
		return
	}
	var o reachFlowBandOverride
	err = h.db.QueryRow(r.Context(), `
		SELECT id, user_id, reach_id::text, low_max, running_min, running_max,
		       high_min, note, created_at, updated_at
		FROM reach_flow_band_overrides
		WHERE user_id = $1 AND reach_id = $2
	`, callerID, reachID).Scan(
		&o.ID, &o.UserID, &o.ReachID, &o.LowMax, &o.RunningMin, &o.RunningMax,
		&o.HighMin, &o.Note, &o.CreatedAt, &o.UpdatedAt,
	)
	if err != nil {
		jsonResponse(w, http.StatusOK, nil)
		return
	}
	jsonResponse(w, http.StatusOK, o)
}

// UpsertOwn handles PUT /api/v1/me/reaches/{slug}/flow-band-override
func (h *FlowBandOverrideHandler) UpsertOwn(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.callerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")
	reachID, err := h.reachIDForSlug(r, slug)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "reach not found")
		return
	}
	var body struct {
		LowMax     *float64 `json:"low_max"`
		RunningMin float64  `json:"running_min"`
		RunningMax float64  `json:"running_max"`
		HighMin    *float64 `json:"high_min"`
		Note       *string  `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.RunningMin <= 0 || body.RunningMax <= 0 || body.RunningMin >= body.RunningMax {
		errorResponse(w, http.StatusBadRequest, "running_min and running_max must be positive and min < max")
		return
	}
	if body.LowMax != nil && *body.LowMax > body.RunningMin {
		errorResponse(w, http.StatusBadRequest, "low_max must be <= running_min")
		return
	}
	if body.HighMin != nil && *body.HighMin < body.RunningMax {
		errorResponse(w, http.StatusBadRequest, "high_min must be >= running_max")
		return
	}
	note := body.Note
	if note != nil {
		t := strings.TrimSpace(*note)
		note = &t
		if *note == "" {
			note = nil
		}
	}
	var id string
	err = h.db.QueryRow(r.Context(), `
		INSERT INTO reach_flow_band_overrides
		    (user_id, reach_id, low_max, running_min, running_max, high_min, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (user_id, reach_id) DO UPDATE SET
		    low_max     = EXCLUDED.low_max,
		    running_min = EXCLUDED.running_min,
		    running_max = EXCLUDED.running_max,
		    high_min    = EXCLUDED.high_min,
		    note        = EXCLUDED.note,
		    updated_at  = now()
		RETURNING id
	`, callerID, reachID, body.LowMax, body.RunningMin, body.RunningMax, body.HighMin, note).Scan(&id)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "save failed: "+err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{"id": id})
}

// DeleteOwn handles DELETE /api/v1/me/reaches/{slug}/flow-band-override
func (h *FlowBandOverrideHandler) DeleteOwn(w http.ResponseWriter, r *http.Request) {
	callerID, ok := h.callerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	slug := chi.URLParam(r, "slug")
	reachID, err := h.reachIDForSlug(r, slug)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "reach not found")
		return
	}
	_, _ = h.db.Exec(r.Context(),
		`DELETE FROM reach_flow_band_overrides WHERE user_id = $1 AND reach_id = $2`,
		callerID, reachID,
	)
	w.WriteHeader(http.StatusNoContent)
}

// AdminListForReach handles GET /api/v1/admin/reaches/{slug}/flow-band-overrides
func (h *FlowBandOverrideHandler) AdminListForReach(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	reachID, err := h.reachIDForSlug(r, slug)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "reach not found")
		return
	}
	rows, err := h.db.Query(r.Context(), `
		SELECT o.id, o.user_id, o.reach_id::text,
		       o.low_max, o.running_min, o.running_max, o.high_min,
		       o.note, o.created_at, o.updated_at,
		       up.handle
		FROM reach_flow_band_overrides o
		LEFT JOIN user_profiles up ON up.owner_id = o.user_id
		WHERE o.reach_id = $1
		ORDER BY o.updated_at DESC
	`, reachID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()
	type listRow struct {
		reachFlowBandOverride
		AuthorHandle *string `json:"author_handle"`
	}
	list := make([]listRow, 0)
	for rows.Next() {
		var o listRow
		if err := rows.Scan(
			&o.ID, &o.UserID, &o.ReachID,
			&o.LowMax, &o.RunningMin, &o.RunningMax, &o.HighMin,
			&o.Note, &o.CreatedAt, &o.UpdatedAt, &o.AuthorHandle,
		); err == nil {
			list = append(list, o)
		}
	}
	jsonResponse(w, http.StatusOK, list)
}

// AdminQueue handles GET /api/v1/admin/reaches/flow-band-override-queue
// Returns reaches with 2+ overrides, with median values and canonical comparison.
func (h *FlowBandOverrideHandler) AdminQueue(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(r.Context(), `
		SELECT
		    r.id::text,
		    r.slug,
		    r.name,
		    COALESCE(r.river_name, rv.name) AS river_name,
		    COUNT(*)                         AS override_count,
		    PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY o.low_max)     AS median_low_max,
		    PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY o.running_min) AS median_running_min,
		    PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY o.running_max) AS median_running_max,
		    PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY o.high_min)    AS median_high_min,
		    (SELECT value FROM flow_ranges WHERE reach_id = r.id AND label = 'Running' LIMIT 1) AS canonical_running_value,
		    (SELECT value FROM flow_ranges WHERE reach_id = r.id AND label = 'High'    LIMIT 1) AS canonical_high_value
		FROM reach_flow_band_overrides o
		JOIN reaches r ON r.id = o.reach_id
		LEFT JOIN rivers rv ON rv.id = r.river_id
		GROUP BY r.id, r.slug, r.name, r.river_name, rv.name
		HAVING COUNT(*) >= 2
		ORDER BY override_count DESC, r.name ASC
	`)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()
	type queueRow struct {
		ReachID               string   `json:"reach_id"`
		Slug                  string   `json:"slug"`
		Name                  string   `json:"name"`
		RiverName             *string  `json:"river_name"`
		OverrideCount         int      `json:"override_count"`
		MedianLowMax          *float64 `json:"median_low_max"`
		MedianRunningMin      *float64 `json:"median_running_min"`
		MedianRunningMax      *float64 `json:"median_running_max"`
		MedianHighMin         *float64 `json:"median_high_min"`
		CanonicalRunningValue *float64 `json:"canonical_running_value"`
		CanonicalHighValue    *float64 `json:"canonical_high_value"`
	}
	out := make([]queueRow, 0)
	for rows.Next() {
		var q queueRow
		if err := rows.Scan(
			&q.ReachID, &q.Slug, &q.Name, &q.RiverName, &q.OverrideCount,
			&q.MedianLowMax, &q.MedianRunningMin, &q.MedianRunningMax, &q.MedianHighMin,
			&q.CanonicalRunningValue, &q.CanonicalHighValue,
		); err == nil {
			out = append(out, q)
		}
	}
	jsonResponse(w, http.StatusOK, out)
}

// AdminApplyMedian handles POST /api/v1/admin/reaches/{slug}/apply-override-median
// Computes the median of all user overrides and writes them to flow_ranges.
func (h *FlowBandOverrideHandler) AdminApplyMedian(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	var reachID string
	var gaugeID *string
	err := h.db.QueryRow(r.Context(),
		`SELECT id::text, primary_gauge_id::text FROM reaches WHERE slug = $1`, slug,
	).Scan(&reachID, &gaugeID)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "reach not found")
		return
	}
	var (
		lowMax, runMin, runMax, highMin *float64
		count                           int
	)
	err = h.db.QueryRow(r.Context(), `
		SELECT
		    COUNT(*),
		    PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY low_max),
		    PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY running_min),
		    PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY running_max),
		    PERCENTILE_CONT(0.5) WITHIN GROUP (ORDER BY high_min)
		FROM reach_flow_band_overrides WHERE reach_id = $1
	`, reachID).Scan(&count, &lowMax, &runMin, &runMax, &highMin)
	if err != nil || count < 2 {
		errorResponse(w, http.StatusBadRequest, "not enough overrides to apply median")
		return
	}
	if runMin == nil || runMax == nil {
		errorResponse(w, http.StatusBadRequest, "missing required medians")
		return
	}
	upsert := func(label string, val float64, color string) error {
		_, e := h.db.Exec(r.Context(), `
			INSERT INTO flow_ranges
			    (gauge_id, reach_id, label, value, color, data_source, verified)
			VALUES ($1, $2, $3, $4, $5, 'override_median', true)
			ON CONFLICT (reach_id, label) DO UPDATE SET
			    value       = EXCLUDED.value,
			    color       = EXCLUDED.color,
			    data_source = 'override_median',
			    verified    = true
		`, gaugeID, reachID, label, val, color)
		return e
	}
	if err := upsert("Running", *runMin, "green-3"); err != nil {
		errorResponse(w, http.StatusInternalServerError, "Running upsert: "+err.Error())
		return
	}
	if err := upsert("High", *runMax, "orange-3"); err != nil {
		errorResponse(w, http.StatusInternalServerError, "High upsert: "+err.Error())
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{
		"applied":     true,
		"low_max":     lowMax,
		"running_min": runMin,
		"running_max": runMax,
		"high_min":    highMin,
		"from_count":  count,
	})
}
