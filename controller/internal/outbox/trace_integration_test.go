package outbox

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/yourorg/ztna/controller/internal/telemetry"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// PENDING-10: an event published inside a traced request stores the request
// span's traceparent, and its dispatch span starts a new trace that links to
// that span. With tracing off, nothing is stored and the dispatch span has no
// link.
func TestOutboxTraceLinkIntegration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	dbName := outboxTestDBName(t)

	adminPool := outboxTestPool(t, ctx, adminDSN)
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

	testDSN, err := outboxTestDSN(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test database DSN: %v", err)
	}
	pool := outboxTestPool(t, ctx, testDSN)
	defer pool.Close()

	if err := applyOutboxTestMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	workspaceID := insertOutboxTestWorkspace(t, ctx, pool)
	store := NewOutbox(pool)
	registry := NewHandlerRegistry()
	if err := registry.RegisterHandler("test.trace.link", processorTestHandler{}); err != nil {
		t.Fatalf("register handler: %v", err)
	}
	processor, err := NewProcessor(store, registry)
	if err != nil {
		t.Fatalf("create processor: %v", err)
	}

	// enqueueAndDispatch publishes one event with pubCtx, claims and dispatches
	// it, and returns the stored traceparent.
	enqueueAndDispatch := func(t *testing.T, pubCtx context.Context) *string {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := store.Enqueue(pubCtx, tx, Event{
			EventType:     "test.trace.link",
			WorkspaceID:   workspaceID,
			CorrelationID: uuid.New(),
			Payload:       json.RawMessage(`{}`),
		}); err != nil {
			tx.Rollback(ctx)
			t.Fatalf("enqueue: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		claimed, err := store.ClaimEvents(ctx, 1)
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim: %v (%d events)", err, len(claimed))
		}
		if err := processor.processEvent(ctx, claimed[0]); err != nil {
			t.Fatalf("process: %v", err)
		}
		return claimed[0].Traceparent
	}

	t.Run("tracing on links dispatch to publisher", func(t *testing.T) {
		rec := tracetest.NewSpanRecorder()
		restore := telemetry.UseTracerProviderForTest(
			sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
		defer restore()

		pubCtx, pubSpan := telemetry.StartSpan(ctx, "POST /graphql")
		tp := enqueueAndDispatch(t, pubCtx)
		pubSpan.End()

		pub := pubSpan.SpanContext()
		want := "00-" + pub.TraceID().String() + "-" + pub.SpanID().String() + "-01"
		if tp == nil || *tp != want {
			t.Fatalf("stored traceparent = %v, want %s", tp, want)
		}

		var dispatch sdktrace.ReadOnlySpan
		for _, s := range rec.Ended() {
			if s.Name() == "outbox.dispatch_test.trace.link" {
				dispatch = s
			}
		}
		if dispatch == nil {
			t.Fatal("no dispatch span recorded")
		}
		if dispatch.Parent().IsValid() {
			t.Fatalf("dispatch span has parent %v, want a new root trace", dispatch.Parent())
		}
		if dispatch.SpanContext().TraceID() == pub.TraceID() {
			t.Fatal("dispatch span is in the publisher's trace, want a new trace")
		}
		links := dispatch.Links()
		if len(links) != 1 {
			t.Fatalf("dispatch span has %d links, want 1", len(links))
		}
		if got := links[0].SpanContext; got.TraceID() != pub.TraceID() || got.SpanID() != pub.SpanID() {
			t.Fatalf("link = %s/%s, want %s/%s",
				got.TraceID(), got.SpanID(), pub.TraceID(), pub.SpanID())
		}
	})

	t.Run("no span in publisher context stores nothing", func(t *testing.T) {
		rec := tracetest.NewSpanRecorder()
		restore := telemetry.UseTracerProviderForTest(
			sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
		defer restore()

		if tp := enqueueAndDispatch(t, ctx); tp != nil {
			t.Fatalf("stored traceparent = %q, want NULL", *tp)
		}
		for _, s := range rec.Ended() {
			if n := len(s.Links()); n != 0 {
				t.Fatalf("span %s has %d links, want 0", s.Name(), n)
			}
		}
	})

	t.Run("schema rejects a malformed traceparent", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO outbox_events (event_type, workspace_id, correlation_id, payload, traceparent)
			 VALUES ('x', $1, $2, '{}', 'not-a-traceparent')`,
			workspaceID, uuid.New())
		if err == nil {
			t.Fatal("malformed traceparent accepted")
		}
	})
}
