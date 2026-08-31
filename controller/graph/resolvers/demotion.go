package resolvers

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// Shield-demotion helpers shared by the connector and shield delete resolvers.
//
// These live OUTSIDE the *.resolvers.go files on purpose. gqlgen rewrites those
// files on every `make gqlgen` and relocates any helper it finds there into a
// trailing commented-out block — which silently breaks the build on the next
// codegen run. gqlgen says so itself in the block it generates: "You have helper
// methods in this file. Move them out to keep these resolver files clean."

func demoteResourcesForConnectorShields(ctx context.Context, tx pgx.Tx, tenantID, connectorID string) (int64, error) {
	res, err := tx.Exec(ctx,
		`UPDATE resources
	    SET status     = 'unprotected',
	        shield_id  = NULL,
	        updated_at = NOW()
	  WHERE tenant_id = $1
	    AND shield_id IN (SELECT id FROM shields WHERE connector_id = $2 AND tenant_id = $1)`,
		tenantID, connectorID,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}

func demoteResourcesForShield(ctx context.Context, tx pgx.Tx, tenantID, shieldID string) (int64, error) {
	res, err := tx.Exec(ctx,
		`UPDATE resources
	    SET status     = 'unprotected',
	        shield_id  = NULL,
	        updated_at = NOW()
	  WHERE tenant_id = $1
	    AND shield_id = $2`,
		tenantID, shieldID,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}
