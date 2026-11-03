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

// Revoke revokes shieldID in tenantID (certificate audit bug 3). In one
// transaction it marks the shield revoked, writes its tombstone (which
// outlives the shields row) and records the propagation work in the outbox,
// so the revocation cannot commit without it. The caller should then wake the
// dispatcher; if it does not, or the process dies first, the dispatcher's
// periodic pass and its startup pass deliver the work anyway. Terminal: a
// revoked shield is ErrNotRevocable.
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
	if _, err := tx.Exec(ctx,
		`INSERT INTO shield_revocations (shield_id, tenant_id) VALUES ($1, $2)
		 ON CONFLICT (shield_id) DO NOTHING`,
		shieldID, tenantID,
	); err != nil {
		return fmt.Errorf("revoke shield: record tombstone: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO shield_revocation_outbox (tenant_id, shield_id) VALUES ($1, $2)`,
		tenantID, shieldID,
	); err != nil {
		return fmt.Errorf("revoke shield: record propagation work: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("revoke shield: commit: %w", err)
	}
	return nil
}

// RevokedShieldIDs returns every revoked shield identity in tenantID, sorted:
// the tombstones, which survive the shields row (certificate audit bug 3).
func RevokedShieldIDs(ctx context.Context, db Querier, tenantID string) ([]string, error) {
	rows, err := db.Query(ctx,
		`SELECT shield_id::text FROM shield_revocations WHERE tenant_id = $1 ORDER BY shield_id::text`,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("list revoked shields: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
