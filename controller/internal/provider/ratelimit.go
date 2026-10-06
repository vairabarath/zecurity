package provider

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/valkey-io/valkey-go/valkeycompat"
)

// Provider login brute-force limits (Sprint 21 Phase H, locked 2026-09-28).
// The limiter is a security control: callers must fail CLOSED (503) on any
// limiter error rather than log in without it.
const (
	loginFailWindow      = 15 * time.Minute
	loginFailMaxPerEmail = 5
	loginFailMaxPerIP    = 20

	defaultLoginLimitPrefix = "provider:login:fail:"
)

// Limit scopes reported in LimitTrip.Scope and the rate-limit audit row.
const (
	LimitScopeEmail = "email"
	LimitScopeIP    = "ip"
)

// LimitTrip describes a counter that crossed its threshold on this failure —
// the start of a lockout. The login handler audits exactly one
// provider_auth.rate_limit row per trip.
type LimitTrip struct {
	Scope      string // LimitScopeEmail | LimitScopeIP
	Value      string // the normalized email or the client IP
	Failures   int64
	RetryAfter time.Duration
}

// LoginLimiter counts failed provider logins per email and per client IP.
type LoginLimiter interface {
	// Blocked reports whether the email or the IP is locked out, and for how
	// long (the longer remaining window when both are).
	Blocked(ctx context.Context, email, ip string) (blocked bool, retryAfter time.Duration, err error)
	// RecordFailure counts one failure for both scopes and returns the scopes
	// whose threshold this failure crossed.
	RecordFailure(ctx context.Context, email, ip string) ([]LimitTrip, error)
	// Reset clears the email counter after a successful login. The IP counter
	// is kept, so one host cannot unlock itself by logging into an account it
	// controls while guessing others.
	Reset(ctx context.Context, email string) error
}

// ValkeyLoginLimiter implements LoginLimiter with Valkey counters:
// INCR per failure, EXPIRE loginFailWindow on the first failure of a window.
type ValkeyLoginLimiter struct {
	rdb    valkeycompat.Cmdable
	prefix string
}

func NewValkeyLoginLimiter(rdb valkeycompat.Cmdable) *ValkeyLoginLimiter {
	return &ValkeyLoginLimiter{rdb: rdb, prefix: defaultLoginLimitPrefix}
}

func (l *ValkeyLoginLimiter) emailKey(email string) string {
	return l.prefix + LimitScopeEmail + ":" + normalizeEmail(email)
}

func (l *ValkeyLoginLimiter) ipKey(ip string) string {
	return l.prefix + LimitScopeIP + ":" + ip
}

func (l *ValkeyLoginLimiter) Blocked(ctx context.Context, email, ip string) (bool, time.Duration, error) {
	var (
		blocked bool
		retry   time.Duration
	)
	for _, c := range []struct {
		key string
		max int64
	}{{l.emailKey(email), loginFailMaxPerEmail}, {l.ipKey(ip), loginFailMaxPerIP}} {
		n, ttl, err := l.count(ctx, c.key)
		if err != nil {
			return false, 0, err
		}
		if n >= c.max {
			blocked = true
			if ttl > retry {
				retry = ttl
			}
		}
	}
	return blocked, retry, nil
}

// count returns the counter and its remaining window. A counter that somehow
// lost its expiry (crash between INCR and EXPIRE) gets one again here, so it
// can never lock an account out forever.
func (l *ValkeyLoginLimiter) count(ctx context.Context, key string) (int64, time.Duration, error) {
	v, err := l.rdb.Get(ctx, key).Result()
	if err == valkeycompat.Nil {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("login limiter get: %w", err)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("login limiter parse %q: %w", v, err)
	}
	ttl, err := l.rdb.TTL(ctx, key).Result()
	if err != nil {
		return 0, 0, fmt.Errorf("login limiter ttl: %w", err)
	}
	if ttl < 0 { // no expiry set
		if err := l.rdb.Expire(ctx, key, loginFailWindow).Err(); err != nil {
			return 0, 0, fmt.Errorf("login limiter expire: %w", err)
		}
		ttl = loginFailWindow
	}
	return n, ttl, nil
}

func (l *ValkeyLoginLimiter) RecordFailure(ctx context.Context, email, ip string) ([]LimitTrip, error) {
	var trips []LimitTrip
	for _, c := range []struct {
		scope, value, key string
		max               int64
	}{
		{LimitScopeEmail, normalizeEmail(email), l.emailKey(email), loginFailMaxPerEmail},
		{LimitScopeIP, ip, l.ipKey(ip), loginFailMaxPerIP},
	} {
		n, err := l.rdb.Incr(ctx, c.key).Result()
		if err != nil {
			return nil, fmt.Errorf("login limiter incr: %w", err)
		}
		if n == 1 {
			if err := l.rdb.Expire(ctx, c.key, loginFailWindow).Err(); err != nil {
				return nil, fmt.Errorf("login limiter expire: %w", err)
			}
		}
		if n == c.max { // this failure crossed the threshold: a lockout starts
			ttl, err := l.rdb.TTL(ctx, c.key).Result()
			if err != nil || ttl < 0 {
				ttl = loginFailWindow
			}
			trips = append(trips, LimitTrip{Scope: c.scope, Value: c.value, Failures: n, RetryAfter: ttl})
		}
	}
	return trips, nil
}

func (l *ValkeyLoginLimiter) Reset(ctx context.Context, email string) error {
	if err := l.rdb.Del(ctx, l.emailKey(email)).Err(); err != nil {
		return fmt.Errorf("login limiter reset: %w", err)
	}
	return nil
}
