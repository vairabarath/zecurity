package resolvers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	pgx "github.com/jackc/pgx/v5"
	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/audit"
	"github.com/yourorg/ztna/controller/internal/invitation"
	"github.com/yourorg/ztna/controller/internal/posture"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

func invitationToGQL(inv *invitation.Invitation) *graph.Invitation {
	return &graph.Invitation{
		ID:        inv.ID,
		Email:     inv.Email,
		Status:    inv.Status,
		ExpiresAt: inv.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		CreatedAt: inv.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
	}
}

// clientDeviceColumns is the select list scanClientDevice reads, for
// client_devices d LEFT JOIN users u ON u.id = d.approved_by.
const clientDeviceColumns = `d.id, d.user_id, d.name, d.os, d.spiffe_id, d.cert_not_after,
		        d.last_seen_at, d.created_at, d.revoked_at, d.approved_at, u.email`

func scanClientDevice(row scanner) (*graph.ClientDevice, error) {
	var (
		id, userID, name, os string
		spiffeID             *string
		certNotAfter         *time.Time
		lastSeenAt           *time.Time
		createdAt            time.Time
		revokedAt            *time.Time
		approvedAt           *time.Time
		approvedByEmail      *string
	)
	if err := row.Scan(&id, &userID, &name, &os, &spiffeID, &certNotAfter, &lastSeenAt, &createdAt, &revokedAt,
		&approvedAt, &approvedByEmail); err != nil {
		return nil, fmt.Errorf("scan: %w", err)
	}

	d := &graph.ClientDevice{
		ID:         id,
		UserID:     userID,
		Name:       name,
		CommonName: name,
		Os:         os,
		SpiffeID:   spiffeID,
		CreatedAt:  createdAt.UTC().Format(time.RFC3339),
	}
	if certNotAfter != nil {
		s := certNotAfter.UTC().Format(time.RFC3339)
		d.CertNotAfter = &s
	}
	if lastSeenAt != nil {
		s := lastSeenAt.UTC().Format(time.RFC3339)
		d.LastSeenAt = &s
	}
	if revokedAt != nil {
		s := revokedAt.UTC().Format(time.RFC3339)
		d.RevokedAt = &s
	}
	if approvedAt != nil {
		s := approvedAt.UTC().Format(time.RFC3339)
		d.ApprovedAt = &s
		d.ApprovedByEmail = approvedByEmail
	}
	return d, nil
}

// setDeviceApproval approves a device, or removes the approval (ADR-030). The
// change and its audit entry commit together; the device's posture is then
// re-evaluated so a profile requiring zecurity.device.approved takes effect
// without waiting for the next report. Already in that state is a no-op.
func (r *mutationResolver) setDeviceApproval(ctx context.Context, deviceID string, approve bool) (*graph.ClientDevice, error) {
	tc, ok := tenant.Get(ctx)
	if !ok {
		return nil, fmt.Errorf("unauthenticated")
	}
	op := "unapprove device"
	if approve {
		op = "approve device"
	}
	workspaceID, err := uuid.Parse(tc.TenantID)
	if err != nil {
		return nil, fmt.Errorf("%s: invalid workspace id", op)
	}
	deviceUUID, err := uuid.Parse(deviceID)
	if err != nil {
		return nil, fmt.Errorf("%s: device not found", op)
	}

	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var (
		name              string
		revoked, approved bool
	)
	err = tx.QueryRow(ctx,
		`SELECT name, revoked_at IS NOT NULL, approved_at IS NOT NULL
		   FROM client_devices
		  WHERE id = $1
		    AND workspace_id = $2
		    FOR UPDATE`,
		deviceUUID, workspaceID,
	).Scan(&name, &revoked, &approved)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%s: device not found", op)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if approve && revoked {
		return nil, fmt.Errorf("%s: device is revoked", op)
	}

	changed := approved != approve
	if changed {
		if approve {
			_, err = tx.Exec(ctx,
				`UPDATE client_devices SET approved_at = NOW(), approved_by = $2 WHERE id = $1`,
				deviceUUID, tc.UserID)
		} else {
			_, err = tx.Exec(ctx,
				`UPDATE client_devices SET approved_at = NULL, approved_by = NULL WHERE id = $1`,
				deviceUUID)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", op, err)
		}
		action := "device.unapprove"
		if approve {
			action = "device.approve"
		}
		if err := audit.RecordTx(ctx, tx, audit.Entry{
			TenantID:    tc.TenantID,
			ActorUserID: tc.UserID,
			ActorEmail:  tc.Email,
			Action:      action,
			TargetType:  "client_device",
			TargetID:    deviceUUID.String(),
			Details:     map[string]any{"device_name": name},
		}); err != nil {
			return nil, fmt.Errorf("%s: audit: %w", op, err)
		}
	}

	device, err := scanClientDevice(tx.QueryRow(ctx,
		`SELECT `+clientDeviceColumns+`
		   FROM client_devices d
		   LEFT JOIN users u ON u.id = d.approved_by
		  WHERE d.id = $1`,
		deviceUUID))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}

	if changed && r.PostureEvaluator != nil {
		// A device with no posture report has no evaluation to update; it
		// already fails every ENFORCE profile.
		if _, err := r.PostureEvaluator.EvaluateDevice(ctx, workspaceID, deviceUUID); err != nil &&
			!errors.Is(err, posture.ErrNotFound) {
			return nil, fmt.Errorf("%s: saved, but re-evaluating access failed (retried on the next posture report): %w", op, err)
		}
	}
	return device, nil
}
