package invitation

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"mime"
	"mime/quotedprintable"
	"net/smtp"
	"strings"
)

// Emailer sends invitation emails via SMTP.
// If SMTP_HOST is empty, it falls back to stdout (dev mode).
type Emailer struct {
	host     string
	port     string
	from     string
	password string
	baseURL  string // e.g. "https://app.zecurity.example.com"
}

// NewEmailer constructs an Emailer from explicit config values rather than
// reading env vars directly, keeping the struct testable and main.go explicit.
func NewEmailer(host, port, from, password, baseURL string) *Emailer {
	return &Emailer{
		host:     host,
		port:     port,
		from:     from,
		password: password,
		baseURL:  baseURL,
	}
}

// SendInvitation emails the invitation link to inv.Email.
// If SMTP is not configured (host == ""), logs to stdout instead.
func (e *Emailer) SendInvitation(inv *Invitation, workspaceName string) error {
	link := fmt.Sprintf("%s/invite/%s", e.baseURL, inv.Token)

	if e.host == "" {
		log.Printf("[INVITE] To: %s | Workspace: %s | Link: %s", inv.Email, workspaceName, link)
		return nil
	}

	body := fmt.Sprintf(
		"You've been invited to join %s on Zecurity.\r\n\r\nAccept here: %s\r\n\r\nThis invitation expires in 7 days.",
		workspaceName, link,
	)

	msg := strings.Join([]string{
		"From: " + e.from,
		"To: " + inv.Email,
		"Subject: You've been invited to " + workspaceName + " on Zecurity",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		body,
	}, "\r\n")

	auth := smtp.PlainAuth("", e.from, e.password, e.host)
	if err := smtp.SendMail(e.host+":"+e.port, auth, e.from, []string{inv.Email}, []byte(msg)); err != nil {
		return fmt.Errorf("send invitation email: %w", err)
	}
	return nil
}

// SendPlain sends one plain-text email (used by the certificate expiry alert,
// F14). Subject and recipient must be single lines. If SMTP is not configured
// (host == ""), it logs the subject and recipient to stdout instead (dev mode).
func (e *Emailer) SendPlain(to, subject, body string) error {
	if strings.ContainsAny(to, "\r\n") || strings.ContainsAny(subject, "\r\n") {
		return fmt.Errorf("send email: recipient and subject must be single lines")
	}
	if e.host == "" {
		log.Printf("[EMAIL] To: %s | Subject: %s (SMTP not configured; not sent)", to, subject)
		return nil
	}
	msg := strings.Join([]string{
		"From: " + e.from,
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		body,
	}, "\r\n")
	auth := smtp.PlainAuth("", e.from, e.password, e.host)
	if err := smtp.SendMail(e.host+":"+e.port, auth, e.from, []string{to}, []byte(msg)); err != nil {
		return fmt.Errorf("send email: %w", err)
	}
	return nil
}

// SendMultipart sends one email with a plain-text and an HTML part
// (multipart/alternative; used by the Sprint 26 invoice email, D-68). The
// subject is RFC 2047 encoded, both parts are quoted-printable UTF-8.
// Recipient and subject must be single lines. If SMTP is not configured
// (host == ""), it logs the subject only (never the recipient or body) and
// sends nothing (dev mode).
func (e *Emailer) SendMultipart(to, subject, text, html string) error {
	if strings.ContainsAny(to, "\r\n") || strings.ContainsAny(subject, "\r\n") {
		return fmt.Errorf("send email: recipient and subject must be single lines")
	}
	if e.host == "" {
		log.Printf("[EMAIL] Subject: %s (SMTP not configured; not sent)", subject)
		return nil
	}
	boundary, err := newBoundary()
	if err != nil {
		return fmt.Errorf("send email: %w", err)
	}
	msg, err := buildMultipart(e.from, to, subject, text, html, boundary)
	if err != nil {
		return fmt.Errorf("send email: %w", err)
	}
	auth := smtp.PlainAuth("", e.from, e.password, e.host)
	if err := smtp.SendMail(e.host+":"+e.port, auth, e.from, []string{to}, msg); err != nil {
		return fmt.Errorf("send email: %w", err)
	}
	return nil
}

func newBoundary() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "zecurity-" + hex.EncodeToString(b), nil
}

// buildMultipart renders the message; pure, for tests.
func buildMultipart(from, to, subject, text, html, boundary string) ([]byte, error) {
	var b bytes.Buffer
	for _, h := range []string{
		"From: " + from,
		"To: " + to,
		"Subject: " + mime.QEncoding.Encode("utf-8", subject),
		"MIME-Version: 1.0",
		`Content-Type: multipart/alternative; boundary="` + boundary + `"`,
		"",
	} {
		b.WriteString(h + "\r\n")
	}
	for _, part := range []struct{ ctype, body string }{
		{"text/plain; charset=utf-8", text},
		{"text/html; charset=utf-8", html},
	} {
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Type: " + part.ctype + "\r\n")
		b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		w := quotedprintable.NewWriter(&b)
		if _, err := w.Write([]byte(part.body)); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		b.WriteString("\r\n")
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes(), nil
}
