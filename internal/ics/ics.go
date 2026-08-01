// Package ics builds RFC 5545 iCalendar payloads for run invites (#246 A4,
// reworked #246 A7 for per-run crew, reworked AGAIN web#354 A2: invites are
// Run-scoped only now — BuildRunInvite emits ONE VCALENDAR with a SINGLE
// VEVENT for exactly one run, replacing the old multi-VEVENT whole-plan
// BuildPlanInvite. Reworked a THIRD time for API-1 Invite Sync
// (web/design_handoff_calendar_explore/INVITE_SYNC_PLAN.md): the calendar
// now emits proper RFC 5546 scheduling messages (METHOD:REQUEST /
// METHOD:CANCEL with SEQUENCE/ORGANIZER/ATTENDEE) instead of a
// disconnected METHOD:PUBLISH snapshot, so Outlook/Google/Apple can
// reconcile an update or cancellation against the SAME imported event
// (matched by UID) instead of importing a duplicate copy every time. Pure
// string templating, no third-party library — hand-rolling keeps the
// escaping/folding/quoting exactly matched to what Apple/Google/Outlook
// import needs (verified against all three, see ics_test.go).
package ics

import (
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Method values BuildRunInvite accepts. RunInviteInput.Method's zero value
// ("") defaults to MethodRequest — PUBLISH is retired entirely for run
// invites as of API-1 (D2: PUBLISH copies don't reconcile later updates,
// see INVITE_SYNC_PLAN.md).
const (
	MethodRequest = "REQUEST"
	MethodCancel  = "CANCEL"
)

// Validation errors BuildRunInvite returns for a malformed RunInviteInput —
// RFC 5546 §3.2.5 (REQUEST) / §3.2.6 (CANCEL) both require ORGANIZER and at
// least one ATTENDEE on a scheduling message; a .ics missing either isn't
// valid iTIP and real clients (Outlook especially) silently reject or
// mishandle it, so BuildRunInvite enforces this at build time rather than
// emitting a malformed calendar.
var (
	ErrInvalidMethod    = errors.New("ics: Method must be \"\" (defaults to REQUEST), REQUEST, or CANCEL")
	ErrMissingOrganizer = errors.New("ics: REQUEST/CANCEL requires OrganizerEmail")
	ErrMissingAttendee  = errors.New("ics: REQUEST/CANCEL requires AttendeeEmail")
)

// RunInviteInput is the data BuildRunInvite needs to render one run invite/
// update/cancellation as a single-VEVENT, single-ATTENDEE VCALENDAR.
type RunInviteInput struct {
	// RunID becomes UID={RunID}@h2oflows.app. Stays fixed across every
	// REQUEST/CANCEL ever sent for this run — the reconciliation key
	// external calendars use to recognize "this is an update to that
	// event", not a new one.
	RunID string
	// Method is "REQUEST" (an invite or an update to one already sent) or
	// "CANCEL" (the run/invite went away). Empty defaults to "REQUEST".
	// Any other value is a caller bug -> ErrInvalidMethod.
	Method string
	// Sequence is RFC 5546's SEQUENCE: 0 on the very first REQUEST for this
	// UID, incremented by 1 on every subsequent REQUEST/CANCEL sent for it.
	// Required for Outlook/Google to accept an update as newer than what
	// they already have instead of ignoring it as a stale duplicate.
	Sequence int
	// OrganizerName/OrganizerEmail render
	// ORGANIZER;CN=<name>:mailto:<email> (CN omitted entirely when
	// OrganizerName is ""). OrganizerEmail MUST be the exact address the
	// containing email is sent From (config.MailFrom, parsed by
	// mail.ParseFromAddress) — Outlook in particular silently drops a
	// scheduling update whose ORGANIZER doesn't match the message's From
	// header. Required (non-empty OrganizerEmail) for REQUEST/CANCEL.
	OrganizerName  string
	OrganizerEmail string
	// AttendeeName/AttendeeEmail render the single per-recipient
	// ATTENDEE;CN=<name>;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:<email>
	// line (CN omitted when AttendeeName is ""). ATTENDEE is per-recipient
	// by RFC 5546 convention — BuildRunInvite renders exactly one .ics per
	// call for exactly one attendee; callers fan out one call per recipient
	// rather than listing everyone on a shared .ics (see
	// internal/handlers/notifications.go). Required (non-empty
	// AttendeeEmail) for REQUEST/CANCEL.
	AttendeeName  string
	AttendeeEmail string
	// AttendeePartStat is this recipient's CURRENT run_invites.status,
	// threaded into ATTENDEE;PARTSTAT= so an update REQUEST reflects their
	// real RSVP state instead of resetting it. Accepted values:
	// "NEEDS-ACTION", "ACCEPTED", "DECLINED" — empty defaults to
	// "NEEDS-ACTION" (the initial-invite path, invites.go's
	// sendRunInviteMail, never sets this — an invitee who hasn't responded
	// yet has nothing else it could be). See attendeeLine for the
	// PARTSTAT->RSVP derivation.
	AttendeePartStat string
	// Cancelled emits STATUS:CANCELLED on the VEVENT. Set together with
	// Method=MethodCancel by every caller in this codebase — kept as its
	// own field rather than inferred from Method so a CANCEL .ics's two
	// "this is cancelled" signals (the calendar-level METHOD:CANCEL and the
	// VEVENT-level STATUS:CANCELLED) stay visibly intentional at each call
	// site instead of one being an implicit side effect of the other.
	Cancelled bool
	// Name is user/reach-derived -> SUMMARY, TEXT-escaped.
	Name string
	// RunDate is YYYY-MM-DD.
	RunDate string
	// RunTime is HH:MM or HH:MM:SS (24h), or "" for an all-day run —
	// renders a timed event (DTSTART floating date-time + a placeholder
	// PT2H DURATION; river-run length isn't tracked) when set, else an
	// all-day event on RunDate (DTSTART/DTEND;VALUE=DATE, DTEND exclusive
	// per the RFC 5545 all-day convention).
	RunTime string
	// MeetupSpot ("meet up at") -> LOCATION, TEXT-escaped. Omitted entirely
	// when empty.
	MeetupSpot string
	// URL -> URL (not escaped; this is always a URI we construct, never raw
	// user input).
	URL string
	// Now stamps DTSTAMP. Zero value defaults to time.Now().UTC() — tests
	// pin this explicitly for deterministic golden output.
	Now time.Time
}

const (
	inputDateLayout    = "2006-01-02"
	inputTimeLayoutSec = "15:04:05"
	inputTimeLayoutMin = "15:04"
	icsDateLayout      = "20060102"
	icsDateTimeLayout  = "20060102T150405Z"
	icsFloatingLayout  = "20060102T150405"
	// maxFoldOctets is RFC 5545 §3.1's content-line limit: 75 octets,
	// excluding the terminating CRLF. Continuation lines are prefixed by a
	// single SPACE, which itself counts toward that continuation line's
	// 75-octet budget (the widely-used convention, matched by e.g. Python's
	// `icalendar` and Ruby's `icalendar` gem).
	maxFoldOctets = 75
)

// BuildRunInvite renders a single-VEVENT, single-ATTENDEE VCALENDAR for a
// run invite/update/cancellation email attachment. RFC 5545 base shape:
// VERSION:2.0, PRODID, one DTSTAMP (UTC, required), TEXT-escaped
// SUMMARY/LOCATION (run/reach names are user/reach-derived input), CRLF
// line endings, 75-octet line folding — plus RFC 5546 scheduling
// properties (METHOD, SEQUENCE, ORGANIZER, ATTENDEE, and STATUS:CANCELLED
// for a cancellation) as of API-1. Returns an error (never panics) when
// Method/OrganizerEmail/AttendeeEmail don't satisfy RFC 5546's REQUEST/
// CANCEL requirements — see the Err* vars. Shaped for, and manually
// verified against, Apple Calendar, Google Calendar, and Outlook .ics
// import.
func BuildRunInvite(in RunInviteInput) (string, error) {
	method := in.Method
	if method == "" {
		method = MethodRequest
	}
	if method != MethodRequest && method != MethodCancel {
		return "", ErrInvalidMethod
	}
	if strings.TrimSpace(in.OrganizerEmail) == "" {
		return "", ErrMissingOrganizer
	}
	if strings.TrimSpace(in.AttendeeEmail) == "" {
		return "", ErrMissingAttendee
	}

	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	rd, err := time.Parse(inputDateLayout, in.RunDate)
	if err != nil {
		rd = now
	}

	lines := []string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//h2oflows//Trip Calendar//EN",
		"METHOD:" + method,
		"BEGIN:VEVENT",
		"UID:" + in.RunID + "@h2oflows.app",
		"SEQUENCE:" + strconv.Itoa(in.Sequence),
		"DTSTAMP:" + now.Format(icsDateTimeLayout),
		organizerLine(in.OrganizerName, in.OrganizerEmail),
		attendeeLine(in.AttendeeName, in.AttendeeEmail, normalizePartStat(in.AttendeePartStat)),
	}

	if rt, ok := parseRunTime(in.RunTime); ok {
		dtstart := time.Date(rd.Year(), rd.Month(), rd.Day(), rt.Hour(), rt.Minute(), rt.Second(), 0, time.UTC)
		lines = append(lines,
			"DTSTART:"+dtstart.Format(icsFloatingLayout),
			"DURATION:PT2H",
		)
	} else {
		lines = append(lines,
			"DTSTART;VALUE=DATE:"+rd.Format(icsDateLayout),
			"DTEND;VALUE=DATE:"+rd.AddDate(0, 0, 1).Format(icsDateLayout),
		)
	}

	lines = append(lines, "SUMMARY:"+escapeText(in.Name))
	if in.MeetupSpot != "" {
		lines = append(lines, "LOCATION:"+escapeText(in.MeetupSpot))
	}
	if in.Cancelled {
		lines = append(lines, "STATUS:CANCELLED")
	}
	if in.URL != "" {
		lines = append(lines, "URL:"+in.URL)
	}
	lines = append(lines, "END:VEVENT", "END:VCALENDAR")

	folded := make([]string, len(lines))
	for i, l := range lines {
		folded[i] = foldLine(l)
	}
	return strings.Join(folded, "\r\n") + "\r\n", nil
}

