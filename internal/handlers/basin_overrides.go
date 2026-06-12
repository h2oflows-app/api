package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BasinOverrideHandler handles /me/basin-overrides routes.
// Overrides are keyed by (user_id, state_abbr, basin_key) where basin_key is
// the raw rivers.basin value (canonical HUC name). One override re-labels every
// run in that basin — including future ones — without touching the rivers table.
type BasinOverrideHandler struct {
	db            *pgxpool.Pool
	devFallbackID string
}

func NewBasinOverrideHandler(db *pgxpool.Pool, devFallbackID string) *BasinOverrideHandler {
	return &BasinOverrideHandler{db: db, devFallbackID: devFallbackID}
}

func (h *BasinOverrideHandler) ownerID(r *http.Request) (string, bool) {
	if id, ok := auth.UserIDFromContext(r.Context()); ok {
		return id, true
	}
	if h.devFallbackID != "" {
		return h.devFallbackID, true
	}
	return "", false
}

// BasinOverride is the JSON representation of a single override row.
type BasinOverride struct {
	ID          string    `json:"id"`
	StateAbbr   string    `json:"state_abbr"`
	BasinKey    string    `json:"basin_key"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ── GET /api/v1/me/basin-overrides ───────────────────────────────────────────
// Returns all of the caller's basin-label overrides.

func (h *BasinOverrideHandler) List(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	rows, err := h.db.Query(r.Context(), `
		SELECT id, state_abbr, basin_key, display_name, created_at, updated_at
		FROM   user_basin_overrides
		WHERE  user_id = $1
		ORDER BY state_abbr, basin_key
	`, ownerID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	overrides := []BasinOverride{}
	for rows.Next() {
		var o BasinOverride
		if err := rows.Scan(&o.ID, &o.StateAbbr, &o.BasinKey, &o.DisplayName, &o.CreatedAt, &o.UpdatedAt); err != nil {
			errorResponse(w, http.StatusInternalServerError, "scan failed")
			return
		}
		overrides = append(overrides, o)
	}
	if err := rows.Err(); err != nil {
		errorResponse(w, http.StatusInternalServerError, "rows error")
		return
	}

	jsonResponse(w, http.StatusOK, overrides)
}

// ── PUT /api/v1/me/basin-overrides ───────────────────────────────────────────
// Upsert (create or update) a single override. Body: { state_abbr, basin_key, display_name }.

func (h *BasinOverrideHandler) Upsert(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var body struct {
		StateAbbr   string `json:"state_abbr"`
		BasinKey    string `json:"basin_key"`
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	body.StateAbbr = strings.ToUpper(strings.TrimSpace(body.StateAbbr))
	body.BasinKey = strings.TrimSpace(body.BasinKey)
	body.DisplayName = strings.TrimSpace(body.DisplayName)

	if body.StateAbbr == "" {
		errorResponse(w, http.StatusBadRequest, "state_abbr is required")
		return
	}
	if body.BasinKey == "" {
		errorResponse(w, http.StatusBadRequest, "basin_key is required")
		return
	}
	if body.DisplayName == "" || len(body.DisplayName) > 80 {
		errorResponse(w, http.StatusBadRequest, "display_name must be 1–80 characters")
		return
	}

	var o BasinOverride
	err := h.db.QueryRow(r.Context(), `
		INSERT INTO user_basin_overrides (user_id, state_abbr, basin_key, display_name)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, state_abbr, basin_key) DO UPDATE
		SET display_name = EXCLUDED.display_name,
		    updated_at   = NOW()
		RETURNING id, state_abbr, basin_key, display_name, created_at, updated_at
	`, ownerID, body.StateAbbr, body.BasinKey, body.DisplayName).Scan(
		&o.ID, &o.StateAbbr, &o.BasinKey, &o.DisplayName, &o.CreatedAt, &o.UpdatedAt,
	)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "upsert failed")
		return
	}

	jsonResponse(w, http.StatusOK, o)
}

// ── DELETE /api/v1/me/basin-overrides?state_abbr=XX&basin_key=YY ─────────────
// Removes a single override. Idempotent — 204 even if no row existed.

func (h *BasinOverrideHandler) Delete(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	stateAbbr := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("state_abbr")))
	basinKey := strings.TrimSpace(r.URL.Query().Get("basin_key"))

	if stateAbbr == "" || basinKey == "" {
		errorResponse(w, http.StatusBadRequest, "state_abbr and basin_key query params required")
		return
	}

	_, err := h.db.Exec(r.Context(), `
		DELETE FROM user_basin_overrides
		WHERE user_id = $1 AND state_abbr = $2 AND basin_key = $3
	`, ownerID, stateAbbr, basinKey)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "delete failed")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
