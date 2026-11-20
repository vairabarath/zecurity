package alerting

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// PayloadVersion is the webhook payload version (D-52). Bump only with a
// new, additive version; receivers branch on it.
const PayloadVersion = 1

// Payload is the generic, versioned webhook body (D-52): exactly these
// fields, no secrets, tokens or tenant content. Tenant alerts are only
// routed to channels allowed to see them (scope_filter all or tenant), and
// platform alerts never carry a tenant (D-50), so target.name and summary
// are safe for every channel that receives them.
type Payload struct {
	Version    int           `json:"version"`
	EventID    string        `json:"event_id"`
	EventType  string        `json:"event_type"`
	Status     string        `json:"status"`
	Severity   string        `json:"severity"`
	Scope      string        `json:"scope"`
	TenantID   *string       `json:"tenant_id"`
	Target     PayloadTarget `json:"target"`
	Summary    string        `json:"summary"`
	StartedAt  string        `json:"started_at"`
	Timestamp  string        `json:"timestamp"`
	ConsoleURL *string       `json:"console_url"`
}

// PayloadTarget is the alert's subject.
type PayloadTarget struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// NewPayload builds the payload of an event. consoleBase is the provider
// console's origin (PROVIDER_CONSOLE_URL); "" leaves console_url null.
func NewPayload(e Event, consoleBase string) Payload {
	p := Payload{
		Version: PayloadVersion, EventID: e.ID, EventType: e.Kind, Status: e.Status,
		Severity: e.Severity, Scope: e.Scope, TenantID: e.TenantID,
		Target:    PayloadTarget{Type: e.TargetType, ID: e.TargetID, Name: e.TargetName},
		Summary:   e.Summary,
		StartedAt: e.StartedAt.UTC().Format(time.RFC3339), Timestamp: e.OccurredAt.UTC().Format(time.RFC3339),
	}
	if base := strings.TrimRight(consoleBase, "/"); base != "" {
		u := base + "/alerts?event=" + e.ID
		p.ConsoleURL = &u
	}
	return p
}

// JSON encodes the payload.
func (p Payload) JSON() []byte {
	b, _ := json.Marshal(p)
	return b
}

// EmailSubject and EmailBody render the same fields as plain text.
func (p Payload) EmailSubject() string {
	return oneLine(fmt.Sprintf("[Zecurity %s] %s: %s", strings.ToUpper(p.Severity), p.Status, p.Summary), 200)
}

// EmailBody is the plain-text body of an alert email.
func (p Payload) EmailBody() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", p.Summary)
	fmt.Fprintf(&b, "Status:   %s\n", p.Status)
	fmt.Fprintf(&b, "Severity: %s\n", p.Severity)
	fmt.Fprintf(&b, "Scope:    %s\n", p.Scope)
	if p.TenantID != nil {
		fmt.Fprintf(&b, "Tenant:   %s\n", *p.TenantID)
	}
	fmt.Fprintf(&b, "Target:   %s %s (%s)\n", p.Target.Type, p.Target.Name, p.Target.ID)
	fmt.Fprintf(&b, "Type:     %s\n", p.EventType)
	fmt.Fprintf(&b, "Started:  %s\n", p.StartedAt)
	fmt.Fprintf(&b, "Time:     %s\n", p.Timestamp)
	if p.ConsoleURL != nil {
		fmt.Fprintf(&b, "\n%s\n", *p.ConsoleURL)
	}
	fmt.Fprintf(&b, "\nEvent %s\n", p.EventID)
	return b.String()
}

// oneLine strips line breaks (a header must be one line) and caps length.
func oneLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' {
			return ' '
		}
		return r
	}, s)
	return truncate(s, n)
}
