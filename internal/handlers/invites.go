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

// InviteHandler handles /plan-runs/{id}/invite(+/resend), /me/invites,
// /invites/*, /plan-runs/{id}/join, and /plan-runs/{id}/crew/* (#246 A4,
// reworked #246 A7 for per-run crew/RSVP, reworked AGAIN web#354 A2 to drop
// the plan/event context entirely — invites are Run-scoped only now, table
// `run_invites` (was `plan_members`, migration 000146; §1/§3 of
// REWORK_354_PLAN.md). One invite = one run_invites row for exactly one run;
// there is no more fan-out across a plan's runs and no more shared
// multi-run token. Split into its own file/handler type from PlanHandler
// (plans.go/plan_runs.go/calendar.go) since it owns a distinct table
// relationship (run_invites) and a new external dependency (mail.Mailer) —
// but the two share package-level helpers (dbQueryer, apiError, parseDate,
// userToday, localNoonUTC, runFilled) defined in plans.go, matching this
// codebase's convention of duplicating only the handler-struct-bound helpers
// (ownerID) per type.
type InviteHandler struct {
	db            *pgxpool.Pool
	devFallbackID string
	mailer        mail.Mailer
	rlInvite      *reportRateLimiter // 20/hr/owner — POST /plan-runs/{id}/invite
	rlJoin        *reportRateLimiter // 10/hr/owner — POST /plan-runs/{id}/join
	rlResend      *reportRateLimiter // 10/hr/owner — POST /plan-runs/{id}/invite/resend
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

// ── Shared invite machinery ──────────────────────────────────────────────

// invitedRunInfo is the run projection InviteToRun/ResendInvite need to send
// an invite (email subject/body + .ics attachment) — web#354 A2: one run,
// not a whole plan's date-contained itinerary (that was requireInviteTargetRuns/
// loadPlanRuns/loadInvitedPlanInfo, all removed — a single-run invite has no
// fan-out to resolve or authorize beyond the run itself).
type invitedRunInfo struct {
	ID         string
	Slug       string
	Name       string
	RiverName  *string
	RunDate    string
	RunTime    string // "" for an untimed run
	MeetupSpot string // "" when unset
	GaugeCFS   *float64
	FlowBand   *string
	HostHandle string
	// HostDisplayName (display-names feature, mig 000130): additive
	// alongside HostHandle, feeds sendRunInviteMail's hostLabel (displayLabel)
	// — nil when the host has no display_name typed in.
	HostDisplayName *string
	ICSSequence     int // API-1: calendar_runs.ics_sequence — RFC 5546 SEQUENCE for this run's .ics UID
	// Paddled (API-2, INVITE_SYNC_PLAN.md Amendments — "No inviting to
	// logged runs"): InviteToRun/ResendInvite both reject with 422 when this
	// is true, mirroring the UI gate already shipped (web#362, Invite button
	// hidden when run.paddled) — a logged run describes a trip that already
	// happened, so there's nothing left to RSVP to.
	Paddled bool
}

// loadInvitedRunInfo fetches runID's invite-relevant fields, gated on
// hostOwnerID owning it — 404 (never 403) when the run doesn't exist or the
// caller isn't its owner, matching this package's existing
// not-found-over-forbidden convention for owner-scoped lookups.
func (h *InviteHandler) loadInvitedRunInfo(ctx context.Context, runID, hostOwnerID string) (invitedRunInfo, error) {
	var ri invitedRunInfo
	// web#354 A4: Name's source flips from the library run's own name
	// (COALESCE(ur.name, 'Paddle')) to the calendar run's own name (cr.name,
	// NOT NULL) — runInviteSubject/buildRunInviteEmailBody/BuildRunInvite
	// (SUMMARY/subject) all key off ri.Name, so this alone repoints the
	// invite email + .ics subject; RiverName (body/LOCATION context) is
	// untouched.
	err := h.db.QueryRow(ctx, `
		SELECT cr.id, cr.slug, cr.name, ur.river_name, cr.run_date::text,
		       COALESCE(cr.run_time::text, ''), COALESCE(cr.meetup_spot, ''),
		       cr.gauge_cfs, cr.flow_band, COALESCE(up.handle, ''), up.display_name, cr.ics_sequence, cr.paddled
		FROM calendar_runs cr
		LEFT JOIN user_reaches ur ON ur.id = cr.user_reach_id
		LEFT JOIN user_profiles up ON up.owner_id = cr.owner_id
		WHERE cr.id = $1::uuid AND cr.owner_id = $2 AND cr.deleted_at IS NULL
	`, runID, hostOwnerID).Scan(&ri.ID, &ri.Slug, &ri.Name, &ri.RiverName, &ri.RunDate,
		&ri.RunTime, &ri.MeetupSpot, &ri.GaugeCFS, &ri.FlowBand, &ri.HostHandle, &ri.HostDisplayName, &ri.ICSSequence, &ri.Paddled)
	if err != nil {
		return invitedRunInfo{}, &apiError{http.StatusNotFound, "run not found"}
	}
	return ri, nil
}

// generateInviteToken mints a random invite claim token: the raw token is
// embedded in the emailed link (?invite=...) and never stored; only its
// SHA-256 hex digest is persisted (run_invites.invite_token_hash) — mirrors
// auth.APIKey's hash-and-lookup shape (internal/auth/apikey.go) and
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

// runInviteSubject builds the email subject (contract §6, web#354 A2
// single-run form): "@host invited you to run {RunName} on
// {M/D/YYYY}[ at {h:MM AM}]". hostLabel is caller-rendered (sendRunInviteMail,
// via displayLabel/the "a paddler" fallback) — display-names feature
// (mig 000130): "Ian K (@iankco)" when the host has one set, plain
// "@iankco" otherwise, so this function no longer bakes the "@" prefix in
// itself.
func runInviteSubject(hostLabel string, run invitedRunInfo) string {
	line := fmt.Sprintf("%s invited you to run %s on %s", hostLabel, run.Name, formatUSDate(run.RunDate))
	if t := formatUSTime(run.RunTime); t != "" {
		line += " at " + t
	}
	return line
}

// buildRunInviteEmailBody renders the invite (contract §6: "a non-user gets
// full trip context without ever signing in") — river, date/time, meetup,
// flow, then a single Accept link. web#354 A2: replaces the whole-plan
// itinerary layout (buildInviteEmailBody) — one run, one link. acceptURL is
// caller-supplied (sendRunInviteMail): {WEB_BASE_URL}/plan-runs/{id}?invite=
// {token} for an email invitee (lands on the run page, claims via token —
// they have no account yet), or the bare {WEB_BASE_URL}/plan-runs/{id} for a
// handle invitee (API-1.x follow-up: they already have an account and
// accept IN-APP — the feed + their member row surface the invite — so there
// is no token to mint or embed). Returns HTML + a plain-text mirror.
//
// API-2 RSVP copy-steer (INVITE_SYNC_PLAN.md Amendments), revised for
// inbound RSVP (api#176 — a Cloudflare Email Worker now relays a mail
// client's own METHOD:REPLY back to a webhook that applies the RSVP, so a
// mail client's native Accept/Decline UI on this .ics's ORGANIZER/ATTENDEE
// lines is no longer a dead end): rsvpButtonHTML/rsvpSteerLine
// (notifications.go) still make OUR Accept link visually primary — it's the
// reliable path across every provider, since not all mail clients render
// native iTIP UI the same way — but the steer copy no longer claims the
// mail client's own reply does nothing, since that's no longer true.
//
// hostLabel is caller-rendered (sendRunInviteMail — same value passed to
// runInviteSubject, see its doc comment) rather than read off run.HostHandle
// directly here — display-names feature (mig 000130), and incidentally
// fixes this body ever going out bearing a bare "@" when a run's host has
// no profile row (HostHandle == ""): sendRunInviteMail's "a paddler"
// fallback now covers this call site too, whereas previously only the
// subject got it.
func buildRunInviteEmailBody(hostLabel string, run invitedRunInfo, acceptURL string) (htmlBody, textBody string) {
	// runDetailLines (notifications.go) — shared with every notify* sender
	// in this package so the river/date/meetup/flow block isn't duplicated
	// per email type.
	htmlDetails, textDetails := runDetailLines(run.RiverName, run.RunDate, run.RunTime, run.MeetupSpot, run.GaugeCFS, run.FlowBand)

	const steer = "Tap Accept to join the crew — your reply lands right on H2OFlows."
	htmlBody = fmt.Sprintf(
		`<p>%s invited you on H2OFlows.</p><h2>%s</h2><ul>%s</ul><p>%s</p>%s`,
		html.EscapeString(hostLabel), html.EscapeString(run.Name), htmlDetails,
		rsvpButtonHTML(acceptURL, "Accept"), rsvpSteerLine(steer),
	)
	textBody = fmt.Sprintf(
		"%s invited you on H2OFlows.\n%s\n%s\nAccept: %s\n\n%s\n",
		hostLabel, run.Name, textDetails, acceptURL, steer,
	)
	return htmlBody, textBody
}

// pendingRunInviteMail carries everything needed to send + attach an invite
// email after the enclosing statement/request completes — an outbound HTTPS
// call to Resend must never run while a DB tx is open (there isn't one for
// InviteToRun's single INSERT, but the async-send-after-commit discipline is
// kept for ResendInvite's UPDATE too, and to keep this codepath consistent
// with sendInviteMail's original contract).
type pendingRunInviteMail struct {
	to string
	// rawToken is "" for a handle invite (API-1.x follow-up): a handle
	// invitee already has an account and accepts IN-APP, so there is no
	// claim token to mint/embed — sendRunInviteMail links plainly to the run
	// instead of {runURL}?invite={rawToken}.
	rawToken  string
	attachICS bool
	// attendeeName feeds the ICS ATTENDEE;CN= for this recipient — "" for an
	// email invitee (no display name known before they have an account,
	// same as before this field existed), the invitee's own
	// user_profiles.display_name when set (display-names feature, mig
	// 000130) else their handle, for a handle invite.
	attendeeName string
	run          invitedRunInfo
}

// sendRunInviteMail builds the invite email (+ .ics attachment when
// requested) and sends it. Package-level (not a method) so it can be fired
// from a detached goroutine with its own context.Background()+timeout — the
// triggering HTTP request's context dies at response. web#354 A2: one
// VEVENT per invite (ics.BuildRunInvite), not the multi-VEVENT whole-plan
// attachment. API-1 (Invite Sync, D2): the attachment is now
// METHOD:REQUEST — not PUBLISH — with SEQUENCE=p.run.ICSSequence (0 for a
// fresh invite; the run's actual current sequence for a ResendInvite call,
// which reuses this same function), ORGANIZER = the configured MAIL_FROM
// identity (organizerName/organizerEmail, notifications.go —
// SetMailFrom'd at startup), and ATTENDEE = the invitee (p.to, CN=
// p.attendeeName — "" for an email-only invitee with no display name known
// yet). PUBLISH copies don't reconcile later updates in Outlook/Google/
// Apple — a REQUEST does, via the fixed UID + this SEQUENCE.
//
// API-1.x follow-up: also reused (with rawToken=="") from InviteToRun's
// handle branch — see pendingRunInviteMail's field docs above for how the
// accept link and ATTENDEE CN differ for that path.
func sendRunInviteMail(mailer mail.Mailer, p pendingRunInviteMail) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// hostLabel (display-names feature, mig 000130): "Ian K (@iankco)" when
	// the host has a display_name set, plain "@iankco" otherwise
	// (displayLabel), or "a paddler" when there's no handle at all to
	// render (a profile-less host — rare, pre-existing fallback). Computed
	// once and threaded into both runInviteSubject and
	// buildRunInviteEmailBody so subject/body never disagree.
	hostLabel := "a paddler"
	if p.run.HostHandle != "" {
		hostLabel = displayLabel(p.run.HostDisplayName, p.run.HostHandle)
	}
	runURL := fmt.Sprintf("%s/plan-runs/%s", webBaseURL, p.run.ID)

	// acceptURL: an email invitee has no account yet, so the link carries
	// their ?invite= claim token; a handle invitee (p.rawToken == "") already
	// has one and accepts in-app, so the link is just the plain run page —
	// see pendingRunInviteMail.rawToken's doc comment. Both forms also carry
	// accept=1 (prod-testing follow-up) so the web auto-accepts on landing
	// when the viewer holds a pending invite for this run, instead of
	// requiring a second tap once they're already on the page — distinct
	// from notifyRunMaterialChange's "View run" link (notifications.go),
	// which deliberately omits it since viewing an update isn't accepting.
	acceptURL := runURL + "?accept=1"
	if p.rawToken != "" {
		acceptURL = fmt.Sprintf("%s?invite=%s&accept=1", runURL, p.rawToken)
	}

	subject := runInviteSubject(hostLabel, p.run)
	htmlBody, textBody := buildRunInviteEmailBody(hostLabel, p.run, acceptURL)

	msg := mail.Message{To: p.to, Subject: subject, HTML: htmlBody, Text: textBody}
	if p.attachICS {
		icsBody, err := ics.BuildRunInvite(ics.RunInviteInput{
			RunID:          p.run.ID,
			Method:         ics.MethodRequest,
			Sequence:       p.run.ICSSequence,
			OrganizerName:  organizerName,
			OrganizerEmail: organizerEmail,
			AttendeeName:   p.attendeeName,
			AttendeeEmail:  p.to,
			Name:           p.run.Name,
			RunDate:        p.run.RunDate,
			RunTime:        p.run.RunTime,
			MeetupSpot:     p.run.MeetupSpot,
			URL:            runURL,
		})
		if err != nil {
			// Degrade to sending the email WITHOUT the .ics rather than not
			// sending at all (e.g. MAIL_FROM unset locally -> empty
			// organizerEmail -> ics.ErrMissingOrganizer) — the accept link
			// in the body still works even if the calendar attachment can't
			// be built.
			log.Printf("invite mail: build ics failed (to=%s run=%s): %v", p.to, p.run.ID, err)
		} else {
			msg.Attachments = append(msg.Attachments, mail.Attachment{
				Filename:    "invite.ics",
				ContentType: "text/calendar; charset=utf-8; method=REQUEST",
				Content:     []byte(icsBody),
			})
		}
	}

	if err := mailer.Send(ctx, msg); err != nil {
		log.Printf("invite mail send failed (to=%s run=%s): %v", p.to, p.run.ID, err)
	}
}

