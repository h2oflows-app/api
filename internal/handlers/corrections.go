package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CorrectionsHandler handles river correction routes.
type CorrectionsHandler struct {
	db *pgxpool.Pool
}

func NewCorrectionsHandler(db *pgxpool.Pool) *CorrectionsHandler {
	return &CorrectionsHandler{db: db}
}

// CreateRiverCorrection submits a user correction for a river field.
// POST /api/v1/me/river-corrections
func (h *CorrectionsHandler) CreateRiverCorrection(w http.ResponseWriter, r *http.Request) {
	userID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	var body struct {
		RiverSlug     string `json:"river_slug"`
		Field         string `json:"field"`
		ProposedValue string `json:"proposed_value"`
		Note          string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.RiverSlug == "" || body.Field == "" || body.ProposedValue == "" {
		errorResponse(w, http.StatusBadRequest, "river_slug, field, and proposed_value are required")
		return
	}
	if body.Field != "basin" && body.Field != "state_abbr" {
		errorResponse(w, http.StatusBadRequest, "field must be 'basin' or 'state_abbr'")
		return
	}

	var riverID string
	if err := h.db.QueryRow(r.Context(), `SELECT id FROM rivers WHERE slug = $1`, body.RiverSlug).Scan(&riverID); err != nil {
		errorResponse(w, http.StatusNotFound, "river not found")
		return
	}

	var id string
	err := h.db.QueryRow(r.Context(), `
		INSERT INTO river_corrections (river_id, proposed_by, field, proposed_value, note)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''))
		RETURNING id
	`, riverID, userID, body.Field, body.ProposedValue, body.Note).Scan(&id)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "insert failed")
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]string{"id": id})
}

// NOTE: ListRiverCorrections (GET /admin/river-corrections) and
// ReviewRiverCorrection (PATCH /admin/river-corrections/{id}) were removed
// (#315) — the admin review queue had no live UI caller. CreateRiverCorrection
// above (the user-facing submission endpoint) is retained.
