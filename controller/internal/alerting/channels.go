package alerting

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yourorg/ztna/controller/internal/auditexport"
)

// Channel types (D-52): one generic webhook, and email.
const (
	ChannelWebhook = "webhook"
	ChannelEmail   = "email"
)

// Channel scope filters (D-50).
const (
	FilterAll      = "all"
	FilterPlatform = ScopePlatform
	FilterTenant   = ScopeTenant
)

var (
	// ErrInvalidChannel: a channel field is out of range.
	ErrInvalidChannel = errors.New("alerting: invalid channel")
	// ErrInvalidURL: the webhook URL is refused (scheme, host, private address).
	ErrInvalidURL = errors.New("alerting: invalid webhook url")
	// ErrChannelNameTaken: another channel has that name.
	ErrChannelNameTaken = errors.New("alerting: channel name taken")
)

// Sealer seals webhook secrets at rest (pki.EncryptSecret).
type Sealer interface {
	EncryptSecret(plaintext []byte, context string) (ciphertextB64, nonceB64 string, err error)
	DecryptSecret(ciphertextB64, nonceB64, context string) ([]byte, error)
}

func secretContext(channelID string) string { return "alert-webhook-secret:" + channelID }

// Channel is a delivery channel as the API shows it. It never carries the
// secret, and the webhook URL only as scheme and host: a path or query
// often holds a token (a Slack or PagerDuty URL is itself a credential).
type Channel struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Type        string    `json:"type"`
	ScopeFilter string    `json:"scope_filter"`
	MinSeverity string    `json:"min_severity"`
	Enabled     bool      `json:"enabled"`
	URLHost     *string   `json:"url_host"`
	Recipients  []string  `json:"recipients"` // email; null = every active super-admin
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// AuditView is the channel as recorded in the provider audit (no secret,
// redacted URL).
func (c Channel) AuditView() map[string]any {
	v := map[string]any{"name": c.Name, "type": c.Type, "scope_filter": c.ScopeFilter,
		"min_severity": c.MinSeverity, "enabled": c.Enabled, "recipients": c.Recipients}
	if c.URLHost != nil {
		v["url_host"] = *c.URLHost
	}
	return v
}

// channelRow is the stored channel, including the URL and the sealed secret.
// It never leaves this package's delivery path.
type channelRow struct {
	Channel
	url             *string
	encryptedSecret *string
	secretNonce     *string
}

const channelColumns = `id::text, name, type, scope_filter, min_severity, enabled, url, encrypted_secret, secret_nonce,
       recipients, created_at, updated_at`

func scanChannel(row pgx.Row) (channelRow, error) {
	var c channelRow
	if err := row.Scan(&c.ID, &c.Name, &c.Type, &c.ScopeFilter, &c.MinSeverity, &c.Enabled, &c.url,
		&c.encryptedSecret, &c.secretNonce, &c.Recipients, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return channelRow{}, err
	}
	if c.url != nil {
		h := auditexport.RedactURL(*c.url)
		c.URLHost = &h
	}
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	return c, nil
}

// URLPolicy is how strictly webhook URLs are checked (D-52): HTTPS and
// public addresses only, unless the dev flag is set with ENV=development.
type URLPolicy struct {
	AllowPrivate bool // dev: private and loopback targets, and plain http
}

// ValidateWebhookURL checks a webhook URL: https (http only in dev), a
// host, no credentials or fragment, no literal private address (names are
// checked again when dialled).
func (p URLPolicy) ValidateWebhookURL(raw string) error {
	raw = strings.TrimSpace(raw)
	check := raw
	if p.AllowPrivate {
		if u, err := url.Parse(raw); err == nil && u.Scheme == "http" {
			u.Scheme = "https"
			check = u.String()
		}
	}
	if err := auditexport.ValidateURL(check, p.AllowPrivate); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidURL, err)
	}
	// ValidateURL lets a hostname through for the dial-time check (which
	// checks the resolved address). Refuse here too a host that is an IP in
	// an unusual spelling (0x7f000001, 2130706433, 127.1): some resolvers
	// read it as an address, and no real webhook host looks like that.
	if u, err := url.Parse(raw); err == nil && !p.AllowPrivate {
		host := strings.Trim(u.Hostname(), "[]")
		if ip, err := netip.ParseAddr(host); err == nil && !auditexport.AddressAllowed(ip) {
			return fmt.Errorf("%w: private or reserved address", ErrInvalidURL)
		}
		if _, err := netip.ParseAddr(host); err != nil && numericHost.MatchString(host) {
			return fmt.Errorf("%w: numeric host must be a plain IP address", ErrInvalidURL)
		}
	}
	return nil
}

