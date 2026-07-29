package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ModerationHandler struct {
	db            *pgxpool.Pool
	devFallbackID string
}

func NewModerationHandler(db *pgxpool.Pool, devFallbackID string) *ModerationHandler {
	return &ModerationHandler{db: db, devFallbackID: devFallbackID}
}

func (h *ModerationHandler) reporterID(r *http.Request) (string, bool) {
	if id, ok := auth.UserIDFromContext(r.Context()); ok {
		return id, true
	}
	if h.devFallbackID != "" {
		return h.devFallbackID, true
	}
	return "", false
}

// ── POST /api/v1/user-runs/{runId}/flag ──────────────────────────────────────

func (h *ModerationHandler) FlagRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "runId")

	reporterID, ok := h.reporterID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var body struct {
		Reason string `json:"reason"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Reason == "" {
		body.Reason = "inappropriate"
	}

	ctx := r.Context()

	var exists bool
	if err := h.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM user_reaches WHERE id = $1 AND visibility = 'public' AND deleted_at IS NULL)`, runID,
	).Scan(&exists); err != nil || !exists {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}

	var flagID string
	var noteVal *string
	if body.Note != "" {
		noteVal = &body.Note
	}
	err := h.db.QueryRow(ctx, `
		INSERT INTO abuse_flags (target_type, target_id, reporter_id, reason, note)
		VALUES ('run', $1, $2, $3, $4)
		ON CONFLICT (target_type, target_id, reporter_id) DO UPDATE
			SET reason = EXCLUDED.reason, note = EXCLUDED.note
		RETURNING id
	`, runID, reporterID, body.Reason, noteVal).Scan(&flagID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("flag failed: %v", err))
		return
	}

	jsonResponse(w, http.StatusCreated, map[string]string{"id": flagID, "status": "flagged"})
}

// ── POST /api/v1/reports/{reportId}/flag ─────────────────────────────────────

func (h *ModerationHandler) FlagReport(w http.ResponseWriter, r *http.Request) {
	reportID := chi.URLParam(r, "reportId")

	reporterID, ok := h.reporterID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var body struct {
		Reason string `json:"reason"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Reason == "" {
		body.Reason = "inappropriate"
	}

	ctx := r.Context()

	var exists bool
	if err := h.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM reports WHERE id = $1 AND deleted_at IS NULL)`, reportID,
	).Scan(&exists); err != nil || !exists {
		errorResponse(w, http.StatusNotFound, "report not found")
		return
	}

	var flagID string
	var noteVal *string
	if body.Note != "" {
		noteVal = &body.Note
	}
	err := h.db.QueryRow(ctx, `
		INSERT INTO abuse_flags (target_type, target_id, reporter_id, reason, note)
		VALUES ('report', $1, $2, $3, $4)
		ON CONFLICT (target_type, target_id, reporter_id) DO UPDATE
			SET reason = EXCLUDED.reason, note = EXCLUDED.note
		RETURNING id
	`, reportID, reporterID, body.Reason, noteVal).Scan(&flagID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("flag failed: %v", err))
		return
	}

	jsonResponse(w, http.StatusCreated, map[string]string{"id": flagID, "status": "flagged"})
}

// ── POST /api/v1/plan-runs/{id}/flag ─────────────────────────────────────────

