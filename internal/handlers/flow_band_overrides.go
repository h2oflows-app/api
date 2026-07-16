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
	err := h.db.QueryRow(r.Context(), `
		SELECT id FROM user_reaches
		WHERE slug = $1
		  AND owner_id = '00000000-0000-0000-0000-000000000001'
		  AND deleted_at IS NULL
	`, slug).Scan(&id)
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
		SELECT id, user_id, user_reach_id::text, low_max, running_min, running_max,
		       high_min, note, created_at, updated_at
		FROM reach_flow_band_overrides
		WHERE user_id = $1 AND user_reach_id = $2
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
		    (user_id, user_reach_id, low_max, running_min, running_max, high_min, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (user_id, user_reach_id) DO UPDATE SET
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
		`DELETE FROM reach_flow_band_overrides WHERE user_id = $1 AND user_reach_id = $2`,
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
		SELECT o.id, o.user_id, o.user_reach_id::text,
		       o.low_max, o.running_min, o.running_max, o.high_min,
		       o.note, o.created_at, o.updated_at,
		       up.handle
		FROM reach_flow_band_overrides o
		LEFT JOIN user_profiles up ON up.owner_id = o.user_id
		WHERE o.user_reach_id = $1
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
