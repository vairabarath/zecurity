package connector

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// StrandedShield is an enrolled shield that revoking its connector would leave
// with no connector to talk to (Sprint 20 Fix 16, F46).
type StrandedShield struct {
	ID   string
	Name string
}

// shieldQuerier runs a query. *db.TenantDB and *pgxpool.Pool satisfy it.
type shieldQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// StrandedShields returns the enrolled (active or disconnected) shields attached
// to connectorID when no other connector of the same remote network could serve
// them (one that is active or disconnected and not revoked). A shield only learns
// about connectors from connectors, so after its last one is revoked it can never
// re-attach and redials the revoked one; revoking the connector must then revoke
// these shields too, explicitly. Empty when another connector can take them over.
func StrandedShields(ctx context.Context, q shieldQuerier, tenantID, connectorID string) ([]StrandedShield, error) {
	rows, err := q.Query(ctx,
		`SELECT s.id, s.name
		   FROM shields s
		  WHERE s.tenant_id = $1
		    AND s.connector_id = $2
		    AND s.status IN ('active', 'disconnected')
		    AND NOT EXISTS (
		        SELECT 1 FROM connectors c
		         WHERE c.tenant_id = $1
		           AND c.remote_network_id = s.remote_network_id
		           AND c.id <> $2
		           AND c.status IN ('active', 'disconnected')
		           AND c.revoked_at IS NULL
		    )
		  ORDER BY s.name`,
		tenantID, connectorID,
	)
	if err != nil {
		return nil, fmt.Errorf("stranded shields: %w", err)
	}
	defer rows.Close()
	var out []StrandedShield
	for rows.Next() {
		var s StrandedShield
		if err := rows.Scan(&s.ID, &s.Name); err != nil {
			return nil, fmt.Errorf("stranded shields: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
