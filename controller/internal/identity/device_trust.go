package identity

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/yourorg/ztna/controller/internal/outbox"
)

// Device-trust outbox event contract.
//
// These symbols are the single source of truth for the device-trust events
// exchanged between the SCIM producer (PENDING-05, deprovision/reactivate) and
// the client-device outbox consumer (PENDING-13 / Track 1). BOTH sides import
// this file, so the event-type strings and payload shape can never drift
// through independent hardcoding. The producer MUST build events with
// NewDeviceTrustRevokeEvent / NewDeviceTrustReEnrollmentRequired (see below),
// which set the typed UserID column in addition to the JSON payload.
//
// Event-type strings are frozen in ADR-025 §5.1. Do not rename without a
// coordinated migration of both producer and consumer.
const (
	// EventDeviceTrustRevokeRequested is emitted when a user is suspended or
	// deleted (SCIM). The consumer revokes every one of the user's client-device
	// certs so they land on the workspace CRL and connectors reject them.
	EventDeviceTrustRevokeRequested = "device.trust.revoke.requested"

	// EventDeviceTrustReEnrollmentRequired is emitted on user reactivation
	// (SCIM). The consumer records the requirement to audit; the user's devices
	// were already cert-revoked on the prior suspend, so they must re-enroll —
	// which already works via interactive login. The proactive client directive
	// is a Track 2 concern (PENDING-13).
	EventDeviceTrustReEnrollmentRequired = "device.trust.re_enrollment_required"
)

// DeviceTrustReason enumerates the deprovision trigger so the consumer's audit
// row can record why a device was revoked. Kept as a small typed set so the
// producer and consumer agree on the vocabulary.
type DeviceTrustReason string

const (
	// DeviceTrustReasonSuspended — SCIM PATCH active=false (deprovision).
	DeviceTrustReasonSuspended DeviceTrustReason = "suspended"
	// DeviceTrustReasonDeleted — SCIM DELETE (soft-delete).
	DeviceTrustReasonDeleted DeviceTrustReason = "deleted"
)

// DeviceTrustEvent is the structured payload carried by a device-trust outbox
// event. The producer marshals a DeviceTrustEvent into the event's opaque JSON
// payload; the consumer unmarshals it. Reason is omitted for the re-enrollment
// event.
//
// Note: the authoritative identity keys (workspace, user) are carried in the
// outbox event's typed WorkspaceID/UserID columns, NOT in this struct. This
// struct holds only the application-specific context. (The SCIM Phase 6 plan
// sketched a `Type` field inside the struct — that is intentionally folded into
// the outbox.EventType constant instead, so there is a single event-type source
// of truth and no duplicate/divergent field.)
type DeviceTrustEvent struct {
	WorkspaceID   string           `json:"workspace_id"`
	UserID        string           `json:"user_id"`
	Reason        DeviceTrustReason `json:"reason,omitempty"`
	CorrelationID string           `json:"correlation_id,omitempty"`
}

// SideEffectSink is the SCIM→outbox boundary (ADR-025 §5.1, Sprint17 Phase 6).
// It lives in identity so scim/identity never import the outbox package
// directly and unit tests can inject a fake. The production implementation
// (DurableOutboxSink) is defined in the scim package and adapts this to
// outbox.Enqueue.
//
// Enqueue runs inside the caller's transaction, so the device-trust event
// commits atomically with the identity mutation (suspend / delete / reactivate).
type SideEffectSink interface {
	Enqueue(ctx context.Context, tx pgx.Tx, evt DeviceTrustEvent) error
}

// NewDeviceTrustRevokeEvent builds a fully-formed outbox event for a SCIM
// deprovision (suspend or delete). It sets BOTH the typed UserID column (so the
// consumer's nil-guard is never hit) AND the JSON payload, guaranteeing the
// producer cannot enqueue a device-trust event without an identifiable user.
//
// reason should be DeviceTrustReasonSuspended or DeviceTrustReasonDeleted.
func NewDeviceTrustRevokeEvent(
	workspaceID uuid.UUID,
	userID uuid.UUID,
	reason DeviceTrustReason,
	correlationID uuid.UUID,
) outbox.Event {
	payload, _ := json.Marshal(DeviceTrustEvent{
		WorkspaceID:   workspaceID.String(),
		UserID:        userID.String(),
		Reason:        reason,
		CorrelationID: correlationID.String(),
	})
	uid := userID
	return outbox.Event{
		EventType:     EventDeviceTrustRevokeRequested,
		WorkspaceID:   workspaceID,
		UserID:        &uid,
		CorrelationID: correlationID,
		Payload:       payload,
	}
}

// NewDeviceTrustReEnrollmentRequired builds a fully-formed outbox event for a
// SCIM reactivation. Like the revoke constructor, it sets the typed UserID and
// the JSON payload (Reason is omitted for this event).
func NewDeviceTrustReEnrollmentRequired(
	workspaceID uuid.UUID,
	userID uuid.UUID,
	correlationID uuid.UUID,
) outbox.Event {
	payload, _ := json.Marshal(DeviceTrustEvent{
		WorkspaceID:   workspaceID.String(),
		UserID:        userID.String(),
		CorrelationID: correlationID.String(),
	})
	uid := userID
	return outbox.Event{
		EventType:     EventDeviceTrustReEnrollmentRequired,
		WorkspaceID:   workspaceID,
		UserID:        &uid,
		CorrelationID: correlationID,
		Payload:       payload,
	}
}
