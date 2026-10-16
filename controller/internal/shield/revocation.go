package shield

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrNotRevocable means the shield does not exist in the tenant or is not in
// a revocable status (revocation is terminal, so a second revoke lands here).
var ErrNotRevocable = errors.New("shield not found or not in revocable status")

// TxBeginner starts a transaction. *db.TenantDB satisfies it.
type TxBeginner interface {
	BeginTx(ctx context.Context) (pgx.Tx, error)
}

// Querier runs a query.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Revoke revokes shieldID in tenantID.
//
// Certificate audit bug 3: this only flips shields.status. Nothing records
// the revocation durably or the work to propagate it.
func Revoke(ctx context.Context, db TxBeginner, tenantID, shieldID string) error {
	tx, err := db.BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("revoke shield: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var revokedID string
	if err := tx.QueryRow(ctx,
		`UPDATE shields
		    SET status = 'revoked', updated_at = NOW()
		  WHERE id = $1
		    AND tenant_id = $2
		    AND status IN ('active', 'disconnected')
		 RETURNING id`,
		shieldID, tenantID,
	).Scan(&revokedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotRevocable
		}
		return fmt.Errorf("revoke shield: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("revoke shield: commit: %w", err)
	}
	return nil
}

// RevokedShieldIDs returns every revoked shield identity in tenantID, sorted.
//
// Certificate audit bug 3: read from shields.status, so the revocation of a
// shield whose row was deleted is lost.
func RevokedShieldIDs(ctx context.Context, db Querier, tenantID string) ([]string, error) {
	rows, err := db.Query(ctx,
		`SELECT id::text FROM shields WHERE tenant_id = $1 AND status = 'revoked' ORDER BY id`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list revoked shields: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
