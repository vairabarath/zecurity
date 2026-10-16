package policy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yourorg/ztna/controller/internal/outbox"
)

// Certificate audit bugs 4/5: lifecycle changes enqueue
// EventPolicyInvalidationRequested in their own transaction. Its handler must
// invalidate the workspace's policy, report failures so the outbox retries,
// tolerate duplicate delivery, and survive a crashed worker.

// flakyNotifier fails its first `failures` calls.
type flakyNotifier struct {
	mu         sync.Mutex
	failures   int
	workspaces []string
}

func (n *flakyNotifier) NotifyPolicyChange(_ context.Context, workspaceID string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.failures > 0 {
		n.failures--
		return errors.New("notifier unavailable")
	}
	n.workspaces = append(n.workspaces, workspaceID)
	return nil
}

func (n *flakyNotifier) notified() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.workspaces...)
}

func TestInvalidationHandler_InvalidatesTheEventsWorkspace(t *testing.T) {
	t.Skip("bugs 4/5: demonstrates the gap; un-skip with the fix")
	ws := uuid.New()
	evt := NewInvalidationEvent(ws, "test")
	if evt.EventType != EventPolicyInvalidationRequested || evt.WorkspaceID != ws {
		t.Fatalf("event = %+v", evt)
	}
	n := &flakyNotifier{failures: 1}
	h := NewInvalidationHandler(n)
	stored := outbox.OutboxEvent{EventType: evt.EventType, WorkspaceID: ws, Payload: evt.Payload}

	if err := h.Handle(context.Background(), stored); err == nil {
		t.Fatal("a failed invalidation must be reported so the outbox retries it")
	}
	// Duplicate delivery is harmless: each delivery just invalidates again.
	for i := 0; i < 2; i++ {
		if err := h.Handle(context.Background(), stored); err != nil {
			t.Fatalf("handle: %v", err)
		}
	}
	if got := n.notified(); len(got) != 2 || got[0] != ws.String() || got[1] != ws.String() {
		t.Fatalf("notified %v, want workspace %s twice", got, ws)
	}
}

// The event survives a failing notifier (retried) and a worker that crashed
// mid-processing (reaped and re-run by a new processor).
func TestInvalidationEvent_RetriedAndRecoveredThroughTheOutbox(t *testing.T) {
	t.Skip("bugs 4/5: demonstrates the gap; un-skip with the fix")
	pool, ctx := lifecycleTestPool(t)
	wsID := mustInsertWorkspace(t, ctx, pool, "invalidation-outbox")
	ob := outbox.NewOutbox(pool)

	enqueue := func() uuid.UUID {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if err := ob.Enqueue(ctx, tx, NewInvalidationEvent(uuid.MustParse(wsID), "test")); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		var id uuid.UUID
		if err := pool.QueryRow(ctx,
			`SELECT id FROM outbox_events WHERE event_type = $1 ORDER BY created_at DESC LIMIT 1`,
			EventPolicyInvalidationRequested).Scan(&id); err != nil {
			t.Fatalf("find event: %v", err)
		}
		return id
	}
	status := func(id uuid.UUID) (string, int) {
		var s string
		var retries int
		if err := pool.QueryRow(ctx, `SELECT status, retry_count FROM outbox_events WHERE id = $1`, id).Scan(&s, &retries); err != nil {
			t.Fatalf("event status: %v", err)
		}
		return s, retries
	}
	runUntilDone := func(n *flakyNotifier, id uuid.UUID) {
		t.Helper()
		reg := outbox.NewHandlerRegistry()
		if err := reg.RegisterHandler(EventPolicyInvalidationRequested, NewInvalidationHandler(n)); err != nil {
			t.Fatalf("register: %v", err)
		}
		p, err := outbox.NewProcessor(ob, reg, outbox.WithPollInterval(50*time.Millisecond))
		if err != nil {
			t.Fatalf("processor: %v", err)
		}
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { _ = p.Run(runCtx, 10); close(done) }()
		defer func() { cancel(); <-done }()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if s, _ := status(id); s == "done" {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		s, r := status(id)
		t.Fatalf("event not delivered: status=%s retries=%d", s, r)
	}

	// 1. The notifier fails twice: the event is retried, then delivered.
	id := enqueue()
	n := &flakyNotifier{failures: 2}
	runUntilDone(n, id)
	if _, retries := status(id); retries != 2 {
		t.Fatalf("retries = %d, want 2", retries)
	}
	if got := n.notified(); len(got) != 1 || got[0] != wsID {
		t.Fatalf("notified %v, want %s once", got, wsID)
	}

	// 2. A worker claimed the event and crashed before finishing: the reaper
	// returns it to the queue and a new processor delivers it.
	id = enqueue()
	claimed, err := ob.ClaimEvents(ctx, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim: %v (%d events)", err, len(claimed))
	}
	if _, err := pool.Exec(ctx, `UPDATE outbox_events SET claimed_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, id); err != nil {
		t.Fatalf("age claim: %v", err)
	}
	if reaped, err := ob.ReapAbandoned(ctx, 30*time.Second); err != nil || reaped != 1 {
		t.Fatalf("reap: %v (%d reaped)", err, reaped)
	}
	n = &flakyNotifier{}
	runUntilDone(n, id)
	if got := n.notified(); len(got) != 1 || got[0] != wsID {
		t.Fatalf("after recovery notified %v, want %s once", got, wsID)
	}
}
