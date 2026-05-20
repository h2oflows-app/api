package handlers

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

var handleRe = regexp.MustCompile(`^[\p{L}\p{N} ',.\-]{1,50}$`)

// ProfileHandler handles /me/profile routes.
type ProfileHandler struct {
	db            *pgxpool.Pool
	devFallbackID string
}

func NewProfileHandler(db *pgxpool.Pool, devFallbackID string) *ProfileHandler {
	return &ProfileHandler{db: db, devFallbackID: devFallbackID}
}

func (h *ProfileHandler) ownerID(r *http.Request) (string, bool) {
	if id, ok := auth.UserIDFromContext(r.Context()); ok {
		return id, true
	}
	if h.devFallbackID != "" {
		return h.devFallbackID, true
	}
	return "", false
}

// GET /me/profile
func (h *ProfileHandler) Get(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var handle string
	err := h.db.QueryRow(r.Context(),
		`SELECT handle FROM user_profiles WHERE owner_id = $1`, ownerID,
	).Scan(&handle)
	if err != nil {
		// No profile yet — return empty
		jsonResponse(w, http.StatusOK, map[string]any{"handle": nil})
		return
	}

	email, _ := auth.EmailFromContext(r.Context())
	jsonResponse(w, http.StatusOK, map[string]any{
		"handle": handle,
		"email":  email,
	})
}

// PATCH /me/profile
func (h *ProfileHandler) Update(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var body struct {
		Handle *string `json:"handle"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	if body.Handle != nil {
		h := strings.TrimSpace(*body.Handle)
		if !handleRe.MatchString(h) {
			errorResponse(w, http.StatusBadRequest, "name must be 1–50 characters")
			return
		}
		*body.Handle = h
	}

	ctx := r.Context()

	if body.Handle != nil {
		_, err := h.db.Exec(ctx, `
			INSERT INTO user_profiles (owner_id, handle)
			VALUES ($1, $2)
			ON CONFLICT (owner_id) DO UPDATE
			SET handle = $2, updated_at = NOW()
		`, ownerID, *body.Handle)
		if err != nil {
			if strings.Contains(err.Error(), "unique") {
				errorResponse(w, http.StatusConflict, "handle already taken")
				return
			}
			errorResponse(w, http.StatusInternalServerError, "save failed")
			return
		}
	}

	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}