// organizerLine renders ORGANIZER;CN=<name>:mailto:<email>, omitting the CN
// parameter entirely when name is "" (a bare ORGANIZER:mailto:... is valid
// RFC 5545 — CN is optional on every CAL-ADDRESS parameter).
func organizerLine(name, email string) string {
	if name == "" {
		return "ORGANIZER:mailto:" + email
	}
	return "ORGANIZER;CN=" + quoteParamValue(name) + ":mailto:" + email
}

// partStatNeedsAction/partStatAccepted/partStatDeclined are the PARTSTAT
// values attendeeLine accepts (RunInviteInput.AttendeePartStat, already
// normalized via normalizePartStat by the time attendeeLine sees it).
const (
	partStatNeedsAction = "NEEDS-ACTION"
	partStatAccepted    = "ACCEPTED"
	partStatDeclined    = "DECLINED"
)

// normalizePartStat uppercases raw (tolerating whatever casing a caller
// passed) and maps anything other than the three known PARTSTAT values —
// including "" (the initial-invite path, which never sets
// AttendeePartStat) — defensively to NEEDS-ACTION, so a bad/unexpected
// run_invites.status value can never produce a malformed or misleading
// ATTENDEE line.
func normalizePartStat(raw string) string {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case partStatAccepted:
		return partStatAccepted
	case partStatDeclined:
		return partStatDeclined
	default:
		return partStatNeedsAction
	}
}

