// Package mail sends transactional email for plan invites (#246 A4). Two
// implementations: ResendMailer, a single hand-wired HTTPS POST to Resend's
// API — no SDK, matching the rest of this codebase's hand-rolled HTTP client
// style (e.g. internal/nldi.Client) and avoiding the port-25 container
// egress issue that already bit DWR polling (feedback_dwr_container_connectivity.md)
// — and NoopMailer, which logs instead of sending when RESEND_API_KEY is
// unset, mirroring the nil-client degrade pattern used for the AI features
// in main.go (enricher/asker/describer all go nil when ANTHROPIC_API_KEY is
// absent).
package mail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

// Attachment is a single file attached to a Message. Content is raw bytes —
// Resend's /emails API expects base64 in the JSON body, so callers pass the
// decoded content and the ResendMailer encodes it at send time.
type Attachment struct {
	Filename    string
	ContentType string
	Content     []byte
}

// Message is provider-agnostic; Mailer implementations translate it into
// their own wire format.
type Message struct {
	To          string
	Subject     string
	HTML        string
	Text        string
	Attachments []Attachment
}

// Mailer sends a single Message. Implementations must be safe to call from a
// detached goroutine — invite sends are fired async with their own
// context.Background()-derived context (see internal/handlers/invites.go
// sendInviteMail) because the triggering HTTP request's context dies at
// response.
type Mailer interface {
	Send(ctx context.Context, msg Message) error
}

// NoopMailer logs the recipient + subject instead of sending. Used when
// RESEND_API_KEY is unset (local dev / CI) — the degrade-cleanly counterpart
// to the nil ai.* clients in main.go.
type NoopMailer struct{}

// Send implements Mailer.
func (NoopMailer) Send(_ context.Context, msg Message) error {
	log.Printf("mail (noop, RESEND_API_KEY unset): to=%s subject=%q attachments=%d", msg.To, msg.Subject, len(msg.Attachments))
	return nil
}

// ResendMailer sends via a single POST https://api.resend.com/emails — hand
// wired net/http, JSON body, base64 attachments, no SDK.
type ResendMailer struct {
	apiKey string
	from   string
	hc     *http.Client
}

// NewResendMailer constructs a ResendMailer. from is the RFC 5322 From
// address/header value (e.g. "H2OFlows <trips@h2oflows.app>") — see
// internal/config MAIL_FROM.
func NewResendMailer(apiKey, from string) *ResendMailer {
	return &ResendMailer{apiKey: apiKey, from: from, hc: &http.Client{Timeout: 15 * time.Second}}
}

type resendAttachment struct {
	Filename    string `json:"filename"`
	Content     string `json:"content"` // base64
	ContentType string `json:"content_type,omitempty"`
}

type resendPayload struct {
	From        string             `json:"from"`
	To          []string           `json:"to"`
	Subject     string             `json:"subject"`
	HTML        string             `json:"html,omitempty"`
	Text        string             `json:"text,omitempty"`
	Attachments []resendAttachment `json:"attachments,omitempty"`
}

// buildResendPayload is split out from Send so the JSON shape (base64
// attachment encoding in particular) is unit-testable without a network
// call — see mail_test.go.
func buildResendPayload(from string, msg Message) resendPayload {
	payload := resendPayload{
		From:    from,
		To:      []string{msg.To},
		Subject: msg.Subject,
		HTML:    msg.HTML,
		Text:    msg.Text,
	}
	for _, a := range msg.Attachments {
		payload.Attachments = append(payload.Attachments, resendAttachment{
			Filename:    a.Filename,
			Content:     base64.StdEncoding.EncodeToString(a.Content),
			ContentType: a.ContentType,
		})
	}
	return payload
}

// Send implements Mailer.
func (m *ResendMailer) Send(ctx context.Context, msg Message) error {
	body, err := json.Marshal(buildResendPayload(m.from, msg))
	if err != nil {
		return fmt.Errorf("mail: marshal payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.resend.com/emails", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("mail: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.apiKey)

	resp, err := m.hc.Do(req)
	if err != nil {
		return fmt.Errorf("mail: send: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("mail: resend api %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
