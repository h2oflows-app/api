// Package ics builds RFC 5545 iCalendar payloads for run invites (#246 A4,
// reworked #246 A7 for per-run crew, reworked AGAIN web#354 A2: invites are
// Run-scoped only now — BuildRunInvite emits ONE VCALENDAR with a SINGLE
// VEVENT for exactly one run, replacing the old multi-VEVENT whole-plan
// BuildPlanInvite (one plan-spanning all-day VEVENT + one VEVENT per
// invited run). Pure string templating, no third-party library —
// hand-rolling keeps the escaping/folding exactly matched to what
// Apple/Google Calendar import needs (verified against both, see
// ics_test.go).
package ics

import (
	"strings"
	"time"
)

// RunInviteInput is the data BuildRunInvite needs to render one run invite
// as a single-VEVENT VCALENDAR.
type RunInviteInput struct {
	// RunID becomes UID={RunID}@h2oflows.app.
	RunID string
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

// BuildRunInvite renders a single-VEVENT VCALENDAR for a run invite email
// attachment (web#354 A2 — replaces the multi-VEVENT whole-plan
// BuildPlanInvite). Strictly RFC 5545: VERSION:2.0, PRODID, METHOD:PUBLISH,
// one DTSTAMP (UTC, required), TEXT-escaped SUMMARY/LOCATION (run/reach
// names are user/reach-derived input), CRLF line endings, 75-octet line
// folding. Shaped for, and manually verified against, Apple Calendar and
// Google Calendar .ics import.
func BuildRunInvite(in RunInviteInput) string {
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
		"METHOD:PUBLISH",
		"BEGIN:VEVENT",
		"UID:" + in.RunID + "@h2oflows.app",
		"DTSTAMP:" + now.Format(icsDateTimeLayout),
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
	if in.URL != "" {
		lines = append(lines, "URL:"+in.URL)
	}
	lines = append(lines, "END:VEVENT", "END:VCALENDAR")

	folded := make([]string, len(lines))
	for i, l := range lines {
		folded[i] = foldLine(l)
	}
	return strings.Join(folded, "\r\n") + "\r\n"
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
// contain any of these.
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
