package handlers

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

// handleRe enforces URL-safe handles: 3-30 chars, lowercase a-z/0-9, hyphen/underscore
// allowed inside, must start with letter/digit. Used for /runs/{handle}/{slug}.
var handleRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{2,29}$`)

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
		hv := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(*body.Handle, "@")))
		if !handleRe.MatchString(hv) {
			errorResponse(w, http.StatusBadRequest, "handle must be 3-30 chars: a-z, 0-9, hyphen, underscore; start with letter/digit")
			return
		}
		*body.Handle = hv
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

// POST /me/profile/suggest — returns up to 5 available handle suggestions derived
// from a seed (email or partial handle). Body: {"seed": "user@example.com"}.
// Used by the post-signup claim modal to propose defaults.
func (h *ProfileHandler) Suggest(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.ownerID(r); !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		Seed string `json:"seed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	seed := strings.ToLower(strings.TrimSpace(body.Seed))
	if i := strings.Index(seed, "@"); i > 0 {
		seed = seed[:i]
	}
	// Slug-ify: keep a-z 0-9; collapse runs of other chars to hyphen.
	cleaned := nonAlnumRe.ReplaceAllString(seed, "-")
	cleaned = strings.Trim(cleaned, "-")
	if len(cleaned) < 3 {
		cleaned = "paddler"
	}
	if len(cleaned) > 28 {
		cleaned = cleaned[:28]
	}

	out := make([]string, 0, 5)
	candidates := []string{cleaned}
	for i := 2; i <= 9 && len(candidates) < 8; i++ {
		candidates = append(candidates, cleaned+"-"+strconv.Itoa(i))
	}
	for _, c := range candidates {
		if !handleRe.MatchString(c) {
			continue
		}
		var taken string
		err := h.db.QueryRow(r.Context(),
			`SELECT handle FROM user_profiles WHERE LOWER(handle) = LOWER($1)`, c,
		).Scan(&taken)
		if err != nil { // not found = available
			out = append(out, c)
			if len(out) >= 5 {
				break
			}
		}
	}
	jsonResponse(w, http.StatusOK, map[string]any{"suggestions": out})
}
