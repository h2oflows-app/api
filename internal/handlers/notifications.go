package handlers

// notifications.go: async email senders for API-1 Invite Sync
// (web/design_handoff_calendar_explore/INVITE_SYNC_PLAN.md) — the
// Outlook-parity fan-out that keeps external calendars in sync with an
// organizer's edits/cancellations and keeps the organizer informed of
// attendee responses. Mirrors invites.go's sendRunInviteMail pattern
// exactly: every notify* function here is package-level (not a method), so
// it can be fired with `go` from a request handler and run with its own
// context.Background()+15s timeout after the triggering HTTP request's
// context has already died at response — and on any failure it only
// log.Printf's, never propagates an error anywhere, because async mail must
// never block or fail the request path it's attached to.
//
// API-1 wires exactly ONE of these (notifyOrganizerAccepted, into
// AcceptInvite — invites.go) since AcceptInvite is already being touched
// for user_emails capture. The rest (notifyRunMaterialChange,
// notifyRunCancelled, notifyOrganizerDeclined, notifyUninvited) exist here,
// fully built and tested via their shared plumbing, for API-2 to call
// directly from UpdateRun/DeleteRun/the new leave endpoint/RunCrewDecline —
// see each function's doc comment for exactly which caller and when, per
// the plan's semantics map.

import (
	"context"
	"fmt"
	"html"
	"log"
	"strings"
	"time"

	"github.com/h2oflow/h2oflow/apps/api/internal/ics"
	"github.com/h2oflow/h2oflow/apps/api/internal/mail"
	"github.com/jackc/pgx/v5/pgxpool"
)

// organizerName/organizerEmail are the ICS ORGANIZER identity for every
// REQUEST/CANCEL this package emits (invites.go's initial-invite path
// included) — parsed once from config.MailFrom at startup via SetMailFrom
// (mirrors invites.go's webBaseURL/SetWebBaseURL) rather than re-parsed per
// send. organizerEmail MUST equal the mailer's configured From address
// (mail.ResendMailer's `from`, same MAIL_FROM value) — Outlook in
// particular silently drops a scheduling update whose ORGANIZER doesn't
// match the message's From header (see ics.RunInviteInput.OrganizerEmail's
// doc comment). Empty until SetMailFrom runs (or if MAIL_FROM is unset —
// local dev without it configured); ics.BuildRunInvite then errors on the
// empty OrganizerEmail (ErrMissingOrganizer) and every .ics-attaching
// sender degrades to logging that failure and sending the plain-text/HTML
// email body without an attachment, never blocking the send entirely.
var (
	organizerName  string
	organizerEmail string
)

// SetMailFrom wires config.MailFrom (the same RFC 5322 string passed to
// mail.NewResendMailer as `from`) at startup, parsing it into the
// ORGANIZER;CN=<name>:mailto:<email> identity every invite/notification
// .ics needs. Call once from main.go, alongside SetWebBaseURL.
func SetMailFrom(from string) {
	organizerName, organizerEmail = mail.ParseFromAddress(from)
}

// runNotifyInfo is the run projection every async notification sender
// needs — same fields as invites.go's invitedRunInfo, plus HostOwnerID
// (needed to resolve the organizer's own notify email) and ICSSequence
// (needed for every REQUEST/CANCEL this file builds). Loaded via
// loadRunForNotify, which — UNLIKE invites.go's loadInvitedRunInfo —
// applies no owner-match gate and no `deleted_at IS NULL` filter: these
// senders run from a detached goroutine on behalf of the SYSTEM (the
// action that triggered them was already authorized by the request handler
// that fired them), and notifyRunCancelled specifically needs to still
// resolve a run's data AFTER DeleteRun's tombstoning UPDATE has set
// deleted_at (a soft delete leaves the row, and every FK'd join, fully
// readable — only *live-facing* queries filter it out).
type runNotifyInfo struct {
	ID          string
	Name        string
	RiverName   *string
	RunDate     string
	RunTime     string // "" for an untimed run
	MeetupSpot  string // "" when unset
	GaugeCFS    *float64
	FlowBand    *string
	HostOwnerID string
	HostHandle  string
	// HostDisplayName (display-names feature, mig 000130): nil when the host
	// has no user_profiles row or hasn't typed a display_name in — feeds
	// displayHost/displayLabel below, additive alongside HostHandle (never a
	// replacement).
	HostDisplayName *string
	ICSSequence     int
}

