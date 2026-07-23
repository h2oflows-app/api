package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/h2oflow/h2oflow/apps/api/internal/auth"
	"github.com/h2oflow/h2oflow/apps/api/internal/ics"
	"github.com/h2oflow/h2oflow/apps/api/internal/mail"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// InviteHandler handles /plans/{id}/invite, /me/invites, /invites/*,
// /plans/{id}/join, and /plans/{id}/crew/* (#246 A4). Split into its own
// file/handler type from PlanHandler (plans.go/plan_runs.go/calendar.go)
// since it owns a distinct table relationship (plan_members) and a new
// external dependency (mail.Mailer) — but the two share package-level
// helpers (dbQueryer, apiError, parseDate, userToday, localNoonUTC,
// validPlanTypes) defined in plans.go, matching this codebase's convention
// of duplicating only the handler-struct-bound helpers (ownerID,
// ensureHandle) per type.
type InviteHandler struct {
	db            *pgxpool.Pool
	devFallbackID string
	mailer        mail.Mailer
	rlInvite      *reportRateLimiter // 20/hr/owner — POST /plans/{id}/invite
	rlJoin        *reportRateLimiter // 10/hr/owner — POST /plans/{id}/join
}

func NewInviteHandler(db *pgxpool.Pool, devFallbackID string, mailer mail.Mailer) *InviteHandler {
	if mailer == nil {
		mailer = mail.NoopMailer{}
	}
	return &InviteHandler{
		db:            db,
		devFallbackID: devFallbackID,
		mailer:        mailer,
		rlInvite:      newReportRateLimiter(),
		rlJoin:        newReportRateLimiter(),
	}
}

func (h *InviteHandler) ownerID(r *http.Request) (string, bool) {
	if id, ok := auth.UserIDFromContext(r.Context()); ok {
		return id, true
	}
	if h.devFallbackID != "" {
		return h.devFallbackID, true
	}
	return "", false
}

func (h *InviteHandler) respondAPIError(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		errorResponse(w, ae.status, ae.msg)
		return
	}
	errorResponse(w, http.StatusInternalServerError, err.Error())
}

// ── Shared invite machinery (reused by POST /plans' invites[] — plans.go) ──

// invitedPlanInfo is the subset of a plan's fields inviteOne and the invite
// email need, gathered once by the caller (either from the in-memory values
// of a just-created plan in PlanHandler.Create, or via loadInvitedPlanInfo
// for the standalone POST /plans/{id}/invite endpoint).
type invitedPlanInfo struct {
	ID         string
	Slug       string
	Name       string
	Location   string
	StartDate  string
	EndDate    string
	HostHandle string
}

// inviteBody is the request shape for a single invite — used both by
// POST /plans/{id}/invite and the `invites` array in POST /plans.
type inviteBody struct {
	Handle    *string `json:"handle"`
	Email     *string `json:"email"`
	AttachICS *bool   `json:"attach_ics"`
}

type inviteResult struct {
	MemberID string
	Status   string
	Created  bool // true -> 201 (new row); false -> 200 (existing/self, no dup)
}

// pendingInviteMail carries everything needed to send + attach an invite
// email after the enclosing transaction commits — an outbound HTTPS call to
// Resend must never run while a DB tx is open. Only produced by the email
// path of inviteOne (handle invites never send mail).
type pendingInviteMail struct {
	to        string
	rawToken  string
	attachICS bool
	plan      invitedPlanInfo
}

