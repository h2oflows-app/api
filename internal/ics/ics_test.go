package ics

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestBuildRunInvite_Golden is a golden-file test: the expected string is
// frozen output from a manually-inspected-correct run of BuildRunInvite
// (RFC 5545 structure, TEXT escaping, CRLF line endings all verified by eye
// against the spec before being committed here). Covers the escaping case —
// a run name containing every TEXT special character (comma, semicolon,
// backslash, and an embedded newline) plus a LOCATION (meet-up spot) that
// ALSO carries a comma, semicolon, and trailing backslash — the property a
// regression that drops escapeText(run.MeetupSpot) in BuildRunInvite would
// fail (plain apostrophes, as used here for "'T'", are NOT escaped and so
// don't exercise this on their own). Also covers the timed-run path
// (DTSTART floating date-time + DURATION:PT2H). web#354 A2: replaces
// TestBuildPlanInvite_Golden/TestBuildPlanInvite_MultiVEventWithRuns — one
// run, one VEVENT, no plan wrapper.
func TestBuildRunInvite_Golden(t *testing.T) {
	got := BuildRunInvite(RunInviteInput{
		RunID:      "11111111-1111-1111-1111-111111111111",
		Name:       "Salmon River, Middle Fork; \"The Big One\" \\ Redux\nSecond Line",
		RunDate:    "2026-08-01",
		RunTime:    "10:00:00",
		MeetupSpot: "The 'T' lot, past the gate; muddy\\",
		URL:        "https://h2oflows.app/plan-runs/11111111-1111-1111-1111-111111111111",
		Now:        time.Date(2026, 7, 23, 18, 4, 5, 0, time.UTC),
	})

	want := "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//h2oflows//Trip Calendar//EN\r\n" +
		"METHOD:PUBLISH\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:11111111-1111-1111-1111-111111111111@h2oflows.app\r\n" +
		"DTSTAMP:20260723T180405Z\r\n" +
		"DTSTART:20260801T100000\r\n" +
		"DURATION:PT2H\r\n" +
		"SUMMARY:Salmon River\\, Middle Fork\\; \"The Big One\" \\\\ Redux\\nSecond Line\r\n" +
		"LOCATION:The 'T' lot\\, past the gate\\; muddy\\\\\r\n" +
		"URL:https://h2oflows.app/plan-runs/11111111-1111-1111-1111-111111111111\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	if got != want {
		t.Errorf("BuildRunInvite mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestBuildRunInvite_AllDayNoLocation covers the untimed/all-day path
// (DTSTART/DTEND;VALUE=DATE, DTEND exclusive) and LOCATION omission when
// MeetupSpot is empty (no plan-level location to fall back to anymore —
// web#354 A2 dropped the whole-plan LOCATION fallback runVEvent used to
// have; a run invite either has its own meetup spot or no LOCATION at all).
func TestBuildRunInvite_AllDayNoLocation(t *testing.T) {
	got := BuildRunInvite(RunInviteInput{
		RunID:   "run-b",
		Name:    "South Platte",
		RunDate: "2026-08-02",
		RunTime: "",
		URL:     "https://h2oflows.app/plan-runs/run-b",
		Now:     time.Date(2026, 7, 23, 18, 4, 5, 0, time.UTC),
	})

	want := "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//h2oflows//Trip Calendar//EN\r\n" +
		"METHOD:PUBLISH\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:run-b@h2oflows.app\r\n" +
		"DTSTAMP:20260723T180405Z\r\n" +
		"DTSTART;VALUE=DATE:20260802\r\n" +
		"DTEND;VALUE=DATE:20260803\r\n" +
		"SUMMARY:South Platte\r\n" +
		"URL:https://h2oflows.app/plan-runs/run-b\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	if got != want {
		t.Errorf("BuildRunInvite mismatch:\ngot:  %q\nwant: %q", got, want)
	}

	// DTEND must be run_date+1 (exclusive all-day convention), never bare
	// run_date — this is the single most common all-day-event bug that makes
	// imported events look one day short in calendar UIs.
	if !strings.Contains(got, "DTEND;VALUE=DATE:20260803") {
		t.Error("DTEND must be RunDate+1 day (exclusive), got a line without 20260803")
	}
}

// TestBuildRunInvite_LongMultibyteName is the folding case: a run name long
// enough (120 raw UTF-8 bytes) to force a fold, built entirely from a
// 2-byte-per-rune character so a naive byte-offset fold would land mid
// sequence roughly half the time. Golden output frozen the same way as
// TestBuildRunInvite_Golden, plus an explicit UTF-8-validity assertion on
// every folded line — the property that actually matters for Apple/Google
// import (a corrupted byte sequence is what breaks those clients, not the
// exact fold column).
func TestBuildRunInvite_LongMultibyteName(t *testing.T) {
	name := strings.Repeat("é", 60) // 60 runes x 2 bytes = 120 bytes
	got := BuildRunInvite(RunInviteInput{
		RunID:   "22222222-2222-2222-2222-222222222222",
		Name:    name,
		RunDate: "2026-09-01",
		URL:     "https://h2oflows.app/plan-runs/22222222-2222-2222-2222-222222222222",
		Now:     time.Date(2026, 7, 23, 18, 4, 5, 0, time.UTC),
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
		"URL:https://h2oflows.app/plan-runs/22222222-2222-2222-2222-222222222222\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	if got != want {
		t.Errorf("BuildRunInvite mismatch:\ngot:  %q\nwant: %q", got, want)
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