// loadRunForNotify fetches runID's notification-relevant fields. See
// runNotifyInfo's doc comment for why this has no owner/deleted_at gate,
// unlike invites.go's loadInvitedRunInfo.
func loadRunForNotify(ctx context.Context, db *pgxpool.Pool, runID string) (runNotifyInfo, error) {
	var ri runNotifyInfo
	err := db.QueryRow(ctx, `
		SELECT cr.id, cr.name, ur.river_name, cr.run_date::text,
		       COALESCE(cr.run_time::text, ''), COALESCE(cr.meetup_spot, ''),
		       cr.gauge_cfs, cr.flow_band, cr.owner_id, COALESCE(up.handle, ''), up.display_name,
		       cr.ics_sequence
		FROM calendar_runs cr
		LEFT JOIN user_reaches ur ON ur.id = cr.user_reach_id
		LEFT JOIN user_profiles up ON up.owner_id = cr.owner_id
		WHERE cr.id = $1::uuid
	`, runID).Scan(&ri.ID, &ri.Name, &ri.RiverName, &ri.RunDate,
		&ri.RunTime, &ri.MeetupSpot, &ri.GaugeCFS, &ri.FlowBand, &ri.HostOwnerID, &ri.HostHandle, &ri.HostDisplayName,
		&ri.ICSSequence)
	if err != nil {
		return runNotifyInfo{}, err
	}
	return ri, nil
}

// notifyRecipient is one row of the run-scoped recipient fan-out (see
// loadRunNotifyRecipients) — a currently-live invitee with a resolvable
// email. DisplayName feeds ATTENDEE;CN=; empty is fine (attendeeLine omits
// CN entirely — see internal/ics). Display-names feature (mig 000130):
// resolution order is user_profiles.display_name (when known) > .handle >
// the invite's captured invite_handle snapshot > "" — see
// loadRunNotifyRecipients' COALESCE chain.
type notifyRecipient struct {
	InviteID      string
	MemberOwnerID *string
	DisplayName   string
	Email         string
	// Status is this row's CURRENT run_invites.status ('invited' or
	// 'accepted' — loadRunNotifyRecipients' WHERE already excludes anything
	// else) — threaded into ics.RunInviteInput.AttendeePartStat by
	// buildAndSendRunICSMail so a material-change REQUEST reflects the
	// recipient's real RSVP state instead of resetting it (see
	// buildAndSendRunICSMail's doc comment).
	Status string
}