// ── POST /plan-runs/{id}/invite ──────────────────────────────────────────
// web#354 A2: replaces POST /plans/{id}/invite (InviteToPlan, REMOVED) — one
// run, one invite. Owner-of-run gated (404 non-owner, same convention as
// every other owner-scoped lookup in this package). Body is either
// {"handle": "..."} (member_owner_id resolved immediately, no token) or
// {"email": "...", "attach_ics"?, "message"?} (token-based, async mail).
//
// Dup handling: a plain INSERT (no ON CONFLICT DO NOTHING — unlike the old
// per-run fan-out, there is exactly one row to insert here, so "already
// invited" is a real conflict, not a partial-success case to skip over) —
// a 23505 unique-violation against run_invites_owner_uk/run_invites_email_uk
// specifically is reported as 409; any OTHER db error (including a 23505 on
// some other constraint, which shouldn't happen here but must never be
// silently swallowed) surfaces as a 500.
func (h *InviteHandler) InviteToRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "id")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !h.rlInvite.allow(ownerID, 20, time.Hour) {
		errorResponse(w, http.StatusTooManyRequests, "rate limit: 20 invites per hour")
		return
	}

	var body struct {
		Handle    *string `json:"handle"`
		Email     *string `json:"email"`
		AttachICS *bool   `json:"attach_ics"`
		Message   *string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	ctx := r.Context()
	run, rerr := h.loadInvitedRunInfo(ctx, runID, ownerID)
	if rerr != nil {
		h.respondAPIError(w, rerr)
		return
	}
	// API-2 (Amendments, "No inviting to logged runs"): server-side twin of
	// the UI gate already shipped in web#362.
	if run.Paddled {
		errorResponse(w, http.StatusUnprocessableEntity, "run already logged")
		return
	}

	switch {
	case body.Handle != nil && strings.TrimSpace(*body.Handle) != "":
		handle := strings.TrimPrefix(strings.TrimSpace(*body.Handle), "@")

		var targetOwnerID, targetHandle string
		var targetDisplayName *string
		if err := h.db.QueryRow(ctx,
			`SELECT owner_id, handle, display_name FROM user_profiles WHERE LOWER(handle) = LOWER($1)`, handle,
		).Scan(&targetOwnerID, &targetHandle, &targetDisplayName); err != nil {
			errorResponse(w, http.StatusNotFound, "user not found")
			return
		}
		if targetOwnerID == ownerID {
			// Host invites themselves — already implicitly "in" the run (no
			// member row exists for the host); no-op, 200 existing.
			jsonResponse(w, http.StatusOK, map[string]string{"status": "existing"})
			return
		}

		// R4 (INVITE_SYNC_PLAN.md risks): run_invites_owner_uk is unique on
		// (run_id, member_owner_id) with NO status filter — a previously
		// DECLINED handle-invite row still occupies that slot forever, so a
		// plain re-INSERT here would always 23505 on it, permanently
		// blocking re-inviting anyone who once declined. Resurrect that row
		// instead (flip back to invited, clear the response) when it
		// exists; only fall through to INSERT when there's nothing to
		// resurrect — which keeps 409 for the genuinely-still-pending case
		// (invited/accepted) unchanged below.
		var inviteID, status string
		resurrectErr := h.db.QueryRow(ctx, `
			UPDATE run_invites
			SET status = 'invited', origin = 'invite', invite_handle = $1, invited_by = $2,
			    responded_at = NULL, dismissed_at = NULL, updated_at = NOW()
			WHERE run_id = $3::uuid AND member_owner_id = $4 AND status = 'declined'
			RETURNING id, status::text
		`, handle, ownerID, runID, targetOwnerID).Scan(&inviteID, &status)
		if resurrectErr != nil {
			if !errors.Is(resurrectErr, pgx.ErrNoRows) {
				errorResponse(w, http.StatusInternalServerError, "invite failed")
				return
			}
			err := h.db.QueryRow(ctx, `
				INSERT INTO run_invites (run_id, member_owner_id, invite_handle, invited_by, origin, status)
				VALUES ($1::uuid, $2, $3, $4, 'invite', 'invited')
				RETURNING id, status::text
			`, runID, targetOwnerID, handle, ownerID).Scan(&inviteID, &status)
			if err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "run_invites_owner_uk" {
					errorResponse(w, http.StatusConflict, "already invited to this run")
					return
				}
				errorResponse(w, http.StatusInternalServerError, "invite failed")
				return
			}
		}

		// API-1.x follow-up: a handle invitee already has an account and
		// accepts IN-APP (their run_invites member row + the feed surface
		// the invite) — but if we already know their address (user_emails,
		// resolveNotifyEmail below), send them the same invite email an
		// email-invitee gets too, minus the claim token (sendRunInviteMail
		// with rawToken=="" links plainly to the run instead of
		// ?invite={token} — see its + pendingRunInviteMail's doc comments).
		// Absent address -> silent skip, matching this branch's prior
		// (email-free) behavior. Async, own goroutine, never blocks this
		// response; response shape is unchanged either way ("sent" stays
		// false — that field reflects the token-based email flow only).
		if toEmail, ok := resolveNotifyEmail(ctx, h.db, &targetOwnerID, nil); ok {
			// ATTENDEE;CN= (display-names feature, mig 000130): the
			// invitee's display_name when they have one set, else their
			// handle — this is a bound row (member_owner_id is set above at
			// invite-creation time for a handle invite), so "else ''" never
			// applies here, unlike the email-invite branch below.
			attendeeName := targetHandle
			if targetDisplayName != nil {
				if dn := strings.TrimSpace(*targetDisplayName); dn != "" {
					attendeeName = dn
				}
			}
			go sendRunInviteMail(h.mailer, pendingRunInviteMail{to: toEmail, attachICS: true, attendeeName: attendeeName, run: run})
		}

		jsonResponse(w, http.StatusCreated, map[string]any{"id": inviteID, "status": status, "sent": false})
		return

	case body.Email != nil && strings.TrimSpace(*body.Email) != "":
		email := strings.ToLower(strings.TrimSpace(*body.Email))

		// Mirror the handle branch's host-self guard above: a host emailing
		// their own login address has no user_profiles.email to resolve
		// against (that table has no email column — see MyInvites/
		// AcceptInvite/DismissInvite, which all read the caller's email off
		// the JWT via auth.EmailFromContext instead), so compare against the
		// HOST's own JWT email the same way. Without this, InviteToRun would
		// insert a real run_invites row the host could later accept via
		// AcceptInvite's email match, becoming accepted crew on their own run.
		if hostEmail, hok := auth.EmailFromContext(ctx); hok && hostEmail != "" && strings.EqualFold(hostEmail, email) {
			jsonResponse(w, http.StatusOK, map[string]string{"status": "existing"})
			return
		}

		attachICS := body.AttachICS == nil || *body.AttachICS // default TRUE

		rawToken, tokenHash, terr := generateInviteToken()
		if terr != nil {
			errorResponse(w, http.StatusInternalServerError, "token generation failed")
			return
		}

		// R4 (INVITE_SYNC_PLAN.md risks): run_invites_email_uk is unique on
		// (run_id, LOWER(invite_email)) WHERE member_owner_id IS NULL — with
		// no status filter either, and decline never binds member_owner_id
		// (only accept does), so a DECLINED email invite occupies that slot
		// forever too. Same resurrect-before-insert fix as the handle
		// branch above: flip the declined row back to invited with a fresh
		// token instead of INSERTing a duplicate.
		var inviteID, status string
		resurrectErr := h.db.QueryRow(ctx, `
			UPDATE run_invites
			SET status = 'invited', origin = 'invite', invite_token_hash = $1, invited_by = $2, message = $3,
			    responded_at = NULL, dismissed_at = NULL, updated_at = NOW()
			WHERE run_id = $4::uuid AND member_owner_id IS NULL AND LOWER(invite_email) = $5 AND status = 'declined'
			RETURNING id, status::text
		`, tokenHash, ownerID, body.Message, runID, email).Scan(&inviteID, &status)
		if resurrectErr != nil {
			if !errors.Is(resurrectErr, pgx.ErrNoRows) {
				errorResponse(w, http.StatusInternalServerError, "invite failed")
				return
			}
			err := h.db.QueryRow(ctx, `
				INSERT INTO run_invites (run_id, invite_email, invited_by, origin, status, invite_token_hash, message)
				VALUES ($1::uuid, $2, $3, 'invite', 'invited', $4, $5)
				RETURNING id, status::text
			`, runID, email, ownerID, tokenHash, body.Message).Scan(&inviteID, &status)
			if err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "run_invites_email_uk" {
					errorResponse(w, http.StatusConflict, "already invited to this run")
					return
				}
				errorResponse(w, http.StatusInternalServerError, "invite failed")
				return
			}
		}

		go sendRunInviteMail(h.mailer, pendingRunInviteMail{to: email, rawToken: rawToken, attachICS: attachICS, run: run})

		jsonResponse(w, http.StatusCreated, map[string]any{"id": inviteID, "status": status, "sent": true})
		return

	default:
		errorResponse(w, http.StatusBadRequest, "handle or email is required")
	}
}