// attendeeLine renders ATTENDEE;CN=<name>;PARTSTAT=<partStat>;RSVP=<rsvp>:
// mailto:<email>, omitting CN when name is "" (a handle-less email invitee
// has no known display name yet). partStat must already be normalized (see
// normalizePartStat) — one of NEEDS-ACTION/ACCEPTED/DECLINED. RSVP is
// derived from it rather than passed separately: NEEDS-ACTION means the
// organizer is still soliciting an answer (RSVP=TRUE, the original/only
// behavior before AttendeePartStat existed), while ACCEPTED/DECLINED means
// the attendee already answered and this .ics is just an in-place update to
// event details — RSVP=FALSE tells Outlook/Google/Apple not to re-prompt
// them for a fresh response (the bug this field exists to fix: an update
// email used to always say RSVP=TRUE regardless of the recipient's real
// status, resetting already-accepted crew back to "needs response").
func attendeeLine(name, email, partStat string) string {
	rsvp := "TRUE"
	if partStat != partStatNeedsAction {
		rsvp = "FALSE"
	}
	if name == "" {
		return "ATTENDEE;PARTSTAT=" + partStat + ";RSVP=" + rsvp + ":mailto:" + email
	}
	return "ATTENDEE;CN=" + quoteParamValue(name) + ";PARTSTAT=" + partStat + ";RSVP=" + rsvp + ":mailto:" + email
}

