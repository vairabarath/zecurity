package client

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/audit"
	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/outbox"
	"github.com/yourorg/ztna/controller/internal/policy"
)

// DeviceTrustRevokeHandler consumes the device.trust.revoke.requested outbox
// event (emitted by SCIM deprovision/reactivate) and revokes every one of the
// affected user's client-device certificates within the workspace.
//
// Revocation is the durable enforcement path: setting revoked_at puts the
// device's cert serial on the workspace CRL (GenerateClientCRL keys off
// revoked_at IS NOT NULL AND cert_serial IS NOT NULL), which connectors poll
// and use to reject the device. The handler itself does not push anything to
// the client — that is Track 2 (PENDING-13 server→client directive).
type DeviceTrustRevokeHandler struct {
	pool     *pgxpool.Pool
	notifier *policy.Notifier
}

// NewDeviceTrustRevokeHandler builds the revoke-requested handler. Notifier is
// used for the best-effort ACL push; a nil notifier is accepted (no push).
func NewDeviceTrustRevokeHandler(
	pool *pgxpool.Pool,
	notifier *policy.Notifier,
) *DeviceTrustRevokeHandler {
	return &DeviceTrustRevokeHandler{pool: pool, notifier: notifier}
}

// Handle implements outbox.EventHandler.
//
// Flow (per PENDING-13 Track 1):
//  1. Validate the typed UserID column is set (malformed event → error, which
//     the outbox surfaces as a failed event rather than silently dropping).
//  2. Autocommit revoke of all (workspace, user) devices (gated by
//     revoked_at IS NULL so replay is a no-op).
//  3. If 0 rows were affected (already revoked on replay), return nil — nothing
//     further to do.
//  4. Best-effort audit (log-and-continue) + best-effort ACL notify. Neither
//     failure fails the event: revoked_at → CRL is the durable path and is
//     already committed.
func (h *DeviceTrustRevokeHandler) Handle(ctx context.Context, evt outbox.OutboxEvent) error {
	if evt.UserID == nil {
		return fmt.Errorf("device.trust.revoke.requested: missing typed UserID column")
	}

	var payload identity.DeviceTrustEvent
	if len(evt.Payload) > 0 {
		if err := json.Unmarshal(evt.Payload, &payload); err != nil {
			return fmt.Errorf("device.trust.revoke.requested: unmarshal payload: %w", err)
		}
	}

	userID := evt.UserID.String()
	workspaceID := evt.WorkspaceID.String()

	count, err := revokeUserDevices(ctx, h.pool, userID, workspaceID)
	if err != nil {
		return fmt.Errorf("device.trust.revoke.requested: %w", err)
	}

	if count == 0 {
		// Already revoked (idempotent replay) — nothing to do.
		return nil
	}

	// Best-effort audit. audit.Record is log-and-continue on failure; a lost
	// audit row is acceptable here — losing it must never block the
	// revocation, which is already committed above.
	_ = audit.Record(ctx, h.pool, audit.Entry{
		TenantID:    workspaceID,
		ActorUserID: userID,
		Action:      "device.revoked",
		TargetType:  "user",
		TargetID:    userID,
		Details: map[string]any{
			"source":         "scim",
			"reason":         payload.Reason,
			"device_count":   count,
			"correlation_id": evt.CorrelationID.String(),
		},
	})

	// Best-effort ACL push. Connectors also poll the CRL directly, so a failed
	// notify does not leave the device un-revoked. Log only — do not fail.
	if h.notifier != nil {
		if err := h.notifier.NotifyPolicyChange(ctx, workspaceID); err != nil {
			fmt.Printf("device.trust.revoke.requested: policy notify failed (workspace=%s): %v\n", workspaceID, err)
		}
	}

	return nil
}

// DeviceTrustReEnrollHandler consumes device.trust.re_enrollment_required
// (emitted on SCIM reactivation). It is honest-minimal: the user's devices were
// already cert-revoked on the prior suspend, so they cannot connect and must
// re-enroll (which already works via interactive login). This handler records
// the requirement to audit and returns nil. It does NOT fake a client prompt or
// mutate any device — the proactive client directive is Track 2. Registering it
// now prevents reactivation events from dead-lettering.
type DeviceTrustReEnrollHandler struct {
	pool *pgxpool.Pool
}

// NewDeviceTrustReEnrollHandler builds the re-enrollment handler.
func NewDeviceTrustReEnrollHandler(pool *pgxpool.Pool) *DeviceTrustReEnrollHandler {
	return &DeviceTrustReEnrollHandler{pool: pool}
}

// Handle implements outbox.EventHandler. Records the re-enrollment requirement
// to audit and returns nil. It performs no device mutation.
func (h *DeviceTrustReEnrollHandler) Handle(ctx context.Context, evt outbox.OutboxEvent) error {
	if evt.UserID == nil {
		return fmt.Errorf("device.trust.re_enrollment_required: missing typed UserID column")
	}

	userID := evt.UserID.String()
	workspaceID := evt.WorkspaceID.String()

	_ = audit.Record(ctx, h.pool, audit.Entry{
		TenantID:    workspaceID,
		ActorUserID: userID,
		Action:      "device.re_enroll_required",
		TargetType:  "user",
		TargetID:    userID,
		Details: map[string]any{
			"source":         "scim",
			"correlation_id": evt.CorrelationID.String(),
		},
	})

	return nil
}

// Compile-time checks that both handlers satisfy the outbox contract.
var (
	_ outbox.EventHandler = (*DeviceTrustRevokeHandler)(nil)
	_ outbox.EventHandler = (*DeviceTrustReEnrollHandler)(nil)
)