// ── POST /plan-runs/{id}/invite/resend ───────────────────────────────────
// web#354 A2: replaces POST /plans/{id}/invite/resend, single-run. Host-only,
// rate-limited 10/hr/owner. Email-only (a handle invite has no token to
// resend — accept there is by owner_id match, nothing to re-deliver). 404 if
// there's no invited row for that email on this run; 409 if it has already
// been accepted/declined.
//
// Judgment call: the raw token is never persisted (only its hash), so a
// resend cannot literally reuse the ORIGINAL link — it always mints a fresh
// token and rotates invite_token_hash, invalidating the previous link. This
// is consistent with "single-use": at most one live accept link should be
// outstanding for a still-pending invite at any time, mirroring how
// AcceptInvite nulls the hash on accept (both keep exactly one valid link in
// play until it's consumed once).
func (h *InviteHandler) ResendInvite(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "id")
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
	run, rerr := h.loadInvitedRunInfo(ctx, runID, ownerID)
	if rerr != nil {
		h.respondAPIError(w, rerr)
		return
	}
	// API-2 (Amendments, "No inviting to logged runs") — see InviteToRun's
	// identical guard above.
	if run.Paddled {
		errorResponse(w, http.StatusUnprocessableEntity, "run already logged")
		return
	}

	var inviteID, status string
	if err := h.db.QueryRow(ctx, `
		SELECT id, status::text FROM run_invites
		WHERE run_id = $1::uuid AND member_owner_id IS NULL AND LOWER(invite_email) = $2 AND origin = 'invite'
	`, runID, email).Scan(&inviteID, &status); err != nil {
		errorResponse(w, http.StatusNotFound, "no invited row for that email on this run")
		return
	}
	if status != "invited" {
		errorResponse(w, http.StatusConflict, "invite has already been accepted or declined")
		return
	}

	rawToken, tokenHash, terr := generateInviteToken()
	if terr != nil {
		errorResponse(w, http.StatusInternalServerError, "token generation failed")
		return
	}

	// AND status = 'invited' closes a TOCTOU window against the SELECT above:
	// without it, an accept that commits between the SELECT and this UPDATE
	// would have this blind write re-arm a fresh invite_token_hash on a row
	// AcceptInvite had just nulled to make single-use, resurrecting the
	// ?invite= anon-read carve-out on an already-consumed invite.
	// RowsAffected()==0 means the invite resolved (accepted/declined) in
	// that window — a no-op 409 instead of silently reviving it.
	tag, err := h.db.Exec(ctx,
		`UPDATE run_invites SET invite_token_hash = $1, updated_at = NOW() WHERE id = $2::uuid AND status = 'invited'`,
		tokenHash, inviteID,
	)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "resend failed")
		return
	}
	if tag.RowsAffected() == 0 {
		errorResponse(w, http.StatusConflict, "invite has already been accepted or declined")
		return
	}

	go sendRunInviteMail(h.mailer, pendingRunInviteMail{to: email, rawToken: rawToken, attachICS: true, run: run})

	jsonResponse(w, http.StatusOK, map[string]string{"status": "resent", "id": inviteID})
}

