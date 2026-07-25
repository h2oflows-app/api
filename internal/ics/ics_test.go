package ics

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestBuildPlanInvite_Golden is a golden-file test: the expected string is
// frozen output from a manually-inspected-correct run of BuildPlanInvite
// (RFC 5545 structure, TEXT escaping, CRLF line endings all verified by eye
// against the spec before being committed here). Covers the escaping case —
// a plan name containing every TEXT special character (comma, semicolon,
// backslash, and an embedded newline) plus a LOCATION. Shaped for, and this
// exact output manually verified importable into, Apple Calendar and Google
// Calendar.
func TestBuildPlanInvite_Golden(t *testing.T) {
	got := BuildPlanInvite(PlanInviteInput{
		PlanID:    "11111111-1111-1111-1111-111111111111",
		Name:      "Salmon River, Middle Fork; \"The Big One\" \\ Redux\nSecond Line",
		Location:  "Confluence, CO",
		StartDate: "2026-08-01",
		EndDate:   "2026-08-03",
		URL:       "https://h2oflows.app/plans/ianskluhsman/salmon-trip",
		Now:       time.Date(2026, 7, 23, 18, 4, 5, 0, time.UTC),
	})

	want := "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//h2oflows//Trip Calendar//EN\r\n" +
		"METHOD:PUBLISH\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:11111111-1111-1111-1111-111111111111@h2oflows.app\r\n" +
		"DTSTAMP:20260723T180405Z\r\n" +
		"DTSTART;VALUE=DATE:20260801\r\n" +
		"DTEND;VALUE=DATE:20260804\r\n" +
		"SUMMARY:Salmon River\\, Middle Fork\\; \"The Big One\" \\\\ Redux\\nSecond Line\r\n" +
		"LOCATION:Confluence\\, CO\r\n" +
		"URL:https://h2oflows.app/plans/ianskluhsman/salmon-trip\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	if got != want {
		t.Errorf("BuildPlanInvite mismatch:\ngot:  %q\nwant: %q", got, want)
	}

	// DTEND must be end_date+1 (exclusive all-day convention), never bare
	// end_date — this is the single most common all-day-event bug that makes
	// imported events look one day short in calendar UIs.
	if !strings.Contains(got, "DTEND;VALUE=DATE:20260804") {
		t.Error("DTEND must be EndDate+1 day (exclusive), got a line without 20260804")
	}
}

// TestBuildPlanInvite_LongMultibyteName is the folding case: a plan name
// long enough (120 raw UTF-8 bytes) to force a fold, built entirely from a
// 2-byte-per-rune character so a naive byte-offset fold would land mid
// sequence roughly half the time. Golden output frozen the same way as
// TestBuildPlanInvite_Golden, plus an explicit UTF-8-validity assertion on
// every folded line — the property that actually matters for Apple/Google
// import (a corrupted byte sequence is what breaks those clients, not the
// exact fold column).
func TestBuildPlanInvite_LongMultibyteName(t *testing.T) {
	name := strings.Repeat("é", 60) // 60 runes x 2 bytes = 120 bytes
	got := BuildPlanInvite(PlanInviteInput{
		PlanID:    "22222222-2222-2222-2222-222222222222",
		Name:      name,
		StartDate: "2026-09-01",
		EndDate:   "2026-09-01",
		URL:       "https://h2oflows.app/plans/ianskluhsman/long-trip",
		Now:       time.Date(2026, 7, 23, 18, 4, 5, 0, time.UTC),
	})

	want := "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//h2oflows//Trip Calendar//EN\r\n" +
		"METHOD:PUBLISH\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:22222222-2222-2222-2222-222222222222@h2oflows.app\r\n" +
		"DTSTAMP:20260723T180405Z\r\n" +
		"DTSTART;VALUE=DATE:20260901\r\n" +
		"DTEND;VALUE=DATE:20260902\r\n" +
		"SUMMARY:ééééééééééééééééééééééééééééééééé\r\n" +
		" ééééééééééééééééééééééééééé\r\n" +
		"URL:https://h2oflows.app/plans/ianskluhsman/long-trip\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	if got != want {
		t.Errorf("BuildPlanInvite mismatch:\ngot:  %q\nwant: %q", got, want)
	}

	for i, line := range strings.Split(got, "\r\n") {
		if !utf8.ValidString(line) {
			t.Errorf("line %d is not valid UTF-8 (fold split a multi-byte rune): %q", i, line)
		}
		if len(line) > 75 {
			t.Errorf("line %d exceeds 75 octets (%d): %q", i, len(line), line)
		}
	}
}