// inviteOne inserts (or finds an existing) plan_members row for a single
// handle/email invite. Runs via q, a plain pool or a tx, so it is shared
// verbatim by POST /plans' single-tx invites[] array (plans.go Create) and
// the standalone POST /plans/{id}/invite — mirrors insertPlanRun's
// dbQueryer-parameterized shape (plan_runs.go).
func inviteOne(ctx context.Context, q dbQueryer, plan invitedPlanInfo, hostOwnerID string, body inviteBody) (inviteResult, *pendingInviteMail, error) {
	switch {
	case body.Handle != nil && strings.TrimSpace(*body.Handle) != "":
		handle := strings.TrimPrefix(strings.TrimSpace(*body.Handle), "@")

		var targetOwnerID string
		if err := q.QueryRow(ctx,
			`SELECT owner_id FROM user_profiles WHERE LOWER(handle) = LOWER($1)`, handle,
		).Scan(&targetOwnerID); err != nil {
			return inviteResult{}, nil, &apiError{http.StatusNotFound, "user not found"}
		}
		if targetOwnerID == hostOwnerID {
			// Host invites themselves — already implicitly "in" the plan
			// (no organizer row exists); no-op, 200 existing.
			return inviteResult{Status: "existing"}, nil, nil
		}

		var memberID, status string
		err := q.QueryRow(ctx, `
			INSERT INTO plan_members (plan_id, member_owner_id, invite_handle, invited_by, origin, status)
			VALUES ($1::uuid, $2, $3, $4, 'invite', 'invited')
			ON CONFLICT (plan_id, member_owner_id) WHERE member_owner_id IS NOT NULL DO NOTHING
			RETURNING id, status::text
		`, plan.ID, targetOwnerID, handle, hostOwnerID).Scan(&memberID, &status)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return inviteResult{}, nil, fmt.Errorf("invite insert: %w", err)
			}
			// ON CONFLICT DO NOTHING with no RETURNING row: already a member.
			if serr := q.QueryRow(ctx,
				`SELECT id, status::text FROM plan_members WHERE plan_id = $1::uuid AND member_owner_id = $2`,
				plan.ID, targetOwnerID,
			).Scan(&memberID, &status); serr != nil {
				return inviteResult{}, nil, fmt.Errorf("invite lookup: %w", serr)
			}
			return inviteResult{MemberID: memberID, Status: status, Created: false}, nil, nil
		}
		return inviteResult{MemberID: memberID, Status: status, Created: true}, nil, nil

	case body.Email != nil && strings.TrimSpace(*body.Email) != "":
		email := strings.ToLower(strings.TrimSpace(*body.Email))
		attachICS := body.AttachICS == nil || *body.AttachICS // default TRUE

		rawToken, tokenHash, terr := generateInviteToken()
		if terr != nil {
			return inviteResult{}, nil, fmt.Errorf("token generation: %w", terr)
		}

		var memberID, status string
		var inserted bool
		err := q.QueryRow(ctx, `
			INSERT INTO plan_members (plan_id, invite_email, invited_by, origin, status, invite_token_hash)
			VALUES ($1::uuid, $2, $3, 'invite', 'invited', $4)
			ON CONFLICT (plan_id, lower(invite_email)) WHERE invite_email IS NOT NULL AND member_owner_id IS NULL DO NOTHING
			RETURNING id, status::text
		`, plan.ID, email, hostOwnerID, tokenHash).Scan(&memberID, &status)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				return inviteResult{}, nil, fmt.Errorf("invite insert: %w", err)
			}
			if serr := q.QueryRow(ctx,
				`SELECT id, status::text FROM plan_members WHERE plan_id = $1::uuid AND lower(invite_email) = $2 AND member_owner_id IS NULL`,
				plan.ID, email,
			).Scan(&memberID, &status); serr != nil {
				return inviteResult{}, nil, fmt.Errorf("invite lookup: %w", serr)
			}
		} else {
			inserted = true
		}

		var pending *pendingInviteMail
		if inserted {
			pending = &pendingInviteMail{to: email, rawToken: rawToken, attachICS: attachICS, plan: plan}
		}
		return inviteResult{MemberID: memberID, Status: status, Created: inserted}, pending, nil

	default:
		return inviteResult{}, nil, &apiError{http.StatusBadRequest, "handle or email is required"}
	}
}