// ── GET /me/invites ───────────────────────────────────────────────────────
// web#354 A2: flat, run_date-sorted list of run_invites rows (origin=invite)
// where the caller is the member (or the invite's email matches theirs) —
// each item embeds a run summary. Grouping by day/event is W4's problem
// (client-side); this endpoint deliberately does not nest.

// inviteRunSummary is the run projection embedded in each /me/invites item —
// contract: {run_id, slug, name/river/state, run_date, run_time, meetup_spot,
// flow fields, crew {filled,max}, host_handle, host_display_name?}.
// Name (web#354 A4) is the calendar run's OWN name (calendar_runs.name, NOT
// NULL) — was sourced from the library run's join (COALESCE(ur.name,
// 'Paddle')) prior to A4; that's now ReachName, kept as a separate field
// (nil for an orphaned run) alongside the pre-existing RiverName (the
// actual named river, e.g. "Blue River" — NOT the same thing as a reach's
// own saved-run name) so the web can still render both as subtitles.
type inviteRunSummary struct {
	RunID      string            `json:"run_id"`
	Slug       string            `json:"slug"`
	Name       string            `json:"name"`
	ReachName  *string           `json:"reach_name,omitempty"`
	RiverName  *string           `json:"river_name,omitempty"`
	StateAbbr  *string           `json:"state_abbr,omitempty"`
	RunDate    string            `json:"run_date"`
	RunTime    *string           `json:"run_time,omitempty"`
	MeetupSpot *string           `json:"meetup_spot,omitempty"`
	GaugeCFS   *float64          `json:"gauge_cfs,omitempty"`
	FlowBand   *string           `json:"flow_band,omitempty"`
	FlowColor  *string           `json:"flow_color,omitempty"`
	Crew       discoverCrewMeter `json:"crew"`
	HostHandle string            `json:"host_handle"`
	// HostDisplayName (display-names feature, mig 000130): additive
	// alongside HostHandle, same nil-means-fall-back-to-@handle contract as
	// planRunSummary.HostDisplayName (plans.go).
	HostDisplayName *string `json:"host_display_name,omitempty"`
}

