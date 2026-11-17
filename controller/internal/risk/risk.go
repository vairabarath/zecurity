// Package risk is the basic per-device risk score of PENDING-06 (M-9).
//
// A device collects points from a small, fixed set of signals. When the
// score reaches Threshold the device must authenticate again
// (client_devices.reauth_required_at) before it may reach any resource whose
// policy has an authentication requirement; device-only policies are not
// affected. A successful re-authentication clears the score (the step-up
// flow in internal/client does that in the same statement that stamps the
// session).
//
// The score is deliberately simple and explainable: no decay, no weighting
// beyond the fixed points, every change audited with its reason.
package risk

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/audit"
)

// Threshold is the score at which a device must authenticate again.
const Threshold = 50

// maxScore caps the stored score; signals can repeat without bound.
const maxScore = 1000

// Signal is one risk event.
type Signal struct {
	Reason string
	Points int
}

var (
	// CountryChanged: the device was seen in a different country than
	// before (both known).
	CountryChanged = Signal{Reason: "country_changed", Points: 50}
	// MFAFailures: five wrong TOTP codes within fifteen minutes.
	MFAFailures = Signal{Reason: "mfa_failures", Points: 50}
	// PostureRegressed: the device stopped satisfying a device profile that
	// a resource policy requires.
	PostureRegressed = Signal{Reason: "posture_regressed", Points: 30}
)

// ActionReauthRequired is the audit action written when a device's score
// first reaches the threshold.
const ActionReauthRequired = "device.reauth_required"

// Notifier recompiles the workspace ACL (policy.Notifier).
type Notifier interface {
	NotifyPolicyChange(ctx context.Context, workspaceID string) error
}

// Service applies risk signals to devices.
type Service struct {
	pool     *pgxpool.Pool
	notifier Notifier
}

func NewService(pool *pgxpool.Pool, notifier Notifier) *Service {
	return &Service{pool: pool, notifier: notifier}
}

// Add applies one signal to an active device. It reports whether this signal
// is the one that made the device need re-authentication. A revoked or
// unknown device is a no-op.
func (s *Service) Add(ctx context.Context, workspaceID, deviceID string, sig Signal) (bool, error) {
	if s == nil || s.pool == nil {
		return false, nil
	}
	var (
		score    int
		demanded bool
		userID   string
	)
	// One statement: the row lock taken by the UPDATE makes concurrent
	// signals add up, and "demanded" is true for exactly one of them.
	err := s.pool.QueryRow(ctx,
		`WITH old AS (
		   SELECT id, reauth_required_at FROM client_devices
		    WHERE id = $1 AND workspace_id = $2 AND revoked_at IS NULL
		    FOR UPDATE
		 )
		 UPDATE client_devices cd
		    SET risk_score = LEAST(cd.risk_score + $3, $5),
		        risk_reasons = CASE WHEN $4 = ANY(cd.risk_reasons) THEN cd.risk_reasons
		                            ELSE array_append(cd.risk_reasons, $4) END,
		        reauth_required_at = CASE
		            WHEN cd.reauth_required_at IS NULL AND cd.risk_score + $3 >= $6 THEN NOW()
		            ELSE cd.reauth_required_at END
		   FROM old
		  WHERE cd.id = old.id
		 RETURNING cd.risk_score,
		           (old.reauth_required_at IS NULL AND cd.reauth_required_at IS NOT NULL),
		           cd.user_id::text`,
		deviceID, workspaceID, sig.Points, sig.Reason, maxScore, Threshold,
	).Scan(&score, &demanded, &userID)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("risk: add %s to device %s: %w", sig.Reason, deviceID, err)
	}
	if !demanded {
		return false, nil
	}
	_ = audit.Record(context.WithoutCancel(ctx), s.pool, audit.Entry{
		TenantID:   workspaceID,
		Action:     ActionReauthRequired,
		TargetType: "client_device",
		TargetID:   deviceID,
		Details:    map[string]any{"reason": sig.Reason, "risk_score": score, "user_id": userID},
	})
	if s.notifier != nil {
		if err := s.notifier.NotifyPolicyChange(context.WithoutCancel(ctx), workspaceID); err != nil {
			log.Printf("risk: notify policy change for workspace %s: %v", workspaceID, err)
		}
	}
	return true, nil
}