// generateInviteToken mints a random invite claim token: the raw token is
// embedded in the emailed link (?invite=...) and never stored; only its
// SHA-256 hex digest is persisted (plan_members.invite_token_hash) —
// mirrors auth.APIKey's hash-and-lookup shape (internal/auth/apikey.go) and
// generateAPIKey's crypto/rand construction (authoring.go).
func generateInviteToken() (raw, hash string, err error) {
	buf := make([]byte, 32)
	if _, err = rand.Read(buf); err != nil {
		return "", "", err
	}
	raw = hex.EncodeToString(buf)
	sum := sha256.Sum256([]byte(raw))
	hash = hex.EncodeToString(sum[:])
	return raw, hash, nil
}

// sendInviteMail builds the invite email (+ .ics attachment when requested)
// and sends it. Package-level (not a method) so both InviteHandler and
// PlanHandler.Create can fire it after their respective transaction commits,
// each with its own detached context — the triggering HTTP request's
// context dies at response, so a fresh context.Background()+timeout is used
// per the contract ("Sends are async... go func with its own
// context.Background+timeout").
func sendInviteMail(mailer mail.Mailer, p pendingInviteMail) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	host := p.plan.HostHandle
	if host == "" {
		host = "a paddler"
	}
	planURL := fmt.Sprintf("https://h2oflows.app/plans/%s/%s", p.plan.HostHandle, p.plan.Slug)
	acceptURL := fmt.Sprintf("%s?invite=%s", planURL, p.rawToken)

	dateRange := p.plan.StartDate
	if p.plan.EndDate != p.plan.StartDate {
		dateRange = fmt.Sprintf("%s – %s", p.plan.StartDate, p.plan.EndDate)
	}

	subject := fmt.Sprintf("@%s invited you to %s", host, p.plan.Name)
	html := fmt.Sprintf(
		`<p>@%s invited you to <strong>%s</strong> (%s).</p><p><a href="%s">View the plan on H2OFlows</a></p>`,
		host, p.plan.Name, dateRange, acceptURL,
	)
	text := fmt.Sprintf("@%s invited you to %s (%s).\n%s", host, p.plan.Name, dateRange, acceptURL)

	msg := mail.Message{To: p.to, Subject: subject, HTML: html, Text: text}
	if p.attachICS {
		icsBody := ics.BuildPlanInvite(ics.PlanInviteInput{
			PlanID:    p.plan.ID,
			Name:      p.plan.Name,
			Location:  p.plan.Location,
			StartDate: p.plan.StartDate,
			EndDate:   p.plan.EndDate,
			URL:       planURL,
		})
		msg.Attachments = append(msg.Attachments, mail.Attachment{
			Filename:    "invite.ics",
			ContentType: "text/calendar; charset=utf-8; method=PUBLISH",
			Content:     []byte(icsBody),
		})
	}

	if err := mailer.Send(ctx, msg); err != nil {
		log.Printf("invite mail send failed (to=%s plan=%s): %v", p.to, p.plan.ID, err)
	}
}

// loadInvitedPlanInfo fetches the host-only plan fields needed to send an
// invite, in one call — 404s (rather than 403) when the plan doesn't exist
// or the caller isn't its host, matching this package's existing
// not-found-over-forbidden convention for owner-scoped lookups (e.g.
// PlanHandler.Update).
func (h *InviteHandler) loadInvitedPlanInfo(ctx context.Context, planID, hostOwnerID string) (invitedPlanInfo, error) {
	var p invitedPlanInfo
	var location *string
	err := h.db.QueryRow(ctx, `
		SELECT p.id, p.slug, p.name, p.location, p.start_date::text, p.end_date::text, COALESCE(up.handle, '')
		FROM plans p
		LEFT JOIN user_profiles up ON up.owner_id = p.owner_id
		WHERE p.id = $1::uuid AND p.owner_id = $2 AND p.deleted_at IS NULL
	`, planID, hostOwnerID).Scan(&p.ID, &p.Slug, &p.Name, &location, &p.StartDate, &p.EndDate, &p.HostHandle)
	if err != nil {
		return invitedPlanInfo{}, &apiError{http.StatusNotFound, "plan not found"}
	}
	if location != nil {
		p.Location = *location
	}
	return p, nil
}

