package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
)

// ── Types (#314) ─────────────────────────────────────────────────────────────

// RateLimit is the per-account write quota shape shared by SpecialUser and
// DirectoryUser.
type RateLimit struct {
	RunsPerHour       int `json:"runs_per_hour"`
	MaxBatch          int `json:"max_batch"`
	RequestsPerMinute int `json:"requests_per_minute"`
	ConcurrentJobs    int `json:"concurrent_jobs"`
}

// SpecialUser is an "official" account (h2oflows curator + future partner
// orgs) that can be granted authoring rights via a user_roles role equal to
// its handle.
type SpecialUser struct {
	ID           string    `json:"id"` // owner_id
	Handle       string    `json:"handle"`
	DisplayName  *string   `json:"display_name"`
	IsSpecial    bool      `json:"is_special"`
	PublicOnMap  bool      `json:"public_on_map"`
	DeleteLocked bool      `json:"delete_locked"`
	RunCount     int       `json:"run_count"`
	MemberCount  int       `json:"member_count"`
	UsageHour    int       `json:"usage_hour"`
	APIKeyLast4  *string   `json:"api_key_last4"`
	CreatedAt    time.Time `json:"created_at"`
	RateLimit    RateLimit `json:"rate_limit"`
}

// h2oflowsHandle is the platform's anchor special account: permanently
// delete-locked, handle immutable, and its role must always keep >= 1 member.
const h2oflowsHandle = "h2oflows"

// RoleMember is a single user_roles grant, resolved against user_profiles
// for display.
type RoleMember struct {
	UserID      string  `json:"user_id"`
	Handle      *string `json:"handle"`
	DisplayName *string `json:"display_name"`
}

// Role is either a system role (site_admin, data_admin) or a special user's
// handle (system=false).
type Role struct {
	Name    string       `json:"name"`
	System  bool         `json:"system"`
	Members []RoleMember `json:"members"`
}

// DirectoryUser is a row in the full user directory (GET /admin/users) —
// every user_profiles row, special or not.
type DirectoryUser struct {
	OwnerID     string    `json:"owner_id"`
	Handle      string    `json:"handle"`
	DisplayName *string   `json:"display_name"`
	IsSpecial   bool      `json:"is_special"`
	Roles       []string  `json:"roles"`
	RunCount    int       `json:"run_count"`
	APIKeyLast4 *string   `json:"api_key_last4"`
	UsageHour   int       `json:"usage_hour"`
	RateLimit   RateLimit `json:"rate_limit"`
	CreatedAt   time.Time `json:"created_at"`
}

var specialHandleRe = regexp.MustCompile(`^[a-z0-9-]+$`)

// ── Special users CRUD ───────────────────────────────────────────────────────

// ListSpecialUsers handles GET /api/v1/admin/special-users.
func (h *AdminHandler) ListSpecialUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := h.db.Query(r.Context(), `
		SELECT
			up.owner_id, up.handle, up.display_name, up.is_special, up.public_on_map,
			up.delete_locked, up.created_at,
			COALESCE((SELECT COUNT(*) FROM user_reaches ur WHERE ur.owner_id = up.owner_id AND ur.deleted_at IS NULL), 0) AS run_count,
			COALESCE((SELECT COUNT(*) FROM user_roles r2 WHERE r2.role = up.handle), 0) AS member_count,
			a.api_key_last4,
			COALESCE(a.usage_hour_count, 0) AS usage_hour,
			COALESCE(a.runs_per_hour, 500) AS runs_per_hour,
			COALESCE(a.max_batch, 50) AS max_batch,
			COALESCE(a.requests_per_minute, 60) AS requests_per_minute,
			COALESCE(a.concurrent_jobs, 1) AS concurrent_jobs
		FROM user_profiles up
		LEFT JOIN user_api_access a ON a.owner_id = up.owner_id
		WHERE up.is_special
		ORDER BY up.created_at
	`)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	out := make([]SpecialUser, 0)
	for rows.Next() {
		var su SpecialUser
		if err := rows.Scan(
			&su.ID, &su.Handle, &su.DisplayName, &su.IsSpecial, &su.PublicOnMap,
			&su.DeleteLocked, &su.CreatedAt, &su.RunCount, &su.MemberCount, &su.APIKeyLast4, &su.UsageHour,
			&su.RateLimit.RunsPerHour, &su.RateLimit.MaxBatch, &su.RateLimit.RequestsPerMinute, &su.RateLimit.ConcurrentJobs,
		); err == nil {
			out = append(out, su)
		}
	}
	jsonResponse(w, http.StatusOK, out)
}

