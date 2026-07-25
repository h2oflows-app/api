// Package ics builds RFC 5545 iCalendar payloads for plan invites (#246 A4,
// reworked #246 A7 for per-run crew: BuildPlanInvite emits one VCALENDAR
// with MULTIPLE VEVENTs — the plan-spanning all-day event (as before) plus
// one VEVENT per plan_run, so a recipient can live entirely off the
// calendar attachment without ever loading the web page (§6 REVISED
// 2026-07-25: "ICS carries the whole plan"). Pure string templating, no
// third-party library — hand-rolling keeps the escaping/folding exactly
// matched to what Apple/Google Calendar import needs (verified against
// both, see ics_test.go).
package ics

import (
	"strings"
	"time"
)

// PlanInviteRun is one plan_run rendered as its own VEVENT inside the plan's
// VCALENDAR — RunTime set renders a timed event (DTSTART date+time, no
// TZID/Z — RFC 5545 "floating time" form, interpreted in the viewer's local
// zone, the simplest correct choice absent a stored per-run TZID) with a
// placeholder PT2H DURATION (river-run length isn't tracked); RunTime empty
// renders an all-day event on RunDate, same DTEND-exclusive convention as
// the plan VEVENT.
type PlanInviteRun struct {
	// ID becomes part of UID={PlanID}-{ID}@h2oflows.app — unique per run
	// within the plan.
	ID string
	// Name is user/reach-derived -> SUMMARY, TEXT-escaped.
	Name string
	// RunDate is YYYY-MM-DD.
	RunDate string
	// RunTime is HH:MM or HH:MM:SS (24h), or "" for an all-day run.
	RunTime string
	// Invited marks this as one of the recipient's invited runs — prefixes
	// SUMMARY with "You're invited: " (contract §6: "invited runs marked in
	// their SUMMARY/DESCRIPTION").
	Invited bool
	// MeetupSpot ("meet up at", product request 2026-07-25) -> this VEVENT's
	// LOCATION, TEXT-escaped, when non-empty. Empty falls back to the
	// enclosing PlanInviteInput.Location (the plan-level location) — see
	// BuildPlanInvite/runVEvent.
	MeetupSpot string
}

// PlanInviteInput is the data BuildPlanInvite needs to render one plan as a
// multi-VEVENT VCALENDAR: one all-day VEVENT spanning the whole plan, plus
// one VEVENT per entry in Runs.
type PlanInviteInput struct {
	// PlanID becomes UID={PlanID}@h2oflows.app for the plan-spanning VEVENT,
	// and the UID prefix ({PlanID}-{run.ID}@h2oflows.app) for each run VEVENT.
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
	// Runs renders one additional VEVENT per entry, in the given order
	// (callers pass them run_date/sort_order-ordered — the itinerary order).
	Runs []PlanInviteRun
	// Now stamps DTSTAMP (same instant reused for every VEVENT in the
	// output). Zero value defaults to time.Now().UTC() — tests pin this
	// explicitly for deterministic golden output.
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

// BuildPlanInvite renders a multi-VEVENT VCALENDAR for a plan invite email
// attachment. Strictly RFC 5545: VERSION:2.0, PRODID, METHOD:PUBLISH, one
// DTSTAMP (UTC, required) per VEVENT, TEXT-escaped SUMMARY/LOCATION (plan
// names/run names are user/reach-derived input), CRLF line endings,
// 75-octet line folding. Shaped for, and manually verified against, Apple
// Calendar and Google Calendar .ics import.
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
	}
	lines = append(lines, planVEvent(in, now, start, dtend)...)
	for _, run := range in.Runs {
		lines = append(lines, runVEvent(in.PlanID, run, in.Location, now)...)
	}
	lines = append(lines, "END:VCALENDAR")

	folded := make([]string, len(lines))
	for i, l := range lines {
		folded[i] = foldLine(l)
	}
	return strings.Join(folded, "\r\n") + "\r\n"
}

// planVEvent is the plan-spanning all-day VEVENT — unchanged in shape from
// the original single-VEVENT BuildPlanInvite (#246 A4).
func planVEvent(in PlanInviteInput, now, start, dtend time.Time) []string {
	lines := []string{
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
	lines = append(lines, "END:VEVENT")
	return lines
}

// runVEvent renders one plan_run as its own VEVENT — timed (DTSTART
// floating date-time + DURATION:PT2H) when RunTime is set, else all-day
// (DTSTART/DTEND;VALUE=DATE) on RunDate, matching planVEvent's exclusive-
// DTEND convention. LOCATION ("meet up at", product request 2026-07-25) is
// run.MeetupSpot when set, else planLocation (the plan-level LOCATION) —
// omitted entirely when both are empty, same as planVEvent's LOCATION.
func runVEvent(planID string, run PlanInviteRun, planLocation string, now time.Time) []string {
	rd, err := time.Parse(inputDateLayout, run.RunDate)
	if err != nil {
		rd = now
	}

	summary := run.Name
	if run.Invited {
		summary = "You're invited: " + summary
	}

	lines := []string{
		"BEGIN:VEVENT",
		"UID:" + planID + "-" + run.ID + "@h2oflows.app",
		"DTSTAMP:" + now.Format(icsDateTimeLayout),
	}
	if rt, ok := parseRunTime(run.RunTime); ok {
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
	lines = append(lines, "SUMMARY:"+escapeText(summary))
	if loc := run.MeetupSpot; loc != "" {
		lines = append(lines, "LOCATION:"+escapeText(loc))
	} else if planLocation != "" {
		lines = append(lines, "LOCATION:"+escapeText(planLocation))
	}
	lines = append(lines, "END:VEVENT")
	return lines
}

// parseRunTime accepts "HH:MM:SS" or "HH:MM" (both shapes plan_runs.run_time
// can come back as text ::text-cast from Postgres TIME); ok=false for ""
// (all-day run) or an unparseable value.
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