// quoteParamValue renders an RFC 5545 §3.2 property-parameter value (CN in
// particular). Neither of RFC 5545's two param-value forms can carry a
// literal DQUOTE: bare PTEXT's alphabet (SAFE-CHAR) excludes it same as
// COLON/SEMICOLON/COMMA, and the QUOTED-STRING alphabet (QSAFE-CHAR)
// excludes it too with no backslash-escape defined for either form (unlike
// TEXT property VALUES — see escapeText, a completely different mechanism
// for a completely different RFC 5545 rule) — so any embedded double quote
// is stripped outright first, the only RFC-legal way to keep the result
// well-formed without inventing non-standard escaping that Apple/Google/
// Outlook wouldn't understand. What's left is then wrapped in DQUOTE
// (QUOTED-STRING) only if it still contains a COLON, SEMICOLON, or COMMA —
// otherwise it's returned bare (PTEXT), since those are the only three
// characters that would otherwise be misread as param/property delimiters.
func quoteParamValue(s string) string {
	// DQUOTE has no escape inside a quoted param value, and CTLs (CR/LF
	// above all) would split the content line — RFC 5545 §3.2 permits
	// neither, so both are dropped regardless of what the caller stored.
	s = strings.Map(func(r rune) rune {
		if r == '"' || unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if strings.ContainsAny(s, ":;,") {
		return `"` + s + `"`
	}
	return s
}

// parseRunTime accepts "HH:MM:SS" or "HH:MM" (both shapes calendar_runs.
// run_time can come back as text ::text-cast from Postgres TIME); ok=false
// for "" (all-day run) or an unparseable value.
func parseRunTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(inputTimeLayoutSec, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(inputTimeLayoutMin, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// escapeText applies RFC 5545 §3.3.11 TEXT escaping: backslash, comma, and
// semicolon are backslash-escaped, and newlines become the literal two-byte
// sequence "\n" (a backslash followed by the letter n — NOT an actual line
// break, which would corrupt the content-line structure of the .ics file).
// Required because run names/meetup spots are free-form user input that can
// contain any of these. NOT used for property-PARAMETER values (CN in
// particular) — see quoteParamValue, which follows entirely different RFC
// 5545 rules (quoting, not escaping).
func escapeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case ',':
			b.WriteString(`\,`)
		case ';':
			b.WriteString(`\;`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			// Swallow bare CR / the CR half of a CRLF pair — it collapses
			// into the following \n's escape (or is dropped entirely for a
			// lone CR), which is the only sane behavior since a real line
			// break inside a TEXT value has no meaning here.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// foldLine folds a single unfolded content line (no CRLF) to RFC 5545's
// 75-octet limit: continuation lines are "CRLF SPACE". The 75-octet count is
// on encoded UTF-8 bytes (RFC 5545 §3.1 counts octets, not characters), but
// the fold point is backed off so it never lands inside a multi-byte UTF-8
// sequence — required for Apple/Google Calendar to import non-ASCII
// SUMMARY/LOCATION text cleanly; a mid-sequence split produces an invalid
// UTF-8 byte on the continuation line that real clients mangle or reject.
func foldLine(line string) string {
	b := []byte(line)
	if len(b) <= maxFoldOctets {
		return line
	}

	var out strings.Builder
	for len(b) > 0 {
		limit := maxFoldOctets
		if out.Len() > 0 {
			limit = maxFoldOctets - 1 // continuation lines pay for their leading space
		}
		if limit >= len(b) {
			if out.Len() > 0 {
				out.WriteString("\r\n ")
			}
			out.Write(b)
			break
		}
		cut := limit
		// Never split a UTF-8 continuation byte (10xxxxxx, 0x80-0xBF) off
		// its lead byte — back off to the start of that rune's sequence.
		for cut > 0 && b[cut]&0xC0 == 0x80 {
			cut--
		}
		if out.Len() > 0 {
			out.WriteString("\r\n ")
		}
		out.Write(b[:cut])
		b = b[cut:]
	}
	return out.String()
}
