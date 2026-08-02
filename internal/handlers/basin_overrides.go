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

// BasinOverrideHandler handles per-river basin override routes.
// A user can reassign any river to a different basin grouping label
// without touching the shared rivers table.
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

// targetOwnerID resolves the owner whose basin-override rows a request should
// read or write. Mirrors UserReachHandler.authorOwnerID (internal/handlers/
// user_reaches.go) exactly, so ?as= behaves identically across both authoring
// surfaces: when the request carries ?as={handle}, the handle must name an
// existing special user AND the caller must hold an app role equal to that
// handle (loaded from user_roles by the LoadAppRoles middleware). When both
// hold, the target is that special user's owner_id. When ?as= is absent, OR
// present but the caller does not hold the matching role, this falls through
// to the caller's own id — the same silent-ignore fallback authorOwnerID
// uses. That is a deliberate, not accidental, choice: an unauthorized ?as=
// never resolves to someone else's owner_id, so there is no privilege
// escalation and no cross-account write — the request just gets treated as
// if ?as= had not been supplied and lands under the caller's own account
// instead of erroring. Returns ("", false, false) when unauthenticated.
func (h *BasinOverrideHandler) targetOwnerID(r *http.Request) (ownerID string, asSpecial bool, ok bool) {
	id, ok := h.ownerID(r)
	if !ok {
		return "", false, false
	}
	if as := r.URL.Query().Get("as"); as != "" {
		for _, role := range auth.AppRolesFromContext(r.Context()) {
			if role != as {
				continue
			}
			var specialOwnerID string
			err := h.db.QueryRow(r.Context(),
				`SELECT owner_id FROM user_profiles WHERE is_special AND handle = $1`, as,
			).Scan(&specialOwnerID)
			if err == nil {
				return specialOwnerID, true, true
			}
			break
		}
	}
	return id, false, true
}

type RiverBasinOverride struct {
	ID        string    `json:"id"`
	RiverID   string    `json:"river_id"`
	BasinKey  string    `json:"basin_key"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ── GET /api/v1/me/river-basin-overrides ─────────────────────────────────────
// Returns all of the caller's per-river basin overrides. Accepts ?as={handle}
// (see targetOwnerID) so the run edit form can read a special user's current
// overrides while authoring as them — without it, an admin editing a run
// ?as=h2oflows would see their own personal overrides instead of h2oflows',
// and the "Override Basin" modal would never pre-fill or show "reset to
// default" correctly.

func (h *BasinOverrideHandler) List(w http.ResponseWriter, r *http.Request) {
	ownerID, _, ok := h.targetOwnerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	rows, err := h.db.Query(r.Context(), `
		SELECT id, river_id, basin_key, created_at, updated_at
		FROM   user_river_basin_overrides
		WHERE  user_id = $1
		ORDER BY created_at
	`, ownerID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	overrides := []RiverBasinOverride{}
	for rows.Next() {
		var o RiverBasinOverride
		if err := rows.Scan(&o.ID, &o.RiverID, &o.BasinKey, &o.CreatedAt, &o.UpdatedAt); err != nil {
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

// ── PUT /api/v1/me/rivers/{riverID}/basin-override ───────────────────────────
// Upsert a basin override for a single river. Body: { basin_key }. Accepts
// ?as={handle} (see targetOwnerID) so an admin can save the override under a
// special user's account.

func (h *BasinOverrideHandler) Upsert(w http.ResponseWriter, r *http.Request) {
	ownerID, _, ok := h.targetOwnerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	riverID := chi.URLParam(r, "riverID")
	if riverID == "" {
		errorResponse(w, http.StatusBadRequest, "riverID is required")
		return
	}

	var body struct {
		BasinKey string `json:"basin_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.BasinKey = strings.TrimSpace(body.BasinKey)
	if body.BasinKey == "" || len(body.BasinKey) > 80 {
		errorResponse(w, http.StatusBadRequest, "basin_key must be 1–80 characters")
		return
	}

	var o RiverBasinOverride
	err := h.db.QueryRow(r.Context(), `
		INSERT INTO user_river_basin_overrides (user_id, river_id, basin_key)
		VALUES ($1, $2::uuid, $3)
		ON CONFLICT (user_id, river_id) DO UPDATE
		SET basin_key  = EXCLUDED.basin_key,
		    updated_at = NOW()
		RETURNING id, river_id, basin_key, created_at, updated_at
	`, ownerID, riverID, body.BasinKey).Scan(
		&o.ID, &o.RiverID, &o.BasinKey, &o.CreatedAt, &o.UpdatedAt,
	)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "upsert failed")
		return
	}

	jsonResponse(w, http.StatusOK, o)
}

// ── DELETE /api/v1/me/rivers/{riverID}/basin-override ────────────────────────
// Removes a river basin override. Idempotent — 204 even if no row existed.
// Accepts ?as={handle} (see targetOwnerID) so an admin can clear the override
// under a special user's account.

func (h *BasinOverrideHandler) Delete(w http.ResponseWriter, r *http.Request) {
	ownerID, _, ok := h.targetOwnerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	riverID := chi.URLParam(r, "riverID")
	if riverID == "" {
		errorResponse(w, http.StatusBadRequest, "riverID is required")
		return
	}

	_, err := h.db.Exec(r.Context(), `
		DELETE FROM user_river_basin_overrides
		WHERE user_id = $1 AND river_id = $2::uuid
	`, ownerID, riverID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "delete failed")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
