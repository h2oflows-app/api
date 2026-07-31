package handlers

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/h2oflow/h2oflow/apps/api/internal/mail"
)

const (
	testRunID = "11111111-1111-1111-1111-111111111111"
	testUID   = testRunID + "@h2oflows.app"
)

// buildICS assembles a minimal, valid VCALENDAR/VEVENT (CRLF line endings)
// for test fixtures — attendeeLine is inserted verbatim so folding tests can
// pass a value that already contains an embedded "\r\n " continuation.
func buildICS(method, uid, attendeeLine string) string {
	lines := []string{
		"BEGIN:VCALENDAR",
		"VERSION:2.0",
		"PRODID:-//Test//EN",
		"METHOD:" + method,
		"BEGIN:VEVENT",
		"UID:" + uid,
		"DTSTAMP:20260722T120000Z",
		attendeeLine,
		"END:VEVENT",
		"END:VCALENDAR",
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}

// gmailStyleMessage builds a multipart/alternative message (text/plain +
// inline text/calendar, base64-encoded) — Gmail's shape for a REPLY.
func gmailStyleMessage(uid, attendeeEmail, partstat string) []byte {
	attendeeLine := fmt.Sprintf("ATTENDEE;CN=Test Attendee;PARTSTAT=%s:mailto:%s", partstat, attendeeEmail)
	ics := buildICS("REPLY", uid, attendeeLine)
	b64 := base64.StdEncoding.EncodeToString([]byte(ics))

	msg := "From: Test Attendee <" + attendeeEmail + ">\r\n" +
		"To: invites@h2oflows.app\r\n" +
		"Subject: Accepted: Trip\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/alternative; boundary=\"BOUND1\"\r\n" +
		"\r\n" +
		"--BOUND1\r\n" +
		"Content-Type: text/plain; charset=\"UTF-8\"\r\n" +
		"\r\n" +
		"Test Attendee has responded to this invitation.\r\n" +
		"\r\n" +
		"--BOUND1\r\n" +
		"Content-Type: text/calendar; charset=\"UTF-8\"; method=REPLY\r\n" +
		"Content-Transfer-Encoding: base64\r\n" +
		"\r\n" +
		b64 + "\r\n" +
		"\r\n" +
		"--BOUND1--\r\n"
	return []byte(msg)
}

// outlookStyleMessage builds a multipart/mixed message with a separate
// invite.ics attachment, quoted-printable-encoded — Outlook's shape for a
// REPLY.
func outlookStyleMessage(uid, attendeeEmail, partstat string) []byte {
	attendeeLine := fmt.Sprintf("ATTENDEE;PARTSTAT=%s;CN=Test Attendee:mailto:%s", partstat, attendeeEmail)
	ics := buildICS("REPLY", uid, attendeeLine)

	var qpBuf bytes.Buffer
	qpw := quotedprintable.NewWriter(&qpBuf)
	_, _ = qpw.Write([]byte(ics))
	_ = qpw.Close()

	msg := "From: Test Attendee <" + attendeeEmail + ">\r\n" +
		"To: invites@h2oflows.app\r\n" +
		"Subject: Declined: Trip\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"BOUND2\"\r\n" +
		"\r\n" +
		"--BOUND2\r\n" +
		"Content-Type: text/plain; charset=\"us-ascii\"\r\n" +
		"\r\n" +
		"Test Attendee has responded to this invitation.\r\n" +
		"\r\n" +
		"--BOUND2\r\n" +
		"Content-Type: application/ics; name=\"invite.ics\"\r\n" +
		"Content-Disposition: attachment; filename=\"invite.ics\"\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n" +
		"\r\n" +
		qpBuf.String() +
		"\r\n" +
		"--BOUND2--\r\n"
	return []byte(msg)
}

// directCalendarMessage builds a non-multipart message whose entire body IS
// the calendar (Content-Type: text/calendar at the top level, no
// transfer-encoding) — used for the folded-ATTENDEE-line cases, where the
// only thing under test is unfold+param-order handling, not MIME nesting.
func directCalendarMessage(fromEmail, uid, attendeeLine string) []byte {
	ics := buildICS("REPLY", uid, attendeeLine)
	msg := "From: Test Attendee <" + fromEmail + ">\r\n" +
		"To: invites@h2oflows.app\r\n" +
		"Subject: Accepted: Trip\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/calendar; method=REPLY; charset=UTF-8\r\n" +
		"\r\n" +
		ics
	return []byte(msg)
}

func TestParseRSVPReply(t *testing.T) {
	t.Run("Gmail-style multipart/alternative, inline base64, ACCEPTED", func(t *testing.T) {
		raw := gmailStyleMessage(testUID, "jane@example.com", "ACCEPTED")
		got, ignoreReason, err := parseRSVPReply(raw)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if ignoreReason != "" {
			t.Fatalf("ignoreReason = %q, want \"\"", ignoreReason)
		}
		if got.RunID != testRunID {
			t.Errorf("RunID = %q, want %q", got.RunID, testRunID)
		}
		if got.Attendee != "jane@example.com" {
			t.Errorf("Attendee = %q, want jane@example.com", got.Attendee)
		}
		if got.FromEmail != "jane@example.com" {
			t.Errorf("FromEmail = %q, want jane@example.com", got.FromEmail)
		}
		if got.PartStat != "ACCEPTED" {
			t.Errorf("PartStat = %q, want ACCEPTED", got.PartStat)
		}
	})

	t.Run("Outlook-style multipart/mixed, .ics attachment, quoted-printable, DECLINED", func(t *testing.T) {
		raw := outlookStyleMessage(testUID, "john@example.com", "DECLINED")
		got, ignoreReason, err := parseRSVPReply(raw)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if ignoreReason != "" {
			t.Fatalf("ignoreReason = %q, want \"\"", ignoreReason)
		}
		if got.RunID != testRunID {
			t.Errorf("RunID = %q, want %q", got.RunID, testRunID)
		}
		if got.Attendee != "john@example.com" {
			t.Errorf("Attendee = %q, want john@example.com", got.Attendee)
		}
		if got.PartStat != "DECLINED" {
			t.Errorf("PartStat = %q, want DECLINED", got.PartStat)
		}
	})

	t.Run("folded ATTENDEE line, PARTSTAT after CN", func(t *testing.T) {
		// Folds mid-value: "...PARTSTAT=ACCEP" + CRLF+space continuation
		// "TED:mailto:jane@example.com" — RFC 5545 folding can land
		// anywhere, even mid-word.
		attendeeLine := "ATTENDEE;CN=Test Attendee;PARTSTAT=ACCEP\r\n TED:mailto:jane@example.com"
		raw := directCalendarMessage("jane@example.com", testUID, attendeeLine)
		got, ignoreReason, err := parseRSVPReply(raw)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if ignoreReason != "" {
			t.Fatalf("ignoreReason = %q, want \"\"", ignoreReason)
		}
		if got.Attendee != "jane@example.com" {
			t.Errorf("Attendee = %q, want jane@example.com", got.Attendee)
		}
		if got.PartStat != "ACCEPTED" {
			t.Errorf("PartStat = %q, want ACCEPTED", got.PartStat)
		}
	})

	t.Run("folded ATTENDEE line, PARTSTAT before CN", func(t *testing.T) {
		attendeeLine := "ATTENDEE;PARTSTAT=DECLINED;CN=John Boa\r\n ter:mailto:john@example.com"
		raw := directCalendarMessage("john@example.com", testUID, attendeeLine)
		got, ignoreReason, err := parseRSVPReply(raw)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if ignoreReason != "" {
			t.Fatalf("ignoreReason = %q, want \"\"", ignoreReason)
		}
		if got.Attendee != "john@example.com" {
			t.Errorf("Attendee = %q, want john@example.com", got.Attendee)
		}
		if got.PartStat != "DECLINED" {
			t.Errorf("PartStat = %q, want DECLINED", got.PartStat)
		}
	})

	t.Run("METHOD:REQUEST -> ignored", func(t *testing.T) {
		attendeeLine := "ATTENDEE;PARTSTAT=NEEDS-ACTION:mailto:jane@example.com"
		ics := buildICS("REQUEST", testUID, attendeeLine)
		msg := "From: Test Attendee <jane@example.com>\r\n" +
			"To: invites@h2oflows.app\r\n" +
			"Content-Type: text/calendar; method=REQUEST\r\n" +
			"\r\n" + ics
		_, ignoreReason, err := parseRSVPReply([]byte(msg))
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if ignoreReason == "" {
			t.Fatal("ignoreReason = \"\", want non-empty (METHOD is not REPLY)")
		}
	})

	t.Run("no calendar part -> ignored", func(t *testing.T) {
		msg := "From: Human <human@example.com>\r\n" +
			"To: invites@h2oflows.app\r\n" +
			"Subject: Re: Trip\r\n" +
			"Content-Type: text/plain; charset=\"UTF-8\"\r\n" +
			"\r\n" +
			"Sounds good, see you there!\r\n"
		_, ignoreReason, err := parseRSVPReply([]byte(msg))
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if ignoreReason == "" {
			t.Fatal("ignoreReason = \"\", want non-empty (no calendar part)")
		}
	})

	t.Run("UID not ours -> ignored", func(t *testing.T) {
		raw := gmailStyleMessage(testRunID+"@some-other-app.example", "jane@example.com", "ACCEPTED")
		_, ignoreReason, err := parseRSVPReply(raw)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if ignoreReason == "" {
			t.Fatal("ignoreReason = \"\", want non-empty (UID not ours)")
		}
	})

	t.Run("bare-LF line endings", func(t *testing.T) {
		raw := gmailStyleMessage(testUID, "jane@example.com", "ACCEPTED")
		bareLF := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
		got, ignoreReason, err := parseRSVPReply(bareLF)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if ignoreReason != "" {
			t.Fatalf("ignoreReason = %q, want \"\"", ignoreReason)
		}
		if got.RunID != testRunID {
			t.Errorf("RunID = %q, want %q", got.RunID, testRunID)
		}
		if got.PartStat != "ACCEPTED" {
			t.Errorf("PartStat = %q, want ACCEPTED", got.PartStat)
		}
	})

	// From != ATTENDEE anti-spoof: the reject decision lives in Inbound
	// (handler), not in the parser (parseRSVPReply's job is only to report
	// what the message SAYS) — this test documents that parseRSVPReply
	// succeeds and surfaces both addresses so the caller can compare them.
	t.Run("From != ATTENDEE — parser still succeeds; caller must reject", func(t *testing.T) {
		raw := gmailStyleMessage(testUID, "jane@example.com", "ACCEPTED")
		raw = bytes.Replace(raw, []byte("From: Test Attendee <jane@example.com>"), []byte("From: Eve <eve@evil.example>"), 1)
		got, ignoreReason, err := parseRSVPReply(raw)
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if ignoreReason != "" {
			t.Fatalf("ignoreReason = %q, want \"\" (parser doesn't know about spoofing)", ignoreReason)
		}
		if strings.EqualFold(got.FromEmail, got.Attendee) {
			t.Fatalf("test fixture bug: FromEmail (%q) unexpectedly matches Attendee (%q)", got.FromEmail, got.Attendee)
		}
	})
}

func TestRedactEmail(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"foo@example.com", "f***@example.com"},
		{"a@x.io", "a***@x.io"},
		{"no-at-sign", "***"},
		{"@x.io", "***"},
	}
	for _, tc := range cases {
		if got := redactEmail(tc.in); got != tc.want {
			t.Errorf("redactEmail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── Handler-level: secret gate only. Everything past the secret/size check
// touches h.db — no live-DB test harness exists in this codebase for
// handlers (see nudges_test.go, the only other internal/handlers test file,
// which tests pure functions; grep for httptest across internal/handlers
// turns up nothing), so those paths (run/invitee resolution, the
// accept/decline transition) are exercised only via parseRSVPReply +
// applyRSVP's SQL being reviewed by hand, not by an automated test here.
// db=nil is safe for every case below because Inbound returns before ever
// touching h.db on each of these paths (secret check, then a parse
// failure/ignore, both short-circuit first).

func TestRSVPInboundHandler_SecretGate(t *testing.T) {
	t.Run("RSVP_INBOUND_SECRET unset -> 503, never touches DB", func(t *testing.T) {
		h := NewRSVPInboundHandler(nil, mail.NoopMailer{}, "")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/rsvp-inbound", strings.NewReader(""))
		w := httptest.NewRecorder()
		h.Inbound(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want %d", w.Code, http.StatusServiceUnavailable)
		}
	})

	t.Run("wrong secret -> 401", func(t *testing.T) {
		h := NewRSVPInboundHandler(nil, mail.NoopMailer{}, "correct-secret")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/rsvp-inbound", strings.NewReader(""))
		req.Header.Set("X-RSVP-Secret", "wrong-secret")
		w := httptest.NewRecorder()
		h.Inbound(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
	})

	t.Run("missing secret header -> 401", func(t *testing.T) {
		h := NewRSVPInboundHandler(nil, mail.NoopMailer{}, "correct-secret")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/rsvp-inbound", strings.NewReader(""))
		w := httptest.NewRecorder()
		h.Inbound(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("status = %d, want %d", w.Code, http.StatusUnauthorized)
		}
	})

	t.Run("correct secret, unparseable body -> 200 ignored, never touches DB", func(t *testing.T) {
		h := NewRSVPInboundHandler(nil, mail.NoopMailer{}, "correct-secret")
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/rsvp-inbound", strings.NewReader("not a mime message at all"))
		req.Header.Set("X-RSVP-Secret", "correct-secret")
		w := httptest.NewRecorder()
		h.Inbound(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
		}
		if !strings.Contains(w.Body.String(), `"ignored"`) {
			t.Errorf("body = %s, want status:ignored", w.Body.String())
		}
	})

	t.Run("correct secret, oversized body -> 413", func(t *testing.T) {
		h := NewRSVPInboundHandler(nil, mail.NoopMailer{}, "correct-secret")
		big := strings.NewReader(strings.Repeat("a", rsvpInboundMaxBytes+1))
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/rsvp-inbound", big)
		req.Header.Set("X-RSVP-Secret", "correct-secret")
		w := httptest.NewRecorder()
		h.Inbound(w, req)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
		}
	})
}
