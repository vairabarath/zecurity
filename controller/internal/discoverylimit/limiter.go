// Package discoverylimit rate-limits the public workspace-discovery entry points
// (FU-11, ADR-009): lookupWorkspacesByEmail, lookupWorkspace and
// lookupIdpConnections over GraphQL, and GET /workspaces/{slug}/auth. They
// answer unauthenticated questions about which workspaces exist and who
// belongs to them; the limits slow enumeration down but do not remove the
// oracle (see ADR-009 → Workspace-discovery enumeration).
//
// One Valkey fixed-window counter per client IP (IPv6 by /64) is shared by all
// four entry points, so switching between them gains nothing. Email lookups
// also draw from a per-identity budget keyed by an HMAC of the canonical email,
// so no raw address is ever stored or logged. The limiter is a security
// control: callers fail CLOSED (503) on any limiter error.
package discoverylimit

import (
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/valkey-io/valkey-go/valkeycompat"

	"github.com/yourorg/ztna/controller/internal/metrics"
)

// Operation names one discovery entry point. The values are metric labels.
type Operation string

const (
	OpEmailLookup    Operation = "lookup_workspaces_by_email"
	OpSlugLookup     Operation = "lookup_workspace"
	OpIdpConnections Operation = "lookup_idp_connections"
	OpRESTAuth       Operation = "workspace_auth_discovery"
)

// Operations lists every Operation (metric pre-creation, tests).
var Operations = []Operation{OpEmailLookup, OpSlugLookup, OpIdpConnections, OpRESTAuth}

// Metric result labels.
const (
	ResultAllowed      = "allowed"
	ResultIPLimited    = "rate_limited_ip"
	ResultEmailLimited = "rate_limited_email"
	ResultLimiterError = "limiter_error"
)

// Denial scopes.
const (
	ScopeIP    = "ip"
	ScopeEmail = "email"
)

// Defaults (FU-11, approved 2026-10-31). Fixed windows: across a window
// boundary a client can get up to about twice the allowance in a short burst.
const (
	DefaultIPLimit    = 30
	DefaultEmailLimit = 5

	ipWindow    = time.Minute
	emailWindow = time.Hour

	keyPrefix = "discovery:"

	// emailKeyContext is the HKDF info for the email-bucket HMAC key. Rotating
	// PKI_MASTER_SECRET changes every bucket: the counters restart, which at
	// worst grants each email one extra window of attempts.
	emailKeyContext = "zecurity/discovery-ratelimit/email/v1"
)

// ErrUnavailable means the limiter could not decide (Valkey unreachable or a
// bad reply). Callers must refuse the request. It carries no infrastructure
// detail; that is logged once, here.
var ErrUnavailable = errors.New("discovery rate limiter unavailable")

func init() {
	for _, op := range Operations {
		metrics.DiscoveryInitSeries(string(op), ResultAllowed, ResultIPLimited, ResultEmailLimited, ResultLimiterError)
	}
}

// Config holds the limits; zero values take the defaults. Windows are fixed
// (60 s for IP, 1 h for email); the fields exist for tests.
type Config struct {
	IPLimit     int
	EmailLimit  int
	IPWindow    time.Duration
	EmailWindow time.Duration
}

// Limiter enforces the discovery budgets.
type Limiter struct {
	rdb      valkeycompat.Cmdable
	cfg      Config
	emailKey []byte
}

// NewLimiter builds the limiter. masterSecret is PKI_MASTER_SECRET (always set:
// pki.Init refuses to start without it); the email-bucket key is derived from
// it with HKDF, so no new secret has to be provisioned.
func NewLimiter(rdb valkeycompat.Cmdable, cfg Config, masterSecret string) (*Limiter, error) {
	if cfg.IPLimit < 0 || cfg.EmailLimit < 0 {
		return nil, fmt.Errorf("discovery limiter: limits must be positive (ip=%d email=%d)", cfg.IPLimit, cfg.EmailLimit)
	}
	if cfg.IPLimit == 0 {
		cfg.IPLimit = DefaultIPLimit
	}
	if cfg.EmailLimit == 0 {
		cfg.EmailLimit = DefaultEmailLimit
	}
	if cfg.IPWindow <= 0 {
		cfg.IPWindow = ipWindow
	}
	if cfg.EmailWindow <= 0 {
		cfg.EmailWindow = emailWindow
	}
	key, err := deriveEmailKey(masterSecret)
	if err != nil {
		return nil, err
	}
	return &Limiter{rdb: rdb, cfg: cfg, emailKey: key}, nil
}