type myInviteItem struct {
	ID          string           `json:"id"`
	Status      string           `json:"status"`
	DismissedAt *string          `json:"dismissed_at,omitempty"`
	InvitedVia  string           `json:"invited_via"` // "handle" | "email"
	CreatedAt   string           `json:"created_at"`
	Run         inviteRunSummary `json:"run"`
}

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
		SELECT ri.id, ri.status::text, ri.dismissed_at, (ri.invite_handle IS NOT NULL) AS via_handle, ri.created_at,
		       cr.id::text, cr.slug, cr.name, ur.name, ur.river_name, COALESCE(ur.state_abbr, rv.state_abbr),
		       cr.run_date::text, cr.run_time::text, cr.meetup_spot,
		       cr.gauge_cfs, cr.flow_band, cr.flow_color,
		       cr.max_crew, COALESCE(cm.filled, 0),
		       COALESCE(up.handle, ''), up.display_name
		FROM run_invites ri
		JOIN calendar_runs cr ON cr.id = ri.run_id AND cr.deleted_at IS NULL
		LEFT JOIN user_reaches ur ON ur.id = cr.user_reach_id
		LEFT JOIN rivers rv ON rv.id = ur.river_id
		LEFT JOIN user_profiles up ON up.owner_id = cr.owner_id
		LEFT JOIN LATERAL (
			SELECT COUNT(*) AS filled FROM run_invites ri2
			WHERE ri2.run_id = cr.id AND ri2.status = 'accepted'
		) cm ON true
		WHERE ri.origin = 'invite'
		  AND (ri.member_owner_id = $1 OR (ri.member_owner_id IS NULL AND LOWER(ri.invite_email) = LOWER($2)))
		ORDER BY cr.run_date, ri.created_at
	`, ownerID, email)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	out := []myInviteItem{}
	for rows.Next() {
		var item myInviteItem
		var dismissedAtRaw *time.Time
		var createdAtRaw time.Time
		var viaHandle bool
		var runTime *string
		if err := rows.Scan(
			&item.ID, &item.Status, &dismissedAtRaw, &viaHandle, &createdAtRaw,
			&item.Run.RunID, &item.Run.Slug, &item.Run.Name, &item.Run.ReachName, &item.Run.RiverName, &item.Run.StateAbbr,
			&item.Run.RunDate, &runTime, &item.Run.MeetupSpot,
			&item.Run.GaugeCFS, &item.Run.FlowBand, &item.Run.FlowColor,
			&item.Run.Crew.Max, &item.Run.Crew.Filled,
			&item.Run.HostHandle, &item.Run.HostDisplayName,
		); err != nil {
			errorResponse(w, http.StatusInternalServerError, "scan failed")
			return
		}
		if runTime != nil && *runTime != "" {
			item.Run.RunTime = runTime
		}
		item.InvitedVia = "email"
		if viaHandle {
			item.InvitedVia = "handle"
		}
		item.CreatedAt = createdAtRaw.Format(time.RFC3339)
		if dismissedAtRaw != nil {
			s := dismissedAtRaw.Format(time.RFC3339)
			item.DismissedAt = &s
		}
		out = append(out, item)
	}

	jsonResponse(w, http.StatusOK, map[string]any{"invites": out})
}

// ── POST /invites/{id}/accept ────────────────────────────────────────────
// web#354 A2: re-keyed to run_invites (run_id, not plan_id/plan_run_id); the
// crew cap check reads the run's own max_crew, same FOR UPDATE serialization
// pattern #246 A7 introduced. Response now names the run so the web can
// route straight to /plan-runs/{slug|id} without a second lookup.

func (h *InviteHandler) AcceptInvite(w http.ResponseWriter, r *http.Request) {
	inviteID := chi.URLParam(r, "id")
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
	var status, runID string
	if err := h.db.QueryRow(ctx, `
		SELECT member_owner_id, invite_email, invite_token_hash, status::text, run_id::text
		FROM run_invites WHERE id = $1::uuid AND origin = 'invite'
	`, inviteID).Scan(&curMemberOwnerID, &curEmail, &curTokenHash, &status, &runID); err != nil {
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

	// Serialize against concurrent RunCrewAccept/JoinRun/AcceptInvite on the
	// same run and recheck filled<max_crew inside the lock — invites count
	// against the cap same as crew requests (contract: max_crew caps total
	// accepted paddlers per run, regardless of origin).
	tx, err := h.db.Begin(ctx)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "tx failed")
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	var maxCrew *int
	var slug string
	if err := tx.QueryRow(ctx,
		`SELECT max_crew, slug FROM calendar_runs WHERE id = $1::uuid AND deleted_at IS NULL FOR UPDATE`, runID,
	).Scan(&maxCrew, &slug); err != nil {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}

	// Re-read the invite inside the run-serialized tx: the pre-lock read is
	// stale under concurrent accepts (double-click / client retry), and the
	// stale 'invited' value let the losing request skip this idempotent
	// return, re-run the UPDATE, and fire a second organizer notification.
	if err := tx.QueryRow(ctx,
		`SELECT status::text, member_owner_id FROM run_invites WHERE id = $1::uuid`, inviteID,
	).Scan(&status, &curMemberOwnerID); err != nil {
		errorResponse(w, http.StatusNotFound, "invite not found")
		return
	}

	if status == "accepted" && curMemberOwnerID != nil && *curMemberOwnerID == ownerID {
		jsonResponse(w, http.StatusOK, map[string]string{"status": "accepted", "run_id": runID, "slug": slug})
		return
	}

	if status != "accepted" {
		filled, ferr := runFilled(ctx, tx, runID)
		if ferr != nil {
			errorResponse(w, http.StatusInternalServerError, "crew count failed")
			return
		}
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
		UPDATE run_invites
		SET member_owner_id = $1, status = 'accepted', responded_at = NOW(), updated_at = NOW(), invite_token_hash = NULL
		WHERE id = $2::uuid AND origin = 'invite' AND status <> 'declined'
		  AND (member_owner_id IS NULL OR member_owner_id = $1)
	`, ownerID, inviteID)
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

	// API-1 Invite Sync, post-commit only (both below are best-effort side
	// effects of a successful accept — neither should roll back or delay
	// the accept itself, so they run after Commit, not inside the tx):
	//   - upsertUserEmail: capture the accepter's email (see its doc
	//     comment, plans.go) — one of the plan's three named call sites.
	//   - notifyOrganizerAccepted (item 6, notifications.go): tell the
	//     organizer "@accepter accepted" — in scope for THIS PR since
	//     AcceptInvite is already being touched here. Fired async (`go`);
	//     silently no-ops if the organizer has no user_emails row yet (R2).
	if email != "" {
		upsertUserEmail(ctx, h.db, ownerID, email)
	}
	var accepterHandle string
	var accepterDisplayName *string
	h.db.QueryRow(ctx, `SELECT COALESCE(handle, ''), display_name FROM user_profiles WHERE owner_id = $1`, ownerID).Scan(&accepterHandle, &accepterDisplayName)
	go notifyOrganizerAccepted(h.mailer, h.db, runID, accepterHandle, accepterDisplayName)

	jsonResponse(w, http.StatusOK, map[string]string{"status": "accepted", "run_id": runID, "slug": slug})
}

