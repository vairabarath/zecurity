package mfa

import (
	"context"
	"fmt"
	"time"

	"github.com/valkey-io/valkey-go/valkeycompat"
)

// Wrong-code limits: the same numbers as the provider operators' TOTP.
const (
	FailWindow = 15 * time.Minute
	FailMax    = 5

	limitPrefix = "mfa:totp:fail:"
)

// ValkeyLimiter implements Limiter with one counter per user: INCR per
// attempt, EXPIRE FailWindow on the first attempt of a window, DEL on a
// correct code.
type ValkeyLimiter struct {
	rdb valkeycompat.Cmdable
}

func NewValkeyLimiter(rdb valkeycompat.Cmdable) *ValkeyLimiter { return &ValkeyLimiter{rdb: rdb} }

func (l *ValkeyLimiter) key(userID string) string { return limitPrefix + userID }

func (l *ValkeyLimiter) Attempt(ctx context.Context, userID string) (int64, time.Duration, error) {
	key := l.key(userID)
	n, err := l.rdb.Incr(ctx, key).Result()
	if err != nil {
		return 0, 0, fmt.Errorf("mfa limiter incr: %w", err)
	}
	if n == 1 {
		if err := l.rdb.Expire(ctx, key, FailWindow).Err(); err != nil {
			return 0, 0, fmt.Errorf("mfa limiter expire: %w", err)
		}
		return n, 0, nil
	}
	if n <= FailMax {
		return n, 0, nil
	}
	ttl, err := l.rdb.TTL(ctx, key).Result()
	if err != nil {
		return 0, 0, fmt.Errorf("mfa limiter ttl: %w", err)
	}
	if ttl < 0 {
		// A counter that lost its expiry (crash between INCR and EXPIRE)
		// gets one again, so it can never lock a user out forever.
		if err := l.rdb.Expire(ctx, key, FailWindow).Err(); err != nil {
			return 0, 0, fmt.Errorf("mfa limiter expire: %w", err)
		}
		ttl = FailWindow
	}
	return n, ttl, nil
}

func (l *ValkeyLimiter) Reset(ctx context.Context, userID string) error {
	return l.rdb.Del(ctx, l.key(userID)).Err()
}
