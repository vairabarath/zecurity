package discoverylimit

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeycompat"
)

const testSecret = "discovery-test-master-secret-0123456789abcdef"

func newTestLimiter(t *testing.T, cfg Config) (*Limiter, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	client, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{mr.Addr()}, DisableCache: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	l, err := NewLimiter(valkeycompat.NewAdapter(client), cfg, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	return l, mr
}

// captureLog collects log output for privacy assertions.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prev) })
	return &buf
}

func TestNewLimiter_Validation(t *testing.T) {
	rdb := valkeycompat.Cmdable(nil)
	if _, err := NewLimiter(rdb, Config{}, ""); err == nil {
		t.Fatal("empty master secret must be refused")
	}
	if _, err := NewLimiter(rdb, Config{IPLimit: -1}, testSecret); err == nil {
		t.Fatal("negative IP limit must be refused")
	}
	if _, err := NewLimiter(rdb, Config{EmailLimit: -1}, testSecret); err == nil {
		t.Fatal("negative email limit must be refused")
	}
	l, err := NewLimiter(rdb, Config{}, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	if l.cfg.IPLimit != DefaultIPLimit || l.cfg.EmailLimit != DefaultEmailLimit ||
		l.cfg.IPWindow != time.Minute || l.cfg.EmailWindow != time.Hour {
		t.Fatalf("defaults not applied: %+v", l.cfg)
	}
}

// All four entry points draw from one per-IP budget.
func TestAllow_SharedIPBudgetAcrossOperations(t *testing.T) {
	l, _ := newTestLimiter(t, Config{IPLimit: 8})
	ctx := context.Background()
	ops := []Operation{OpEmailLookup, OpSlugLookup, OpIdpConnections, OpRESTAuth}
	for i := 0; i < 8; i++ {
		email := ""
		if ops[i%4] == OpEmailLookup {
			email = "user" + string(rune('a'+i)) + "@example.com"
		}
		d, err := l.Allow(ctx, ops[i%4], "4:203.0.113.7", email)
		if err != nil || !d.Allowed {
			t.Fatalf("request %d (%s): allowed=%v err=%v", i, ops[i%4], d.Allowed, err)
		}
	}
	for _, op := range ops {
		d, err := l.Allow(ctx, op, "4:203.0.113.7", "")
		if err != nil {
			t.Fatal(err)
		}
		if d.Allowed || d.Scope != ScopeIP || d.RetryAfter <= 0 || d.RetryAfter > time.Minute {
			t.Fatalf("%s after the budget: %+v, want an IP denial with 0 < retry <= 60s", op, d)
		}
	}
	// Another client is unaffected.
	if d, _ := l.Allow(ctx, OpSlugLookup, "4:203.0.113.8", ""); !d.Allowed {
		t.Fatal("a different client must have its own budget")
	}
}

func TestAllow_EmailBudget(t *testing.T) {
	l, mr := newTestLimiter(t, Config{IPLimit: 1000, EmailLimit: 5})
	ctx := context.Background()
	logs := captureLog(t)
	// Canonicalization: case and surrounding spaces are one identity.
	variants := []string{"Victim@Example.com", " victim@example.com", "VICTIM@EXAMPLE.COM ", "victim@example.com", "victim@Example.COM"}
	for i, e := range variants {
		if d, err := l.Allow(ctx, OpEmailLookup, "4:203.0.113."+string(rune('1'+i)), e); err != nil || !d.Allowed {
			t.Fatalf("attempt %d: %+v %v", i, d, err)
		}
	}
	d, err := l.Allow(ctx, OpEmailLookup, "4:198.51.100.1", "victim@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed || d.Scope != ScopeEmail || d.RetryAfter <= 0 || d.RetryAfter > time.Hour {
		t.Fatalf("6th attempt from a new IP: %+v, want an email denial", d)
	}
	// Distinct identities have independent buckets.
	if d, _ := l.Allow(ctx, OpEmailLookup, "4:198.51.100.1", "other@example.com"); !d.Allowed {
		t.Fatal("another email must have its own bucket")
	}
	// Gmail-style variants are distinct identities, exactly as the user lookup treats them.
	if d, _ := l.Allow(ctx, OpEmailLookup, "4:198.51.100.1", "victim+x@example.com"); !d.Allowed {
		t.Fatal("a plus-address is a distinct identity")
	}

	// Privacy: no key and no log line carries the address.
	for _, k := range mr.Keys() {
		if strings.Contains(k, "@") || strings.Contains(strings.ToLower(k), "victim") || strings.Contains(k, "example") {
			t.Fatalf("raw email in a Valkey key: %q", k)
		}
	}
	if s := logs.String(); strings.Contains(strings.ToLower(s), "victim") || strings.Contains(s, "@") {
		t.Fatalf("raw email in logs: %s", s)
	}
	if !strings.Contains(logs.String(), "email_key=") {
		t.Fatalf("an exhausted email bucket must be logged by its key prefix: %s", logs.String())
	}
}

// A request already over its IP budget does not spend the email budget, so a
// blocked client cannot burn other people's email allowance for free.
func TestAllow_IPDenialDoesNotChargeEmail(t *testing.T) {
	l, _ := newTestLimiter(t, Config{IPLimit: 1, EmailLimit: 2})
	ctx := context.Background()
	if d, _ := l.Allow(ctx, OpSlugLookup, "4:203.0.113.7", ""); !d.Allowed {
		t.Fatal("first request must pass")
	}
	for i := 0; i < 5; i++ {
		if d, _ := l.Allow(ctx, OpEmailLookup, "4:203.0.113.7", "victim@example.com"); d.Allowed || d.Scope != ScopeIP {
			t.Fatalf("over-budget request %d: %+v", i, d)
		}
	}
	for i, ip := range []string{"4:198.51.100.1", "4:198.51.100.2"} {
		if d, _ := l.Allow(ctx, OpEmailLookup, ip, "victim@example.com"); !d.Allowed {
			t.Fatalf("victim's email budget was spent by the blocked client (attempt %d: %+v)", i, d)
		}
	}
}

func TestEmailKey_HMAC(t *testing.T) {
	a, err := deriveEmailKey(testSecret)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := deriveEmailKey(testSecret)
	c, _ := deriveEmailKey(testSecret + "-rotated")
	k1, k2, k3 := emailBucket(a, "victim@example.com"), emailBucket(b, " Victim@Example.com "), emailBucket(c, "victim@example.com")
	if k1 != k2 {
		t.Fatal("the bucket must be stable for one secret and one canonical email")
	}
	if k1 == k3 {
		t.Fatal("rotating the master secret must change every bucket")
	}
	if len(k1) != 64 || strings.ContainsAny(k1, "@.") {
		t.Fatalf("bucket %q is not a hex HMAC", k1)
	}
}

// Fixed windows: the budget refills when the window expires, so a client can
// send up to ~2x the allowance across a window boundary (documented).
func TestAllow_FixedWindowBoundary(t *testing.T) {
	l, mr := newTestLimiter(t, Config{IPLimit: 3})
	ctx := context.Background()
	use := func(n int) (allowed int) {
		for i := 0; i < n; i++ {
			if d, err := l.Allow(ctx, OpSlugLookup, "4:203.0.113.7", ""); err == nil && d.Allowed {
				allowed++
			}
		}
		return
	}
	if got := use(4); got != 3 {
		t.Fatalf("first window: %d allowed, want 3", got)
	}
	mr.FastForward(59 * time.Second)
	if got := use(1); got != 0 {
		t.Fatal("still inside the window")
	}
	mr.FastForward(2 * time.Second)
	if got := use(4); got != 3 {
		t.Fatalf("next window: %d allowed, want 3 (boundary burst of 2x the allowance in ~2 s)", got)
	}
}

// A counter that lost its expiry gets one again on the next request, so it can
// never block a client forever.
func TestAllow_RepairsMissingTTL(t *testing.T) {
	l, mr := newTestLimiter(t, Config{IPLimit: 3})
	key := l.ipKey("4:203.0.113.7")
	if err := mr.Set(key, "99"); err != nil { // no TTL
		t.Fatal(err)
	}
	d, err := l.Allow(context.Background(), OpSlugLookup, "4:203.0.113.7", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("over the limit")
	}
	if ttl := mr.TTL(key); ttl <= 0 || ttl > time.Minute {
		t.Fatalf("TTL after repair = %v, want (0, 60s]", ttl)
	}
	mr.FastForward(61 * time.Second)
	if d, _ := l.Allow(context.Background(), OpSlugLookup, "4:203.0.113.7", ""); !d.Allowed {
		t.Fatal("the repaired counter must expire")
	}
}

// Concurrent requests cannot exceed the budget: the increment and the expiry
// are one atomic script.
func TestAllow_Concurrent(t *testing.T) {
	l, _ := newTestLimiter(t, Config{IPLimit: 30})
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if d, err := l.Allow(context.Background(), OpSlugLookup, "4:203.0.113.7", ""); err == nil && d.Allowed {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if allowed.Load() != 30 {
		t.Fatalf("%d of 100 concurrent requests allowed, want exactly 30", allowed.Load())
	}
}

func TestAllow_ValkeyFailure(t *testing.T) {
	l, mr := newTestLimiter(t, Config{})
	addr := mr.Addr()
	mr.Close() // Valkey is down
	logs := captureLog(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := l.Allow(ctx, OpEmailLookup, "4:203.0.113.7", "victim@example.com")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want ErrUnavailable", err)
	}
	if strings.Contains(err.Error(), addr) || strings.Contains(err.Error(), "connect") {
		t.Fatalf("the returned error must not carry infrastructure detail: %v", err)
	}
	if !strings.Contains(logs.String(), "limiter unavailable") || strings.Contains(logs.String(), "victim") {
		t.Fatalf("infra failure must be logged without the email: %s", logs.String())
	}
}
