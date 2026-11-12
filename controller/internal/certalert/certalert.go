// Package certalert sends the daily certificate expiry email to super-admins
// (F14, decision D-36).
//
// Scope: the certificates the provider owns and has to act on.
//   - Platform CAs (root, intermediate, workspace CAs) don't renew
//     themselves: listed from 30 days before expiry.
//   - Relay certificates and the controller's gRPC certificate renew
//     automatically: listed only within 24 hours of expiry or once expired,
//     which means the renewal didn't happen.
//   - Tenant agent certificates (connectors, shields, client devices) are
//     short-lived (typically 7 days) and renew themselves; they would appear
//     in every email, so they are left to the console's Certificates page.
//
// The candidates come from providerquery.CertificateExpiry, the same
// inventory the Certificates page shows. At most one email per UTC day:
// a Valkey marker is set once the day's email went to at least one
// recipient (or there was nothing to report), so a restart doesn't resend
// and a failed send is retried on the next check. Single controller (D-02).
// Every send is audited in provider_audit_logs; nothing secret is included
// (public certificate fields only: kind, name, serial, expiry).
package certalert

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/valkey-io/valkey-go/valkeycompat"

	"github.com/yourorg/ztna/controller/internal/provider"
	"github.com/yourorg/ztna/controller/internal/providerquery"
)

const (
	// Actor and action of the audit row written for each day's email.
	SystemActor = "system:cert-expiry-alert"
	AuditAction = "cert_expiry.alert_sent"

	// MaxListed bounds the entries listed per section; the rest are counted.
	MaxListed = 50

	markerPrefix = "provider:cert-expiry-alert:"
	markerTTL    = 48 * time.Hour

	// Window for each kind (see the package comment).
	caWindow        = 30 * 24 * time.Hour
	autoRenewWindow = 24 * time.Hour
)

// rule is one kind of certificate and how early it is listed.
type rule struct {
	kind   string
	within time.Duration
}

var rules = []rule{
	{"root", caWindow},
	{"intermediate", caWindow},
	{"workspace_ca", caWindow},
	{"controller_grpc", autoRenewWindow},
	{"relay", autoRenewWindow},
}

// Querier reads the inventory and the recipients (*pgxpool.Pool).
type Querier interface {
	providerquery.Querier
}

// Sender delivers one plain-text email.
type Sender interface {
	SendPlain(to, subject, body string) error
}

// Auditor writes the audit row (*provider.Store).
type Auditor interface {
	InsertAudit(ctx context.Context, e provider.AuditEntry) error
}

// Marker is the subset of Valkey used for the once-a-day marker.
type Marker interface {
	Get(ctx context.Context, key string) *valkeycompat.StringCmd
	Set(ctx context.Context, key string, value any, expiration time.Duration) *valkeycompat.StatusCmd
}

// Job is the daily expiry email.
type Job struct {
	DB         Querier
	Controller providerquery.ControllerCertSource // may be nil
	Sender     Sender
	Audit      Auditor
	Marker     Marker
	Now        func() time.Time // nil = time.Now
}

// Result of one Check, for logs and tests.
type Result struct {
	Skipped    string // why nothing was sent: "already_sent", "nothing_to_report", "no_recipients"
	Entries    int
	Recipients int
	Sent       int
}

// Run checks shortly after start and then hourly until ctx ends.
func (j *Job) Run(ctx context.Context, firstDelay, interval time.Duration) {
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			res, err := j.Check(ctx)
			switch {
			case err != nil:
				log.Printf("cert expiry alert: %v", err)
			case res.Sent > 0:
				log.Printf("cert expiry alert: sent to %d of %d super-admins (%d certificates)", res.Sent, res.Recipients, res.Entries)
			}
			timer.Reset(interval)
		}
	}
}

// Check sends today's email unless it was already sent today.
func (j *Job) Check(ctx context.Context) (Result, error) {
	now := time.Now().UTC()
	if j.Now != nil {
		now = j.Now().UTC()
	}
	day := now.Format("2006-01-02")
	key := markerPrefix + day

	switch err := j.Marker.Get(ctx, key).Err(); {
	case err == nil:
		return Result{Skipped: "already_sent"}, nil
	case !errors.Is(err, valkeycompat.Nil):
		return Result{}, fmt.Errorf("read daily marker: %w", err) // retry next check
	}

	entries, err := j.collect(ctx, now)
	if err != nil {
		return Result{}, err
	}
	if len(entries) == 0 {
		return Result{Skipped: "nothing_to_report"}, j.mark(ctx, key)
	}
	recipients, err := superAdmins(ctx, j.DB)
	if err != nil {
		return Result{}, err
	}
	res := Result{Entries: len(entries), Recipients: len(recipients)}
	if len(recipients) == 0 {
		res.Skipped = "no_recipients"
		return res, nil // nobody to tell; retried next check
	}

	subject, body := Compose(entries, now)
	var failed []string
	for _, to := range recipients {
		if err := j.Sender.SendPlain(to, subject, body); err != nil {
			log.Printf("cert expiry alert: send to %s: %v", to, err)
			failed = append(failed, to)
			continue
		}
		res.Sent++
	}
	if res.Sent == 0 {
		return res, fmt.Errorf("no email sent (%d recipients failed); retrying next check", len(failed))
	}

	counts := map[string]int{}
	for _, e := range entries {
		counts[e.Bucket]++
	}
	details := map[string]any{
		"recipients": len(recipients), "sent": res.Sent, "failed": len(failed),
		"certificates": len(entries),
		"expired":      counts[providerquery.BucketExpired],
		"lt_24h":       counts[providerquery.BucketLT24h],
		"lt_7d":        counts[providerquery.BucketLT7d],
		"lt_30d":       counts[providerquery.BucketLT30d],
	}
	if err := j.Audit.InsertAudit(ctx, provider.AuditEntry{
		ProviderEmail: SystemActor,
		Action:        AuditAction,
		TargetType:    "certificate_expiry",
		TargetID:      day,
		Details:       details,
	}); err != nil {
		// The email went out: mark the day anyway so it isn't resent.
		log.Printf("cert expiry alert: audit: %v", err)
	}
	return res, j.mark(ctx, key)
}