// CreateSpecialUser handles POST /api/v1/admin/special-users.
// Body: {handle, display_name, public_on_map?, runs_per_hour?}
// Creates the user_profiles row (generated owner_id, is_special=true), a
// user_api_access row (defaults 500/50/60/1, runs_per_hour overridable), and
// an API key returned ONCE. Membership is granted separately via
// /admin/roles/{handle}/members — a special user needs no role row of its
// own; the role exists implicitly via its handle.
func (h *AdminHandler) CreateSpecialUser(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Handle      string  `json:"handle"`
		DisplayName *string `json:"display_name"`
		PublicOnMap *bool   `json:"public_on_map"`
		RunsPerHour *int    `json:"runs_per_hour"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	body.Handle = strings.ToLower(strings.TrimSpace(body.Handle))
	if !specialHandleRe.MatchString(body.Handle) {
		errorResponse(w, http.StatusBadRequest, "handle must match [a-z0-9-]+")
		return
	}

	var exists bool
	_ = h.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM user_profiles WHERE handle = $1)`, body.Handle).Scan(&exists)
	if exists {
		errorResponse(w, http.StatusConflict, "handle already in use")
		return
	}

	publicOnMap := false
	if body.PublicOnMap != nil {
		publicOnMap = *body.PublicOnMap
	}
	runsPerHour := specialRunsPerHour
	if body.RunsPerHour != nil && *body.RunsPerHour > 0 {
		runsPerHour = *body.RunsPerHour
	}

	fullKey, keyHash, last4, err := generateAPIKey()
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "key generation failed")
		return
	}

	tx, err := h.db.Begin(r.Context())
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "tx begin failed")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck

	var ownerID string
	var createdAt time.Time
	// New special accounts start delete_locked=true — unlocking is a deliberate
	// admin action before a delete, so a big imported run database (AW, ...)
	// can't be dropped by a stray click.
	if err := tx.QueryRow(r.Context(), `
		INSERT INTO user_profiles (owner_id, handle, display_name, is_special, public_on_map, delete_locked)
		VALUES (gen_random_uuid()::text, $1, $2, true, $3, true)
		RETURNING owner_id, created_at
	`, body.Handle, body.DisplayName, publicOnMap).Scan(&ownerID, &createdAt); err != nil {
		errorResponse(w, http.StatusInternalServerError, "create failed: "+err.Error())
		return
	}

	if _, err := tx.Exec(r.Context(), `
		INSERT INTO user_api_access
			(owner_id, api_key_hash, api_key_last4, runs_per_hour, max_batch, requests_per_minute, concurrent_jobs)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, ownerID, keyHash, last4, runsPerHour, specialMaxBatch, specialRequestsPerMinute, specialConcurrentJobs); err != nil {
		errorResponse(w, http.StatusInternalServerError, "api access create failed: "+err.Error())
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		errorResponse(w, http.StatusInternalServerError, "commit failed")
		return
	}

	su := SpecialUser{
		ID: ownerID, Handle: body.Handle, DisplayName: body.DisplayName,
		IsSpecial: true, PublicOnMap: publicOnMap, DeleteLocked: true,
		APIKeyLast4: &last4, CreatedAt: createdAt,
		RateLimit: RateLimit{
			RunsPerHour: runsPerHour, MaxBatch: specialMaxBatch,
			RequestsPerMinute: specialRequestsPerMinute, ConcurrentJobs: specialConcurrentJobs,
		},
	}
	jsonResponse(w, http.StatusCreated, map[string]any{
		"special_user": su,
		"api_key":      fullKey,
	})
}

// UpdateSpecialUser handles PATCH /api/v1/admin/special-users/{ownerId}.
// Body: {handle?, display_name?, public_on_map?}. When handle changes,
// memberships follow: UPDATE user_roles SET role=new WHERE role=old.
func (h *AdminHandler) UpdateSpecialUser(w http.ResponseWriter, r *http.Request) {
	ownerID := chi.URLParam(r, "ownerId")
	var body struct {
		Handle       *string `json:"handle"`
		DisplayName  *string `json:"display_name"`
		PublicOnMap  *bool   `json:"public_on_map"`
		DeleteLocked *bool   `json:"delete_locked"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	var oldHandle string
	if err := h.db.QueryRow(r.Context(),
		`SELECT handle FROM user_profiles WHERE owner_id = $1 AND is_special`, ownerID,
	).Scan(&oldHandle); err != nil {
		errorResponse(w, http.StatusNotFound, "special user not found")
		return
	}

	// h2oflows is the platform anchor: permanently delete-locked and its
	// handle is immutable (the hero map and anon explore default are pinned
	// to /users/h2oflows/...).
	if oldHandle == h2oflowsHandle {
		if body.DeleteLocked != nil && !*body.DeleteLocked {
			errorResponse(w, http.StatusConflict, "@h2oflows is permanently locked for delete and cannot be unlocked")
			return
		}
		if body.Handle != nil && strings.ToLower(strings.TrimSpace(*body.Handle)) != h2oflowsHandle {
			errorResponse(w, http.StatusConflict, "the @h2oflows handle cannot be changed")
			return
		}
	}

	newHandle := oldHandle
	if body.Handle != nil {
		nh := strings.ToLower(strings.TrimSpace(*body.Handle))
		if !specialHandleRe.MatchString(nh) {
			errorResponse(w, http.StatusBadRequest, "handle must match [a-z0-9-]+")
			return
		}
		newHandle = nh
	}

	tx, err := h.db.Begin(r.Context())
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "tx begin failed")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck

	if _, err := tx.Exec(r.Context(), `
		UPDATE user_profiles
		SET handle = $2,
		    display_name = COALESCE($3, display_name),
		    public_on_map = COALESCE($4, public_on_map),
		    delete_locked = COALESCE($5, delete_locked),
		    updated_at = NOW()
		WHERE owner_id = $1
	`, ownerID, newHandle, body.DisplayName, body.PublicOnMap, body.DeleteLocked); err != nil {
		if isUniqueViolation(err) {
			errorResponse(w, http.StatusConflict, "handle already in use")
		} else {
			errorResponse(w, http.StatusInternalServerError, "update failed: "+err.Error())
		}
		return
	}

	if newHandle != oldHandle {
		if _, err := tx.Exec(r.Context(), `UPDATE user_roles SET role = $2 WHERE role = $1`, oldHandle, newHandle); err != nil {
			errorResponse(w, http.StatusInternalServerError, "membership migration failed: "+err.Error())
			return
		}
	}

	if err := tx.Commit(r.Context()); err != nil {
		errorResponse(w, http.StatusInternalServerError, "commit failed")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{"handle": newHandle})
}