// ── POST /plans/{id}/invite ──────────────────────────────────────────────

func (h *InviteHandler) InviteToPlan(w http.ResponseWriter, r *http.Request) {
	planID := chi.URLParam(r, "id")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !h.rlInvite.allow(ownerID, 20, time.Hour) {
		errorResponse(w, http.StatusTooManyRequests, "rate limit: 20 invites per hour")
		return
	}

	var body inviteBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	ctx := r.Context()
	plan, perr := h.loadInvitedPlanInfo(ctx, planID, ownerID)
	if perr != nil {
		h.respondAPIError(w, perr)
		return
	}

	result, pending, err := inviteOne(ctx, h.db, plan, ownerID, body)
	if err != nil {
		h.respondAPIError(w, err)
		return
	}

	sent := false
	if pending != nil {
		sent = true
		go sendInviteMail(h.mailer, *pending)
	}

	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	jsonResponse(w, status, map[string]any{
		"member_id": result.MemberID,
		"status":    result.Status,
		"sent":      sent,
	})
}

// ── GET /me/invites ───────────────────────────────────────────────────────

func (h *InviteHandler) MyInvites(w http.ResponseWriter, r *http.Request) {
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()
	// email may be "" for dev-fallback / API-key-style auth (auth.Claims.Email
	// is only populated for real Supabase JWTs) — the LOWER(email)=LOWER('')
	// comparison below never matches a real invite_email, so this safely
	// degrades to member_owner_id-only matching (contract: "if absent from
	// the token, document and fall back to member_owner_id-only").
	email, _ := auth.EmailFromContext(ctx)

	rows, err := h.db.Query(ctx, `
		SELECT pm.id, pm.status::text, pm.dismissed_at, (pm.invite_handle IS NOT NULL) AS via_handle, pm.created_at,
		       p.id, p.slug, p.name, p.type::text, p.start_date::text, p.end_date::text, p.location,
		       COALESCE(up.handle, '')
		FROM plan_members pm
		JOIN plans p ON p.id = pm.plan_id AND p.deleted_at IS NULL
		LEFT JOIN user_profiles up ON up.owner_id = p.owner_id
		WHERE pm.origin = 'invite'
		  AND (pm.member_owner_id = $1 OR (pm.member_owner_id IS NULL AND LOWER(pm.invite_email) = LOWER($2)))
		ORDER BY pm.created_at DESC
	`, ownerID, email)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	type invitedPlan struct {
		ID         string  `json:"id"`
		Slug       string  `json:"slug"`
		Name       string  `json:"name"`
		Type       string  `json:"type"`
		StartDate  string  `json:"start_date"`
		EndDate    string  `json:"end_date"`
		Location   *string `json:"location,omitempty"`
		HostHandle string  `json:"host_handle"`
	}
	type myInvite struct {
		MemberID    string      `json:"member_id"`
		Status      string      `json:"status"`
		DismissedAt *string     `json:"dismissed_at,omitempty"`
		InvitedVia  string      `json:"invited_via"` // "handle" | "email"
		CreatedAt   string      `json:"created_at"`
		Plan        invitedPlan `json:"plan"`
	}

	out := []myInvite{}
	for rows.Next() {
		var mi myInvite
		var dismissedAtRaw *time.Time
		var createdAtRaw time.Time
		var viaHandle bool
		if err := rows.Scan(
			&mi.MemberID, &mi.Status, &dismissedAtRaw, &viaHandle, &createdAtRaw,
			&mi.Plan.ID, &mi.Plan.Slug, &mi.Plan.Name, &mi.Plan.Type, &mi.Plan.StartDate, &mi.Plan.EndDate, &mi.Plan.Location,
			&mi.Plan.HostHandle,
		); err != nil {
			errorResponse(w, http.StatusInternalServerError, "scan failed")
			return
		}
		if dismissedAtRaw != nil {
			s := dismissedAtRaw.Format(time.RFC3339)
			mi.DismissedAt = &s
		}
		mi.CreatedAt = createdAtRaw.Format(time.RFC3339)
		mi.InvitedVia = "email"
		if viaHandle {
			mi.InvitedVia = "handle"
		}
		out = append(out, mi)
	}

	jsonResponse(w, http.StatusOK, map[string]any{"invites": out})
}

