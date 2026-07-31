package handlers

import (
	"encoding/json"
	"fmt"
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
	var displayName *string
	err := h.db.QueryRow(r.Context(),
		`SELECT handle, display_name FROM user_profiles WHERE owner_id = $1`, ownerID,
	).Scan(&handle, &displayName)
	if err != nil {
		// No profile yet — return empty. display_name included (null) even
		// here for shape consistency — the web's contract is "this key is
		// always present, nullable" rather than "check for the key first".
		jsonResponse(w, http.StatusOK, map[string]any{"handle": nil, "display_name": nil})
		return
	}

	email, _ := auth.EmailFromContext(r.Context())
	jsonResponse(w, http.StatusOK, map[string]any{
		"handle":       handle,
		"email":        email,
		"display_name": displayName,
	})
}

// maxDisplayNameLen caps user_profiles.display_name as written through THIS
// endpoint. The column itself predates this feature (mig 000130, #314
// special users — "H2OFlows" etc.) and was, until now, only writable by an
// admin via special_users.go (CreateSpecialUser/UpdateSpecialUser,
// unconstrained length) for is_special accounts; PATCH /me/profile below
// extends write access to any user's OWN row, for the display-names
// feature's product decision — ONE free-text field users type themselves
// (e.g. "Ian K"), a self-abbreviated last name rather than split
// first/last columns. 60 is generous for "First L." / "First Last" while
// still bounding email/UI layout; it constrains only this self-service
// path, not the pre-existing admin one.
const maxDisplayNameLen = 60

// PATCH /me/profile
func (h *ProfileHandler) Update(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var body struct {
		Handle      *string `json:"handle"`
		DisplayName *string `json:"display_name"`
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

	// dnProvided/dnValue: display_name is nullable and "" clears it to NULL
	// (contract: absent means "use @handle"), so a *string alone can't carry
	// both "key omitted" (leave column untouched) and "key sent as ''" (clear
	// it) — dnProvided disambiguates the two, same shape as plan_runs.go's
	// meetupSpotProvided/materialFieldPresent convention for this package's
	// other COALESCE/CASE-WHEN patch endpoints.
	var dnProvided bool
	var dnValue *string
	if body.DisplayName != nil {
		dnProvided = true
		trimmed := strings.TrimSpace(*body.DisplayName)
		if len(trimmed) > maxDisplayNameLen {
			errorResponse(w, http.StatusUnprocessableEntity, fmt.Sprintf("display_name must be %d characters or fewer", maxDisplayNameLen))
			return
		}
		if trimmed != "" {
			dnValue = &trimmed
		}
		// trimmed == "" leaves dnValue nil -> stored as SQL NULL below.
	}

	ctx := r.Context()

	switch {
	case body.Handle != nil:
		// Handle claims/renames the row (INSERT ... ON CONFLICT, unchanged
		// from before this endpoint touched display_name) — folds in
		// display_name too when both arrive in the same PATCH, via a CASE
		// WHEN so an un-provided display_name doesn't clobber an existing
		// value (ELSE branch on UPDATE) or write a bogus non-NULL on a fresh
		// INSERT (ELSE branch there is a literal NULL, correct for a
		// brand-new row).
		_, err := h.db.Exec(ctx, `
			INSERT INTO user_profiles (owner_id, handle, display_name)
			VALUES ($1, $2, CASE WHEN $3 THEN $4 ELSE NULL END)
			ON CONFLICT (owner_id) DO UPDATE
			SET handle = $2,
			    display_name = CASE WHEN $3 THEN $4 ELSE user_profiles.display_name END,
			    updated_at = NOW()
		`, ownerID, *body.Handle, dnProvided, dnValue)
		if err != nil {
			if strings.Contains(err.Error(), "unique") {
				errorResponse(w, http.StatusConflict, "handle already taken")
				return
			}
			errorResponse(w, http.StatusInternalServerError, "save failed")
			return
		}

	case dnProvided:
		// display_name-only PATCH: no handle in this request, so there's no
		// INSERT path (user_profiles.handle is NOT NULL — a row can't exist
		// without one). A plain UPDATE against an existing row; 0 rows
		// affected means the caller has never claimed a handle yet, which is
		// a real 422 rather than a silent no-op (there's nowhere to attach a
		// display name to).
		tag, err := h.db.Exec(ctx,
			`UPDATE user_profiles SET display_name = $1, updated_at = NOW() WHERE owner_id = $2`,
			dnValue, ownerID,
		)
		if err != nil {
			errorResponse(w, http.StatusInternalServerError, "save failed")
			return
		}
		if tag.RowsAffected() == 0 {
			errorResponse(w, http.StatusUnprocessableEntity, "claim a handle before setting a display name")
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