// DeleteSpecialUser handles DELETE /api/v1/admin/special-users/{ownerId}.
// 409s (with the offending count) if any non-deleted user_reaches rows still
// exist; otherwise deletes the profile (cascades user_api_access) and the
// role's membership rows.
func (h *AdminHandler) DeleteSpecialUser(w http.ResponseWriter, r *http.Request) {
	ownerID := chi.URLParam(r, "ownerId")
	var handle string
	var deleteLocked bool
	if err := h.db.QueryRow(r.Context(),
		`SELECT handle, delete_locked FROM user_profiles WHERE owner_id = $1 AND is_special`, ownerID,
	).Scan(&handle, &deleteLocked); err != nil {
		errorResponse(w, http.StatusNotFound, "special user not found")
		return
	}
	// Delete-lock: refuse outright. h2oflows can never be unlocked; other
	// special users must be explicitly unlocked (PATCH delete_locked=false)
	// before a delete is accepted.
	if deleteLocked {
		errorResponse(w, http.StatusConflict, fmt.Sprintf("@%s is locked for delete — unlock it first", handle))
		return
	}

	var runCount int
	_ = h.db.QueryRow(r.Context(),
		`SELECT COUNT(*) FROM user_reaches WHERE owner_id = $1 AND deleted_at IS NULL`, ownerID,
	).Scan(&runCount)
	if runCount > 0 {
		errorResponse(w, http.StatusConflict, fmt.Sprintf("special user still owns %d run(s)", runCount))
		return
	}

	tx, err := h.db.Begin(r.Context())
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "tx begin failed")
		return
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck

	if _, err := tx.Exec(r.Context(), `DELETE FROM user_roles WHERE role = $1`, handle); err != nil {
		errorResponse(w, http.StatusInternalServerError, "membership cleanup failed")
		return
	}
	if _, err := tx.Exec(r.Context(), `DELETE FROM user_profiles WHERE owner_id = $1`, ownerID); err != nil {
		errorResponse(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		errorResponse(w, http.StatusInternalServerError, "commit failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RotateAPIKey handles POST /api/v1/admin/special-users/{ownerId}/rotate-key
// and POST /api/v1/admin/users/{ownerId}/rotate-key — either route rotates
// (or first-issues) the API key for any user_profiles owner_id. Returns the
// full key once.
func (h *AdminHandler) RotateAPIKey(w http.ResponseWriter, r *http.Request) {
	ownerID := chi.URLParam(r, "ownerId")
	var isSpecial bool
	if err := h.db.QueryRow(r.Context(),
		`SELECT is_special FROM user_profiles WHERE owner_id = $1`, ownerID,
	).Scan(&isSpecial); err != nil {
		errorResponse(w, http.StatusNotFound, "user not found")
		return
	}

	fullKey, keyHash, last4, err := generateAPIKey()
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "key generation failed")
		return
	}

	runsPerHour, maxBatch, reqPerMin, concurrent := defaultRunsPerHour, defaultMaxBatch, defaultRequestsPerMinute, defaultConcurrentJobs
	if isSpecial {
		runsPerHour, maxBatch, reqPerMin, concurrent = specialRunsPerHour, specialMaxBatch, specialRequestsPerMinute, specialConcurrentJobs
	}

	_, err = h.db.Exec(r.Context(), `
		INSERT INTO user_api_access
			(owner_id, api_key_hash, api_key_last4, runs_per_hour, max_batch, requests_per_minute, concurrent_jobs)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (owner_id) DO UPDATE SET
			api_key_hash  = EXCLUDED.api_key_hash,
			api_key_last4 = EXCLUDED.api_key_last4,
			updated_at    = NOW()
	`, ownerID, keyHash, last4, runsPerHour, maxBatch, reqPerMin, concurrent)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "rotate failed")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{"api_key": fullKey})
}

