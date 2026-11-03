package client

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/identity"
)

// errIssuanceNotAuthorized means the token's user may not be issued a device
// certificate right now (certificate audit bug 4).
var errIssuanceNotAuthorized = errors.New("user is not authorized for device certificates")

// authorizeDeviceIssuance checks, against current database state rather than
// the access token alone, that the token's user may be issued (or renewed) a
// device certificate (certificate audit bug 4). Mirrors /auth/refresh: the
// user must exist in the token's workspace, be active (CheckLifecycle), and
// the token must not predate a session-generation bump (CheckGeneration). If
// the user has a workspace membership row it must be active; a missing row
// does not deny (SCIM and first-login users have none).
//
// errIssuanceNotAuthorized means deny; any other error means the state could
// not be read — callers fail closed on both.
func authorizeDeviceIssuance(ctx context.Context, db *pgxpool.Pool, claims *auth.AccessTokenClaims) error {
	var tenantID, status string
	var generation int
	err := db.QueryRow(ctx,
		`SELECT tenant_id::text, status, identity_generation FROM users WHERE id = $1`,
		claims.UserID,
	).Scan(&tenantID, &status, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: user not found", errIssuanceNotAuthorized)
	}
	if err != nil {
		return fmt.Errorf("read user lifecycle state: %w", err)
	}
	if tenantID != claims.TenantID {
		return fmt.Errorf("%w: user is not in the token's workspace", errIssuanceNotAuthorized)
	}
	if err := identity.CheckLifecycle(status); err != nil {
		return fmt.Errorf("%w: %v", errIssuanceNotAuthorized, err)
	}
	if err := identity.CheckGeneration(claims.Generation, generation); err != nil {
		return fmt.Errorf("%w: %v", errIssuanceNotAuthorized, err)
	}
	var inactiveMembership bool
	if err := db.QueryRow(ctx,
		`SELECT EXISTS (
		     SELECT 1 FROM workspace_members
		      WHERE workspace_id = $1 AND user_id = $2 AND status <> 'active')`,
		claims.TenantID, claims.UserID,
	).Scan(&inactiveMembership); err != nil {
		return fmt.Errorf("read workspace membership: %w", err)
	}
	if inactiveMembership {
		return fmt.Errorf("%w: workspace membership is not active", errIssuanceNotAuthorized)
	}
	return nil
}

// deviceOwnerActiveSQL re-checks, inside the final conditional write on
// client_devices, that the device's user is still active and has no
// non-active membership — so a suspension that lands while a certificate is
// being issued means no certificate is returned.
const deviceOwnerActiveSQL = `
		    AND EXISTS (SELECT 1 FROM users u
		                 WHERE u.id = client_devices.user_id
		                   AND u.tenant_id = client_devices.workspace_id
		                   AND u.status = 'active')
		    AND NOT EXISTS (SELECT 1 FROM workspace_members wm
		                     WHERE wm.workspace_id = client_devices.workspace_id
		                       AND wm.user_id = client_devices.user_id
		                       AND wm.status <> 'active')`