// ── POST /invites/{memberId}/accept ──────────────────────────────────────

func (h *InviteHandler) AcceptInvite(w http.ResponseWriter, r *http.Request) {
	memberID := chi.URLParam(r, "memberId")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var body struct {
		Token *string `json:"token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // body is optional (handle/email invitees send none)

	ctx := r.Context()
	email, _ := auth.EmailFromContext(ctx)

	var curMemberOwnerID, curEmail, curTokenHash *string
	var status, planID string
	if err := h.db.QueryRow(ctx, `
		SELECT member_owner_id, invite_email, invite_token_hash, status::text, plan_id::text
		FROM plan_members WHERE id = $1::uuid AND origin = 'invite'
	`, memberID).Scan(&curMemberOwnerID, &curEmail, &curTokenHash, &status, &planID); err != nil {
		errorResponse(w, http.StatusNotFound, "invite not found")
		return
	}

	allowed := curMemberOwnerID != nil && *curMemberOwnerID == ownerID
	if !allowed && curEmail != nil && email != "" && strings.EqualFold(*curEmail, email) {
		allowed = true
	}
	if !allowed && body.Token != nil && *body.Token != "" && curTokenHash != nil {
		sum := sha256.Sum256([]byte(*body.Token))
		if hex.EncodeToString(sum[:]) == *curTokenHash {
			allowed = true
		}
	}
	if !allowed {
		errorResponse(w, http.StatusForbidden, "not authorized to accept this invite")
		return
	}

	if status == "declined" {
		errorResponse(w, http.StatusConflict, "invite was declined")
		return
	}
	if status == "accepted" && curMemberOwnerID != nil && *curMemberOwnerID == ownerID {
		jsonResponse(w, http.StatusOK, map[string]string{"status": "accepted"})
		return
	}

	// Serialize against concurrent CrewAccept/JoinPlan/AcceptInvite on the
	// same plan and recheck filled<max_crew inside the lock — invites count
	// against the cap same as crew requests (contract: max_crew caps total
	// accepted paddlers regardless of origin, see CrewList/CrewAccept).
	tx, err := h.db.Begin(ctx)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "tx failed")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	var maxCrew *int
	if err := tx.QueryRow(ctx,
		`SELECT max_crew FROM plans WHERE id = $1::uuid AND deleted_at IS NULL FOR UPDATE`, planID,
	).Scan(&maxCrew); err != nil {
		errorResponse(w, http.StatusNotFound, "plan not found")
		return
	}

	if status != "accepted" {
		var filled int
		tx.QueryRow(ctx, `SELECT COUNT(*) FROM plan_members WHERE plan_id = $1::uuid AND status = 'accepted'`, planID).Scan(&filled)
		if maxCrew != nil && filled >= *maxCrew {
			errorResponse(w, http.StatusConflict, "crew is full")
			return
		}
	}

	// Binding member_owner_id is the point an email-token invite becomes a
	// real member row — accept REQUIRES an account (contract decision #8);
	// the token is only ever a landing/lookup key. The member_owner_id guard
	// in the WHERE clause plus nulling invite_token_hash makes the email
	// link single-use, so a forwarded/leaked invite link can't reassign an
	// already-accepted membership away from its current holder.
	tag, err := tx.Exec(ctx, `
		UPDATE plan_members
		SET member_owner_id = $1, status = 'accepted', responded_at = NOW(), updated_at = NOW(), invite_token_hash = NULL
		WHERE id = $2::uuid AND origin = 'invite' AND status <> 'declined'
		  AND (member_owner_id IS NULL OR member_owner_id = $1)
	`, ownerID, memberID)
	if err != nil {
		// Binding member_owner_id can collide with plan_members_owner_uk if
		// the caller is already a member of this plan via a separate row
		// (e.g. accepted a handle-invite, now also holds an email-invite
		// link) — surface as 409. Any other DB error (timeout, connection
		// drop) is a genuine 500, not a duplicate-membership conflict.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			errorResponse(w, http.StatusConflict, "already a member of this plan")
		} else {
			errorResponse(w, http.StatusInternalServerError, "accept failed")
		}
		return
	}
	if tag.RowsAffected() == 0 {
		errorResponse(w, http.StatusConflict, "invite cannot be accepted")
		return
	}

	if err := tx.Commit(ctx); err != nil {
		errorResponse(w, http.StatusInternalServerError, "commit failed")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{"status": "accepted"})
}

// ── POST /invites/{memberId}/dismiss ─────────────────────────────────────
// Dismiss keeps the row (and its status) in GET /me/invites — only
// dismissed_at is set (contract: "row STAYS listed").

func (h *InviteHandler) DismissInvite(w http.ResponseWriter, r *http.Request) {
	memberID := chi.URLParam(r, "memberId")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()
	email, _ := auth.EmailFromContext(ctx)

	tag, err := h.db.Exec(ctx, `
		UPDATE plan_members SET dismissed_at = NOW(), updated_at = NOW()
		WHERE id = $1::uuid AND origin = 'invite'
		  AND (member_owner_id = $2 OR (member_owner_id IS NULL AND LOWER(invite_email) = LOWER($3)))
	`, memberID, ownerID, email)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "dismiss failed")
		return
	}
	if tag.RowsAffected() == 0 {
		errorResponse(w, http.StatusNotFound, "invite not found")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{"status": "dismissed"})
}

// ── POST /plans/{id}/join ─────────────────────────────────────────────────

func (h *InviteHandler) JoinPlan(w http.ResponseWriter, r *http.Request) {
	planID := chi.URLParam(r, "id")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !h.rlJoin.allow(ownerID, 10, time.Hour) {
		errorResponse(w, http.StatusTooManyRequests, "rate limit: 10 join requests per hour")
		return
	}

	var body struct {
		PlanRunID *string `json:"plan_run_id"`
		Message   *string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	ctx := r.Context()

	var visibility string
	var lookingForCrew bool
	var maxCrew *int
	var hostOwnerID string
	if err := h.db.QueryRow(ctx, `
		SELECT visibility::text, looking_for_crew, max_crew, owner_id
		FROM plans WHERE id = $1::uuid AND deleted_at IS NULL
	`, planID).Scan(&visibility, &lookingForCrew, &maxCrew, &hostOwnerID); err != nil {
		errorResponse(w, http.StatusNotFound, "plan not found")
		return
	}
	if hostOwnerID == ownerID {
		errorResponse(w, http.StatusConflict, "host cannot request to join their own plan")
		return
	}

	// Already a member (any origin/status) -> 200 existing, no dup.
	var existingID, existingStatus string
	if err := h.db.QueryRow(ctx,
		`SELECT id, status::text FROM plan_members WHERE plan_id = $1::uuid AND member_owner_id = $2`,
		planID, ownerID,
	).Scan(&existingID, &existingStatus); err == nil {
		jsonResponse(w, http.StatusOK, map[string]string{"member_id": existingID, "status": existingStatus})
		return
	}

	if visibility != "public" || !lookingForCrew {
		errorResponse(w, http.StatusConflict, "this plan is not open for crew requests")
		return
	}

	var filled int
	h.db.QueryRow(ctx, `SELECT COUNT(*) FROM plan_members WHERE plan_id = $1::uuid AND status = 'accepted'`, planID).Scan(&filled)
	if maxCrew == nil || filled >= *maxCrew {
		errorResponse(w, http.StatusConflict, "crew is full")
		return
	}

	var planRunID *string
	if body.PlanRunID != nil && *body.PlanRunID != "" {
		var exists bool
		h.db.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM plan_runs WHERE id = $1::uuid AND plan_id = $2::uuid AND deleted_at IS NULL)`,
			*body.PlanRunID, planID,
		).Scan(&exists)
		if exists {
			planRunID = body.PlanRunID
		}
	}

	var memberID string
	err := h.db.QueryRow(ctx, `
		INSERT INTO plan_members (plan_id, member_owner_id, origin, status, plan_run_id, message)
		VALUES ($1::uuid, $2, 'request', 'requested', $3::uuid, $4)
		RETURNING id
	`, planID, ownerID, planRunID, body.Message).Scan(&memberID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, fmt.Sprintf("join request failed: %v", err))
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]string{"member_id": memberID, "status": "requested"})
}

