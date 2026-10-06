package outbox

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Processor.Run shutdown behaviour, tested deterministically without a real
// database by pointing the outbox at endpoints that hang or refuse.

func newShutdownTestProcessor(t *testing.T, addr string) *Processor {
	t.Helper()
	pool, err := pgxpool.New(context.Background(),
		fmt.Sprintf("postgres://u:p@%s/outbox_test?sslmode=disable&connect_timeout=30", addr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	p, err := NewProcessor(NewOutbox(pool), NewHandlerRegistry(), WithPollInterval(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func runUntilCancelled(t *testing.T, p *Processor, runFor time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx, 10) }()
	time.Sleep(runFor)
	cancel()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("Processor.Run did not return after cancel (wedged)")
		return nil
	}
}

// A claim query still in flight when Run is cancelled fails with the
// cancelled context. That is a clean shutdown, not an error. (Regression: the
// processor reported it, and TestProcessorRunReapsAbandonedIntegration flaked
// in CI whenever the cancel landed mid-query.)
func TestProcessorRun_ShutdownDuringClaimIsClean(t *testing.T) {
	// A server that accepts connections and never answers: every claim query
	// blocks until the context is cancelled.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		conns []net.Conn
	)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})

	p := newShutdownTestProcessor(t, ln.Addr().String())
	if err := runUntilCancelled(t, p, 150*time.Millisecond); err != nil {
		t.Fatalf("shutdown during an in-flight claim returned %v, want nil", err)
	}
}

// Repeated loop errors (database unreachable for several ticks) must neither
// wedge the loop nor block shutdown; Run still reports the failure.
// (Regression: with a 2-slot error channel, the third blocking send wedged the
// claim loop and Run hung forever on shutdown.)
func TestProcessorRun_RepeatedErrorsDoNotHangShutdown(t *testing.T) {
	// A port with nothing listening: every claim fails immediately.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	p := newShutdownTestProcessor(t, addr)
	err = runUntilCancelled(t, p, 300*time.Millisecond) // ≈30 poll ticks, far more than 2 errors
	if err == nil {
		t.Fatal("Run returned nil although every claim failed; want the claim error")
	}
}