// numericHost matches hosts made only of numeric labels (decimal, octal or
// hex), i.e. IPv4 in a non-canonical spelling.
var numericHost = regexp.MustCompile(`(?i)^(0x[0-9a-f]*|[0-9]+)(\.(0x[0-9a-f]*|[0-9]+))*\.?$`)

// ChannelInput is a channel to create or the fields to change (nil =
// unchanged on update).
type ChannelInput struct {
	Name        *string
	Type        string // create only
	ScopeFilter *string
	MinSeverity *string
	Enabled     *bool
	URL         *string   // webhook
	Recipients  *[]string // email; pointer to nil slice = every super-admin
}

func validName(n string) bool {
	t := strings.TrimSpace(n)
	if t == "" || t != n || utf8.RuneCountInString(n) > 100 {
		return false
	}
	for _, r := range n {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validRecipients(list []string) error {
	if list == nil {
		return nil
	}
	if len(list) == 0 || len(list) > 50 {
		return fmt.Errorf("%w: 1..50 recipients, or none for every super-admin", ErrInvalidChannel)
	}
	seen := map[string]bool{}
	for _, r := range list {
		a, err := mail.ParseAddress(r)
		if err != nil || a.Address != r || a.Name != "" || strings.ContainsAny(r, "\r\n") || len(r) > 254 {
			return fmt.Errorf("%w: invalid recipient", ErrInvalidChannel)
		}
		if seen[strings.ToLower(r)] {
			return fmt.Errorf("%w: duplicate recipient", ErrInvalidChannel)
		}
		seen[strings.ToLower(r)] = true
	}
	return nil
}

// newSecret returns a fresh webhook signing secret (256 bits).
func newSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "whsec_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// ChannelAuditFunc writes the provider audit row with the change.
type ChannelAuditFunc func(ctx context.Context, tx pgx.Tx, before, after *Channel) error

// ChannelStore manages channels. Seal seals webhook secrets.
type ChannelStore struct {
	store  *Store
	seal   Sealer
	policy URLPolicy
}

// NewChannelStore returns a ChannelStore.
func NewChannelStore(s *Store, seal Sealer, policy URLPolicy) *ChannelStore {
	return &ChannelStore{store: s, seal: seal, policy: policy}
}

// ListChannels returns every channel (no secrets, redacted URLs).
func (cs *ChannelStore) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := cs.store.pool.Query(ctx, `SELECT `+channelColumns+` FROM alert_channels ORDER BY name, id`)
	if err != nil {
		return nil, fmt.Errorf("list alert channels: %w", err)
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (channelRow, error) { return scanChannel(r) })
	if err != nil {
		return nil, fmt.Errorf("list alert channels: %w", err)
	}
	out := make([]Channel, 0, len(list))
	for _, c := range list {
		out = append(out, c.Channel)
	}
	return out, nil
}

func (cs *ChannelStore) validate(c Channel, rawURL *string) error {
	switch {
	case !validName(c.Name):
		return fmt.Errorf("%w: name must be 1..100 characters without surrounding spaces", ErrInvalidChannel)
	case c.Type != ChannelWebhook && c.Type != ChannelEmail:
		return fmt.Errorf("%w: type must be webhook or email", ErrInvalidChannel)
	case c.ScopeFilter != FilterAll && c.ScopeFilter != FilterPlatform && c.ScopeFilter != FilterTenant:
		return fmt.Errorf("%w: scope_filter must be all, platform or tenant", ErrInvalidChannel)
	case !ValidSeverity(c.MinSeverity):
		return fmt.Errorf("%w: unknown min_severity", ErrInvalidChannel)
	}
	if c.Type == ChannelWebhook {
		if rawURL == nil {
			return fmt.Errorf("%w: url is required", ErrInvalidURL)
		}
		if c.Recipients != nil {
			return fmt.Errorf("%w: a webhook has no recipients", ErrInvalidChannel)
		}
		return cs.policy.ValidateWebhookURL(*rawURL)
	}
	if rawURL != nil {
		return fmt.Errorf("%w: an email channel has no url", ErrInvalidChannel)
	}
	return validRecipients(c.Recipients)
}

// CreateChannel inserts a channel. For a webhook it returns the signing
// secret, which is shown this once and never again (sealed at rest).
func (cs *ChannelStore) CreateChannel(ctx context.Context, in ChannelInput, audit ChannelAuditFunc) (Channel, string, error) {
	c := Channel{Type: in.Type, ScopeFilter: FilterAll, MinSeverity: SeverityWarning, Enabled: true}
	if in.Name != nil {
		c.Name = *in.Name
	}
	if in.ScopeFilter != nil {
		c.ScopeFilter = *in.ScopeFilter
	}
	if in.MinSeverity != nil {
		c.MinSeverity = *in.MinSeverity
	}
	if in.Enabled != nil {
		c.Enabled = *in.Enabled
	}
	if in.Recipients != nil {
		c.Recipients = *in.Recipients
	}
	var rawURL *string
	if in.URL != nil {
		u := strings.TrimSpace(*in.URL)
		rawURL = &u
	}
	if err := cs.validate(c, rawURL); err != nil {
		return Channel{}, "", err
	}
	id := uuid.NewString()
	var secret string
	var enc, nonce *string
	if c.Type == ChannelWebhook {
		var err error
		if secret, err = newSecret(); err != nil {
			return Channel{}, "", fmt.Errorf("generate secret: %w", err)
		}
		e, n, err := cs.seal.EncryptSecret([]byte(secret), secretContext(id))
		if err != nil {
			return Channel{}, "", fmt.Errorf("seal secret: %w", err)
		}
		enc, nonce = &e, &n
	}
	var out Channel
	err := cs.store.inTx(ctx, func(tx pgx.Tx) error {
		row, err := scanChannel(tx.QueryRow(ctx, `
INSERT INTO alert_channels (id, name, type, scope_filter, min_severity, enabled, url, encrypted_secret, secret_nonce, recipients)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING `+channelColumns,
			id, c.Name, c.Type, c.ScopeFilter, c.MinSeverity, c.Enabled, rawURL, enc, nonce, c.Recipients))
		if isUniqueViolation(err) {
			return ErrChannelNameTaken
		}
		if err != nil {
			return fmt.Errorf("create alert channel: %w", err)
		}
		out = row.Channel
		return audit(ctx, tx, nil, &out)
	})
	if err != nil {
		return Channel{}, "", err
	}
	return out, secret, nil
}

func (cs *ChannelStore) lockChannel(ctx context.Context, tx pgx.Tx, id string) (channelRow, error) {
	c, err := scanChannel(tx.QueryRow(ctx, `SELECT `+channelColumns+` FROM alert_channels WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return channelRow{}, ErrNotFound
	}
	return c, err
}

// UpdateChannel changes a channel; the type can't change. A new webhook URL
// keeps the secret (rotate it separately).
func (cs *ChannelStore) UpdateChannel(ctx context.Context, id string, in ChannelInput, audit ChannelAuditFunc) (Channel, error) {
	var out Channel
	err := cs.store.inTx(ctx, func(tx pgx.Tx) error {
		before, err := cs.lockChannel(ctx, tx, id)
		if err != nil {
			return err
		}
		after := before.Channel
		if in.Type != "" && in.Type != before.Type {
			return fmt.Errorf("%w: the type can't change", ErrInvalidChannel)
		}
		if in.Name != nil {
			after.Name = *in.Name
		}
		if in.ScopeFilter != nil {
			after.ScopeFilter = *in.ScopeFilter
		}
		if in.MinSeverity != nil {
			after.MinSeverity = *in.MinSeverity
		}
		if in.Enabled != nil {
			after.Enabled = *in.Enabled
		}
		if in.Recipients != nil {
			after.Recipients = *in.Recipients
		}
		rawURL := before.url
		if in.URL != nil {
			u := strings.TrimSpace(*in.URL)
			rawURL = &u
		}
		if err := cs.validate(after, rawURL); err != nil {
			return err
		}
		row, err := scanChannel(tx.QueryRow(ctx, `
UPDATE alert_channels SET name = $2, scope_filter = $3, min_severity = $4, enabled = $5, url = $6,
       recipients = $7, updated_at = NOW()
 WHERE id = $1 RETURNING `+channelColumns,
			id, after.Name, after.ScopeFilter, after.MinSeverity, after.Enabled, rawURL, after.Recipients))
		if isUniqueViolation(err) {
			return ErrChannelNameTaken
		}
		if err != nil {
			return fmt.Errorf("update alert channel: %w", err)
		}
		out = row.Channel
		return audit(ctx, tx, &before.Channel, &out)
	})
	return out, err
}

// DeleteChannel removes a channel; its queued deliveries go with it. The
// delivery history (alert_deliveries) cascades as well.
func (cs *ChannelStore) DeleteChannel(ctx context.Context, id string, audit ChannelAuditFunc) error {
	return cs.store.inTx(ctx, func(tx pgx.Tx) error {
		before, err := cs.lockChannel(ctx, tx, id)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM alert_channels WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete alert channel: %w", err)
		}
		return audit(ctx, tx, &before.Channel, nil)
	})
}

// RotateSecret replaces a webhook's signing secret and returns the new one,
// shown this once.
func (cs *ChannelStore) RotateSecret(ctx context.Context, id string, audit ChannelAuditFunc) (string, error) {
	secret, err := newSecret()
	if err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	enc, nonce, err := cs.seal.EncryptSecret([]byte(secret), secretContext(id))
	if err != nil {
		return "", fmt.Errorf("seal secret: %w", err)
	}
	err = cs.store.inTx(ctx, func(tx pgx.Tx) error {
		before, err := cs.lockChannel(ctx, tx, id)
		if err != nil {
			return err
		}
		if before.Type != ChannelWebhook {
			return fmt.Errorf("%w: only a webhook has a secret", ErrInvalidChannel)
		}
		if _, err := tx.Exec(ctx, `UPDATE alert_channels SET encrypted_secret = $2, secret_nonce = $3, updated_at = NOW() WHERE id = $1`,
			id, enc, nonce); err != nil {
			return fmt.Errorf("rotate secret: %w", err)
		}
		return audit(ctx, tx, &before.Channel, &before.Channel)
	})
	if err != nil {
		return "", err
	}
	return secret, nil
}

// loadChannel reads a channel with its URL and opened secret, for delivery.
func (cs *ChannelStore) loadChannel(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (channelRow, []byte, error) {
	c, err := scanChannel(q.QueryRow(ctx, `SELECT `+channelColumns+` FROM alert_channels WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return channelRow{}, nil, ErrNotFound
	}
	if err != nil {
		return channelRow{}, nil, err
	}
	if c.Type != ChannelWebhook {
		return c, nil, nil
	}
	if c.encryptedSecret == nil || c.secretNonce == nil {
		return channelRow{}, nil, errors.New("webhook channel without a secret")
	}
	secret, err := cs.seal.DecryptSecret(*c.encryptedSecret, *c.secretNonce, secretContext(c.ID))
	if err != nil {
		return channelRow{}, nil, fmt.Errorf("open webhook secret: %w", err)
	}
	return c, secret, nil
}