func deriveEmailKey(masterSecret string) ([]byte, error) {
	if masterSecret == "" {
		return nil, errors.New("discovery limiter: PKI_MASTER_SECRET is empty")
	}
	key, err := hkdf.Key(sha256.New, []byte(masterSecret), nil, emailKeyContext, 32)
	if err != nil {
		return nil, fmt.Errorf("discovery limiter: derive email key: %w", err)
	}
	return key, nil
}

// canonicalEmail is the application's email rule (users, invitations,
// provider accounts, the lookup itself): trimmed and lower-cased, nothing more.
func canonicalEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// emailBucket is the hex HMAC-SHA256 of the canonical email.
func emailBucket(key []byte, email string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(canonicalEmail(email)))
	return hex.EncodeToString(m.Sum(nil))
}

func (l *Limiter) ipKey(clientKey string) string { return keyPrefix + "ip:" + clientKey }

func (l *Limiter) emailCounterKey(bucket string) string { return keyPrefix + "email:" + bucket }

// fixedWindowScript increments the counter and gives it the window's expiry if
// it has none: a new key, or one that lost its TTL. One script, so concurrent
// requests can neither skip the expiry nor exceed the budget. Returns
// {count, remaining ms}.
const fixedWindowScript = `
local n = redis.call('INCR', KEYS[1])
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then
  redis.call('PEXPIRE', KEYS[1], ARGV[1])
  ttl = tonumber(ARGV[1])
end
return {n, ttl}`

func (l *Limiter) incr(ctx context.Context, key string, window time.Duration) (int64, time.Duration, error) {
	v, err := l.rdb.Eval(ctx, fixedWindowScript, []string{key}, window.Milliseconds()).Int64Slice()
	if err != nil {
		return 0, 0, err
	}
	if len(v) != 2 {
		return 0, 0, fmt.Errorf("unexpected reply %v", v)
	}
	return v[0], time.Duration(v[1]) * time.Millisecond, nil
}

// Decision is the outcome of one charged request.
type Decision struct {
	Allowed    bool
	Scope      string // ScopeIP | ScopeEmail when denied
	RetryAfter time.Duration
}

// Allow charges one request from clientKey (see ClientKey) to the shared IP
// budget and, for email lookups (email non-empty), to that identity's budget.
// The email budget is charged only once the IP budget has allowed the request,
// so a client already over its limit can't spend other people's allowance.
// Every outcome is counted in discovery_requests_total; denials and failures
// are logged without the email.
func (l *Limiter) Allow(ctx context.Context, op Operation, clientKey, email string) (Decision, error) {
	n, ttl, err := l.incr(ctx, l.ipKey(clientKey), l.cfg.IPWindow)
	if err != nil {
		return l.fail(op, clientKey, err)
	}
	if n > int64(l.cfg.IPLimit) {
		metrics.DiscoveryRequest(string(op), ResultIPLimited)
		if n == int64(l.cfg.IPLimit)+1 { // once per window, not per refused request
			log.Printf("discovery: rate limited op=%s scope=ip client=%s limit=%d window=%s retry_after=%s",
				op, clientKey, l.cfg.IPLimit, l.cfg.IPWindow, ttl.Round(time.Second))
		}
		return Decision{Scope: ScopeIP, RetryAfter: ttl}, nil
	}
	if op == OpEmailLookup {
		bucket := emailBucket(l.emailKey, email)
		n, ttl, err := l.incr(ctx, l.emailCounterKey(bucket), l.cfg.EmailWindow)
		if err != nil {
			return l.fail(op, clientKey, err)
		}
		if n > int64(l.cfg.EmailLimit) {
			metrics.DiscoveryRequest(string(op), ResultEmailLimited)
			// Repeated targeting of one identity: log the first refusal of the
			// window by key prefix (never the address), plus every 10th after.
			if over := n - int64(l.cfg.EmailLimit); over == 1 || over%10 == 0 {
				log.Printf("discovery: rate limited op=%s scope=email email_key=%s client=%s attempts=%d limit=%d window=%s retry_after=%s",
					op, bucket[:12], clientKey, n, l.cfg.EmailLimit, l.cfg.EmailWindow, ttl.Round(time.Second))
			}
			return Decision{Scope: ScopeEmail, RetryAfter: ttl}, nil
		}
	}
	metrics.DiscoveryRequest(string(op), ResultAllowed)
	return Decision{Allowed: true}, nil
}

func (l *Limiter) fail(op Operation, clientKey string, err error) (Decision, error) {
	metrics.DiscoveryRequest(string(op), ResultLimiterError)
	log.Printf("discovery: limiter unavailable, failing closed op=%s client=%s: %v", op, clientKey, err)
	return Decision{}, ErrUnavailable
}
