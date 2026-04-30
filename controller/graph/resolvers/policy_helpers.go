package resolvers

import (
	"context"
	"errors"
	"fmt"
	"time"

	pgx "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/models"
)

// loadUser fetches a single user row by ID.
func loadUser(ctx context.Context, pool *pgxpool.Pool, userID string) (*models.User, error) {
	var u models.User
	var lastLoginAt *time.Time
	err := pool.QueryRow(ctx,
		`SELECT id, tenant_id, email, provider, provider_sub, role, status, last_login_at, created_at, updated_at
		 FROM users WHERE id = $1`,
		userID,
	).Scan(&u.ID, &u.TenantID, &u.Email, &u.Provider, &u.ProviderSub,
		&u.Role, &u.Status, &lastLoginAt, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("user %s not found", userID)
	}
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}
	u.LastLoginAt = lastLoginAt
	return &u, nil
}

// loadResourceByID fetches a minimal resource record by ID for policy responses.
func loadResourceByID(ctx context.Context, pool *pgxpool.Pool, resourceID string) (*graph.Resource, error) {
	var (
		res          graph.Resource
		description  *string
		errorMessage *string
		appliedAt    *time.Time
		lastVerified *time.Time
		createdAt    time.Time
		networkID    string
		networkName  string
	)
	err := pool.QueryRow(ctx,
		`SELECT r.id, r.name, r.description, r.host, r.protocol, r.port_from, r.port_to,
		        r.status, r.error_message, r.applied_at, r.last_verified_at, r.created_at,
		        r.remote_network_id, rn.name
		 FROM resources r
		 JOIN remote_networks rn ON rn.id = r.remote_network_id
		 WHERE r.id = $1 AND r.deleted_at IS NULL`,
		resourceID,
	).Scan(
		&res.ID, &res.Name, &description, &res.Host, &res.Protocol, &res.PortFrom, &res.PortTo,
		&res.Status, &errorMessage, &appliedAt, &lastVerified, &createdAt,
		&networkID, &networkName,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("resource %s not found", resourceID)
	}
	if err != nil {
		return nil, fmt.Errorf("load resource: %w", err)
	}
	res.Description = description
	res.ErrorMessage = errorMessage
	res.AppliedAt = fmtTimePtr(appliedAt)
	res.LastVerifiedAt = fmtTimePtr(lastVerified)
	res.CreatedAt = fmtTime(createdAt)
	res.RemoteNetwork = &graph.RemoteNetwork{ID: networkID, Name: networkName}
	res.Groups = []*graph.Group{}
	return &res, nil
}