// loadRunNotifyRecipients is the run-scoped recipient fan-out query from
// the plan (§API): every currently-live invitee — accepted crew AND
// still-pending, non-dismissed invites — with a resolvable email. Powers
// notifyRunMaterialChange and notifyRunCancelled, which both fan out to
// "everyone who currently has (or is waiting on) this run." Email
// resolution is COALESCE(user_emails[member_owner_id], invite_email) (R1:
// resolveNotifyEmail's per-row logic, done here in SQL for the whole set at
// once) — a row where that COALESCE is NULL (a handle invitee who's never
// made an authed write since API-1 shipped, R2) is filtered out in SQL, not
// after scanning, so the Go side never has to branch on an empty email.
func loadRunNotifyRecipients(ctx context.Context, db *pgxpool.Pool, runID string) ([]notifyRecipient, error) {
	rows, err := db.Query(ctx, `
		SELECT ri.id, ri.member_owner_id, COALESCE(up.display_name, up.handle, ri.invite_handle, ''),
		       COALESCE(ue.email, ri.invite_email) AS notify_email, ri.status::text
		FROM run_invites ri
		LEFT JOIN user_profiles up ON up.owner_id = ri.member_owner_id
		LEFT JOIN user_emails ue ON ue.owner_id = ri.member_owner_id
		WHERE ri.run_id = $1::uuid AND ri.status IN ('invited', 'accepted') AND ri.dismissed_at IS NULL
		  AND COALESCE(ue.email, ri.invite_email) IS NOT NULL
	`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []notifyRecipient
	for rows.Next() {
		var rcpt notifyRecipient
		if err := rows.Scan(&rcpt.InviteID, &rcpt.MemberOwnerID, &rcpt.DisplayName, &rcpt.Email, &rcpt.Status); err != nil {
			return nil, err
		}
		out = append(out, rcpt)
	}
	return out, rows.Err()
}

// resolveNotifyEmail is the single-row form of loadRunNotifyRecipients'
// COALESCE(user_emails[member_owner_id], invite_email) resolution (plan
// §API) — for a caller that already has ONE run_invites row in hand (from
// its own SELECT, e.g. RunCrewDecline/the leave endpoint, both API-2) and
// just needs that row's resolvable email, not a full run-scoped fan-out
// query. ok=false means no resolvable address (R2 gap, or a handle invite
// with neither a captured email nor an invite_email) — skip, in-app only.
func resolveNotifyEmail(ctx context.Context, db *pgxpool.Pool, memberOwnerID, inviteEmail *string) (string, bool) {
	if memberOwnerID != nil {
		var email string
		if err := db.QueryRow(ctx, `SELECT email FROM user_emails WHERE owner_id = $1`, *memberOwnerID).Scan(&email); err == nil && email != "" {
			return email, true
		}
	}
	if inviteEmail != nil && strings.TrimSpace(*inviteEmail) != "" {
		return *inviteEmail, true
	}
	return "", false
}

// resolveOrganizerNotifyEmail looks up hostOwnerID's stored email
// (user_emails) — the host/organizer counterpart to loadRunNotifyRecipients'
// per-attendee resolution (an organizer is never an invite-email row, only
// ever a user_emails one). Returns ("", false) when absent (R2: no
// captured email yet), the signal notifyOrganizerAccepted/
// notifyOrganizerDeclined use to no-op silently per this PR's item 6
// ("skip silently if absent").
func resolveOrganizerNotifyEmail(ctx context.Context, db *pgxpool.Pool, hostOwnerID string) (string, bool) {
	var email string
	if err := db.QueryRow(ctx, `SELECT email FROM user_emails WHERE owner_id = $1`, hostOwnerID).Scan(&email); err != nil || email == "" {
		return "", false
	}
	return email, true
}

// runDetailLines renders the shared <li>/plain-text detail block (river,
// date/time, meetup, flow) every run-related email body uses — factored out
// of invites.go's buildRunInviteEmailBody so this file's senders reuse it
// instead of duplicating it (takes the primitive fields rather than either
// invitedRunInfo or runNotifyInfo, since those two types otherwise have no
// relationship to each other).
func runDetailLines(riverName *string, runDate, runTime, meetupSpot string, gaugeCFS *float64, flowBand *string) (htmlDetails, textDetails string) {
	var hb, tb strings.Builder
	if riverName != nil && strings.TrimSpace(*riverName) != "" {
		hb.WriteString(fmt.Sprintf("<li>River: %s</li>", html.EscapeString(*riverName)))
		tb.WriteString(fmt.Sprintf("River: %s\n", *riverName))
	}
	dateLine := formatUSDate(runDate)
	if t := formatUSTime(runTime); t != "" {
		dateLine += " at " + t
	}
	hb.WriteString(fmt.Sprintf("<li>Date: %s</li>", html.EscapeString(dateLine)))
	tb.WriteString(fmt.Sprintf("Date: %s\n", dateLine))
	if meetupSpot != "" {
		hb.WriteString(fmt.Sprintf("<li>Meet at: %s</li>", html.EscapeString(meetupSpot)))
		tb.WriteString(fmt.Sprintf("Meet at: %s\n", meetupSpot))
	}
	if gaugeCFS != nil {
		flowLine := fmt.Sprintf("%.0f CFS", *gaugeCFS)
		if flowBand != nil && *flowBand != "" {
			flowLine += " (" + *flowBand + ")"
		}
		hb.WriteString(fmt.Sprintf("<li>Flow: %s</li>", html.EscapeString(flowLine)))
		tb.WriteString(fmt.Sprintf("Flow: %s\n", flowLine))
	}
	return hb.String(), tb.String()
}

// rsvpButtonHTML renders OUR Accept/View-run link as the visually primary
// element of a METHOD:REQUEST-bearing email (API-2, INVITE_SYNC_PLAN.md
// Amendments — "RSVP copy-steer"). Every REQUEST .ics carries ORGANIZER +
// ATTENDEE lines (RFC 5546), which makes mail clients render their OWN
// native Accept/Decline UI — a one-way iTIP REPLY nothing on our side reads
// yet (inbound RSVP processing is a FUTURE, post-pilot wave). That native
// UI would otherwise out-compete a plain text link, so this renders a
// button-styled anchor (inline styles only — email clients strip external
// CSS/JS) instead of buildRunInviteEmailBody/notifyRunMaterialChange's
// previous bare `<a>`. Used by both initial-invite paths (handle and email —
// they share buildRunInviteEmailBody, invites.go) and
// notifyRunMaterialChange below; CANCEL emails (notifyRunCancelled,
// notifyUninvited) don't call this — the Amendments explicitly exempt them.
func rsvpButtonHTML(url, label string) string {
	return fmt.Sprintf(
		`<a href="%s" style="display:inline-block;padding:12px 24px;background:#0ea5e9;color:#ffffff;text-decoration:none;border-radius:6px;font-weight:bold;font-size:16px;">%s</a>`,
		url, html.EscapeString(label),
	)
}

// rsvpSteerLine wraps the Amendments' mandated copy in a de-emphasized
// (small, gray) paragraph placed right under rsvpButtonHTML's button —
// de-emphasized because the button above it, not this sentence, is the
// primary call to action; the sentence only exists to explain why tapping
// the mail client's own native RSVP UI wouldn't do what the reader expects.
func rsvpSteerLine(sentence string) string {
	return fmt.Sprintf(`<p style="color:#666666;font-size:13px;">%s</p>`, html.EscapeString(sentence))
}

// runInvitesStatusToPartStat maps a run_invites.status value to the ICS
// PARTSTAT it should carry on a REQUEST/CANCEL .ics — 'accepted' ->
// ics.RunInviteInput.AttendeePartStat "ACCEPTED" (so the update preserves
// their RSVP instead of resetting it to needs-action, see
// buildAndSendRunICSMail's doc comment), 'invited' (and anything else —
// defensive default, matching ics.normalizePartStat's own fallback) ->
// "NEEDS-ACTION". Applied unconditionally regardless of method: for a
// CANCEL (notifyRunCancelled) the PARTSTAT is immaterial since STATUS:
// CANCELLED already tells the client the event is gone, so mapping it the
// same way here is harmless and keeps this one small helper method-agnostic
// rather than special-casing CANCEL.
func runInvitesStatusToPartStat(status string) string {
	if status == "accepted" {
		return "ACCEPTED"
	}
	return "NEEDS-ACTION"
}

// buildAndSendRunICSMail builds one email (with a per-recipient .ics
// attachment) and sends it — the shared plumbing behind every sender in
// this file that emits a REQUEST/CANCEL (notifyRunMaterialChange,
// notifyRunCancelled, notifyUninvited). ATTENDEE is per-recipient (RFC
// 5546 convention, see internal/ics's package doc) — this renders and sends
// exactly one .ics for exactly one recipient; a fan-out to N recipients
// calls this N times. ctx is the CALLER's own context.Background()+timeout
// (this function doesn't create one) so a fan-out to N recipients pays the
// 15s budget once, not N times. A failed ics.BuildRunInvite (e.g.
// organizerEmail unset — see the package var's doc comment) logs and sends
// the email WITHOUT the attachment rather than not sending at all — the
// in-app state change is still real even if the external-calendar sync
// isn't available.
//
// AttendeePartStat (runInvitesStatusToPartStat(rcpt.Status)) is the fix for
// an already-accepted crew member getting reset to "needs response" by a
// material-change update REQUEST: previously every ATTENDEE line
// hardcoded PARTSTAT=NEEDS-ACTION;RSVP=TRUE regardless of the recipient's
// actual run_invites.status, so Outlook/Gmail would re-prompt someone who'd
// already accepted every time the organizer edited the date/time/meetup/
// name. rcpt.Status is only populated by loadRunNotifyRecipients (notifyUninvited's
// caller-supplied rcpt leaves it "" -> NEEDS-ACTION default, which is
// correct there too — an uninvite CANCEL's PARTSTAT is immaterial, same
// reasoning as notifyRunCancelled above).
func buildAndSendRunICSMail(ctx context.Context, mailer mail.Mailer, run runNotifyInfo, rcpt notifyRecipient, method, subject, htmlBody, textBody string) {
	icsBody, err := ics.BuildRunInvite(ics.RunInviteInput{
		RunID:            run.ID,
		Method:           method,
		Sequence:         run.ICSSequence,
		Cancelled:        method == ics.MethodCancel,
		OrganizerName:    organizerName,
		OrganizerEmail:   organizerEmail,
		AttendeeName:     rcpt.DisplayName,
		AttendeeEmail:    rcpt.Email,
		AttendeePartStat: runInvitesStatusToPartStat(rcpt.Status),
		Name:             run.Name,
		RunDate:          run.RunDate,
		RunTime:          run.RunTime,
		MeetupSpot:       run.MeetupSpot,
		URL:              fmt.Sprintf("%s/plan-runs/%s", webBaseURL, run.ID),
	})

	msg := mail.Message{To: rcpt.Email, Subject: subject, HTML: htmlBody, Text: textBody}
	if err != nil {
		log.Printf("notifications: build ics failed (to=%s run=%s method=%s): %v", rcpt.Email, run.ID, method, err)
	} else {
		msg.Attachments = append(msg.Attachments, mail.Attachment{
			Filename:    "invite.ics",
			ContentType: fmt.Sprintf("text/calendar; charset=utf-8; method=%s", method),
			Content:     []byte(icsBody),
		})
	}

	if err := mailer.Send(ctx, msg); err != nil {
		log.Printf("notifications: send failed (to=%s run=%s method=%s): %v", rcpt.Email, run.ID, method, err)
	}
}

// notifyRunMaterialChange fans a REQUEST .ics (same UID, bumped SEQUENCE)
// out to every live invitee (accepted crew + pending invites,
// loadRunNotifyRecipients) of runID. API-2's UpdateRun calls this after it
// detects a material-field change (run_date/run_time/meetup_spot/name —
// see the plan's diff-gating, D3) and PERSISTS the bumped
// calendar_runs.ics_sequence — this function reads it fresh via
// loadRunForNotify rather than taking a pre-loaded run, so callers only
// ever pass a run_id (matching every other sender here).
func notifyRunMaterialChange(mailer mail.Mailer, db *pgxpool.Pool, runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	run, err := loadRunForNotify(ctx, db, runID)
	if err != nil {
		log.Printf("notifyRunMaterialChange: load run %s failed: %v", runID, err)
		return
	}
	recipients, err := loadRunNotifyRecipients(ctx, db, runID)
	if err != nil {
		log.Printf("notifyRunMaterialChange: load recipients run=%s failed: %v", runID, err)
		return
	}

	host := displayHost(run.HostDisplayName, run.HostHandle)
	subject := fmt.Sprintf("Trip updated: %s", run.Name)
	runURL := fmt.Sprintf("%s/plan-runs/%s", webBaseURL, run.ID)
	// leaveURL carries ?leave=1 (web's plan-runs/[id].vue, mirroring
	// sendRunInviteMail's ?accept=1 convention) for the "Not going" button
	// below — API-1 (Part A, this same change): now that an already-
	// accepted recipient's ATTENDEE PARTSTAT is preserved (RSVP=FALSE) on
	// this REQUEST instead of always resetting to NEEDS-ACTION;RSVP=TRUE,
	// their mail client no longer shows its own native Decline UI on this
	// update, so this is the one remaining way to bail from the email
	// itself rather than opening the app and hunting for the leave control.
	leaveURL := runURL + "?leave=1"
	htmlDetails, textDetails := runDetailLines(run.RiverName, run.RunDate, run.RunTime, run.MeetupSpot, run.GaugeCFS, run.FlowBand)

	// API-2 RSVP copy-steer (Amendments), revised for Part A/B (PARTSTAT
	// preservation + the Not-going button). loadRunNotifyRecipients returns
	// BOTH accepted crew and still-pending invitees, and the steer line has
	// to be honest to each — so the body is built PER RECIPIENT here rather
	// than once. An accepted recipient's calendar already reflects the update
	// in place (Part A: PARTSTAT=ACCEPTED;RSVP=FALSE, no re-prompt), so there
	// is genuinely nothing for them to do unless they now can't make it. A
	// still-pending invitee's .ics still carries NEEDS-ACTION;RSVP=TRUE (their
	// mail client is still asking them to RSVP), so telling them "you're on
	// the crew, nothing to do" would be false and contradict their own
	// invite — they get an actionable line instead.
	for _, rcpt := range recipients {
		steer := "Heads up — the details changed. Tap View run to see them and RSVP. Can't make it? Tap Not going."
		if rcpt.Status == "accepted" {
			steer = "You're on the crew — the new details are already on your calendar, nothing to do. Can't make it? Tap Not going."
		}
		htmlBody := fmt.Sprintf(
			`<p>%s updated <strong>%s</strong> on H2OFlows.</p><ul>%s</ul><p>%s %s</p>%s`,
			html.EscapeString(host), html.EscapeString(run.Name), htmlDetails,
			rsvpButtonHTML(runURL, "View run"), rsvpButtonHTML(leaveURL, "Not going"), rsvpSteerLine(steer),
		)
		textBody := fmt.Sprintf(
			"%s updated %s.\n%s\nView run: %s\nCan't make it? Cancel your RSVP: %s\n\n%s\n",
			host, run.Name, textDetails, runURL, leaveURL, steer,
		)
		buildAndSendRunICSMail(ctx, mailer, run, rcpt, ics.MethodRequest, subject, htmlBody, textBody)
	}
}

// notifyRunCancelled fans a CANCEL .ics out to every live invitee of runID
// (loadRunNotifyRecipients — the run_invites rows are untouched by
// DeleteRun, only calendar_runs is tombstoned). API-2's DeleteRun calls
// this after it soft-deletes the run (SEQUENCE bumped +1 by the caller
// first, same convention as notifyRunMaterialChange) — loadRunForNotify
// has no deleted_at filter (see runNotifyInfo's doc comment), so this still
// resolves the run's data post-tombstone.
func notifyRunCancelled(mailer mail.Mailer, db *pgxpool.Pool, runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	run, err := loadRunForNotify(ctx, db, runID)
	if err != nil {
		log.Printf("notifyRunCancelled: load run %s failed: %v", runID, err)
		return
	}
	recipients, err := loadRunNotifyRecipients(ctx, db, runID)
	if err != nil {
		log.Printf("notifyRunCancelled: load recipients run=%s failed: %v", runID, err)
		return
	}

	host := displayHost(run.HostDisplayName, run.HostHandle)
	subject := fmt.Sprintf("Trip cancelled: %s", run.Name)
	htmlBody := fmt.Sprintf(`<p>%s cancelled <strong>%s</strong> on H2OFlows.</p>`, html.EscapeString(host), html.EscapeString(run.Name))
	textBody := fmt.Sprintf("%s cancelled %s.\n", host, run.Name)

	for _, rcpt := range recipients {
		buildAndSendRunICSMail(ctx, mailer, run, rcpt, ics.MethodCancel, subject, htmlBody, textBody)
	}
}

// notifyUninvited emails + CANCELs (single-recipient .ics, SEQUENCE bumped
// by the caller same as the two senders above) the ONE person an organizer
// just uninvited. API-2's RunCrewDecline calls this when the declined row
// had already been invited or accepted (a real uninvite) — never for a
// fresh join-request decline, which stays silent per D5 ("Host declines a
// join request | — | — | nobody (v1)"). rcpt is passed in rather than
// re-resolved here: RunCrewDecline already has the row (id/
// member_owner_id/handle/invite_email) from its own SELECT, and
// resolveNotifyEmail is the right tool to turn that into a notifyRecipient
// without a redundant round trip.
func notifyUninvited(mailer mail.Mailer, db *pgxpool.Pool, runID string, rcpt notifyRecipient) {
	if rcpt.Email == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	run, err := loadRunForNotify(ctx, db, runID)
	if err != nil {
		log.Printf("notifyUninvited: load run %s failed: %v", runID, err)
		return
	}

	subject := fmt.Sprintf("You were removed from %s", run.Name)
	runURL := fmt.Sprintf("%s/plan-runs/%s", webBaseURL, run.ID)
	htmlBody := fmt.Sprintf(`<p>You were removed from <strong>%s</strong> on H2OFlows.</p><p><a href="%s">View run</a></p>`,
		html.EscapeString(run.Name), runURL)
	textBody := fmt.Sprintf("You were removed from %s.\nView run: %s\n", run.Name, runURL)

	buildAndSendRunICSMail(ctx, mailer, run, rcpt, ics.MethodCancel, subject, htmlBody, textBody)
}

// notifyOrganizerDeclined emails the run's host/organizer that
// declinerHandle declined their invite or removed themselves from runID's
// crew. API-2's new POST /plan-runs/{id}/leave endpoint calls this (D5:
// "notify organizer on accept AND on decline/leave"). No .ics attached —
// organizer-facing response notifications carry no calendar payload (see
// the plan's semantics map: the Accept/decline-to-organizer rows have no
// METHOD). Silently no-ops if the host has no user_emails row yet (R2) —
// resolveOrganizerNotifyEmail's ok=false. declinerDisplayName (display-names
// feature, mig 000130) is nil-safe (displayHost) and additive — the caller
// resolves it alongside declinerHandle from the same user_profiles row.
func notifyOrganizerDeclined(mailer mail.Mailer, db *pgxpool.Pool, runID, declinerHandle string, declinerDisplayName *string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	run, err := loadRunForNotify(ctx, db, runID)
	if err != nil {
		log.Printf("notifyOrganizerDeclined: load run %s failed: %v", runID, err)
		return
	}
	email, ok := resolveOrganizerNotifyEmail(ctx, db, run.HostOwnerID)
	if !ok {
		return
	}

	who := displayHost(declinerDisplayName, declinerHandle)
	subject := fmt.Sprintf("%s can't make %s — invite someone else", who, run.Name)
	runURL := fmt.Sprintf("%s/plan-runs/%s", webBaseURL, run.ID)
	htmlBody := fmt.Sprintf(`<p>%s declined %s. Invite someone else to fill the spot.</p><p><a href="%s">Manage crew</a></p>`,
		html.EscapeString(who), html.EscapeString(run.Name), runURL)
	textBody := fmt.Sprintf("%s declined %s. Invite someone else to fill the spot.\nManage crew: %s\n", who, run.Name, runURL)

	if err := mailer.Send(ctx, mail.Message{To: email, Subject: subject, HTML: htmlBody, Text: textBody}); err != nil {
		log.Printf("notifyOrganizerDeclined send failed (to=%s run=%s): %v", email, runID, err)
	}
}

// notifyOrganizerAccepted emails the run's host/organizer that
// accepterHandle accepted their invite to runID. Wired into
// InviteHandler.AcceptInvite directly in THIS PR (API-1) — see item 6 of
// the API-1 scope — since AcceptInvite is already being touched for
// user_emails capture (the two land in the same commit). No .ics attached
// (see notifyOrganizerDeclined's doc comment — same reasoning). Silently
// no-ops if the host has no user_emails row yet (R2). accepterDisplayName
// (display-names feature, mig 000130) is nil-safe (displayHost) and
// additive — the caller resolves it alongside accepterHandle from the same
// user_profiles row.
func notifyOrganizerAccepted(mailer mail.Mailer, db *pgxpool.Pool, runID, accepterHandle string, accepterDisplayName *string) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	run, err := loadRunForNotify(ctx, db, runID)
	if err != nil {
		log.Printf("notifyOrganizerAccepted: load run %s failed: %v", runID, err)
		return
	}
	email, ok := resolveOrganizerNotifyEmail(ctx, db, run.HostOwnerID)
	if !ok {
		return
	}

	who := displayHost(accepterDisplayName, accepterHandle)
	subject := fmt.Sprintf("%s accepted your invite to %s", who, run.Name)
	runURL := fmt.Sprintf("%s/plan-runs/%s", webBaseURL, run.ID)
	htmlBody := fmt.Sprintf(`<p>%s accepted your invite to <strong>%s</strong>.</p><p><a href="%s">View run</a></p>`,
		html.EscapeString(who), html.EscapeString(run.Name), runURL)
	textBody := fmt.Sprintf("%s accepted your invite to %s.\nView run: %s\n", who, run.Name, runURL)

	if err := mailer.Send(ctx, mail.Message{To: email, Subject: subject, HTML: htmlBody, Text: textBody}); err != nil {
		log.Printf("notifyOrganizerAccepted send failed (to=%s run=%s): %v", email, runID, err)
	}
}

