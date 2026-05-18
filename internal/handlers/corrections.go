package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
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

// ListRiverCorrections returns pending corrections for admin review.
// GET /api/v1/admin/river-corrections?status=pending
func (h *CorrectionsHandler) ListRiverCorrections(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending"
	}

	rows, err := h.db.Query(r.Context(), `
		SELECT rc.id, rc.river_id, rv.slug AS river_slug, rv.name AS river_name,
		       rv.state_abbr AS river_state, rv.basin AS river_basin,
		       rc.proposed_by, rc.field, rc.proposed_value, rc.note,
		       rc.status, rc.reviewed_by, rc.reviewed_at, rc.review_note, rc.created_at
		FROM river_corrections rc
		JOIN rivers rv ON rv.id = rc.river_id
		WHERE rc.status = $1
		ORDER BY rc.created_at DESC
	`, status)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	type CorrectionRow struct {
		ID            string     `json:"id"`
		RiverID       string     `json:"river_id"`
		RiverSlug     string     `json:"river_slug"`
		RiverName     string     `json:"river_name"`
		RiverState    *string    `json:"river_state"`
		RiverBasin    *string    `json:"river_basin"`
		ProposedBy    string     `json:"proposed_by"`
		Field         string     `json:"field"`
		ProposedValue string     `json:"proposed_value"`
		Note          *string    `json:"note"`
		Status        string     `json:"status"`
		ReviewedBy    *string    `json:"reviewed_by"`
		ReviewedAt    *time.Time `json:"reviewed_at"`
		ReviewNote    *string    `json:"review_note"`
		CreatedAt     time.Time  `json:"created_at"`
	}

	result := make([]CorrectionRow, 0)
	for rows.Next() {
		var c CorrectionRow
		if err := rows.Scan(
			&c.ID, &c.RiverID, &c.RiverSlug, &c.RiverName,
			&c.RiverState, &c.RiverBasin,
			&c.ProposedBy, &c.Field, &c.ProposedValue, &c.Note,
			&c.Status, &c.ReviewedBy, &c.ReviewedAt, &c.ReviewNote, &c.CreatedAt,
		); err != nil {
			continue
		}
		result = append(result, c)
	}
	jsonResponse(w, http.StatusOK, result)
}

// ReviewRiverCorrection accepts or rejects a pending correction.
// PATCH /api/v1/admin/river-corrections/{id}
func (h *CorrectionsHandler) ReviewRiverCorrection(w http.ResponseWriter, r *http.Request) {
	corrID := chi.URLParam(r, "id")
	reviewerID, ok := auth.UserIDFromContext(r.Context())
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	var body struct {
		Action     string `json:"action"`      // "accept" or "reject"
		ReviewNote string `json:"review_note"` // optional
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Action != "accept" && body.Action != "reject" {
		errorResponse(w, http.StatusBadRequest, "action must be 'accept' or 'reject'")
		return
	}

	status := "accepted"
	if body.Action == "reject" {
		status = "rejected"
	}

	var riverID, field, proposedValue string
	err := h.db.QueryRow(r.Context(), `
		UPDATE river_corrections
		SET    status      = $2,
		       reviewed_by = $3,
		       reviewed_at = NOW(),
		       review_note = NULLIF($4, '')
		WHERE  id = $1 AND status = 'pending'
		RETURNING river_id, field, proposed_value
	`, corrID, status, reviewerID, body.ReviewNote).Scan(&riverID, &field, &proposedValue)
	if err != nil {
		errorResponse(w, http.StatusNotFound, "correction not found or already reviewed")
		return
	}

	if status == "accepted" {
		var q string
		switch field {
		case "basin":
			q = `UPDATE rivers SET basin = $1 WHERE id = $2`
		case "state_abbr":
			q = `UPDATE rivers SET state_abbr = $1 WHERE id = $2`
		default:
			errorResponse(w, http.StatusBadRequest, "unknown field")
			return
		}
		if _, err := h.db.Exec(r.Context(), q, proposedValue, riverID); err != nil {
			errorResponse(w, http.StatusInternalServerError, "apply correction failed")
			return
		}
	}

	w.WriteHeader(http.StatusNoContent)
}
