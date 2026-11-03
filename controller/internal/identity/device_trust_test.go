package identity

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestNewDeviceTrustRevokeEvent verifies the sanctioned producer constructor
// sets the typed UserID column (the contract Sathiya's SCIM Phase 6 must use).
// If this passes, the consumer's "missing typed UserID" guard can never fire
// for an event built through the official constructor.
func TestNewDeviceTrustRevokeEvent(t *testing.T) {
	wsID := uuid.New()
	userID := uuid.New()
	corr := uuid.New()

	evt := NewDeviceTrustRevokeEvent(wsID, userID, DeviceTrustReasonSuspended, corr)

	if evt.EventType != EventDeviceTrustRevokeRequested {
		t.Fatalf("EventType = %q, want %q", evt.EventType, EventDeviceTrustRevokeRequested)
	}
	if evt.WorkspaceID != wsID {
		t.Fatalf("WorkspaceID = %v, want %v", evt.WorkspaceID, wsID)
	}
	if evt.UserID == nil {
		t.Fatal("UserID must be set (typed column) — this is the coordination contract")
	}
	if *evt.UserID != userID {
		t.Fatalf("UserID = %v, want %v", *evt.UserID, userID)
	}
	if evt.CorrelationID != corr {
		t.Fatalf("CorrelationID = %v, want %v", evt.CorrelationID, corr)
	}

	var payload DeviceTrustEvent
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.UserID != userID.String() {
		t.Fatalf("payload UserID = %q, want %q", payload.UserID, userID.String())
	}
	if payload.Reason != DeviceTrustReasonSuspended {
		t.Fatalf("payload Reason = %q, want %q", payload.Reason, DeviceTrustReasonSuspended)
	}
	if payload.CorrelationID != corr.String() {
		t.Fatalf("payload CorrelationID = %q, want %q", payload.CorrelationID, corr.String())
	}
}

// TestNewDeviceTrustReEnrollmentRequired verifies the reactivation constructor
// sets the typed UserID and omits Reason in the payload.
func TestNewDeviceTrustReEnrollmentRequired(t *testing.T) {
	wsID := uuid.New()
	userID := uuid.New()
	corr := uuid.New()

	evt := NewDeviceTrustReEnrollmentRequired(wsID, userID, corr)

	if evt.EventType != EventDeviceTrustReEnrollmentRequired {
		t.Fatalf("EventType = %q, want %q", evt.EventType, EventDeviceTrustReEnrollmentRequired)
	}
	if evt.UserID == nil || *evt.UserID != userID {
		t.Fatalf("UserID must be set to %v, got %v", userID, evt.UserID)
	}
	if evt.WorkspaceID != wsID {
		t.Fatalf("WorkspaceID = %v, want %v", evt.WorkspaceID, wsID)
	}

	var payload DeviceTrustEvent
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Reason != "" {
		t.Fatalf("re-enroll payload must omit Reason, got %q", payload.Reason)
	}
}

// TestDurableOutboxSinkShape documents the production adapter contract: a
// SideEffectSink implementation enqueues via outbox.Enqueue using the typed
// columns. We assert the interface is satisfiable (a fake sink qualifies) and
// that the sanctioned constructor yields an event with the typed columns the
// adapter would hand to outbox.Enqueue.
func TestSideEffectSinkInterface(t *testing.T) {
	var _ SideEffectSink = fakeSink{}

	wsID := uuid.New()
	userID := uuid.New()

	// The production DurableOutboxSink (scim package) will do roughly:
	//   outbox.Enqueue(ctx, tx, NewDeviceTrustRevokeEvent(wsID, userID, reason, corr))
	// Assert the constructor output carries the columns Enqueue requires.
	evt := NewDeviceTrustRevokeEvent(wsID, userID, DeviceTrustReasonDeleted, uuid.New())
	if evt.WorkspaceID != wsID || evt.UserID == nil {
		t.Fatal("constructor output missing required typed columns for Enqueue")
	}
	_ = context.Background()
	_ = pgx.Tx(nil)
}

type fakeSink struct{}

func (fakeSink) Enqueue(_ context.Context, _ pgx.Tx, _ DeviceTrustEvent) error { return nil }