// displayLabel renders a known handle for email subjects/bodies (display-
// names feature, mig 000130): "Name (@handle)" when displayName is set and
// non-blank, otherwise plain "@handle". handle must be non-empty — this
// does NOT handle the "no handle at all" case (a profile-less host/decliner,
// rare); callers with that possibility wrap it in their own fallback (see
// displayHost below) rather than baking one generic fallback string in here,
// since different call sites want different copy for it ("A paddler" vs.
// invites.go's "a paddler" mid-sentence).
//
// Shared between this file (displayHost, four call sites) and invites.go
// (runInviteSubject/buildRunInviteEmailBody/sendRunInviteMail — the initial-
// invite path), so every place "@handle" appears in email copy renders the
// same way. Distinct from the ICS ATTENDEE;CN= resolution
// (loadRunNotifyRecipients/notifyRecipient.DisplayName, RunCrewDecline's
// rcpt build): those want a single best-available STRING, falling back
// through display_name, then handle, then invite_handle, then an empty
// string (COALESCE) — not this paired "Name (@handle)" rendering. Email
// subject/body copy always names the @handle explicitly even when a
// display_name is also shown, per the product contract ("@handle stays the
// primary identifier... everywhere").
func displayLabel(displayName *string, handle string) string {
	if displayName != nil {
		if dn := strings.TrimSpace(*displayName); dn != "" {
			return fmt.Sprintf("%s (@%s)", dn, handle)
		}
	}
	return "@" + handle
}

// displayHost renders a host/decliner/accepter for email copy via
// displayLabel, or a generic fallback when handle itself is empty (mirrors
// invites.go's runInviteSubject/sendRunInviteMail inline "a paddler"
// fallback, generalized into one helper since this file needs the same
// shape in four places for two different roles — host and
// decliner/accepter). displayName is nil-safe (see displayLabel).
func displayHost(displayName *string, handle string) string {
	if handle == "" {
		return "A paddler"
	}
	return displayLabel(displayName, handle)
}