// ── Roles ────────────────────────────────────────────────────────────────────

// ListRoles handles GET /api/v1/admin/roles — system roles (site_admin,
// data_admin) plus one Role per special user (name=handle, system=false).
func (h *AdminHandler) ListRoles(w http.ResponseWriter, r *http.Request) {
	roles := make([]Role, 0)

	roleMembers := func(role string) []RoleMember {
		members := make([]RoleMember, 0)
		rows, err := h.db.Query(r.Context(), `
			SELECT ur.user_id, up.handle, up.display_name
			FROM user_roles ur
			LEFT JOIN user_profiles up ON up.owner_id = ur.user_id
			WHERE ur.role = $1 AND ur.river_id IS NULL
			ORDER BY ur.created_at
		`, role)
		if err != nil {
			return members
		}
		defer rows.Close()
		for rows.Next() {
			var m RoleMember
			if rows.Scan(&m.UserID, &m.Handle, &m.DisplayName) == nil {
				members = append(members, m)
			}
		}
		return members
	}

	for _, sysRole := range []string{"site_admin", "data_admin"} {
		roles = append(roles, Role{Name: sysRole, System: true, Members: roleMembers(sysRole)})
	}

	handleRows, err := h.db.Query(r.Context(), `SELECT handle FROM user_profiles WHERE is_special ORDER BY created_at`)
	if err == nil {
		var handles []string
		for handleRows.Next() {
			var handle string
			if handleRows.Scan(&handle) == nil {
				handles = append(handles, handle)
			}
		}
		handleRows.Close()
		for _, handle := range handles {
			roles = append(roles, Role{Name: handle, System: false, Members: roleMembers(handle)})
		}
	}

	jsonResponse(w, http.StatusOK, roles)
}

