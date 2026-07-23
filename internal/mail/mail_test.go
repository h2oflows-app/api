package mail

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// TestBuildResendPayloadMarshal locks the wire shape ResendMailer.Send
// depends on: attachment content must be base64 (Resend's /emails API
// expects it inline in the JSON body, not multipart), "to" must be an array
// even for a single recipient, and empty fields (no attachments, no Text)
// must be omitted rather than emitted as null/empty.
func TestBuildResendPayloadMarshal(t *testing.T) {
	msg := Message{
		To:      "paddler@example.com",
		Subject: "You're invited",
		HTML:    "<p>hi</p>",
		Text:    "hi",
		Attachments: []Attachment{{
			Filename:    "invite.ics",
			ContentType: "text/calendar; charset=utf-8; method=PUBLISH",
			Content:     []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"),
		}},
	}

	body, err := json.Marshal(buildResendPayload("H2OFlows <trips@h2oflows.app>", msg))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded["from"] != "H2OFlows <trips@h2oflows.app>" {
		t.Errorf("from = %v, want the configured MAIL_FROM value", decoded["from"])
	}
	to, ok := decoded["to"].([]any)
	if !ok || len(to) != 1 || to[0] != "paddler@example.com" {
		t.Errorf("to = %v, want [\"paddler@example.com\"]", decoded["to"])
	}
	if decoded["subject"] != "You're invited" {
		t.Errorf("subject = %v", decoded["subject"])
	}

	atts, ok := decoded["attachments"].([]any)
	if !ok || len(atts) != 1 {
		t.Fatalf("attachments = %v, want exactly 1", decoded["attachments"])
	}
	att, ok := atts[0].(map[string]any)
	if !ok {
		t.Fatalf("attachment[0] is not an object: %v", atts[0])
	}
	if att["filename"] != "invite.ics" {
		t.Errorf("attachment filename = %v", att["filename"])
	}
	if att["content_type"] != "text/calendar; charset=utf-8; method=PUBLISH" {
		t.Errorf("attachment content_type = %v", att["content_type"])
	}
	wantContent := base64.StdEncoding.EncodeToString([]byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"))
	if att["content"] != wantContent {
		t.Errorf("attachment content = %v, want base64 %v", att["content"], wantContent)
	}
}

// TestBuildResendPayloadMarshal_NoAttachments verifies the omitempty on
// Attachments — a plain handle-invite (no .ics) must not emit an
// "attachments":[] or "attachments":null key.
func TestBuildResendPayloadMarshal_NoAttachments(t *testing.T) {
	body, err := json.Marshal(buildResendPayload("H2OFlows <trips@h2oflows.app>", Message{
		To: "a@example.com", Subject: "hi",
	}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := decoded["attachments"]; present {
		t.Errorf("attachments key present with no attachments: %v", decoded["attachments"])
	}
	if _, present := decoded["html"]; present {
		t.Errorf("html key present with empty HTML: %v", decoded["html"])
	}
}