// ── POST /invites/{id}/dismiss ───────────────────────────────────────────
// Dismiss keeps the row (and its status) in GET /me/invites — only
// dismissed_at is set. Re-keyed to run_invites; shape unchanged from #246 A4.

func (h *InviteHandler) DismissInvite(w http.ResponseWriter, r *http.Request) {
	inviteID := chi.URLParam(r, "id")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()
	email, _ := auth.EmailFromContext(ctx)

	tag, err := h.db.Exec(ctx, `
		UPDATE run_invites SET dismissed_at = NOW(), updated_at = NOW()
		WHERE id = $1::uuid AND origin = 'invite'
		  AND (member_owner_id = $2 OR (member_owner_id IS NULL AND LOWER(invite_email) = LOWER($3)))
	`, inviteID, ownerID, email)
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

// ── POST /plan-runs/{id}/leave ────────────────────────────────────────────
// API-2 (INVITE_SYNC_PLAN.md item 4, D6): the attendee-facing counterpart to
// RunCrewDecline below — "remove myself from this run's calendar" rather
// than the host uninviting someone. Run-scoped per D6 ("leave is run-scoped
// POST /plan-runs/{id}/leave" — distinct from DismissInvite, which only
// hides a row from the /me/invites feed and leaves status untouched).
// Targets the caller's OWN row on runID — member match OR the same
// email-fallback every other invite endpoint in this file uses (MyInvites/
// AcceptInvite/DismissInvite) for a still-pending email invite that hasn't
// bound member_owner_id yet.
//
// Flips from invited/accepted/requested (review fix — a lingering
// 'requested' join-request row used to survive "leave" untouched: the flip
// only targeted invited/accepted, so a requester who called leave got a
// false 200 "ok" while their request stayed live for the host to later
// accept, seating them on a run they believed they'd left. There's no
// separate withdraw-a-join-request endpoint, so leave is the only
// attendee-facing "remove me" path and needs to cover all three live
// states.
//
// deleted_at gate (review fix — leaving a cancelled run used to email the
// organizer): the UPDATE now requires the run itself to still be live
// (EXISTS ... deleted_at IS NULL), matching RunCrewDecline's cr.deleted_at
// IS NULL gate and renderPlanRun. DeleteRun tombstones calendar_runs but
// never touches run_invites, so without this a leave call racing (or
// arriving after) a delete would still flip a live invite row and fire
// notifyOrganizerDeclined for a run the organizer already deleted.
//
// origin-gated notify (review fix, same finding as the requested-row fix
// above): flipping a 'requested' row is a genuine withdrawal, but it must
// NOT trigger notifyOrganizerDeclined — that email's copy ("X declined —
// invite someone else") assumes X was actually invited/seated, which is
// wrong for someone withdrawing their own unactioned join request (the
// plan's D5 deliberately keeps join-request churn silent, same reasoning as
// RunCrewDecline's origin='request' skip). RETURNING origin decides.
//
// Idempotent by design: ErrNoRows doesn't distinguish "run is gone" /
// "already declined" / "never a member of this run" on its own, so the
// fallback below layers those checks in order — "keep it simple + safe" per
// the plan, rather than threading the pre-flip status through an extra
// SELECT-before-UPDATE like RunCrewDecline does above (that extra work
// exists there to decide WHICH email to send AND to close a double-decline
// race; this endpoint's only two notify branches are "origin='invite'" vs
// "nothing", decided by the same RETURNING that performs the flip, so no
// separate locking read is needed here).
func (h *InviteHandler) LeaveRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "id")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()
	email, _ := auth.EmailFromContext(ctx)

	var origin string
	err := h.db.QueryRow(ctx, `
		UPDATE run_invites
		SET status = 'declined', responded_at = NOW(), updated_at = NOW()
		WHERE run_id = $1::uuid AND status IN ('invited', 'accepted', 'requested')
		  AND (member_owner_id = $2 OR (member_owner_id IS NULL AND LOWER(invite_email) = LOWER($3)))
		  AND EXISTS (SELECT 1 FROM calendar_runs cr WHERE cr.id = $1::uuid AND cr.deleted_at IS NULL)
		RETURNING origin::text
	`, runID, ownerID, email).Scan(&origin)
	if err == nil {
		// D5 ("notify organizer on accept AND on decline/leave") — fired
		// ONLY for an actual invite/accept flip, never for a withdrawn join
		// request (origin='request', see doc comment above) and never on
		// the idempotent no-op path below. No .ics (see
		// notifyOrganizerDeclined's own doc comment) and no ics_sequence
		// bump — nothing material about the RUN itself changed, only this
		// one attendee's own status, so other recipients' calendars have
		// nothing to reconcile.
		if origin == "invite" {
			var declinerHandle string
			var declinerDisplayName *string
			h.db.QueryRow(ctx, `SELECT COALESCE(handle, ''), display_name FROM user_profiles WHERE owner_id = $1`, ownerID).Scan(&declinerHandle, &declinerDisplayName)
			go notifyOrganizerDeclined(h.mailer, h.db, runID, declinerHandle, declinerDisplayName)
		}
		jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		errorResponse(w, http.StatusInternalServerError, "leave failed")
		return
	}

	// No flip. Could be: the run is gone (404, consistent with
	// renderPlanRun's own deleted_at gate), the caller was never a member
	// (404), or they'd already left/declined (idempotent 200) — check in
	// that order.
	var runLive bool
	h.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM calendar_runs WHERE id = $1::uuid AND deleted_at IS NULL)`, runID).Scan(&runLive)
	if !runLive {
		errorResponse(w, http.StatusNotFound, "run not found")
		return
	}

	var exists bool
	h.db.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM run_invites WHERE run_id = $1::uuid
			AND (member_owner_id = $2 OR (member_owner_id IS NULL AND LOWER(invite_email) = LOWER($3))))
	`, runID, ownerID, email).Scan(&exists)
	if !exists {
		errorResponse(w, http.StatusNotFound, "not a member of this run")
		return
	}
	// Already declined (or otherwise resolved) — idempotent no-op, same 200
	// shape as the real-flip branch above so the web doesn't need to
	// distinguish "you just left" from "you'd already left".
	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ── POST /plan-runs/{id}/join ─────────────────────────────────────────────