func (h *ModerationHandler) FlagPlanRun(w http.ResponseWriter, r *http.Request) {
	planRunID := chi.URLParam(r, "id")

	reporterID, ok := h.reporterID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var body struct {
		Reason string `json:"reason"`
		Note   string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if body.Reason == "" {
		body.Reason = "inappropriate"
	}

	ctx := r.Context()
	// Same email fallback as renderPlanRun's visibility gate (plan_runs.go)
	// and MyInvites/AcceptInvite/DismissInvite (invites.go): a pending email
	// invite has member_owner_id=NULL until accept, so matching only
	// member_owner_id=$2 below would 404 a logged-in invitee whose JWT email
	// matches a still-pending invite_email.
	reporterEmail, _ := auth.EmailFromContext(ctx)

	// web#354 A1: the old "parent plan visibility=public" gate is gone along
	// with the visibility concept entirely (calendar_runs has no plan_id to
	// join on either) — mirror renderPlanRun's new gate instead: 404 unless
	// the run is paddled (any authed user may see/flag a logged run), the
	// reporter is the run's own owner, or an accepted/invited crew member
	// (run_invites, re-keyed web#354 A2, was plan_members, keyed by
	// plan_run_id) — same no-existence-oracle shape as FlagRun's public gate
	// above.
	var exists bool
	if err := h.db.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM calendar_runs cr
			WHERE cr.id = $1 AND cr.deleted_at IS NULL
			  AND (
			    cr.paddled
			    OR cr.owner_id = $2
			    OR EXISTS(
			      SELECT 1 FROM run_invites ri
			      WHERE ri.run_id = cr.id AND ri.status IN ('invited','accepted')
			        AND (ri.member_owner_id = $2 OR (ri.member_owner_id IS NULL AND LOWER(ri.invite_email) = LOWER($3)))
			    )
			  )
		)
	`, planRunID, reporterID, reporterEmail).Scan(&exists); err != nil || !exists {
		errorResponse(w, http.StatusNotFound, "plan run not found")
		return
	}

	var flagID string
	var noteVal *string
	if body.Note != "" {
		noteVal = &body.Note
	}
	err := h.db.QueryRow(ctx, `
		INSERT INTO abuse_flags (target_type, target_id, reporter_id, reason, note)
		VALUES ('plan_run', $1, $2, $3, $4)
		ON CONFLICT (target_type, target_id, reporter_id) DO UPDATE
			SET reason = EXCLUDED.reason, note = EXCLUDED.note
		RETURNING id
	`, planRunID, reporterID, body.Reason, noteVal).Scan(&flagID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("flag failed: %v", err))
		return
	}

	jsonResponse(w, http.StatusCreated, map[string]string{"id": flagID, "status": "flagged"})
}

// ── GET /admin/moderation/queue ───────────────────────────────────────────────

func (h *ModerationHandler) Queue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := h.db.Query(ctx, `
		SELECT
			af.id, af.target_type, af.target_id::TEXT, af.reporter_id,
			af.reason, af.note, af.created_at,
			COALESCE(up.handle, af.reporter_id) AS reporter_handle,
			CASE
				WHEN af.target_type = 'run'
					THEN (SELECT ur.name FROM user_reaches ur WHERE ur.id = af.target_id)
				WHEN af.target_type = 'report'
					THEN (SELECT rp.name FROM reports rp WHERE rp.id = af.target_id)
				WHEN af.target_type = 'plan_run'
					-- web#354 A4: cr.name is the calendar run's own name (NOT
					-- NULL, mig 000147) — no longer needs the library-run
					-- fallback join this used before A4 added the column.
					THEN (SELECT cr.name || ' — ' || cr.run_date::text
					      FROM calendar_runs cr
					      WHERE cr.id = af.target_id)
			END AS target_name,
			CASE
				WHEN af.target_type = 'run'
					THEN (SELECT ur.slug FROM user_reaches ur WHERE ur.id = af.target_id)
				WHEN af.target_type = 'report'
					THEN (SELECT rp.slug FROM reports rp WHERE rp.id = af.target_id)
				WHEN af.target_type = 'plan_run'
					THEN (SELECT cr.slug FROM calendar_runs cr WHERE cr.id = af.target_id)
			END AS target_slug
		FROM abuse_flags af
		LEFT JOIN user_profiles up ON up.owner_id = af.reporter_id
		WHERE af.resolved_at IS NULL
		ORDER BY af.created_at ASC
		LIMIT 200
	`)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	type flagRow struct {
		ID             string  `json:"id"`
		TargetType     string  `json:"target_type"`
		TargetID       string  `json:"target_id"`
		ReporterID     string  `json:"reporter_id"`
		ReporterHandle string  `json:"reporter_handle"`
		Reason         string  `json:"reason"`
		Note           *string `json:"note,omitempty"`
		CreatedAt      string  `json:"created_at"`
		TargetName     *string `json:"target_name,omitempty"`
		TargetSlug     *string `json:"target_slug,omitempty"`
	}

	var flags []flagRow
	for rows.Next() {
		var f flagRow
		var createdAt time.Time
		if err := rows.Scan(
			&f.ID, &f.TargetType, &f.TargetID, &f.ReporterID,
			&f.Reason, &f.Note, &createdAt,
			&f.ReporterHandle,
			&f.TargetName, &f.TargetSlug,
		); err != nil {
			errorResponse(w, http.StatusInternalServerError, "scan failed")
			return
		}
		f.CreatedAt = createdAt.Format(time.RFC3339)
		flags = append(flags, f)
	}
	if flags == nil {
		flags = []flagRow{}
	}

	jsonResponse(w, http.StatusOK, map[string]any{"flags": flags})
}

// ── POST /admin/moderation/flags/{flagId}/dismiss ─────────────────────────────

func (h *ModerationHandler) Dismiss(w http.ResponseWriter, r *http.Request) {
	flagID := chi.URLParam(r, "flagId")
	ctx := r.Context()

	adminID, _ := auth.UserIDFromContext(ctx)

	tag, err := h.db.Exec(ctx, `
		UPDATE abuse_flags
		SET resolved_at = NOW(), resolved_by = $2, resolution = 'dismissed'
		WHERE id = $1 AND resolved_at IS NULL
	`, flagID, adminID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "update failed")
		return
	}
	if tag.RowsAffected() == 0 {
		errorResponse(w, http.StatusNotFound, "flag not found or already resolved")
		return
	}

	jsonResponse(w, http.StatusOK, map[string]string{"status": "dismissed"})
}

// ── POST /admin/moderation/flags/{flagId}/action ──────────────────────────────
// Soft-deletes the flagged target and marks the flag resolved.

func (h *ModerationHandler) Action(w http.ResponseWriter, r *http.Request) {
	flagID := chi.URLParam(r, "flagId")
	ctx := r.Context()

	adminID, _ := auth.UserIDFromContext(ctx)

	var targetType, targetID string
	if err := h.db.QueryRow(ctx,
		`SELECT target_type, target_id::TEXT FROM abuse_flags WHERE id = $1 AND resolved_at IS NULL`, flagID,
	).Scan(&targetType, &targetID); err != nil {
		errorResponse(w, http.StatusNotFound, "flag not found or already resolved")
		return
	}

	var deleteErr error
	switch targetType {
	case "run":
		_, deleteErr = h.db.Exec(ctx,
			`UPDATE user_reaches SET deleted_at = NOW() WHERE id = $1 AND deleted_at IS NULL`, targetID)
	case "report":
		_, deleteErr = h.db.Exec(ctx,
			`UPDATE reports SET deleted_at = NOW() WHERE id = $1 AND deleted_at IS NULL`, targetID)
	case "plan_run":
		_, deleteErr = h.db.Exec(ctx,
			`UPDATE calendar_runs SET deleted_at = NOW() WHERE id = $1 AND deleted_at IS NULL`, targetID)
	default:
		errorResponse(w, http.StatusBadRequest, "unknown target type")
		return
	}
	if deleteErr != nil {
		errorResponse(w, http.StatusInternalServerError, "delete failed")
		return
	}

	h.db.Exec(ctx, `
		UPDATE abuse_flags
		SET resolved_at = NOW(), resolved_by = $2, resolution = 'actioned'
		WHERE id = $1
	`, flagID, adminID)

	jsonResponse(w, http.StatusOK, map[string]string{"status": "actioned"})
}
