package scim

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/outbox"
)

// DurableOutboxSink is the production identity.SideEffectSink (ADR-025 §5.1,
// Sprint17 Phase 6). It serializes a DeviceTrustEvent into the merged durable
// outbox (Sprint 18 / fixed-pendings) inside the caller's transaction.
//
// The scim package is the ONLY place that imports outbox; identity.DirectoryService
// depends only on the SideEffectSink interface, so the identity package never
// imports outbox directly. Enqueue runs inside the caller's tx, so the
// device-trust event commits atomically with the identity mutation
// (suspend / delete / reactivate).
type DurableOutboxSink struct {
	outbox *outbox.Outbox
}

// NewDurableOutboxSink builds the production sink from the outbox store already
// wired in main.go.
func NewDurableOutboxSink(o *outbox.Outbox) *DurableOutboxSink {
	return &DurableOutboxSink{outbox: o}
}

// Enqueue converts the DeviceTrustEvent into the canonical outbox.Event via the
// frozen constructors (which set the typed UserID column, so the consumer's
// nil-guard is never hit) and enqueues it inside tx.
func (s *DurableOutboxSink) Enqueue(ctx context.Context, tx pgx.Tx, evt identity.DeviceTrustEvent) error {
	ws, err := uuid.Parse(evt.WorkspaceID)
	if err != nil {
		return fmt.Errorf("sink: parse workspace id: %w", err)
	}
	uid, err := uuid.Parse(evt.UserID)
	if err != nil {
		return fmt.Errorf("sink: parse user id: %w", err)
	}
	corr := uuid.Nil
	if evt.CorrelationID != "" {
		if corr, err = uuid.Parse(evt.CorrelationID); err != nil {
			return fmt.Errorf("sink: parse correlation id: %w", err)
		}
	}
	var event outbox.Event
	if evt.Reason == "" {
		// No reason → re-enrollment (reactivate) event.
		event = identity.NewDeviceTrustReEnrollmentRequired(ws, uid, corr)
	} else {
		event = identity.NewDeviceTrustRevokeEvent(ws, uid, evt.Reason, corr)
	}
	return s.outbox.Enqueue(ctx, tx, event)
}
