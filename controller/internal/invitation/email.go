package invitation

import (
	"fmt"
	"log"
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