// #246 A7: replaces POST /plans/{id}/join — crew requests RSVP to a specific
// run. Gates: THAT RUN's looking_for_crew + run-filled < run-max_crew.
// web#354 A2: re-keyed plan_members -> run_invites; there is no more
// plan_id/plan_run_id notion to carry (run_invites has neither — it is
// Run-scoped by construction, not by a nullable carve-out).
func (h *InviteHandler) JoinRun(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "id")
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

	var hostOwnerID string
	var lookingForCrew bool
	var maxCrew *int
	if err := h.db.QueryRow(ctx, `
		SELECT cr.owner_id, cr.looking_for_crew, cr.max_crew
		FROM calendar_runs cr
		WHERE cr.id = $1::uuid AND cr.deleted_at IS NULL
	`, runID).Scan(&hostOwnerID, &lookingForCrew, &maxCrew); err != nil {
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
		`SELECT id, status::text FROM run_invites WHERE run_id = $1::uuid AND member_owner_id = $2`,
		runID, ownerID,
	).Scan(&existingID, &existingStatus); err == nil {
		jsonResponse(w, http.StatusOK, map[string]string{"member_id": existingID, "status": existingStatus})
		return
	}

	if !lookingForCrew {
		errorResponse(w, http.StatusConflict, "this run is not open for crew requests")
		return
	}

	filled, ferr := runFilled(ctx, h.db, runID)
	if ferr != nil {
		errorResponse(w, http.StatusInternalServerError, "crew count failed")
		return
	}
	if maxCrew == nil || filled >= *maxCrew {
		errorResponse(w, http.StatusConflict, "crew is full")
		return
	}

	// ON CONFLICT DO NOTHING guards the race the SELECT above can't: a
	// double-clicked or concurrent join for the same (run_id,
	// member_owner_id) can pass the guard twice, and without this the second
	// INSERT would hit run_invites_owner_uk and 500 with a raw pgx error
	// string leaked to the client (#246 review).
	var memberID, status string
	err := h.db.QueryRow(ctx, `
		INSERT INTO run_invites (member_owner_id, origin, status, run_id, message)
		VALUES ($1, 'request', 'requested', $2::uuid, $3)
		ON CONFLICT (run_id, member_owner_id) WHERE member_owner_id IS NOT NULL DO NOTHING
		RETURNING id, status::text
	`, ownerID, runID, body.Message).Scan(&memberID, &status)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			errorResponse(w, http.StatusInternalServerError, "join request failed")
			return
		}
		// Lost the race: re-select and return 200 existing.
		if serr := h.db.QueryRow(ctx,
			`SELECT id, status::text FROM run_invites WHERE run_id = $1::uuid AND member_owner_id = $2`,
			runID, ownerID,
		).Scan(&memberID, &status); serr != nil {
			errorResponse(w, http.StatusInternalServerError, "join request failed")
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"member_id": memberID, "status": status})
		return
	}
	jsonResponse(w, http.StatusCreated, map[string]string{"member_id": memberID, "status": status})
}

// ── GET /plan-runs/{id}/crew ──────────────────────────────────────────────
// #246 A7: replaces GET /plans/{id}/crew — one roster per run. Both pending
// requests and accepted crew (of either origin — an accepted handle-invite is
// also "crew") so the host sees one roster; filled counts status='accepted'
// regardless of origin. web#354 A2: re-keyed plan_members -> run_invites.
//
// Host-only feedback gap fix (post-A2, W2 review): the roster ALSO now
// includes still-pending, non-dismissed invites (origin='invite',
// status='invited') — previously invisible here, so a host had no way to see
// who they'd already invited or to know who a resend targets. This endpoint
// is host-gated (403 below for anyone else), so the added rows — and the new
// InviteEmail field they carry for email-only invites — are owner-only by
// construction; there is no non-owner branch of this response that could
// leak invite_email to another crew member. Origin+Status together
// disambiguate a row's kind: origin='invite'+status='invited' = pending
// invite (this addition); origin='request'+status='requested' = pending
// join request; status='accepted' = seated crew (either origin).