// AddRoleMember handles POST /api/v1/admin/roles/{role}/members. Body:
// {user_id}. Validated the same way as AssignRole.
func (h *AdminHandler) AddRoleMember(w http.ResponseWriter, r *http.Request) {
	role := chi.URLParam(r, "role")
	var body struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UserID == "" {
		errorResponse(w, http.StatusBadRequest, "user_id is required")
		return
	}
	if !isValidRoleName(r.Context(), h.db, role) {
		errorResponse(w, http.StatusBadRequest, "unknown role")
		return
	}

	var id string
	err := h.db.QueryRow(r.Context(), `
		INSERT INTO user_roles (user_id, role)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING
		RETURNING id
	`, body.UserID, role).Scan(&id)
	if err != nil {
		// ON CONFLICT DO NOTHING means no rows returned if duplicate — that's fine.
		jsonResponse(w, http.StatusCreated, map[string]string{"id": id})
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]string{"id": id})
}

// RemoveRoleMember handles DELETE /api/v1/admin/roles/{role}/members/{userId}.
func (h *AdminHandler) RemoveRoleMember(w http.ResponseWriter, r *http.Request) {
	role := chi.URLParam(r, "role")
	userID := chi.URLParam(r, "userId")

	// Last-member guards: the platform must always retain at least one
	// site_admin membership row, and the h2oflows role must always keep at
	// least one member (someone must be able to steward the anchor account).
	// The COUNT guard is inside the DELETE so check-and-delete is atomic.
	tag, err := h.db.Exec(r.Context(), `
		DELETE FROM user_roles
		WHERE role = $1 AND user_id = $2 AND river_id IS NULL
		  AND ($1 NOT IN ('site_admin', 'h2oflows')
		       OR (SELECT COUNT(*) FROM user_roles WHERE role = $1 AND river_id IS NULL) > 1)
	`, role, userID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "delete failed")
		return
	}
	if tag.RowsAffected() == 0 {
		// Nothing deleted: either the membership doesn't exist, or it's the last
		// member of a guarded role. Disambiguate for a useful error.
		var exists bool
		_ = h.db.QueryRow(r.Context(),
			`SELECT EXISTS(SELECT 1 FROM user_roles WHERE role = $1 AND user_id = $2 AND river_id IS NULL)`,
			role, userID,
		).Scan(&exists)
		if exists {
			errorResponse(w, http.StatusConflict,
				fmt.Sprintf("cannot remove the last member of the %s role — at least one member must remain", role))
			return
		}
		errorResponse(w, http.StatusNotFound, "membership not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Directory ────────────────────────────────────────────────────────────────

// ListDirectoryUsers handles GET /api/v1/admin/users?q=&limit=&offset= — every
// user_profiles row (special or not), with roles/run-count/rate-limit joined.
func (h *AdminHandler) ListDirectoryUsers(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	limit := clampInt(parseIntOr(r.URL.Query().Get("limit"), 50), 1, 200)
	offset := max(parseIntOr(r.URL.Query().Get("offset"), 0), 0)
	pattern := "%" + q + "%"

	rows, err := h.db.Query(r.Context(), `
		SELECT
			up.owner_id, up.handle, up.display_name, up.is_special, up.created_at,
			COALESCE((SELECT COUNT(*) FROM user_reaches ur WHERE ur.owner_id = up.owner_id AND ur.deleted_at IS NULL), 0) AS run_count,
			a.api_key_last4,
			COALESCE(a.usage_hour_count, 0) AS usage_hour,
			COALESCE(a.runs_per_hour, 100) AS runs_per_hour,
			COALESCE(a.max_batch, 10) AS max_batch,
			COALESCE(a.requests_per_minute, 30) AS requests_per_minute,
			COALESCE(a.concurrent_jobs, 1) AS concurrent_jobs,
			COALESCE(
				(SELECT array_agg(ur2.role) FROM user_roles ur2 WHERE ur2.user_id = up.owner_id AND ur2.river_id IS NULL),
				'{}'
			) AS roles
		FROM user_profiles up
		LEFT JOIN user_api_access a ON a.owner_id = up.owner_id
		WHERE ($1 = '' OR up.handle ILIKE $2 OR up.display_name ILIKE $2)
		ORDER BY up.created_at DESC
		LIMIT $3 OFFSET $4
	`, q, pattern, limit, offset)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	out := make([]DirectoryUser, 0)
	for rows.Next() {
		var du DirectoryUser
		if err := rows.Scan(
			&du.OwnerID, &du.Handle, &du.DisplayName, &du.IsSpecial, &du.CreatedAt,
			&du.RunCount, &du.APIKeyLast4, &du.UsageHour,
			&du.RateLimit.RunsPerHour, &du.RateLimit.MaxBatch, &du.RateLimit.RequestsPerMinute, &du.RateLimit.ConcurrentJobs,
			&du.Roles,
		); err == nil {
			if du.Roles == nil {
				du.Roles = []string{}
			}
			out = append(out, du)
		}
	}
	jsonResponse(w, http.StatusOK, out)
}

// UpdateRateLimit handles PATCH /api/v1/admin/users/{ownerId}/rate-limit.
// Body: {runs_per_hour?, max_batch?, requests_per_minute?, concurrent_jobs?}.
// Upserts user_api_access; standard defaults (100/10/30/1) apply to any field
// missing on first insert.
func (h *AdminHandler) UpdateRateLimit(w http.ResponseWriter, r *http.Request) {
	ownerID := chi.URLParam(r, "ownerId")
	var body struct {
		RunsPerHour       *int `json:"runs_per_hour"`
		MaxBatch          *int `json:"max_batch"`
		RequestsPerMinute *int `json:"requests_per_minute"`
		ConcurrentJobs    *int `json:"concurrent_jobs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	var exists bool
	_ = h.db.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM user_profiles WHERE owner_id = $1)`, ownerID).Scan(&exists)
	if !exists {
		errorResponse(w, http.StatusNotFound, "user not found")
		return
	}

	_, err := h.db.Exec(r.Context(), `
		INSERT INTO user_api_access (owner_id, runs_per_hour, max_batch, requests_per_minute, concurrent_jobs)
		VALUES ($1, COALESCE($2, 100), COALESCE($3, 10), COALESCE($4, 30), COALESCE($5, 1))
		ON CONFLICT (owner_id) DO UPDATE SET
			runs_per_hour       = COALESCE($2, user_api_access.runs_per_hour),
			max_batch           = COALESCE($3, user_api_access.max_batch),
			requests_per_minute = COALESCE($4, user_api_access.requests_per_minute),
			concurrent_jobs     = COALESCE($5, user_api_access.concurrent_jobs),
			updated_at          = NOW()
	`, ownerID, body.RunsPerHour, body.MaxBatch, body.RequestsPerMinute, body.ConcurrentJobs)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "update failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ── Me ───────────────────────────────────────────────────────────────────────

// AuthorableAccounts handles GET /api/v1/me/authorable-accounts — special
// users whose handle is one of the caller's app roles. Authenticated, no
// admin gate.
func (h *AdminHandler) AuthorableAccounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := auth.UserIDFromContext(r.Context()); !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	type authorableAccount struct {
		OwnerID     string  `json:"owner_id"`
		Handle      string  `json:"handle"`
		DisplayName *string `json:"display_name"`
	}
	accounts := make([]authorableAccount, 0)

	roles := auth.AppRolesFromContext(r.Context())
	if len(roles) == 0 {
		jsonResponse(w, http.StatusOK, accounts)
		return
	}

	rows, err := h.db.Query(r.Context(), `
		SELECT owner_id, handle, display_name FROM user_profiles
		WHERE is_special AND handle = ANY($1::text[])
	`, roles)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var a authorableAccount
		if rows.Scan(&a.OwnerID, &a.Handle, &a.DisplayName) == nil {
			accounts = append(accounts, a)
		}
	}
	jsonResponse(w, http.StatusOK, accounts)
}
