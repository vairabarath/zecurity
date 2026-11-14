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
// failure, EXPIRE FailWindow on the first failure of a window.
type ValkeyLimiter struct {
	rdb valkeycompat.Cmdable
}

func NewValkeyLimiter(rdb valkeycompat.Cmdable) *ValkeyLimiter { return &ValkeyLimiter{rdb: rdb} }

func (l *ValkeyLimiter) key(userID string) string { return limitPrefix + userID }

func (l *ValkeyLimiter) Blocked(ctx context.Context, userID string) (bool, time.Duration, error) {
	n, err := l.rdb.Get(ctx, l.key(userID)).Int64()
	if err == valkeycompat.Nil {
		return false, 0, nil
	}
	if err != nil {
		return false, 0, fmt.Errorf("mfa limiter get: %w", err)
	}
	if n < FailMax {
		return false, 0, nil
	}
	ttl, err := l.rdb.TTL(ctx, l.key(userID)).Result()
	if err != nil {
		return false, 0, fmt.Errorf("mfa limiter ttl: %w", err)
	}
	if ttl < 0 {
		// A counter that lost its expiry (crash between INCR and EXPIRE)
		// gets one again, so it can never lock a user out forever.
		if err := l.rdb.Expire(ctx, l.key(userID), FailWindow).Err(); err != nil {
			return false, 0, fmt.Errorf("mfa limiter expire: %w", err)
		}
		ttl = FailWindow
	}
	return true, ttl, nil
}

func (l *ValkeyLimiter) RecordFailure(ctx context.Context, userID string) (bool, error) {
	n, err := l.rdb.Incr(ctx, l.key(userID)).Result()
	if err != nil {
		return false, fmt.Errorf("mfa limiter incr: %w", err)
	}
	if n == 1 {
		if err := l.rdb.Expire(ctx, l.key(userID), FailWindow).Err(); err != nil {
			return false, fmt.Errorf("mfa limiter expire: %w", err)
		}
	}
	return n == FailMax, nil
}

func (l *ValkeyLimiter) Reset(ctx context.Context, userID string) error {
	return l.rdb.Del(ctx, l.key(userID)).Err()
}
