package mfa

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeycompat"
)

func newValkeyLimiter(t *testing.T) (*ValkeyLimiter, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)
	client, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{mr.Addr()}, DisableCache: true})
	if err != nil {
		t.Fatalf("valkey client: %v", err)
	}
	t.Cleanup(client.Close)
	return NewValkeyLimiter(valkeycompat.NewAdapter(client)), mr
}

// Parallel attempts are counted before any code check, so at most FailMax
// of them get a code checked in one window.
func TestValkeyLimiter_ParallelAttemptsBounded(t *testing.T) {
	l, mr := newValkeyLimiter(t)
	ctx := context.Background()
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, _, err := l.Attempt(ctx, "u1")
			if err != nil {
				t.Errorf("Attempt: %v", err)
				return
			}
			if n <= FailMax {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != FailMax {
		t.Fatalf("allowed %d attempts, want %d", got, FailMax)
	}
	n, retry, err := l.Attempt(ctx, "u1")
	if err != nil || n <= FailMax || retry <= 0 || retry > FailWindow {
		t.Fatalf("over limit: n=%d retry=%v err=%v", n, retry, err)
	}
	if mr.TTL(limitPrefix+"u1") <= 0 {
		t.Fatal("counter has no expiry")
	}
	if err := l.Reset(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := l.Attempt(ctx, "u1"); n != 1 {
		t.Fatalf("after reset n=%d, want 1", n)
	}
}

// A counter that lost its expiry gets one again when it refuses.
func TestValkeyLimiter_RestoresLostExpiry(t *testing.T) {
	l, mr := newValkeyLimiter(t)
	if err := mr.Set(limitPrefix+"u2", "9"); err != nil {
		t.Fatal(err)
	}
	_, retry, err := l.Attempt(context.Background(), "u2")
	if err != nil || retry != FailWindow || mr.TTL(limitPrefix+"u2") <= 0 {
		t.Fatalf("retry=%v err=%v ttl=%v", retry, err, mr.TTL(limitPrefix+"u2"))
	}
}
