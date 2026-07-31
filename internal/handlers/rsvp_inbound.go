// rsvp_inbound.go: POST /api/v1/hooks/rsvp-inbound — the inbound half of
// Invite Sync (project_invite_sync.md's "future wave", started once API-1/
// API-2 landed). sendRunInviteMail/notifyRunMaterialChange (invites.go/
// notifications.go) already send RFC 5546 METHOD:REQUEST invites with a
// fixed UID ({run_id}@h2oflows.app) and an ORGANIZER = MAIL_FROM = From
// header; when a recipient clicks Accept/Decline in Outlook/Gmail/Apple
// Mail, their client emails a METHOD:REPLY back to that ORGANIZER address.
// A Cloudflare Email Worker (infra/cloudflare/rsvp-inbound-worker.js)
// receives mail addressed to invites@h2oflows.app and POSTs the raw RFC 822
// message here — this file parses that message and applies the RSVP.
//
// Contract (deliberately asymmetric — see each piece below):
//   - Auth: a single shared secret (X-RSVP-Secret, RSVP_INBOUND_SECRET),
//     compared constant-time. This only proves the Worker relayed SOME mail
//     addressed to invites@h2oflows.app — anyone on the internet can send
//     mail to that address, so it proves nothing about WHO sent it.
//   - Identity: proven instead by requiring the message's own From address
//     to equal the REPLY's ATTENDEE mailto: address (mail clients RSVP from
//     the attendee's own mailbox) — see Inbound's anti-spoof compare.
//   - Failure mode: almost everything this endpoint doesn't understand or
//     doesn't act on (wrong METHOD, no calendar part, unknown UID, spoofed
//     From, an already-applied PARTSTAT, a paddled/deleted run, ...) is
//     reported as 200 {"status":"ignored"}, NOT a 4xx/5xx — Cloudflare
//     Workers has no useful retry story for an Email Worker, and a non-2xx
//     response has no sender to bounce back to. Only genuine
//     request-shaped failures (bad secret, oversized body) get a non-200
//     status; a wrong HTTP method never reaches this handler at all (chi
//     only routes POST here).
package handlers

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	netmail "net/mail"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/h2oflow/h2oflow/apps/api/internal/mail"
)

// rsvpInboundMaxBytes caps the request body via http.MaxBytesReader (413
// over) — a real .ics-bearing REPLY is a few KB; 5 MB only needs to be
// generous enough for a REPLY with an oversized quoted original-message
// body some clients attach, not for arbitrary uploads.
const rsvpInboundMaxBytes = 5 << 20 // 5 MB

// RSVPInboundHandler implements POST /api/v1/hooks/rsvp-inbound. Public
// route (cmd/server/main.go registers it outside every auth.Required/
// RequireAdmin group) — see this file's package doc comment for why a
// Supabase JWT has no meaning here and what gates the endpoint instead.
type RSVPInboundHandler struct {
	db     *pgxpool.Pool
	mailer mail.Mailer
	// secret is config.RSVPInboundSecret, read once at startup (main.go).
	// "" means the endpoint must refuse every request (Inbound checks this
	// before anything else) — see NewRSVPInboundHandler's doc comment for
	// why this is deliberately NOT re-read per request.
	secret string
}

// NewRSVPInboundHandler constructs the handler. secret == "" is a valid,
// expected local-dev/CI state (RSVP_INBOUND_SECRET unset) — main.go logs
// "rsvp-inbound disabled: RSVP_INBOUND_SECRET unset" once at startup in
// that case (mirroring every other optional-integration log line there,
// e.g. RESEND_API_KEY/ANTHROPIC_API_KEY unset); Inbound itself stays
// silent on the per-request 503 path so a stray prober/retry storm can't
// fill the log.
func NewRSVPInboundHandler(db *pgxpool.Pool, mailer mail.Mailer, secret string) *RSVPInboundHandler {
	if mailer == nil {
		mailer = mail.NoopMailer{}
	}
	return &RSVPInboundHandler{db: db, mailer: mailer, secret: secret}
}

