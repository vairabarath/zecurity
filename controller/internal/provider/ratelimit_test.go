package provider

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeycompat"
)

// newTestLimiter connects to Valkey (AUTH_TEST_VALKEY_URL) and returns a
// limiter whose keys use a unique prefix; the test deletes only its own keys
// (never FLUSHDB — the shared helper ignores the /db suffix, so tests share
// database 0 with anything else on that Valkey).
func newTestLimiter(t *testing.T) (*ValkeyLoginLimiter, valkeycompat.Cmdable) {
	t.Helper()
	raw := os.Getenv("AUTH_TEST_VALKEY_URL")
	if raw == "" {
		t.Skip("AUTH_TEST_VALKEY_URL not set")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		t.Fatalf("bad AUTH_TEST_VALKEY_URL %q: %v", raw, err)
	}
	client, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{u.Host}})
	if err != nil {
		t.Fatalf("valkey client: %v", err)
	}
	rdb := valkeycompat.NewAdapter(client)
	prefix := fmt.Sprintf("test:provider:login:fail:%d:%d:", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		ctx := context.Background()
		if keys, err := rdb.Keys(ctx, prefix+"*").Result(); err == nil && len(keys) > 0 {
			_ = rdb.Del(ctx, keys...).Err()
		}
		client.Close()
	})
	return &ValkeyLoginLimiter{rdb: rdb, prefix: prefix}, rdb
}

func TestValkeyLoginLimiter_EmailThreshold(t *testing.T) {
	l, _ := newTestLimiter(t)
	ctx := context.Background()
	const email, ip = "Target@InkYank.com", "203.0.113.7"

	for i := 1; i <= loginFailMaxPerEmail; i++ {
		if blocked, _, err := l.Blocked(ctx, email, ip); err != nil || blocked {
			t.Fatalf("attempt %d: blocked=%v err=%v before the threshold", i, blocked, err)
		}
		trips, err := l.RecordFailure(ctx, email, ip)
		if err != nil {
			t.Fatal(err)
		}
		if i < loginFailMaxPerEmail && len(trips) != 0 {
			t.Fatalf("failure %d tripped early: %+v", i, trips)
		}
		if i == loginFailMaxPerEmail {
			if len(trips) != 1 || trips[0].Scope != LimitScopeEmail || trips[0].Value != "target@inkyank.com" || trips[0].RetryAfter <= 0 {
				t.Fatalf("threshold failure must trip the email scope once: %+v", trips)
			}
		}
	}

	blocked, retry, err := l.Blocked(ctx, "target@inkyank.com", "198.51.100.1")
	if err != nil || !blocked || retry <= 0 || retry > loginFailWindow {
		t.Fatalf("after %d failures: blocked=%v retry=%v err=%v", loginFailMaxPerEmail, blocked, retry, err)
	}
	// A further failure while locked does not trip again (one audit per lockout).
	if trips, _ := l.RecordFailure(ctx, email, ip); len(trips) != 0 {
		t.Fatalf("failure while locked tripped again: %+v", trips)
	}

	if err := l.Reset(ctx, email); err != nil {
		t.Fatal(err)
	}
	if blocked, _, _ := l.Blocked(ctx, email, "198.51.100.1"); blocked {
		t.Fatal("Reset did not clear the email lockout")
	}
}

func TestValkeyLoginLimiter_IPThreshold(t *testing.T) {
	l, _ := newTestLimiter(t)
	ctx := context.Background()
	const ip = "203.0.113.99"

	var ipTrips int
	for i := 0; i < loginFailMaxPerIP; i++ {
		trips, err := l.RecordFailure(ctx, fmt.Sprintf("user%d@inkyank.com", i), ip)
		if err != nil {
			t.Fatal(err)
		}
		for _, tr := range trips {
			if tr.Scope == LimitScopeIP {
				ipTrips++
			}
		}
	}
	if ipTrips != 1 {
		t.Fatalf("IP scope tripped %d times, want 1", ipTrips)
	}
	if blocked, _, err := l.Blocked(ctx, "fresh@inkyank.com", ip); err != nil || !blocked {
		t.Fatalf("IP should be locked for any email: blocked=%v err=%v", blocked, err)
	}
	// Reset of an email does not unlock the IP.
	_ = l.Reset(ctx, "fresh@inkyank.com")
	if blocked, _, _ := l.Blocked(ctx, "fresh@inkyank.com", ip); !blocked {
		t.Fatal("email Reset must not clear the IP counter")
	}
}

// A counter that lost its expiry is given one again on read, so it can never
// lock an account out forever.
func TestValkeyLoginLimiter_HealsMissingExpiry(t *testing.T) {
	l, rdb := newTestLimiter(t)
	ctx := context.Background()
	key := l.emailKey("stuck@inkyank.com")
	if err := rdb.Set(ctx, key, "9", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := l.Blocked(ctx, "stuck@inkyank.com", "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	ttl, err := rdb.TTL(ctx, key).Result()
	if err != nil || ttl <= 0 || ttl > loginFailWindow {
		t.Fatalf("expiry not restored: ttl=%v err=%v", ttl, err)
	}
}