// TestBuildPlanInvite_MultiVEventWithRuns is the #246 A7 golden case: a plan
// VEVENT plus two run VEVENTs — one timed + invited (SUMMARY prefixed,
// DURATION-based end), one all-day + not invited (DTEND-exclusive, no
// prefix) — verifying UID composition, per-VEVENT DTSTAMP, and the "You're
// invited" marker. Short fake IDs (not full UUIDs) keep every line under the
// 75-octet fold threshold so this test isolates multi-VEVENT structure from
// the folding behavior already covered above. Also covers "meet up at"
// (product request 2026-07-25): run-a has its own MeetupSpot (LOCATION
// overrides the plan's), run-b has none (LOCATION falls back to the plan's
// "Idaho").
func TestBuildPlanInvite_MultiVEventWithRuns(t *testing.T) {
	got := BuildPlanInvite(PlanInviteInput{
		PlanID:    "plan1",
		Name:      "Salmon Trip",
		Location:  "Idaho",
		StartDate: "2026-08-01",
		EndDate:   "2026-08-02",
		URL:       "https://h2oflows.app/plans/ianskluhsman/salmon-trip",
		Now:       time.Date(2026, 7, 23, 18, 4, 5, 0, time.UTC),
		Runs: []PlanInviteRun{
			{ID: "run-a", Name: "Foxton", RunDate: "2026-08-01", RunTime: "10:00:00", Invited: true, MeetupSpot: "The 'T' parking lot"},
			{ID: "run-b", Name: "South Platte", RunDate: "2026-08-02", RunTime: "", Invited: false},
		},
	})

	want := "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//h2oflows//Trip Calendar//EN\r\n" +
		"METHOD:PUBLISH\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:plan1@h2oflows.app\r\n" +
		"DTSTAMP:20260723T180405Z\r\n" +
		"DTSTART;VALUE=DATE:20260801\r\n" +
		"DTEND;VALUE=DATE:20260803\r\n" +
		"SUMMARY:Salmon Trip\r\n" +
		"LOCATION:Idaho\r\n" +
		"URL:https://h2oflows.app/plans/ianskluhsman/salmon-trip\r\n" +
		"END:VEVENT\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:plan1-run-a@h2oflows.app\r\n" +
		"DTSTAMP:20260723T180405Z\r\n" +
		"DTSTART:20260801T100000\r\n" +
		"DURATION:PT2H\r\n" +
		"SUMMARY:You're invited: Foxton\r\n" +
		"LOCATION:The 'T' parking lot\r\n" +
		"END:VEVENT\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:plan1-run-b@h2oflows.app\r\n" +
		"DTSTAMP:20260723T180405Z\r\n" +
		"DTSTART;VALUE=DATE:20260802\r\n" +
		"DTEND;VALUE=DATE:20260803\r\n" +
		"SUMMARY:South Platte\r\n" +
		"LOCATION:Idaho\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	if got != want {
		t.Errorf("BuildPlanInvite mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestFoldLine_ShortLineUnchanged(t *testing.T) {
	short := "SUMMARY:short and sweet"
	if got := foldLine(short); got != short {
		t.Errorf("foldLine(%q) = %q, want unchanged", short, got)
	}
}

func TestEscapeText(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"plain":          "plain",
		"a,b":            `a\,b`,
		"a;b":            `a\;b`,
		"a\\b":           `a\\b`,
		"line1\nline2":   `line1\nline2`,
		"line1\r\nline2": `line1\nline2`,
	}
	for in, want := range cases {
		if got := escapeText(in); got != want {
			t.Errorf("escapeText(%q) = %q, want %q", in, got, want)
		}
	}
}