func (j *Job) mark(ctx context.Context, key string) error {
	if err := j.Marker.Set(ctx, key, "1", markerTTL).Err(); err != nil {
		return fmt.Errorf("set daily marker: %w", err)
	}
	return nil
}

// collect returns every certificate in scope, soonest first.
func (j *Job) collect(ctx context.Context, now time.Time) ([]providerquery.CertificateEntry, error) {
	var out []providerquery.CertificateEntry
	for _, r := range rules {
		cursor := ""
		for {
			page, err := providerquery.CertificateExpiry(ctx, j.DB, j.Controller, providerquery.CertificateFilter{
				Within: r.within, Kind: r.kind, Limit: 200, Cursor: cursor, Now: now,
			})
			if err != nil {
				return nil, fmt.Errorf("certificates (%s): %w", r.kind, err)
			}
			out = append(out, page.Items...)
			if page.NextCursor == nil {
				break
			}
			cursor = *page.NextCursor
		}
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].NotAfter.Before(out[b].NotAfter) })
	return out, nil
}

// superAdmins returns the active super-admins' emails.
func superAdmins(ctx context.Context, q Querier) ([]string, error) {
	rows, err := q.Query(ctx,
		`SELECT email FROM provider_users WHERE role = $1 AND disabled_at IS NULL ORDER BY email`, provider.RoleSuperAdmin)
	if err != nil {
		return nil, fmt.Errorf("list super-admins: %w", err)
	}
	emails, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("list super-admins: %w", err)
	}
	return emails, nil
}

var sections = []struct{ bucket, title string }{
	{providerquery.BucketExpired, "Expired"},
	{providerquery.BucketLT24h, "Expires within 24 hours"},
	{providerquery.BucketLT7d, "Expires within 7 days"},
	{providerquery.BucketLT30d, "Expires within 30 days"},
}

var kindLabel = map[string]string{
	"root": "Root CA", "intermediate": "Intermediate CA", "workspace_ca": "Workspace CA",
	"controller_grpc": "Controller gRPC certificate", "relay": "Relay",
}

// Compose builds the subject and plain-text body for entries (soonest first).
func Compose(entries []providerquery.CertificateEntry, now time.Time) (subject, body string) {
	n := len(entries)
	subject = fmt.Sprintf("Zecurity: %d certificates need attention", n)
	if n == 1 {
		subject = "Zecurity: 1 certificate needs attention"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Certificate expiry report for %s (UTC).\r\n", now.Format("2006-01-02"))
	for _, s := range sections {
		var in []providerquery.CertificateEntry
		for _, e := range entries {
			if e.Bucket == s.bucket {
				in = append(in, e)
			}
		}
		if len(in) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\r\n%s (%d)\r\n", s.title, len(in))
		for i, e := range in {
			if i == MaxListed {
				fmt.Fprintf(&b, "  ... and %d more (see Certificates in the provider console)\r\n", len(in)-MaxListed)
				break
			}
			b.WriteString("  - " + line(e) + "\r\n")
		}
	}
	b.WriteString("\r\nPlatform CAs are listed from 30 days before they expire. Relay and controller\r\n" +
		"certificates renew automatically, so they are listed only within 24 hours of\r\n" +
		"expiry, when a renewal is overdue. Tenant agent certificates (connectors,\r\n" +
		"shields, client devices) renew themselves and are not included; see\r\n" +
		"Certificates in the provider console.\r\n\r\n" +
		"You receive this because you are a super-admin. At most one email per day.\r\n")
	return subject, b.String()
}

func line(e providerquery.CertificateEntry) string {
	s := kindLabel[e.Kind]
	if s == "" {
		s = e.Kind
	}
	if e.EntityName != nil && *e.EntityName != "" && e.Kind != "root" && e.Kind != "intermediate" && e.Kind != "controller_grpc" {
		s += ` "` + clean(*e.EntityName) + `"`
	}
	if e.TenantID != nil {
		s += " (workspace " + *e.TenantID + ")"
	}
	if e.Serial != nil {
		s += ", serial " + clean(*e.Serial)
	}
	verb := "expires"
	if e.Bucket == providerquery.BucketExpired {
		verb = "expired"
	}
	return s + ": " + verb + " " + e.NotAfter.UTC().Format("2006-01-02 15:04 UTC")
}

// clean keeps a name on one line and short: control characters are dropped.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 100 {
		s = string(r[:100]) + "…"
	}
	return s
}
