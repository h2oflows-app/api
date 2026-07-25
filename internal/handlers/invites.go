package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
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

// InviteHandler handles /plans/{id}/invite(+/resend), /me/invites,
// /invites/*, /plan-runs/{id}/join, and /plan-runs/{id}/crew/* (#246 A4,
// reworked #246 A7 for per-run crew/RSVP — see the membership-rule package
// comment on PlanHandler in plans.go). Split into its own file/handler type
// from PlanHandler (plans.go/plan_runs.go/calendar.go) since it owns a
// distinct table relationship (plan_members) and a new external dependency
// (mail.Mailer) — but the two share package-level helpers (dbQueryer,
// apiError, parseDate, userToday, localNoonUTC, validPlanTypes, runFilled)
// defined in plans.go, matching this codebase's convention of duplicating
// only the handler-struct-bound helpers (ownerID, ensureHandle) per type.
type InviteHandler struct {
	db            *pgxpool.Pool
	devFallbackID string
	mailer        mail.Mailer
	rlInvite      *reportRateLimiter // 20/hr/owner — POST /plans/{id}/invite
	rlJoin        *reportRateLimiter // 10/hr/owner — POST /plan-runs/{id}/join
	rlResend      *reportRateLimiter // 10/hr/owner — POST /plans/{id}/invite/resend
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
		rlResend:      newReportRateLimiter(),
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

// inviteRunInfo is a minimal plan_runs projection used both to fan out
// plan_members rows (one per targeted run) and to render the whole-plan
// itinerary recap in the invite email/.ics attachment (#246 A7). RunTime is
// "" for an untimed run.
type inviteRunInfo struct {
	ID      string
	Name    string
	RunDate string
	RunTime string
}

// loadPlanRuns fetches every LIVE plan_runs row for planID, ordered by
// run_date/sort_order — the plan's full itinerary. Used as both the
// "default = all runs" invite target set (resolveInviteTargets) and the
// whole-plan recap baked into every invite email (contract §6: "a non-user
// gets full trip context without ever signing in").
func loadPlanRuns(ctx context.Context, q dbQueryer, planID string) ([]inviteRunInfo, error) {
	rows, err := q.Query(ctx, `
		SELECT pr.id, COALESCE(ur.name, 'Paddle'), pr.run_date::text, COALESCE(pr.run_time::text, '')
		FROM plan_runs pr
		LEFT JOIN user_reaches ur ON ur.id = pr.user_reach_id
		WHERE pr.plan_id = $1::uuid AND pr.deleted_at IS NULL
		ORDER BY pr.run_date, pr.sort_order
	`, planID)
	if err != nil {
		return nil, fmt.Errorf("loadPlanRuns: %w", err)
	}
	defer rows.Close()

	var out []inviteRunInfo
	for rows.Next() {
		var ri inviteRunInfo
		if err := rows.Scan(&ri.ID, &ri.Name, &ri.RunDate, &ri.RunTime); err != nil {
			return nil, fmt.Errorf("loadPlanRuns scan: %w", err)
		}
		out = append(out, ri)
	}
	return out, nil
}

// resolveInviteTargets resolves the plan_run_ids an invite/request targets:
// empty `requested` -> ALL of allRuns (contract default, POST
// /plans/{id}/invite: "plan_run_ids []string (optional; default = ALL live
// runs of the plan)"); non-empty -> exactly those runs, each validated
// against allRuns (400, not 404 — the mismatch is the caller's own request
// body, not a missing resource).
func resolveInviteTargets(allRuns []inviteRunInfo, requested []string) ([]inviteRunInfo, error) {
	if len(requested) == 0 {
		return allRuns, nil
	}
	want := make(map[string]bool, len(requested))
	for _, id := range requested {
		want[id] = true
	}
	out := make([]inviteRunInfo, 0, len(requested))
	for _, run := range allRuns {
		if want[run.ID] {
			out = append(out, run)
			delete(want, run.ID)
		}
	}
	if len(want) > 0 {
		return nil, &apiError{http.StatusBadRequest, "one or more plan_run_ids do not belong to this plan"}
	}
	return out, nil
}

// invitedPlanInfo is the subset of a plan's fields inviteOne and the invite
// email need, gathered once by the caller (either from the in-memory values
// of a just-created plan in PlanHandler.Create, or via loadInvitedPlanInfo
// for the standalone POST /plans/{id}/invite endpoint). AllRuns is the full
// itinerary (#246 A7 — needed for the "whole plan laid out" email recap and
// as the invite's default target-run set).
type invitedPlanInfo struct {
	ID         string
	Slug       string
	Name       string
	Location   string
	StartDate  string
	EndDate    string
	HostHandle string
	AllRuns    []inviteRunInfo
}

// inviteBody is the request shape for a single invite — used both by
// POST /plans/{id}/invite and the `invites` array in POST /plans.
// PlanRunIDs is #246 A7: the runs this invite RSVPs to (nil/empty = all live
// runs of the plan).
type inviteBody struct {
	Handle     *string  `json:"handle"`
	Email      *string  `json:"email"`
	AttachICS  *bool    `json:"attach_ics"`
	PlanRunIDs []string `json:"plan_run_ids"`
}

// inviteRowResult is one fanned-out plan_members row's outcome — #246 A7
// replaces the old single-row inviteResult since one inviteOne call now
// produces one row PER targeted run.
type inviteRowResult struct {
	PlanRunID string `json:"plan_run_id"`
	MemberID  string `json:"member_id"`
	Status    string `json:"status"`
	Created   bool   `json:"-"` // true -> new row; false -> pre-existing row for that run, skipped
}

// pendingInviteMail carries everything needed to send + attach an invite
// email after the enclosing transaction commits — an outbound HTTPS call to
// Resend must never run while a DB tx is open. Only produced by the email
// path of inviteOne (handle invites never send mail). InvitedRuns is the
// subset of plan.AllRuns this particular recipient was newly invited to —
// used to build the subject line and the "You're invited" emphasis in the
// whole-plan email body (#246 A7).
type pendingInviteMail struct {
	to          string
	rawToken    string
	attachICS   bool
	plan        invitedPlanInfo
	invitedRuns []inviteRunInfo
}

// inviteOne inserts (or finds an existing) plan_members row for EACH run in
// targetRuns, for a single handle/email invite. Runs via q, a plain pool or
// a tx, so it is shared verbatim by POST /plans' single-tx invites[] array
// (plans.go Create) and the standalone POST /plans/{id}/invite. An email
// invite shares ONE rawToken/token_hash across the whole fan-out (contract
// §6: "same rawToken/token_hash shared across the fan-out for an email
// invite"); a handle invite needs no token (accept is by owner_id match).
func inviteOne(ctx context.Context, q dbQueryer, plan invitedPlanInfo, hostOwnerID string, body inviteBody, targetRuns []inviteRunInfo) ([]inviteRowResult, *pendingInviteMail, error) {
	switch {
	case body.Handle != nil && strings.TrimSpace(*body.Handle) != "":
		handle := strings.TrimPrefix(strings.TrimSpace(*body.Handle), "@")

		var targetOwnerID string
		if err := q.QueryRow(ctx,
			`SELECT owner_id FROM user_profiles WHERE LOWER(handle) = LOWER($1)`, handle,
		).Scan(&targetOwnerID); err != nil {
			return nil, nil, &apiError{http.StatusNotFound, "user not found"}
		}
		if targetOwnerID == hostOwnerID {
			// Host invites themselves — already implicitly "in" the plan
			// (no organizer row exists); no-op, 200 existing.
			return []inviteRowResult{{Status: "existing"}}, nil, nil
		}

		results := make([]inviteRowResult, 0, len(targetRuns))
		for _, run := range targetRuns {
			var memberID, status string
			err := q.QueryRow(ctx, `
				INSERT INTO plan_members (plan_id, member_owner_id, invite_handle, invited_by, origin, status, plan_run_id)
				VALUES ($1::uuid, $2, $3, $4, 'invite', 'invited', $5::uuid)
				ON CONFLICT (plan_run_id, member_owner_id) WHERE member_owner_id IS NOT NULL AND plan_run_id IS NOT NULL DO NOTHING
				RETURNING id, status::text
			`, plan.ID, targetOwnerID, handle, hostOwnerID, run.ID).Scan(&memberID, &status)
			if err != nil {
				if !errors.Is(err, pgx.ErrNoRows) {
					return nil, nil, fmt.Errorf("invite insert: %w", err)
				}
				// ON CONFLICT DO NOTHING with no RETURNING row: already invited to this run.
				if serr := q.QueryRow(ctx,
					`SELECT id, status::text FROM plan_members WHERE plan_run_id = $1::uuid AND member_owner_id = $2`,
					run.ID, targetOwnerID,
				).Scan(&memberID, &status); serr != nil {
					return nil, nil, fmt.Errorf("invite lookup: %w", serr)
				}
				results = append(results, inviteRowResult{PlanRunID: run.ID, MemberID: memberID, Status: status, Created: false})
				continue
			}
			results = append(results, inviteRowResult{PlanRunID: run.ID, MemberID: memberID, Status: status, Created: true})
		}
		return results, nil, nil

	case body.Email != nil && strings.TrimSpace(*body.Email) != "":
		email := strings.ToLower(strings.TrimSpace(*body.Email))
		attachICS := body.AttachICS == nil || *body.AttachICS // default TRUE

		rawToken, tokenHash, terr := generateInviteToken()
		if terr != nil {
			return nil, nil, fmt.Errorf("token generation: %w", terr)
		}

		results := make([]inviteRowResult, 0, len(targetRuns))
		var invitedRuns []inviteRunInfo
		for _, run := range targetRuns {
			var memberID, status string
			err := q.QueryRow(ctx, `
				INSERT INTO plan_members (plan_id, invite_email, invited_by, origin, status, invite_token_hash, plan_run_id)
				VALUES ($1::uuid, $2, $3, 'invite', 'invited', $4, $5::uuid)
				ON CONFLICT (plan_run_id, lower(invite_email)) WHERE invite_email IS NOT NULL AND member_owner_id IS NULL AND plan_run_id IS NOT NULL DO NOTHING
				RETURNING id, status::text
			`, plan.ID, email, hostOwnerID, tokenHash, run.ID).Scan(&memberID, &status)
			if err != nil {
				if !errors.Is(err, pgx.ErrNoRows) {
					return nil, nil, fmt.Errorf("invite insert: %w", err)
				}
				if serr := q.QueryRow(ctx,
					`SELECT id, status::text FROM plan_members WHERE plan_run_id = $1::uuid AND lower(invite_email) = $2 AND member_owner_id IS NULL`,
					run.ID, email,
				).Scan(&memberID, &status); serr != nil {
					return nil, nil, fmt.Errorf("invite lookup: %w", serr)
				}
				results = append(results, inviteRowResult{PlanRunID: run.ID, MemberID: memberID, Status: status, Created: false})
				continue
			}
			results = append(results, inviteRowResult{PlanRunID: run.ID, MemberID: memberID, Status: status, Created: true})
			invitedRuns = append(invitedRuns, run)
		}

		var pending *pendingInviteMail
		if len(invitedRuns) > 0 {
			pending = &pendingInviteMail{to: email, rawToken: rawToken, attachICS: attachICS, plan: plan, invitedRuns: invitedRuns}
		}
		return results, pending, nil

	default:
		return nil, nil, &apiError{http.StatusBadRequest, "handle or email is required"}
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

// inviteTokenMemberIDs resolves token (the raw ?invite= query param) to
// every plan_members.id it hashes to, scoped to planID — the one read
// carve-out for the calendar domain (#246 A6, PlanHandler.renderPlan), used
// both to grant read access (ok==true is the old anonInviteTokenValid
// signal) and, for an AUTHED caller, to tell the frontend which member rows
// an unbound (member_owner_id IS NULL) email invite corresponds to so it can
// drive InviteAcceptCard/AcceptInvite before the invite is ever bound to
// their account. #246 A7: an email invite's token is now shared across ITS
// WHOLE RUN FAN-OUT (inviteOne above), so this returns every matching row,
// not just one (was inviteTokenMemberID, singular, pre-A7). An empty/absent
// token or a token for a DIFFERENT plan_id both fail closed (nil/false),
// same as AcceptInvite's token check below — the token only ever grants
// access to the plan it was minted for, never a general bearer credential.
func inviteTokenMemberIDs(ctx context.Context, db *pgxpool.Pool, planID, token string) ([]string, bool) {
	if token == "" {
		return nil, false
	}
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	rows, err := db.Query(ctx,
		`SELECT id::text FROM plan_members WHERE plan_id = $1::uuid AND invite_token_hash = $2`, planID, hash)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, false
	}
	return ids, true
}

// webBaseURL is the web origin embedded in invite emails + .ics links.
// Overridden from config (WEB_BASE_URL) in main.go so staging emails link to
// the staging web deploy instead of prod (which bit the first staging test:
// the emailed link went to prod, where the plan page wasn't deployed yet).
var webBaseURL = "https://h2oflows.app"

// SetWebBaseURL wires config.WebBaseURL at startup (trailing slash stripped).
func SetWebBaseURL(u string) {
	if u != "" {
		webBaseURL = strings.TrimRight(u, "/")
	}
}

// formatUSDate renders a YYYY-MM-DD string as M/D/YYYY (contract §6 email
// copy: "on 7/26/2026"). Falls back to the raw string on a parse failure
// (defensive — every caller's date already came from Postgres via ::text).
func formatUSDate(ymd string) string {
	t, err := time.Parse("2006-01-02", ymd)
	if err != nil {
		return ymd
	}
	return fmt.Sprintf("%d/%d/%d", int(t.Month()), t.Day(), t.Year())
}

// formatUSTime renders a "15:04:05" or "15:04" string as "3:04 PM" (contract
// §6: "at 10:00 AM"). "" (untimed run) returns "".
func formatUSTime(hms string) string {
	if hms == "" {
		return ""
	}
	if t, err := time.Parse("15:04:05", hms); err == nil {
		return t.Format("3:04 PM")
	}
	if t, err := time.Parse("15:04", hms); err == nil {
		return t.Format("3:04 PM")
	}
	return ""
}

// inviteSubject builds the email subject naming the FIRST invited run with
// date+time (contract §6): "@host invited you to run {RunName} on
// {M/D/YYYY}[ at {h:MM AM}]" — "...and N more" when the recipient was
// invited to more than one run.
func inviteSubject(host string, invitedRuns []inviteRunInfo) string {
	if len(invitedRuns) == 0 {
		// Runless-plan invite (membership rule, plans.go package comment) —
		// no run to name.
		return fmt.Sprintf("@%s invited you to a trip", host)
	}
	first := invitedRuns[0]
	line := fmt.Sprintf("@%s invited you to run %s on %s", host, first.Name, formatUSDate(first.RunDate))
	if t := formatUSTime(first.RunTime); t != "" {
		line += " at " + t
	}
	if len(invitedRuns) > 1 {
		line += fmt.Sprintf(" and %d more", len(invitedRuns)-1)
	}
	return line
}

// buildInviteEmailBody renders the WHOLE plan (contract §6: "a non-user
// gets full trip context without ever signing in") — plan name/type/date
// range/location, then every day's runs, with the recipient's invited runs
// emphasized (bold + "You're invited") and carrying their own accept link
// ({WEB_BASE_URL}/plans/{handle}/{slug}?invite={token}&run={plan_run_id}).
// Returns HTML + a plain-text mirror.
func buildInviteEmailBody(plan invitedPlanInfo, invitedRuns []inviteRunInfo, planURL, rawToken string) (htmlBody, textBody string) {
	invited := make(map[string]bool, len(invitedRuns))
	for _, r := range invitedRuns {
		invited[r.ID] = true
	}

	dateRange := formatUSDate(plan.StartDate)
	if plan.EndDate != plan.StartDate {
		dateRange = fmt.Sprintf("%s – %s", formatUSDate(plan.StartDate), formatUSDate(plan.EndDate))
	}

	var htmlItems, textItems strings.Builder
	for _, run := range plan.AllRuns {
		acceptURL := fmt.Sprintf("%s?invite=%s&run=%s", planURL, rawToken, run.ID)
		line := run.Name + " — " + formatUSDate(run.RunDate)
		if t := formatUSTime(run.RunTime); t != "" {
			line += " at " + t
		}
		if invited[run.ID] {
			htmlItems.WriteString(fmt.Sprintf(`<li><strong>%s</strong> — You're invited! <a href="%s">Accept</a></li>`, html.EscapeString(line), acceptURL))
			textItems.WriteString(fmt.Sprintf("* %s — You're invited! Accept: %s\n", line, acceptURL))
		} else {
			htmlItems.WriteString(fmt.Sprintf("<li>%s</li>", html.EscapeString(line)))
			textItems.WriteString(fmt.Sprintf("- %s\n", line))
		}
	}
	if len(plan.AllRuns) == 0 {
		htmlItems.WriteString("<li>(no runs scheduled yet)</li>")
		textItems.WriteString("(no runs scheduled yet)\n")
	}

	htmlLoc, textLoc := "", ""
	if plan.Location != "" {
		htmlLoc = " · " + html.EscapeString(plan.Location)
		textLoc = " · " + plan.Location
	}

	htmlBody = fmt.Sprintf(
		`<p>@%s invited you on H2OFlows.</p><h2>%s</h2><p>%s%s</p><ul>%s</ul><p><a href="%s">View the full plan</a></p>`,
		html.EscapeString(plan.HostHandle), html.EscapeString(plan.Name), dateRange, htmlLoc, htmlItems.String(), planURL,
	)
	textBody = fmt.Sprintf(
		"@%s invited you on H2OFlows.\n%s\n%s%s\n\n%s\nView the full plan: %s\n",
		plan.HostHandle, plan.Name, dateRange, textLoc, textItems.String(), planURL,
	)
	return htmlBody, textBody
}

// sendInviteMail builds the invite email (+ .ics attachment when requested)
// and sends it. Package-level (not a method) so both InviteHandler and
// PlanHandler.Create can fire it after their respective transaction commits,
// each with its own detached context — the triggering HTTP request's
// context dies at response, so a fresh context.Background()+timeout is used
// per the contract ("Sends are async... go func with its own
// context.Background+timeout"). #246 A7 rework: whole-plan layout + one
// .ics VEVENT per run (see internal/ics).
func sendInviteMail(mailer mail.Mailer, p pendingInviteMail) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	host := p.plan.HostHandle
	if host == "" {
		host = "a paddler"
	}
	planURL := fmt.Sprintf("%s/plans/%s/%s", webBaseURL, p.plan.HostHandle, p.plan.Slug)

	subject := inviteSubject(host, p.invitedRuns)
	htmlBody, textBody := buildInviteEmailBody(p.plan, p.invitedRuns, planURL, p.rawToken)

	msg := mail.Message{To: p.to, Subject: subject, HTML: htmlBody, Text: textBody}
	if p.attachICS {
		invited := make(map[string]bool, len(p.invitedRuns))
		for _, r := range p.invitedRuns {
			invited[r.ID] = true
		}
		icsRuns := make([]ics.PlanInviteRun, 0, len(p.plan.AllRuns))
		for _, run := range p.plan.AllRuns {
			icsRuns = append(icsRuns, ics.PlanInviteRun{
				ID:      run.ID,
				Name:    run.Name,
				RunDate: run.RunDate,
				RunTime: run.RunTime,
				Invited: invited[run.ID],
			})
		}
		icsBody := ics.BuildPlanInvite(ics.PlanInviteInput{
			PlanID:    p.plan.ID,
			Name:      p.plan.Name,
			Location:  p.plan.Location,
			StartDate: p.plan.StartDate,
			EndDate:   p.plan.EndDate,
			URL:       planURL,
			Runs:      icsRuns,
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

// loadInvitedPlanInfo fetches the host-only plan fields (+ full itinerary)
// needed to send an invite, in one call — 404s (rather than 403) when the
// plan doesn't exist or the caller isn't its host, matching this package's
// existing not-found-over-forbidden convention for owner-scoped lookups
// (e.g. PlanHandler.Update).
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
	allRuns, rerr := loadPlanRuns(ctx, h.db, planID)
	if rerr != nil {
		return invitedPlanInfo{}, rerr
	}
	p.AllRuns = allRuns
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

	targets, terr := resolveInviteTargets(plan.AllRuns, body.PlanRunIDs)
	if terr != nil {
		h.respondAPIError(w, terr)
		return
	}

	results, pending, err := inviteOne(ctx, h.db, plan, ownerID, body, targets)
	if err != nil {
		h.respondAPIError(w, err)
		return
	}

	sent := false
	if pending != nil {
		sent = true
		go sendInviteMail(h.mailer, *pending)
	}

	memberIDs := make([]string, 0, len(results))
	anyCreated := false
	for _, res := range results {
		if res.MemberID != "" {
			memberIDs = append(memberIDs, res.MemberID)
		}
		if res.Created {
			anyCreated = true
		}
	}
	status := "existing"
	httpStatus := http.StatusOK
	if anyCreated {
		status = "invited"
		httpStatus = http.StatusCreated
	}

	jsonResponse(w, httpStatus, map[string]any{
		"member_ids": memberIDs,
		"status":     status,
		"sent":       sent,
		// results: per-row detail (contract "Dup handling per row (existing
		// row -> skipped, reported)") — member_ids/status/sent above stay the
		// bulk-level summary the contract's Response shape names literally.
		"results": results,
	})
}

// ── POST /plans/{id}/invite/resend ───────────────────────────────────────
// Host-only, rate-limited 10/hr/owner (reuses reportRateLimiter). Regenerates
// a fresh shared token, rotates invite_token_hash on every still-invited row
// for that email across the whole plan, and re-sends (same email/.ics
// rework as InviteToPlan). 404 if the email has no invited rows on this
// plan at all; 409 if every row for that email has already been
// accepted/declined (nothing left to resend).

func (h *InviteHandler) ResendInvite(w http.ResponseWriter, r *http.Request) {
	planID := chi.URLParam(r, "id")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !h.rlResend.allow(ownerID, 10, time.Hour) {
		errorResponse(w, http.StatusTooManyRequests, "rate limit: 10 resends per hour")
		return
	}

	var body struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	email := strings.ToLower(strings.TrimSpace(body.Email))
	if email == "" {
		errorResponse(w, http.StatusBadRequest, "email is required")
		return
	}

	ctx := r.Context()
	plan, perr := h.loadInvitedPlanInfo(ctx, planID, ownerID)
	if perr != nil {
		h.respondAPIError(w, perr)
		return
	}

	rawToken, tokenHash, terr := generateInviteToken()
	if terr != nil {
		errorResponse(w, http.StatusInternalServerError, "token generation failed")
		return
	}

	rows, err := h.db.Query(ctx, `
		UPDATE plan_members SET invite_token_hash = $1, updated_at = NOW()
		WHERE plan_id = $2::uuid AND member_owner_id IS NULL AND lower(invite_email) = $3
		  AND origin = 'invite' AND status = 'invited'
		RETURNING plan_run_id::text
	`, tokenHash, planID, email)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "resend failed")
		return
	}
	var updatedRunIDs []string
	for rows.Next() {
		var runID *string
		if serr := rows.Scan(&runID); serr == nil && runID != nil {
			updatedRunIDs = append(updatedRunIDs, *runID)
		}
	}
	rows.Close()

	if len(updatedRunIDs) == 0 {
		var anyRow bool
		h.db.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM plan_members
				WHERE plan_id = $1::uuid AND member_owner_id IS NULL AND lower(invite_email) = $2 AND origin = 'invite')
		`, planID, email).Scan(&anyRow)
		if !anyRow {
			errorResponse(w, http.StatusNotFound, "no invited rows for that email")
			return
		}
		errorResponse(w, http.StatusConflict, "all invites for that email have already been accepted or declined")
		return
	}

	updated := make(map[string]bool, len(updatedRunIDs))
	for _, id := range updatedRunIDs {
		updated[id] = true
	}
	var invitedRuns []inviteRunInfo
	for _, run := range plan.AllRuns {
		if updated[run.ID] {
			invitedRuns = append(invitedRuns, run)
		}
	}

	go sendInviteMail(h.mailer, pendingInviteMail{
		to: email, rawToken: rawToken, attachICS: true, plan: plan, invitedRuns: invitedRuns,
	})

	jsonResponse(w, http.StatusOK, map[string]any{
		"status":       "resent",
		"plan_run_ids": updatedRunIDs,
	})
}

// ── GET /me/invites ───────────────────────────────────────────────────────
// #246 A7: rows are now run-scoped — grouped by plan into one feed item per
// plan, each carrying its invited runs [{member_id, plan_run_id, run_name,
// run_date, run_time, status, dismissed_at}] (contract §3). A runless-plan
// invite (plan_run_id NULL, membership rule) still surfaces as one run-entry
// with the run fields empty.

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
	// degrades to member_owner_id-only matching.
	email, _ := auth.EmailFromContext(ctx)

	rows, err := h.db.Query(ctx, `
		SELECT pm.id, pm.status::text, pm.dismissed_at, (pm.invite_handle IS NOT NULL) AS via_handle, pm.created_at,
		       pm.plan_run_id::text, pr.run_date::text, pr.run_time::text, COALESCE(ur.name, 'Paddle'),
		       p.id, p.slug, p.name, p.type::text, p.start_date::text, p.end_date::text, p.location,
		       COALESCE(up.handle, '')
		FROM plan_members pm
		JOIN plans p ON p.id = pm.plan_id AND p.deleted_at IS NULL
		LEFT JOIN plan_runs pr ON pr.id = pm.plan_run_id AND pr.deleted_at IS NULL
		LEFT JOIN user_reaches ur ON ur.id = pr.user_reach_id
		LEFT JOIN user_profiles up ON up.owner_id = p.owner_id
		WHERE pm.origin = 'invite'
		  AND (pm.member_owner_id = $1 OR (pm.member_owner_id IS NULL AND LOWER(pm.invite_email) = LOWER($2)))
		ORDER BY p.start_date, pm.created_at
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
	type inviteRunEntry struct {
		MemberID    string  `json:"member_id"`
		PlanRunID   *string `json:"plan_run_id,omitempty"`
		RunName     *string `json:"run_name,omitempty"`
		RunDate     *string `json:"run_date,omitempty"`
		RunTime     *string `json:"run_time,omitempty"`
		Status      string  `json:"status"`
		DismissedAt *string `json:"dismissed_at,omitempty"`
	}
	type invitedPlanFeedItem struct {
		Plan       invitedPlan      `json:"plan"`
		InvitedVia string           `json:"invited_via"` // "handle" | "email"
		CreatedAt  string           `json:"created_at"`  // earliest row's created_at
		Runs       []inviteRunEntry `json:"runs"`
	}

	order := []string{}
	byPlan := map[string]*invitedPlanFeedItem{}

	for rows.Next() {
		var memberID, status string
		var dismissedAtRaw *time.Time
		var createdAtRaw time.Time
		var viaHandle bool
		var planRunID, runDate, runTime, runName *string
		var pl invitedPlan
		if err := rows.Scan(
			&memberID, &status, &dismissedAtRaw, &viaHandle, &createdAtRaw,
			&planRunID, &runDate, &runTime, &runName,
			&pl.ID, &pl.Slug, &pl.Name, &pl.Type, &pl.StartDate, &pl.EndDate, &pl.Location,
			&pl.HostHandle,
		); err != nil {
			errorResponse(w, http.StatusInternalServerError, "scan failed")
			return
		}

		item, ok := byPlan[pl.ID]
		if !ok {
			item = &invitedPlanFeedItem{
				Plan:       pl,
				InvitedVia: "email",
				CreatedAt:  createdAtRaw.Format(time.RFC3339),
			}
			if viaHandle {
				item.InvitedVia = "handle"
			}
			byPlan[pl.ID] = item
			order = append(order, pl.ID)
		}

		entry := inviteRunEntry{MemberID: memberID, PlanRunID: planRunID, Status: status}
		if planRunID != nil {
			entry.RunName = runName
			entry.RunDate = runDate
			if runTime != nil && *runTime != "" {
				entry.RunTime = runTime
			}
		}
		if dismissedAtRaw != nil {
			s := dismissedAtRaw.Format(time.RFC3339)
			entry.DismissedAt = &s
		}
		item.Runs = append(item.Runs, entry)
	}

	out := make([]invitedPlanFeedItem, 0, len(order))
	for _, planID := range order {
		out = append(out, *byPlan[planID])
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

	var curMemberOwnerID, curEmail, curTokenHash, curPlanRunID *string
	var status, planID string
	if err := h.db.QueryRow(ctx, `
		SELECT member_owner_id, invite_email, invite_token_hash, status::text, plan_id::text, plan_run_id::text
		FROM plan_members WHERE id = $1::uuid AND origin = 'invite'
	`, memberID).Scan(&curMemberOwnerID, &curEmail, &curTokenHash, &status, &planID, &curPlanRunID); err != nil {
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

	// Serialize against concurrent RunCrewAccept/JoinRun/AcceptInvite on the
	// same plan_run and recheck filled<max_crew inside the lock — invites
	// count against the cap same as crew requests (contract: max_crew caps
	// total accepted paddlers per run, regardless of origin). #246 A7: the
	// cap is now the RUN's max_crew (curPlanRunID), not the plan's — a
	// plan-level row (runless plan, membership rule) has no cap at all.
	tx, err := h.db.Begin(ctx)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "tx failed")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	if curPlanRunID != nil {
		var maxCrew *int
		if err := tx.QueryRow(ctx,
			`SELECT max_crew FROM plan_runs WHERE id = $1::uuid AND deleted_at IS NULL FOR UPDATE`, *curPlanRunID,
		).Scan(&maxCrew); err != nil {
			errorResponse(w, http.StatusNotFound, "plan run not found")
			return
		}

		if status != "accepted" {
			filled, ferr := runFilled(ctx, tx, *curPlanRunID)
			if ferr != nil {
				errorResponse(w, http.StatusInternalServerError, "crew count failed")
				return
			}
			if maxCrew != nil && filled >= *maxCrew {
				errorResponse(w, http.StatusConflict, "crew is full")
				return
			}
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
		// Binding member_owner_id can collide with a run-scoped unique index
		// if the caller is already a member of this SAME RUN via a separate
		// row (e.g. accepted a handle-invite, now also holds an email-invite
		// link for the same run) — surface as 409. Any other DB error
		// (timeout, connection drop) is a genuine 500, not a duplicate-
		// membership conflict.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			errorResponse(w, http.StatusConflict, "already a member of this run")
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
// dismissed_at is set (contract: "row STAYS listed"). Per-row (= per run),
// unchanged shape from #246 A4.

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

// ── POST /plan-runs/{id}/join ─────────────────────────────────────────────
// #246 A7: replaces POST /plans/{id}/join (REMOVED, main.go) — crew requests
// RSVP to a specific run, not the whole plan. Gates: parent plan public +
// THAT RUN's looking_for_crew + run-filled < run-max_crew.

func (h *InviteHandler) JoinRun(w http.ResponseWriter, r *http.Request) {
	planRunID := chi.URLParam(r, "id")
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
		Message *string `json:"message"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // optional body

	ctx := r.Context()

	var planID, visibility, hostOwnerID string
	var lookingForCrew bool
	var maxCrew *int
	if err := h.db.QueryRow(ctx, `
		SELECT pr.plan_id::text, p.visibility::text, p.owner_id, pr.looking_for_crew, pr.max_crew
		FROM plan_runs pr
		JOIN plans p ON p.id = pr.plan_id AND p.deleted_at IS NULL
		WHERE pr.id = $1::uuid AND pr.deleted_at IS NULL
	`, planRunID).Scan(&planID, &visibility, &hostOwnerID, &lookingForCrew, &maxCrew); err != nil {
		errorResponse(w, http.StatusNotFound, "plan run not found")
		return
	}
	if hostOwnerID == ownerID {
		errorResponse(w, http.StatusConflict, "host cannot request to join their own run")
		return
	}

	// Already a member of THIS run (any origin/status) -> 200 existing, no dup.
	var existingID, existingStatus string
	if err := h.db.QueryRow(ctx,
		`SELECT id, status::text FROM plan_members WHERE plan_run_id = $1::uuid AND member_owner_id = $2`,
		planRunID, ownerID,
	).Scan(&existingID, &existingStatus); err == nil {
		jsonResponse(w, http.StatusOK, map[string]string{"member_id": existingID, "status": existingStatus})
		return
	}

	if visibility != "public" || !lookingForCrew {
		errorResponse(w, http.StatusConflict, "this run is not open for crew requests")
		return
	}

	filled, ferr := runFilled(ctx, h.db, planRunID)
	if ferr != nil {
		errorResponse(w, http.StatusInternalServerError, "crew count failed")
		return
	}
	if maxCrew == nil || filled >= *maxCrew {
		errorResponse(w, http.StatusConflict, "crew is full")
		return
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

// ── GET /plan-runs/{id}/crew ──────────────────────────────────────────────
// #246 A7: replaces GET /plans/{id}/crew (REMOVED, main.go) — one roster per
// run. Both pending requests and accepted crew (of either origin — an
// accepted handle-invite is also "crew") so the host sees one roster; filled
// counts status='accepted' regardless of origin (contract: max_crew caps
// total accepted paddlers on THIS run, host not counted).

func (h *InviteHandler) RunCrewList(w http.ResponseWriter, r *http.Request) {
	planRunID := chi.URLParam(r, "id")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()

	var hostOwnerID string
	var maxCrew *int
	var lookingForCrew bool
	if err := h.db.QueryRow(ctx, `
		SELECT p.owner_id, pr.max_crew, pr.looking_for_crew
		FROM plan_runs pr
		JOIN plans p ON p.id = pr.plan_id AND p.deleted_at IS NULL
		WHERE pr.id = $1::uuid AND pr.deleted_at IS NULL
	`, planRunID).Scan(&hostOwnerID, &maxCrew, &lookingForCrew); err != nil {
		errorResponse(w, http.StatusNotFound, "plan run not found")
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
		WHERE pm.plan_run_id = $1::uuid AND (pm.origin = 'request' OR pm.status = 'accepted')
		ORDER BY pm.created_at
	`, planRunID)
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

	filled, ferr := runFilled(ctx, h.db, planRunID)
	if ferr != nil {
		errorResponse(w, http.StatusInternalServerError, "crew count failed")
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"members": members,
		"meter":   map[string]any{"filled": filled, "max": maxCrew, "looking_for_crew": lookingForCrew},
	})
}

// ── POST /plan-runs/{id}/crew/{memberId}/accept ──────────────────────────
// #246 A7: replaces POST /plans/{id}/crew/{memberId}/accept. Locks the
// plan_runs row for the duration of the tx so concurrent accepts against
// the same run serialize — the filled<max_crew recheck must happen inside
// that lock, not as a separate pre-check.

func (h *InviteHandler) RunCrewAccept(w http.ResponseWriter, r *http.Request) {
	planRunID := chi.URLParam(r, "id")
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
	if err := tx.QueryRow(ctx, `
		SELECT p.owner_id, pr.max_crew
		FROM plan_runs pr
		JOIN plans p ON p.id = pr.plan_id AND p.deleted_at IS NULL
		WHERE pr.id = $1::uuid AND pr.deleted_at IS NULL FOR UPDATE
	`, planRunID).Scan(&hostOwnerID, &maxCrew); err != nil {
		errorResponse(w, http.StatusNotFound, "plan run not found")
		return
	}
	if hostOwnerID != ownerID {
		errorResponse(w, http.StatusForbidden, "host only")
		return
	}

	var curStatus string
	if err := tx.QueryRow(ctx,
		`SELECT status::text FROM plan_members WHERE id = $1::uuid AND plan_run_id = $2::uuid`, memberID, planRunID,
	).Scan(&curStatus); err != nil {
		errorResponse(w, http.StatusNotFound, "crew request not found")
		return
	}
	if curStatus == "declined" {
		errorResponse(w, http.StatusConflict, "request was declined")
		return
	}

	if curStatus != "accepted" {
		filled, ferr := runFilled(ctx, tx, planRunID)
		if ferr != nil {
			errorResponse(w, http.StatusInternalServerError, "crew count failed")
			return
		}
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

	// The accept itself already committed above — a count-read failure here
	// only affects the meter numbers in this response body, not whether the
	// accept succeeded.
	filled, ferr := runFilled(ctx, h.db, planRunID)
	if ferr != nil {
		errorResponse(w, http.StatusInternalServerError, "crew count failed")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"status": "accepted", "filled": filled, "max": maxCrew})
}

// ── POST /plan-runs/{id}/crew/{memberId}/decline ─────────────────────────
// #246 A7: replaces POST /plans/{id}/crew/{memberId}/decline.

func (h *InviteHandler) RunCrewDecline(w http.ResponseWriter, r *http.Request) {
	planRunID := chi.URLParam(r, "id")
	memberID := chi.URLParam(r, "memberId")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()

	var hostOwnerID string
	if err := h.db.QueryRow(ctx, `
		SELECT p.owner_id
		FROM plan_runs pr
		JOIN plans p ON p.id = pr.plan_id AND p.deleted_at IS NULL
		WHERE pr.id = $1::uuid AND pr.deleted_at IS NULL
	`, planRunID).Scan(&hostOwnerID); err != nil {
		errorResponse(w, http.StatusNotFound, "plan run not found")
		return
	}
	if hostOwnerID != ownerID {
		errorResponse(w, http.StatusForbidden, "host only")
		return
	}

	tag, err := h.db.Exec(ctx, `
		UPDATE plan_members SET status = 'declined', responded_at = NOW(), updated_at = NOW()
		WHERE id = $1::uuid AND plan_run_id = $2::uuid
	`, memberID, planRunID)
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
