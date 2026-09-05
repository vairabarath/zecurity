package identity

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Resolver maps a proven external identity to a canonical Zecurity user.
type Resolver struct{ pool *pgxpool.Pool }

// NewResolver constructs a Resolver over the given pool.
func NewResolver(pool *pgxpool.Pool) *Resolver { return &Resolver{pool: pool} }

// Resolve finds the canonical user linked to (connectionID, subject) via
// external_identities — the ADR-024 identity key. It NEVER resolves by email.
//
// tenantID scopes the lookup when the workspace is already known (the CLI
// logs in workspace-first). The platform web flow passes "" and takes the
// stable first match — mirroring the pre-Phase-5 provider_sub LIMIT 1 behavior
// for a shared platform IdP used across workspaces.
//
// Returns (core, true, nil) on a hit, (nil, false, nil) when no identity is
// linked yet (the Linker will JIT-create), or a non-nil error on failure.
func (r *Resolver) Resolve(ctx context.Context, connectionID, subject, tenantID string) (*PrincipalCore, bool, error) {
	const cols = `u.id, ei.tenant_id, u.role, u.email, u.status, u.identity_generation`

	var row pgx.Row
	if tenantID == "" {
		row = r.pool.QueryRow(ctx,
			`SELECT `+cols+`
			   FROM external_identities ei
			   JOIN users u ON u.id = ei.user_id
			  WHERE ei.connection_id = $1 AND ei.subject = $2
			  ORDER BY ei.created_at
			  LIMIT 1`,
			connectionID, subject)
	} else {
		row = r.pool.QueryRow(ctx,
			`SELECT `+cols+`
			   FROM external_identities ei
			   JOIN users u ON u.id = ei.user_id
			  WHERE ei.connection_id = $1 AND ei.subject = $2 AND ei.tenant_id = $3
			  LIMIT 1`,
			connectionID, subject, tenantID)
	}

	var c PrincipalCore
	err := row.Scan(&c.UserID, &c.TenantID, &c.Role, &c.Email, &c.Status, &c.Generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("resolve external identity: %w", err)
	}
	return &c, true, nil
}
