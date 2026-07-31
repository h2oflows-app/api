package ics

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// TestBuildRunInvite_Golden is a golden-file test: the expected string is
// frozen output from a manually-inspected-correct run of BuildRunInvite
// (RFC 5545 structure, TEXT escaping, CRLF line endings, plus API-1's RFC
// 5546 scheduling properties — all verified by eye against the spec before
// being committed here). Covers the escaping case — a run name containing
// every TEXT special character (comma, semicolon, backslash, and an
// embedded newline) plus a LOCATION (meet-up spot) that ALSO carries a
// comma, semicolon, and trailing backslash — the property a regression that
// drops escapeText(run.MeetupSpot) in BuildRunInvite would fail (plain
// apostrophes, as used here for "'T'", are NOT escaped and so don't
// exercise this on their own). Also covers the timed-run path (DTSTART
// floating date-time + DURATION:PT2H). Retargeted for API-1: METHOD flips
// PUBLISH -> REQUEST (D2), plus the new SEQUENCE/ORGANIZER/ATTENDEE lines —
// see INVITE_SYNC_PLAN.md.
func TestBuildRunInvite_Golden(t *testing.T) {
	got, err := BuildRunInvite(RunInviteInput{
		RunID:          "11111111-1111-1111-1111-111111111111",
		Name:           "Salmon River, Middle Fork; \"The Big One\" \\ Redux\nSecond Line",
		RunDate:        "2026-08-01",
		RunTime:        "10:00:00",
		MeetupSpot:     "The 'T' lot, past the gate; muddy\\",
		URL:            "https://h2oflows.app/plan-runs/11111111-1111-1111-1111-111111111111",
		Now:            time.Date(2026, 7, 23, 18, 4, 5, 0, time.UTC),
		OrganizerName:  "H2OFlows",
		OrganizerEmail: "trips@h2oflows.app",
		AttendeeName:   "Jamie",
		AttendeeEmail:  "j@example.com",
	})
	if err != nil {
		t.Fatalf("BuildRunInvite error: %v", err)
	}

	want := "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//h2oflows//Trip Calendar//EN\r\n" +
		"METHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:11111111-1111-1111-1111-111111111111@h2oflows.app\r\n" +
		"SEQUENCE:0\r\n" +
		"DTSTAMP:20260723T180405Z\r\n" +
		"ORGANIZER;CN=H2OFlows:mailto:trips@h2oflows.app\r\n" +
		"ATTENDEE;CN=Jamie;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:j@example.com\r\n" +
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
// (DTSTART/DTEND;VALUE=DATE, DTEND exclusive), LOCATION omission when
// MeetupSpot is empty, a non-zero SEQUENCE (an update, not the initial
// invite), and the no-CN-parameter ATTENDEE shape (AttendeeName unset — a
// handle-less email invitee has no known display name yet).
func TestBuildRunInvite_AllDayNoLocation(t *testing.T) {
	got, err := BuildRunInvite(RunInviteInput{
		RunID:          "run-b",
		Name:           "South Platte",
		RunDate:        "2026-08-02",
		RunTime:        "",
		URL:            "https://h2oflows.app/plan-runs/run-b",
		Now:            time.Date(2026, 7, 23, 18, 4, 5, 0, time.UTC),
		Sequence:       3,
		OrganizerName:  "H2OFlows",
		OrganizerEmail: "trips@h2oflows.app",
		AttendeeEmail:  "b@example.com",
	})
	if err != nil {
		t.Fatalf("BuildRunInvite error: %v", err)
	}

	want := "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//h2oflows//Trip Calendar//EN\r\n" +
		"METHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:run-b@h2oflows.app\r\n" +
		"SEQUENCE:3\r\n" +
		"DTSTAMP:20260723T180405Z\r\n" +
		"ORGANIZER;CN=H2OFlows:mailto:trips@h2oflows.app\r\n" +
		"ATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:b@example.com\r\n" +
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

// TestBuildRunInvite_CancelGolden covers the CANCEL path added by API-1:
// METHOD:CANCEL, STATUS:CANCELLED, and a non-zero SEQUENCE (a cancellation
// always follows at least one prior REQUEST for the same UID). Also
// exercises LOCATION's TEXT-escaping (comma) on a fresh golden, distinct
// from CN's quoting rules (see TestQuoteParamValue below) — the two use
// different RFC 5545 mechanisms and must not be conflated.
func TestBuildRunInvite_CancelGolden(t *testing.T) {
	got, err := BuildRunInvite(RunInviteInput{
		RunID:          "33333333-3333-3333-3333-333333333333",
		Name:           "Clear Creek Play Run",
		RunDate:        "2026-08-05",
		RunTime:        "09:30",
		MeetupSpot:     "Kayak launch, upper lot",
		URL:            "https://h2oflows.app/plan-runs/33333333-3333-3333-3333-333333333333",
		Now:            time.Date(2026, 7, 23, 18, 4, 5, 0, time.UTC),
		Method:         MethodCancel,
		Sequence:       2,
		Cancelled:      true,
		OrganizerName:  "H2OFlows",
		OrganizerEmail: "trips@h2oflows.app",
		AttendeeName:   "Jamie",
		AttendeeEmail:  "jamie@example.com",
	})
	if err != nil {
		t.Fatalf("BuildRunInvite error: %v", err)
	}

	want := "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//h2oflows//Trip Calendar//EN\r\n" +
		"METHOD:CANCEL\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:33333333-3333-3333-3333-333333333333@h2oflows.app\r\n" +
		"SEQUENCE:2\r\n" +
		"DTSTAMP:20260723T180405Z\r\n" +
		"ORGANIZER;CN=H2OFlows:mailto:trips@h2oflows.app\r\n" +
		"ATTENDEE;CN=Jamie;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:jamie@example.com\r\n" +
		"DTSTART:20260805T093000\r\n" +
		"DURATION:PT2H\r\n" +
		"SUMMARY:Clear Creek Play Run\r\n" +
		"LOCATION:Kayak launch\\, upper lot\r\n" +
		"STATUS:CANCELLED\r\n" +
		"URL:https://h2oflows.app/plan-runs/33333333-3333-3333-3333-333333333333\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	if got != want {
		t.Errorf("BuildRunInvite mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestBuildRunInvite_RequiresOrganizer / _RequiresAttendee / _InvalidMethod
// lock in the RFC 5546 enforcement BuildRunInvite added for API-1: a
// REQUEST/CANCEL missing ORGANIZER or ATTENDEE, or carrying an unrecognized
// Method, must error rather than silently emit a malformed scheduling
// message (see the Err* vars' doc comments).
func TestBuildRunInvite_RequiresOrganizer(t *testing.T) {
	_, err := BuildRunInvite(RunInviteInput{
		RunID:         "r1",
		Name:          "Test",
		RunDate:       "2026-08-01",
		AttendeeEmail: "a@example.com",
	})
	if !errors.Is(err, ErrMissingOrganizer) {
		t.Errorf("err = %v, want ErrMissingOrganizer", err)
	}
}

func TestBuildRunInvite_RequiresAttendee(t *testing.T) {
	_, err := BuildRunInvite(RunInviteInput{
		RunID:          "r1",
		Name:           "Test",
		RunDate:        "2026-08-01",
		OrganizerEmail: "trips@h2oflows.app",
	})
	if !errors.Is(err, ErrMissingAttendee) {
		t.Errorf("err = %v, want ErrMissingAttendee", err)
	}
}

func TestBuildRunInvite_InvalidMethod(t *testing.T) {
	_, err := BuildRunInvite(RunInviteInput{
		RunID:          "r1",
		Name:           "Test",
		RunDate:        "2026-08-01",
		Method:         "PUBLISH",
		OrganizerEmail: "trips@h2oflows.app",
		AttendeeEmail:  "a@example.com",
	})
	if !errors.Is(err, ErrInvalidMethod) {
		t.Errorf("err = %v, want ErrInvalidMethod", err)
	}
}

// TestBuildRunInvite_CNQuoting exercises RFC 5545 §3.2 param-value quoting
// on a live BuildRunInvite call (not just the quoteParamValue unit test
// below): a CN containing a comma must come back double-quoted, never
// backslash-escaped (backslash-escaping is a TEXT-value rule — SUMMARY/
// LOCATION, see escapeText — not a param-value rule; conflating the two
// would produce a CN Apple/Google/Outlook parse as literal backslash
// characters instead of quoting). Deliberately short attendee/organizer
// data so the ATTENDEE content line stays under the 75-octet fold
// threshold — folding-with-quoting interaction isn't this test's concern
// (see TestBuildRunInvite_LongMultibyteName for fold mechanics).
func TestBuildRunInvite_CNQuoting(t *testing.T) {
	got, err := BuildRunInvite(RunInviteInput{
		RunID:          "r2",
		Name:           "Test Run",
		RunDate:        "2026-08-01",
		OrganizerEmail: "trips@h2oflows.app",
		AttendeeName:   "A, B",
		AttendeeEmail:  "ab@x.io",
	})
	if err != nil {
		t.Fatalf("BuildRunInvite error: %v", err)
	}
	want := `ATTENDEE;CN="A, B";PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:ab@x.io`
	if !strings.Contains(got, want) {
		t.Errorf("got %q, want a line containing %q", got, want)
	}
	if strings.Contains(got, `CN=A\, B`) {
		t.Error("CN comma was backslash-escaped (TEXT-value rule) instead of quoted (param-value rule)")
	}
}

// TestBuildRunInvite_CNControlChars: a CRLF inside AttendeeName (user-typed
// display_name is the only CN source that can carry one) must never split
// the ATTENDEE content line — that would be property injection into the
// VEVENT. Defense is layered (profile.go strips CTLs at the write boundary
// too); this pins the ics-side guarantee independently.
func TestBuildRunInvite_CNControlChars(t *testing.T) {
	got, err := BuildRunInvite(RunInviteInput{
		RunID:          "r3",
		Name:           "Test Run",
		RunDate:        "2026-08-01",
		OrganizerEmail: "trips@h2oflows.app",
		AttendeeName:   "Foo\r\nX-EVIL;CN=x",
		AttendeeEmail:  "foo@x.io",
	})
	if err != nil {
		t.Fatalf("BuildRunInvite error: %v", err)
	}
	// Unfold (RFC 5545 §3.1: CRLF + single space) before matching — the
	// legitimate 75-octet fold is not the injection under test.
	unfolded := strings.ReplaceAll(got, "\r\n ", "")
	want := `ATTENDEE;CN="FooX-EVIL;CN=x";PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:foo@x.io`
	if !strings.Contains(unfolded, want) {
		t.Errorf("CRLF in CN split the content line: got %q, want a line containing %q", got, want)
	}
}

// TestQuoteParamValue is the direct unit-level check of the RFC 5545 §3.2
// param-value quoting rules quoteParamValue implements: any embedded DQUOTE
// is stripped first (neither PTEXT nor QUOTED-STRING can represent one),
// and only THEN is the (now DQUOTE-free) result wrapped in DQUOTE — but
// only if it still contains a COLON, SEMICOLON, or COMMA; otherwise it's
// returned bare (PTEXT).
func TestQuoteParamValue(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"Plain Name": "Plain Name",
		"A, B":       `"A, B"`,
		"A; B":       `"A; B"`,
		"A: B":       `"A: B"`,
		`Say "hi"`:   `Say hi`,    // DQUOTE stripped; no :;, left -> bare
		`A, "B", C`:  `"A, B, C"`, // DQUOTE stripped, comma still forces quoting
		// CTLs dropped — RFC 5545 §3.2 permits no control char in a param
		// value; an embedded CRLF would otherwise split the content line
		// (property injection via user-typed display_name).
		"A\r\nB":                        "AB",
		"Foo\r\nATTENDEE;CN=x:mailto:x": `"FooATTENDEE;CN=x:mailto:x"`,
		"Tab\there":                     "Tabhere",
	}
	for in, want := range cases {
		if got := quoteParamValue(in); got != want {
			t.Errorf("quoteParamValue(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildRunInvite_LongMultibyteName is the folding case: a run name long
// enough (120 raw UTF-8 bytes) to force a fold, built entirely from a
// 2-byte-per-rune character so a naive byte-offset fold would land mid
// sequence roughly half the time. Golden output frozen the same way as
// TestBuildRunInvite_Golden, plus an explicit UTF-8-validity assertion on
// every folded line — the property that actually matters for Apple/Google
// import (a corrupted byte sequence is what breaks those clients, not the
// exact fold column). Retargeted for API-1's new required
// SEQUENCE/ORGANIZER/ATTENDEE lines (kept short/ASCII here — folding
// interaction with those is exercised structurally, not by golden byte
// count, since the point of this test is the multibyte SUMMARY fold).
func TestBuildRunInvite_LongMultibyteName(t *testing.T) {
	name := strings.Repeat("é", 60) // 60 runes x 2 bytes = 120 bytes
	got, err := BuildRunInvite(RunInviteInput{
		RunID:          "22222222-2222-2222-2222-222222222222",
		Name:           name,
		RunDate:        "2026-09-01",
		URL:            "https://h2oflows.app/plan-runs/22222222-2222-2222-2222-222222222222",
		Now:            time.Date(2026, 7, 23, 18, 4, 5, 0, time.UTC),
		OrganizerName:  "H2OFlows",
		OrganizerEmail: "trips@h2oflows.app",
		AttendeeEmail:  "c@example.com",
	})
	if err != nil {
		t.Fatalf("BuildRunInvite error: %v", err)
	}

	want := "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"PRODID:-//h2oflows//Trip Calendar//EN\r\n" +
		"METHOD:REQUEST\r\n" +
		"BEGIN:VEVENT\r\n" +
		"UID:22222222-2222-2222-2222-222222222222@h2oflows.app\r\n" +
		"SEQUENCE:0\r\n" +
		"DTSTAMP:20260723T180405Z\r\n" +
		"ORGANIZER;CN=H2OFlows:mailto:trips@h2oflows.app\r\n" +
		"ATTENDEE;PARTSTAT=NEEDS-ACTION;RSVP=TRUE:mailto:c@example.com\r\n" +
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