// Inbound is the HTTP entry point. Every early-return before the DB work
// follows the "only request-shaped failures get non-200" rule from the
// package doc comment: disabled/bad-secret -> 503/401, oversized body ->
// 413. Everything past that point — a message this parser can't or won't
// act on, or even a DB error while applying a valid RSVP — degrades to 200
// {"status":"ignored"} (logged, but not surfaced to the caller): a
// Cloudflare Email Worker relaying mail has no sender to bounce a non-2xx
// back to, and retrying delivery of the same REPLY email would just repeat
// whatever failed.
func (h *RSVPInboundHandler) Inbound(w http.ResponseWriter, r *http.Request) {
	if h.secret == "" {
		errorResponse(w, http.StatusServiceUnavailable, "rsvp inbound webhook is disabled")
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-RSVP-Secret")), []byte(h.secret)) != 1 {
		errorResponse(w, http.StatusUnauthorized, "invalid secret")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, rsvpInboundMaxBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var mbErr *http.MaxBytesError
		if errors.As(err, &mbErr) {
			errorResponse(w, http.StatusRequestEntityTooLarge, "message too large")
			return
		}
		errorResponse(w, http.StatusBadRequest, "failed to read body")
		return
	}

	// Informational only (logging) — the envelope-from is NOT trusted for
	// the anti-spoof check below (that reads the parsed message's own From
	// header, which is what a mail client actually stamps with the
	// attendee's address; the SMTP envelope-from can differ, e.g. via
	// forwarding).
	envelopeFrom := r.Header.Get("X-Envelope-From")

	parsed, ignoreReason, perr := parseRSVPReply(raw)
	if perr != nil {
		log.Printf("rsvp-inbound: parse error envelope-from=%s: %v", redactEmail(envelopeFrom), perr)
		jsonResponse(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	if ignoreReason != "" {
		log.Printf("rsvp-inbound: ignored (%s) envelope-from=%s", ignoreReason, redactEmail(envelopeFrom))
		jsonResponse(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	// Anti-spoof (see package doc comment): the shared secret only proves
	// the Worker relayed SOME mail sent to invites@h2oflows.app — anyone on the
	// internet can address mail there. The only signal that this REPLY
	// actually came from the attendee is that their mail client sends it
	// From their own account, so require From == ATTENDEE exactly (case-
	// insensitively — RFC 5321 domain-parts are case-insensitive, and in
	// practice no real mailbox provider enforces local-part case either).
	if !strings.EqualFold(parsed.FromEmail, parsed.Attendee) {
		log.Printf("rsvp-inbound: ignored (From != ATTENDEE) run=%s from=%s attendee=%s",
			parsed.RunID, redactEmail(parsed.FromEmail), redactEmail(parsed.Attendee))
		jsonResponse(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	switch parsed.PartStat {
	case "ACCEPTED", "DECLINED":
		action, applyIgnoreReason, aerr := h.applyRSVP(r.Context(), parsed.RunID, parsed.Attendee, parsed.PartStat)
		if aerr != nil {
			// Internal failure, not a content-we-don't-process case — still
			// 200 per the package doc comment, but logged distinctly (grep
			// "APPLY ERROR") so it doesn't get lost among routine ignores.
			log.Printf("rsvp-inbound: APPLY ERROR run=%s: %v", parsed.RunID, aerr)
			jsonResponse(w, http.StatusOK, map[string]string{"status": "ignored"})
			return
		}
		if applyIgnoreReason != "" {
			log.Printf("rsvp-inbound: ignored (%s) run=%s attendee=%s", applyIgnoreReason, parsed.RunID, redactEmail(parsed.Attendee))
			jsonResponse(w, http.StatusOK, map[string]string{"status": "ignored"})
			return
		}
		// Task spec: log run id + action, NEVER the full email (invite_email/
		// user_emails.email are PII — see mig 000148's own doc comment on
		// user_emails being "the ONLY stored-address table").
		log.Printf("rsvp-inbound: applied run=%s action=%s", parsed.RunID, action)
		jsonResponse(w, http.StatusOK, map[string]string{"status": "applied", "action": action})
	default:
		log.Printf("rsvp-inbound: ignored (partstat %q not actionable) run=%s", parsed.PartStat, parsed.RunID)
		jsonResponse(w, http.StatusOK, map[string]string{"status": "ignored"})
	}
}

// applyRSVP resolves the run_invites row this (runID, attendeeEmail) pair
// targets and applies the ACCEPTED/DECLINED transition, race-safe (locks
// the row with FOR UPDATE for the duration of the transaction — a
// re-delivered REPLY or a double-click-equivalent double-send from a mail
// client serializes against itself the same way AcceptInvite/RunCrewAccept
// lock calendar_runs). partstat must be "ACCEPTED" or "DECLINED" — callers
// filter TENTATIVE/NEEDS-ACTION/unknown before ever calling this (Inbound's
// switch above).
//
// action is "" whenever nothing was applied — either because ignoreReason
// explains a legitimate no-op (idempotent replay, unresolvable attendee,
// run gone/paddled) or because err is non-nil (a genuine failure the
// caller logs distinctly). Exactly one of {action != "", ignoreReason != "",
// err != nil} holds on return.
func (h *RSVPInboundHandler) applyRSVP(ctx context.Context, runID, attendeeEmail, partstat string) (action, ignoreReason string, err error) {
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return "", "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	// Run guard: must be live and not yet logged. Mirrors InviteToRun/
	// ResendInvite's "no inviting to logged runs" gate (invites.go, API-2
	// Amendments) — a REPLY for a deleted or already-paddled run has
	// nothing left to RSVP to.
	var runExists bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM calendar_runs WHERE id = $1::uuid AND deleted_at IS NULL AND NOT paddled)
	`, runID).Scan(&runExists); err != nil {
		return "", "", fmt.Errorf("run guard: %w", err)
	}
	if !runExists {
		return "", "run not found, deleted, or already paddled", nil
	}

	// Resolve the invitee row for this run — email-invite match, OR a bound
	// member whose captured address (user_emails) matches. This generalizes
	// invites.go's repeated "member_owner_id = $ownerID OR (member_owner_id
	// IS NULL AND LOWER(invite_email) = LOWER($email))" pattern (LeaveRun/
	// DismissInvite/MyInvites all use exactly that shape) for a caller that
	// has only an EMAIL ADDRESS in hand, not an authenticated ownerID —
	// inbound mail proves nothing about identity beyond "this address sent
	// this REPLY" (checked by the From==ATTENDEE compare in Inbound above),
	// so resolution runs the opposite direction here: email -> owner_id,
	// not owner_id -> email.
	var inviteID, status, origin string
	var memberOwnerID *string
	err = tx.QueryRow(ctx, `
		SELECT ri.id, ri.status::text, ri.origin::text, ri.member_owner_id
		FROM run_invites ri
		WHERE ri.run_id = $1::uuid
		  AND (
		    (ri.member_owner_id IS NULL AND LOWER(ri.invite_email) = LOWER($2))
		    OR ri.member_owner_id IN (SELECT owner_id FROM user_emails WHERE LOWER(email) = LOWER($2))
		  )
		FOR UPDATE
	`, runID, attendeeEmail).Scan(&inviteID, &status, &origin, &memberOwnerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "no run_invites row resolves to this attendee", nil
		}
		return "", "", fmt.Errorf("resolve invitee: %w", err)
	}

	var notify bool
	switch partstat {
	case "ACCEPTED":
		switch status {
		case "accepted":
			return "", "already accepted (idempotent)", nil
		case "declined":
			// Judgment call (flagged in the PR report): InviteToRun/
			// ResendInvite resurrect a declined run_invites row
			// (R4, INVITE_SYNC_PLAN.md risks) ONLY on a fresh HOST-initiated
			// re-invite. A decline here can ALSO come from RunCrewDecline —
			// the HOST uninviting someone — which flips status to
			// 'declined' without touching origin, so origin='invite' alone
			// can't distinguish "I changed my mind after declining" from
			// "the host removed me and I'm replaying my original invite
			// email's Accept button". Resurrecting on nothing but an
			// inbound mail reply would let a removed attendee force their
			// way back onto a run the host explicitly uninvited them from.
			// Conservative choice: never resurrect from this endpoint — a
			// genuine re-accept has to go through the host re-inviting (or
			// the web UI), same as any other declined row.
			return "", "was declined; inbound accept never resurrects a declined row (see code comment)", nil
		}
		// status IN ('invited', 'requested').
		if _, uerr := tx.Exec(ctx, `
			UPDATE run_invites SET status = 'accepted', responded_at = NOW(), updated_at = NOW()
			WHERE id = $1::uuid
		`, inviteID); uerr != nil {
			return "", "", fmt.Errorf("flip to accepted: %w", uerr)
		}
		action = "accepted"
		notify = true // same side effect as AcceptInvite (invites.go): notifyOrganizerAccepted.

	case "DECLINED":
		if status == "declined" {
			return "", "already declined (idempotent)", nil
		}
		// status IN ('invited', 'accepted', 'requested') — the same three
		// source states LeaveRun (invites.go) flips from.
		if _, uerr := tx.Exec(ctx, `
			UPDATE run_invites SET status = 'declined', responded_at = NOW(), updated_at = NOW()
			WHERE id = $1::uuid
		`, inviteID); uerr != nil {
			return "", "", fmt.Errorf("flip to declined: %w", uerr)
		}
		action = "declined"
		// LeaveRun's origin-gated notify (invites.go): a withdrawn join
		// request (origin='request') stays silent — notifyOrganizerDeclined's
		// copy ("X declined — invite someone else") only makes sense for
		// someone who was actually invited/seated, not for someone
		// withdrawing their own unactioned join request.
		notify = origin == "invite"
	}

	if err := tx.Commit(ctx); err != nil {
		return "", "", fmt.Errorf("commit: %w", err)
	}

	if notify {
		// Best-effort handle/display-name resolution for the organizer
		// email's "@handle accepted/declined" copy — same query AcceptInvite/
		// LeaveRun run post-commit, error intentionally ignored (their
		// existing convention: an unresolvable handle just renders as "A
		// paddler", displayHost's fallback, not a failure of the RSVP
		// itself). Skipped entirely when memberOwnerID is nil (a still-
		// email-only invitee with no account to look up — same as those
		// two callers' own nil-safety).
		var handle string
		var displayName *string
		if memberOwnerID != nil {
			h.db.QueryRow(ctx, `SELECT COALESCE(handle, ''), display_name FROM user_profiles WHERE owner_id = $1`, *memberOwnerID).
				Scan(&handle, &displayName) //nolint:errcheck // best-effort, see comment above
		}
		if action == "accepted" {
			go notifyOrganizerAccepted(h.mailer, h.db, runID, handle, displayName)
		} else {
			go notifyOrganizerDeclined(h.mailer, h.db, runID, handle, displayName)
		}
	}

	return action, "", nil
}

// redactEmail renders a PII-safe form for logging ("f***@x.io") — keeps the
// first character of the local part (useful for eyeballing "does this look
// like the same reporter across log lines" without a full re-identifying
// address) and the whole domain (useful for "is a given provider's mail
// getting through"). invite_email/user_emails.email are the only stored
// addresses in this schema (mig 000148's doc comment) and this package
// otherwise never logs them in full either.
func redactEmail(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return ""
	}
	at := strings.IndexByte(addr, '@')
	if at <= 0 || at == len(addr)-1 {
		return "***"
	}
	return string(addr[0]) + "***@" + addr[at+1:]
}

// ── Parsing (internal/handlers/rsvp_inbound_test.go table-drives this) ────

// parsedRSVP is what parseRSVPReply extracts from a raw RFC 822 message.
type parsedRSVP struct {
	RunID     string // bare uuid, lowercased — the UID's "{uuid}" with the "@h2oflows.app" suffix stripped
	FromEmail string // the message's own From header address, lowercased
	Attendee  string // the REPLY's ATTENDEE mailto: address, lowercased
	PartStat  string // upper-cased PARTSTAT param value (ACCEPTED/DECLINED/TENTATIVE/NEEDS-ACTION/anything else a client sends)
}

// uidDomainSuffix is the fixed suffix every UID this app ever emits carries
// (ics.BuildRunInvite: "UID:" + runID + "@h2oflows.app") — a REPLY whose UID
// doesn't end in exactly this, or whose prefix isn't a well-formed UUID, is
// never a reply to anything we sent.
const uidDomainSuffix = "@h2oflows.app"

// uuidRE matches the bare 8-4-4-4-12 hex grouping — deliberately not
// version/variant-strict (this codebase has no uuid library, see go.mod;
// every other UUID "validation" in this codebase is just the Postgres
// ::uuid cast at query time — a well-formed-looking but bogus UUID here
// simply resolves to "no run found" downstream, which is a safe outcome).
var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// parseRSVPReply parses raw (a full RFC 822 message, as POSTed by the
// Cloudflare Email Worker) looking for a METHOD:REPLY iCalendar part
// addressed to one of our run UIDs. ignoreReason is set (parsedRSVP left
// zero-valued) for every "not something this endpoint acts on" case —
// per the package doc comment, Inbound turns any non-empty ignoreReason
// into 200 {"status":"ignored"}, never an error status. err is reserved for
// a message so malformed net/mail itself can't even read it (RFC 822
// header parsing failure) — Inbound treats that identically to
// ignoreReason (still 200), just logs it under a different message so a
// spike in genuinely garbled input is distinguishable from routine
// non-REPLY mail traffic.
func parseRSVPReply(raw []byte) (parsedRSVP, string, error) {
	raw = normalizeLineEndings(raw)

	msg, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return parsedRSVP{}, "", fmt.Errorf("read message: %w", err)
	}

	fromAddr, ferr := netmail.ParseAddress(msg.Header.Get("From"))
	if ferr != nil || fromAddr.Address == "" {
		return parsedRSVP{}, "no parseable From address", nil
	}
	fromEmail := strings.ToLower(fromAddr.Address)

	calBody, found := findCalendarPart(msg.Header, msg.Body)
	if !found {
		return parsedRSVP{}, "no calendar part found", nil
	}

	unfolded := unfoldICS(calBody)
	method, uid, attendeeEmail, partstat := extractICSFields(unfolded)

	if !strings.EqualFold(method, "REPLY") {
		return parsedRSVP{}, fmt.Sprintf("method %q is not REPLY", method), nil
	}

	runID, ok := extractRunIDFromUID(uid)
	if !ok {
		return parsedRSVP{}, "UID is not ours", nil
	}

	if attendeeEmail == "" {
		return parsedRSVP{}, "no ATTENDEE with a PARTSTAT found", nil
	}

	return parsedRSVP{
		RunID:     runID,
		FromEmail: fromEmail,
		Attendee:  attendeeEmail,
		PartStat:  strings.ToUpper(partstat),
	}, "", nil
}

// extractRunIDFromUID splits "{uuid}@h2oflows.app" into its bare, lowercased
// uuid — ok=false for any other shape (wrong/missing domain suffix, or a
// prefix that isn't a well-formed UUID).
func extractRunIDFromUID(uid string) (string, bool) {
	if !strings.HasSuffix(uid, uidDomainSuffix) {
		return "", false
	}
	id := strings.TrimSuffix(uid, uidDomainSuffix)
	if !uuidRE.MatchString(id) {
		return "", false
	}
	return strings.ToLower(id), true
}

// normalizeLineEndings converts bare LF to CRLF throughout raw. net/mail and
// mime/multipart are written against RFC 822/2046's CRLF requirement, but
// this endpoint must also tolerate a message that arrives entirely LF-only
// (some relays normalize line endings in transit). CRLF pairs already
// present are left untouched; a lone CR with no following LF is left as-is
// (rare enough not to be worth guessing at).
func normalizeLineEndings(raw []byte) []byte {
	if !bytes.Contains(raw, []byte("\n")) {
		return raw
	}
	out := make([]byte, 0, len(raw)+len(raw)/16)
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		if b == '\n' && (i == 0 || raw[i-1] != '\r') {
			out = append(out, '\r')
		}
		out = append(out, b)
	}
	return out
}

// headerGetter is satisfied by both net/mail.Header (the top-level message)
// and net/textproto.MIMEHeader (== multipart.Part.Header, every nested
// part) — findCalendarPart/attachmentFilename work on either without a
// conversion at each recursion step.
type headerGetter interface {
	Get(key string) string
}

// findCalendarPart walks a MIME entity depth-first looking for a
// text/calendar (or application/ics, or an attachment whose filename ends
// .ics regardless of declared Content-Type — some clients mislabel it)
// leaf, decoding Content-Transfer-Encoding (base64/quoted-printable) as it
// goes. Handles arbitrary nesting (multipart/mixed containing
// multipart/alternative, etc.) via plain recursion. Returns ("", false) if
// no such part exists anywhere in the tree (a human just replying with
// text, most commonly).
func findCalendarPart(header headerGetter, body io.Reader) (string, bool) {
	mediaType, params, err := mime.ParseMediaType(header.Get("Content-Type"))
	if err != nil {
		// RFC 2045 §5.2 default when Content-Type is absent or unparseable.
		mediaType = "text/plain"
		params = nil
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return "", false
		}
		mr := multipart.NewReader(body, boundary)
		for {
			part, perr := mr.NextPart()
			if perr != nil {
				return "", false // io.EOF (ran out of parts) or a malformed part — either way, nothing found down this branch
			}
			if content, ok := findCalendarPart(part.Header, part); ok {
				return content, true
			}
		}
	}

	isCalendar := mediaType == "text/calendar" || mediaType == "application/ics"
	if !isCalendar {
		if fn := attachmentFilename(header); strings.HasSuffix(strings.ToLower(fn), ".ics") {
			isCalendar = true
		}
	}
	if !isCalendar {
		return "", false
	}

	content, rerr := io.ReadAll(body)
	if rerr != nil {
		return "", false
	}
	return decodePartBody(header.Get("Content-Transfer-Encoding"), content), true
}

// attachmentFilename reads a leaf part's filename off Content-Disposition
// (preferred — the RFC 2183 home for it) or, failing that, Content-Type's
// "name" param (the older, less correct but still common convention).
func attachmentFilename(header headerGetter) string {
	if cd := header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if fn := params["filename"]; fn != "" {
				return fn
			}
		}
	}
	if ct := header.Get("Content-Type"); ct != "" {
		if _, params, err := mime.ParseMediaType(ct); err == nil {
			if fn := params["name"]; fn != "" {
				return fn
			}
		}
	}
	return ""
}

// decodePartBody applies Content-Transfer-Encoding (base64 or
// quoted-printable — the two forms real mail clients use for a .ics part;
// 7bit/8bit/binary/absent all mean "use the bytes as-is"). Charset is
// assumed UTF-8 (us-ascii tolerated as a strict subset) — no charset
// conversion library is pulled in, matching internal/ics's own
// no-third-party-library convention.
func decodePartBody(cte string, raw []byte) string {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		// Mail base64 bodies are wrapped at ~76 cols with CRLF; StdEncoding
		// rejects embedded whitespace, so strip it all before decoding.
		cleaned := make([]byte, 0, len(raw))
		for _, b := range raw {
			switch b {
			case ' ', '\t', '\r', '\n':
				continue
			}
			cleaned = append(cleaned, b)
		}
		decoded, err := base64.StdEncoding.DecodeString(string(cleaned))
		if err != nil {
			return string(raw) // best-effort: not valid base64, use as-is rather than drop the part entirely
		}
		return string(decoded)
	case "quoted-printable":
		decoded, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
		if err != nil {
			return string(raw)
		}
		return string(decoded)
	default:
		return string(raw)
	}
}

// unfoldICS reverses RFC 5545 §3.1 content-line folding: a CRLF (or bare
// LF — tolerated, see normalizeLineEndings' doc comment for why the whole
// message may already have collapsed to LF-only by the time this runs)
// immediately followed by a single SPACE or TAB is a fold point, not a real
// line break; both the line break and that one leading whitespace character
// are removed.
func unfoldICS(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	var b strings.Builder
	b.Grow(len(s))
	for i, line := range lines {
		if i > 0 && len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
			b.WriteString(line[1:])
			continue
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	return b.String()
}

// icsProp is one unfolded content line, split into its property name,
// parameters, and value.
type icsProp struct {
	Name   string
	Params map[string]string // upper-cased keys, quotes stripped
	Value  string
}

// parseICSLine splits "NAME[;PARAM=VALUE...]:VALUE" — VALUE is everything
// after the first colon NOT enclosed in a quoted parameter value (a
// quoted CN can itself contain a colon, e.g. CN="Smith: Jane").
func parseICSLine(line string) (icsProp, bool) {
	idx := -1
	inQuotes := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '"':
			inQuotes = !inQuotes
		case ':':
			if !inQuotes {
				idx = i
			}
		}
		if idx >= 0 {
			break
		}
	}
	if idx < 0 {
		return icsProp{}, false
	}

	head, value := line[:idx], line[idx+1:]
	segs := strings.Split(head, ";")
	prop := icsProp{
		Name:   strings.ToUpper(strings.TrimSpace(segs[0])),
		Params: map[string]string{},
		Value:  value,
	}
	for _, seg := range segs[1:] {
		kv := strings.SplitN(seg, "=", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.ToUpper(strings.TrimSpace(kv[0]))
		val := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		prop.Params[key] = val
	}
	return prop, true
}

// extractICSFields walks an unfolded VCALENDAR's content lines for the
// three properties this endpoint cares about: the calendar-level METHOD,
// and (scoped to the first VEVENT) UID and the first ATTENDEE that carries
// a PARTSTAT param (RFC 5546: a REPLY carries exactly one ATTENDEE; task
// spec: if a client somehow sends more than one, use the first with a
// PARTSTAT). Property/param name matching is case-insensitive throughout
// (parseICSLine upper-cases both); param VALUE order (e.g. PARTSTAT before
// or after CN) never matters since params land in a map.
func extractICSFields(unfolded string) (method, uid, attendeeEmail, partstat string) {
	inVEVENT := false
	for _, raw := range strings.Split(unfolded, "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			continue
		}
		prop, ok := parseICSLine(line)
		if !ok {
			continue
		}
		switch prop.Name {
		case "METHOD":
			if method == "" {
				method = strings.TrimSpace(prop.Value)
			}
		case "BEGIN":
			if strings.EqualFold(strings.TrimSpace(prop.Value), "VEVENT") {
				inVEVENT = true
			}
		case "END":
			if strings.EqualFold(strings.TrimSpace(prop.Value), "VEVENT") {
				inVEVENT = false
			}
		case "UID":
			if inVEVENT && uid == "" {
				uid = strings.TrimSpace(prop.Value)
			}
		case "ATTENDEE":
			if !inVEVENT || attendeeEmail != "" {
				continue
			}
			ps := prop.Params["PARTSTAT"]
			if ps == "" {
				continue // no PARTSTAT on this ATTENDEE — not the reply target; keep looking
			}
			attendeeEmail = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(prop.Value)), "mailto:")
			partstat = ps
		}
	}
	return method, uid, attendeeEmail, partstat
}