func (h *InviteHandler) RunCrewList(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "id")
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
		SELECT cr.owner_id, cr.max_crew, cr.looking_for_crew
		FROM calendar_runs cr
		WHERE cr.id = $1::uuid AND cr.deleted_at IS NULL
	`, runID).Scan(&hostOwnerID, &maxCrew, &lookingForCrew); err != nil {
		errorResponse(w, http.StatusNotFound, "plan run not found")
		return
	}
	if hostOwnerID != ownerID {
		errorResponse(w, http.StatusForbidden, "host only")
		return
	}

	rows, err := h.db.Query(ctx, `
		SELECT ri.id, ri.status::text, ri.origin::text, ri.message, ri.created_at,
		       COALESCE(up.handle, ri.invite_handle, ''), ri.invite_email, up.display_name
		FROM run_invites ri
		LEFT JOIN user_profiles up ON up.owner_id = ri.member_owner_id
		WHERE ri.run_id = $1::uuid
		  AND (
		    ri.origin = 'request'
		    OR ri.status = 'accepted'
		    OR (ri.origin = 'invite' AND ri.status = 'invited' AND ri.dismissed_at IS NULL)
		    -- Declined invite-origin rows too (WEB-3/4): the owner's roster
		    -- shows "Declined" with a Re-invite affordance, and InviteToRun
		    -- resurrects the same row — invisible here meant un-re-invitable.
		    OR (ri.origin = 'invite' AND ri.status = 'declined')
		  )
		ORDER BY ri.created_at
	`, runID)
	if err != nil {
		errorResponse(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()

	type crewMember struct {
		MemberID    string  `json:"member_id"`
		Status      string  `json:"status"`
		Origin      string  `json:"origin"`
		Message     *string `json:"message,omitempty"`
		CreatedAt   string  `json:"created_at"`
		Handle      string  `json:"handle"`
		InviteEmail *string `json:"invite_email,omitempty"` // owner-only (see doc comment above); set only for pending email invites
		// DisplayName (display-names feature, mig 000130): straight from the
		// user_profiles join, additive alongside Handle — never a
		// replacement. nil for a still-pending EMAIL invite (member_owner_id
		// unset, so the LEFT JOIN has nothing to match — no account, no
		// profile, no display_name) as well as for a bound member who simply
		// hasn't typed one in.
		DisplayName *string `json:"display_name,omitempty"`
	}
	members := []crewMember{}
	for rows.Next() {
		var cm crewMember
		var createdAtRaw time.Time
		if err := rows.Scan(&cm.MemberID, &cm.Status, &cm.Origin, &cm.Message, &createdAtRaw, &cm.Handle, &cm.InviteEmail, &cm.DisplayName); err != nil {
			errorResponse(w, http.StatusInternalServerError, "scan failed")
			return
		}
		cm.CreatedAt = createdAtRaw.Format(time.RFC3339)
		members = append(members, cm)
	}

	filled, ferr := runFilled(ctx, h.db, runID)
	if ferr != nil {
		errorResponse(w, http.StatusInternalServerError, "crew count failed")
		return
	}

	jsonResponse(w, http.StatusOK, map[string]any{
		"members": members,
		"meter":   map[string]any{"filled": filled, "max": maxCrew, "looking_for_crew": lookingForCrew},
	})
}

// ── POST /plan-runs/{id}/crew/{inviteId}/accept ──────────────────────────
// #246 A7: replaces POST /plans/{id}/crew/{memberId}/accept. Locks the
// calendar_runs row for the duration of the tx so concurrent accepts against
// the same run serialize — the filled<max_crew recheck must happen inside
// that lock, not as a separate pre-check. web#354 A2: re-keyed
// plan_members -> run_invites.

func (h *InviteHandler) RunCrewAccept(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "id")
	inviteID := chi.URLParam(r, "inviteId")
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
		SELECT cr.owner_id, cr.max_crew
		FROM calendar_runs cr
		WHERE cr.id = $1::uuid AND cr.deleted_at IS NULL FOR UPDATE
	`, runID).Scan(&hostOwnerID, &maxCrew); err != nil {
		errorResponse(w, http.StatusNotFound, "plan run not found")
		return
	}
	if hostOwnerID != ownerID {
		errorResponse(w, http.StatusForbidden, "host only")
		return
	}

	var curStatus string
	if err := tx.QueryRow(ctx,
		`SELECT status::text FROM run_invites WHERE id = $1::uuid AND run_id = $2::uuid`, inviteID, runID,
	).Scan(&curStatus); err != nil {
		errorResponse(w, http.StatusNotFound, "crew request not found")
		return
	}
	if curStatus == "declined" {
		errorResponse(w, http.StatusConflict, "request was declined")
		return
	}

	if curStatus != "accepted" {
		filled, ferr := runFilled(ctx, tx, runID)
		if ferr != nil {
			errorResponse(w, http.StatusInternalServerError, "crew count failed")
			return
		}
		if maxCrew == nil || filled >= *maxCrew {
			errorResponse(w, http.StatusConflict, "crew is full")
			return
		}
		if _, err := tx.Exec(ctx,
			`UPDATE run_invites SET status = 'accepted', responded_at = NOW(), updated_at = NOW() WHERE id = $1::uuid`,
			inviteID,
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
	filled, ferr := runFilled(ctx, h.db, runID)
	if ferr != nil {
		errorResponse(w, http.StatusInternalServerError, "crew count failed")
		return
	}
	jsonResponse(w, http.StatusOK, map[string]any{"status": "accepted", "filled": filled, "max": maxCrew})
}

// ── POST /plan-runs/{id}/crew/{inviteId}/decline ─────────────────────────
// #246 A7: replaces POST /plans/{id}/crew/{memberId}/decline. web#354 A2:
// re-keyed plan_members -> run_invites.
//
// API-2 uninvite email (item 5): this is the host-driven "uninvite" —
// distinct from the attendee-driven LeaveRun above. The pre-flip
// status/origin decides whether this is a real uninvite (an already-accepted
// crew member, or a still-pending invite) vs. a join-request decline
// (origin='request', "Host declines a join request | — | — | nobody (v1)",
// D5) — the latter sends nothing, matching the plan's semantics map exactly.
//
// Concurrency (review fix, double-decline finding): the pre-flip read and
// the flip itself are ONE atomic statement — `old` is a locking CTE
// (SELECT ... FOR UPDATE) feeding an UPDATE that only flips a row NOT
// already 'declined', RETURNING the pre-flip status/origin it just read.
// Two concurrent decline calls on the same accepted row serialize on the
// FOR UPDATE lock: the first to commit flips and returns old.status=
// 'accepted'; the second, once unblocked, re-evaluates `old` against the
// now-'declined' row, the UPDATE's `status <> 'declined'` predicate excludes
// it, and it gets ErrNoRows — same as LeaveRun's status-guarded UPDATE
// above, which this endpoint previously lacked. Only the winner emails/
// bumps; the loser falls into the idempotent "already declined" 200 path
// below (same response shape a repeat call always got).
func (h *InviteHandler) RunCrewDecline(w http.ResponseWriter, r *http.Request) {
	runID := chi.URLParam(r, "id")
	inviteID := chi.URLParam(r, "inviteId")
	ownerID, ok := h.ownerID(r)
	if !ok {
		errorResponse(w, http.StatusUnauthorized, "authentication required")
		return
	}
	ctx := r.Context()

	var hostOwnerID string
	if err := h.db.QueryRow(ctx, `
		SELECT cr.owner_id
		FROM calendar_runs cr
		WHERE cr.id = $1::uuid AND cr.deleted_at IS NULL
	`, runID).Scan(&hostOwnerID); err != nil {
		errorResponse(w, http.StatusNotFound, "plan run not found")
		return
	}
	if hostOwnerID != ownerID {
		errorResponse(w, http.StatusForbidden, "host only")
		return
	}

	var preStatus, preOrigin string
	var memberOwnerID, inviteEmail *string
	err := h.db.QueryRow(ctx, `
		WITH old AS (
			SELECT status::text, origin::text, member_owner_id, invite_email
			FROM run_invites WHERE id = $1::uuid AND run_id = $2::uuid FOR UPDATE
		)
		UPDATE run_invites SET status = 'declined', responded_at = NOW(), updated_at = NOW()
		FROM old
		WHERE run_invites.id = $1::uuid AND run_invites.status <> 'declined'
		RETURNING old.status, old.origin, old.member_owner_id, old.invite_email
	`, inviteID, runID).Scan(&preStatus, &preOrigin, &memberOwnerID, &inviteEmail)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			errorResponse(w, http.StatusInternalServerError, "decline failed")
			return
		}
		// No flip: either the row never existed, or it lost the race above
		// (already 'declined' — by this same caller's retry, a concurrent
		// decline, or a prior call). Distinguish the same way LeaveRun does:
		// existence (any status) decides 404 vs. the idempotent 200 every
		// repeat/losing decline call gets.
		var exists bool
		h.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM run_invites WHERE id = $1::uuid AND run_id = $2::uuid)`, inviteID, runID).Scan(&exists)
		if !exists {
			errorResponse(w, http.StatusNotFound, "crew request not found")
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"status": "declined"})
		return
	}

	isUninvite := preStatus == "accepted" || (preOrigin == "invite" && preStatus == "invited")
	if isUninvite {
		if notifyEmail, ok := resolveNotifyEmail(ctx, h.db, memberOwnerID, inviteEmail); ok {
			// ATTENDEE;CN= resolution (display-names feature, mig 000130):
			// display_name when known > handle > the invite's captured
			// invite_handle snapshot > '' — same COALESCE chain as
			// loadRunNotifyRecipients (notifications.go), which this
			// mirrors for the single-recipient uninvite case.
			var handle string
			h.db.QueryRow(ctx, `
				SELECT COALESCE(up.display_name, up.handle, ri.invite_handle, '')
				FROM run_invites ri
				LEFT JOIN user_profiles up ON up.owner_id = ri.member_owner_id
				WHERE ri.id = $1::uuid
			`, inviteID).Scan(&handle)
			// ics_sequence bumps ONLY when a CANCEL actually goes out (item
			// 5) — an unresolvable address (R2 gap) means notifyUninvited
			// has nothing to send, so there's no scheduling message whose
			// SEQUENCE needs to advance.
			if _, serr := h.db.Exec(ctx, `UPDATE calendar_runs SET ics_sequence = ics_sequence + 1 WHERE id = $1::uuid`, runID); serr != nil {
				log.Printf("RunCrewDecline: bump ics_sequence failed run=%s: %v", runID, serr)
			} else {
				rcpt := notifyRecipient{InviteID: inviteID, MemberOwnerID: memberOwnerID, DisplayName: handle, Email: notifyEmail}
				go notifyUninvited(h.mailer, h.db, runID, rcpt)
			}
		}
	}
	jsonResponse(w, http.StatusOK, map[string]string{"status": "declined"})
}
