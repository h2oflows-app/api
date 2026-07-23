// Package ics builds RFC 5545 iCalendar payloads for plan invites (#246 A4).
// Pure string templating, no third-party library — a single all-day VEVENT
// is simple enough that a dependency isn't worth it, and hand-rolling keeps
// the escaping/folding exactly matched to what Apple/Google Calendar import
// needs (verified against both, see ics_test.go).
package ics

import (
	"strings"
	"time"
)

// PlanInviteInput is the data BuildPlanInvite needs to render one plan as an
// all-day (possibly multi-day) VEVENT.
type PlanInviteInput struct {
	// PlanID becomes UID={PlanID}@h2oflows.app.
	PlanID string
	// Name is user input -> SUMMARY, TEXT-escaped.
	Name string
	// Location is user input -> LOCATION, TEXT-escaped. Omitted entirely
	// when empty.
	Location string
	// StartDate/EndDate are YYYY-MM-DD, both inclusive of the trip's actual
	// dates -> DTSTART;VALUE=DATE=StartDate, DTEND;VALUE=DATE=EndDate+1 (the
	// RFC 5545 all-day convention: DTEND is exclusive).
	StartDate string
	EndDate   string
	// URL -> URL (not escaped; this is always a URI we construct, never raw
	// user input).
	URL string
	// Now stamps DTSTAMP. Zero value defaults to time.Now().UTC() — tests
	// pin this explicitly for deterministic golden output.
	Now time.Time
}

const (
	inputDateLayout   = "2006-01-02"
	icsDateLayout     = "20060102"
	icsDateTimeLayout = "20060102T150405Z"
	// maxFoldOctets is RFC 5545 §3.1's content-line limit: 75 octets,
	// excluding the terminating CRLF. Continuation lines are prefixed by a
	// single SPACE, which itself counts toward that continuation line's
	// 75-octet budget (the widely-used convention, matched by e.g. Python's
	// `icalendar` and Ruby's `icalendar` gem).
	maxFoldOctets = 75
)

// BuildPlanInvite renders a single-VEVENT VCALENDAR for a plan invite email
// attachment. Strictly RFC 5545: VERSION:2.0, PRODID, METHOD:PUBLISH,
// UID={planID}@h2oflows.app, DTSTAMP (UTC, required), all-day
// DTSTART/DTEND;VALUE=DATE (DTEND exclusive = EndDate+1 day), TEXT-escaped
// SUMMARY/LOCATION (plan names and locations are user input), CRLF line
// endings, 75-octet line folding. Shaped for, and manually verified against,
// Apple Calendar and Google Calendar .ics import.
func BuildPlanInvite(in PlanInviteInput) string {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC()

	start, err := time.Parse(inputDateLayout, in.StartDate)
	if err != nil {
		start = now
	}
	end, err := time.Parse(inputDateLayout, in.EndDate)
	if err != nil {
		end = start
	}
	dtend := end.AddDate(0, 0, 1) // exclusive per RFC 5545 all-day convention

	lines := []string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//h2oflows//Trip Calendar//EN",
		"METHOD:PUBLISH",
		"BEGIN:VEVENT",
		"UID:" + in.PlanID + "@h2oflows.app",
		"DTSTAMP:" + now.Format(icsDateTimeLayout),
		"DTSTART;VALUE=DATE:" + start.Format(icsDateLayout),
		"DTEND;VALUE=DATE:" + dtend.Format(icsDateLayout),
		"SUMMARY:" + escapeText(in.Name),
	}
	if in.Location != "" {
		lines = append(lines, "LOCATION:"+escapeText(in.Location))
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

// escapeText applies RFC 5545 §3.3.11 TEXT escaping: backslash, comma, and
// semicolon are backslash-escaped, and newlines become the literal two-byte
// sequence "\n" (a backslash followed by the letter n — NOT an actual line
// break, which would corrupt the content-line structure of the .ics file).
// Required because plan names/locations are free-form user input that can
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
