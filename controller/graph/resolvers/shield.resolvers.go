package resolvers

import (
	"context"
	"errors"
	"fmt"

	pgx "github.com/jackc/pgx/v5"
	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// GenerateShieldToken is the resolver for the generateShieldToken field.
func (r *mutationResolver) GenerateShieldToken(ctx context.Context, remoteNetworkID string, shieldName string) (*graph.ShieldToken, error) {
	tc := tenant.MustGet(ctx)

	// Select a placeholder connector to satisfy the NOT NULL FK constraint.
	// token.go's selectConnector overwrites this with the least-loaded connector.
	var placeholderConnectorID string
	err := r.TenantDB.QueryRow(ctx,
		`SELECT id FROM connectors
		  WHERE remote_network_id = $1
		    AND tenant_id = $2
		    AND status = 'active'
		  ORDER BY last_heartbeat_at DESC NULLS LAST
		  LIMIT 1`,
		remoteNetworkID, tc.TenantID,
	).Scan(&placeholderConnectorID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("generate shield token: no active connector found in remote network")
		}
		return nil, fmt.Errorf("generate shield token: lookup connector: %w", err)
	}

	var shieldID string
	err = r.TenantDB.QueryRow(ctx,
		`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status)
		 VALUES ($1, $2, $3, $4, 'pending')
		 RETURNING id`,
		tc.TenantID, remoteNetworkID, placeholderConnectorID, shieldName,
	).Scan(&shieldID)
	if err != nil {
		return nil, fmt.Errorf("generate shield token: insert shield: %w", err)
	}

	_, installCmd, err := r.ShieldSvc.GenerateShieldToken(ctx, remoteNetworkID, tc.TenantID, tc.TenantID, shieldID, shieldName)
	if err != nil {
		return nil, fmt.Errorf("generate shield token: %w", err)
	}

	return &graph.ShieldToken{
		ShieldID:       shieldID,
		InstallCommand: installCmd,
	}, nil
}

// RevokeShield is the resolver for the revokeShield field.
func (r *mutationResolver) RevokeShield(ctx context.Context, id string) (bool, error) {
	tc := tenant.MustGet(ctx)

	var discardedID string
	err := r.TenantDB.QueryRow(ctx,
		`UPDATE shields
		    SET status = 'revoked', updated_at = NOW()
		  WHERE id = $1
		    AND tenant_id = $2
		    AND status IN ('active', 'disconnected')
		 RETURNING id`,
		id, tc.TenantID,
	).Scan(&discardedID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("revoke shield: shield not found or not in revocable status")
		}
		return false, fmt.Errorf("revoke shield: %w", err)
	}

	return true, nil
}

// DeleteShield is the resolver for the deleteShield field.
func (r *mutationResolver) DeleteShield(ctx context.Context, id string) (bool, error) {
	tc := tenant.MustGet(ctx)

	var discardedID string
	err := r.TenantDB.QueryRow(ctx,
		`DELETE FROM shields
		  WHERE id = $1
		    AND tenant_id = $2
		    AND status IN ('pending', 'revoked')
		 RETURNING id`,
		id, tc.TenantID,
	).Scan(&discardedID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("delete shield: shield not found or must be revoked before deletion")
		}
		return false, fmt.Errorf("delete shield: %w", err)
	}

	return true, nil
}

// Shields is the resolver for the shields field.
func (r *queryResolver) Shields(ctx context.Context, remoteNetworkID string) ([]*graph.Shield, error) {
	tc := tenant.MustGet(ctx)

	shields, err := r.loadShields(ctx, tc.TenantID, remoteNetworkID)
	if err != nil {
		return nil, fmt.Errorf("shields: %w", err)
	}

	return shields, nil
}

// Shield is the resolver for the shield field.
func (r *queryResolver) Shield(ctx context.Context, id string) (*graph.Shield, error) {
	tc := tenant.MustGet(ctx)

	sh, err := scanShield(r.TenantDB.QueryRow(ctx,
		`SELECT id, name, status, remote_network_id, connector_id,
		        last_heartbeat_at, version, hostname, public_ip,
		        interface_addr, cert_not_after, created_at
		   FROM shields
		  WHERE id = $1
		    AND tenant_id = $2`,
		id, tc.TenantID,
	))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("shield: %w", err)
	}

	return sh, nil
}