// ── GET /plans/{id}/crew ──────────────────────────────────────────────────
// Both pending requests and accepted crew (of either origin — an accepted
// handle-invite is also "crew") so the host sees one roster; filled counts
// status='accepted' regardless of origin (contract: max_crew caps total
// accepted paddlers, host not counted).

func (h *InviteHandler) CrewList(w http.ResponseWriter, r *http.Request) {
	planID := chi.URLParam(r, "id")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()

	var hostOwnerID string
	var maxCrew *int
	if err := h.db.QueryRow(ctx,
		`SELECT owner_id, max_crew FROM plans WHERE id = $1::uuid AND deleted_at IS NULL`, planID,
	).Scan(&hostOwnerID, &maxCrew); err != nil {
		errorResponse(w, http.StatusNotFound, "plan not found")
		return
	}
	if hostOwnerID != ownerID {
		errorResponse(w, http.StatusForbidden, "host only")
		return
	}

	rows, err := h.db.Query(ctx, `
		SELECT pm.id, pm.status::text, pm.origin::text, pm.message, pm.created_at, COALESCE(up.handle, pm.invite_handle, '')
		FROM plan_members pm
		LEFT JOIN user_profiles up ON up.owner_id = pm.member_owner_id
		WHERE pm.plan_id = $1::uuid AND (pm.origin = 'request' OR pm.status = 'accepted')
		ORDER BY pm.created_at
	`, planID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	type crewMember struct {
		MemberID  string  `json:"member_id"`
		Status    string  `json:"status"`
		Origin    string  `json:"origin"`
		Message   *string `json:"message,omitempty"`
		CreatedAt string  `json:"created_at"`
		Handle    string  `json:"handle"`
	}
	members := []crewMember{}
	for rows.Next() {
		var cm crewMember
		var createdAtRaw time.Time
		if err := rows.Scan(&cm.MemberID, &cm.Status, &cm.Origin, &cm.Message, &createdAtRaw, &cm.Handle); err != nil {
			errorResponse(w, http.StatusInternalServerError, "scan failed")
			return
		}
		cm.CreatedAt = createdAtRaw.Format(time.RFC3339)
		members = append(members, cm)
	}

	var filled int
	h.db.QueryRow(ctx, `SELECT COUNT(*) FROM plan_members WHERE plan_id = $1::uuid AND status = 'accepted'`, planID).Scan(&filled)

	jsonResponse(w, http.StatusOK, map[string]any{
		"members": members,
		"meter":   map[string]any{"filled": filled, "max": maxCrew},
	})
}

// ── POST /plans/{id}/crew/{memberId}/accept ──────────────────────────────
// Locks the parent plans row for the duration of the tx so concurrent
// accepts against the same plan serialize — the filled<max_crew recheck
// must happen inside that lock, not as a separate pre-check (contract:
// "recheck filled<max_crew inside the UPDATE tx (409 at cap)").

func (h *InviteHandler) CrewAccept(w http.ResponseWriter, r *http.Request) {
	planID := chi.URLParam(r, "id")
	memberID := chi.URLParam(r, "memberId")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()

	tx, err := h.db.Begin(ctx)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "tx failed")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	var hostOwnerID string
	var maxCrew *int
	if err := tx.QueryRow(ctx,
		`SELECT owner_id, max_crew FROM plans WHERE id = $1::uuid AND deleted_at IS NULL FOR UPDATE`, planID,
	).Scan(&hostOwnerID, &maxCrew); err != nil {
		errorResponse(w, http.StatusNotFound, "plan not found")
		return
	}
	if hostOwnerID != ownerID {
		errorResponse(w, http.StatusForbidden, "host only")
		return
	}

	var curStatus string
	if err := tx.QueryRow(ctx,
		`SELECT status::text FROM plan_members WHERE id = $1::uuid AND plan_id = $2::uuid`, memberID, planID,
	).Scan(&curStatus); err != nil {
		errorResponse(w, http.StatusNotFound, "crew request not found")
		return
	}
	if curStatus == "declined" {
		errorResponse(w, http.StatusConflict, "request was declined")
		return
	}

	if curStatus != "accepted" {
		var filled int
		tx.QueryRow(ctx, `SELECT COUNT(*) FROM plan_members WHERE plan_id = $1::uuid AND status = 'accepted'`, planID).Scan(&filled)
		if maxCrew == nil || filled >= *maxCrew {
			errorResponse(w, http.StatusConflict, "crew is full")
			return
		}
		if _, err := tx.Exec(ctx,
			`UPDATE plan_members SET status = 'accepted', responded_at = NOW(), updated_at = NOW() WHERE id = $1::uuid`,
			memberID,
		); err != nil {
			errorResponse(w, http.StatusInternalServerError, "accept failed")
			return
		}
	}

	if err := tx.Commit(ctx); err != nil {
		errorResponse(w, http.StatusInternalServerError, "commit failed")
		return
	}

	var filled int
	h.db.QueryRow(ctx, `SELECT COUNT(*) FROM plan_members WHERE plan_id = $1::uuid AND status = 'accepted'`, planID).Scan(&filled)
	jsonResponse(w, http.StatusOK, map[string]any{"status": "accepted", "filled": filled, "max": maxCrew})
}

// ── POST /plans/{id}/crew/{memberId}/decline ─────────────────────────────

func (h *InviteHandler) CrewDecline(w http.ResponseWriter, r *http.Request) {
	planID := chi.URLParam(r, "id")
	memberID := chi.URLParam(r, "memberId")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()

	var hostOwnerID string
	if err := h.db.QueryRow(ctx,
		`SELECT owner_id FROM plans WHERE id = $1::uuid AND deleted_at IS NULL`, planID,
	).Scan(&hostOwnerID); err != nil {
		errorResponse(w, http.StatusNotFound, "plan not found")
		return
	}
	if hostOwnerID != ownerID {
		errorResponse(w, http.StatusForbidden, "host only")
		return
	}

	tag, err := h.db.Exec(ctx, `
		UPDATE plan_members SET status = 'declined', responded_at = NOW(), updated_at = NOW()
		WHERE id = $1::uuid AND plan_id = $2::uuid
	`, memberID, planID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "decline failed")
		return
	}
	if tag.RowsAffected() == 0 {
		errorResponse(w, http.StatusNotFound, "crew request not found")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]string{"status": "declined"})
}
