package rbac

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrLastAdmin refuses a change that would leave a workspace with no active
// Admin (PENDING-19 §7, R-11).
var ErrLastAdmin = errors.New("rbac: would leave the workspace with no active admin")

// LockActiveAdmins locks every active Admin row of a workspace in id order and
// returns their ids. Every path that can demote, suspend or delete an Admin
// calls this first in its transaction, before touching the target row, so
// concurrent changes serialize on the same locks in the same order (same
// pattern as provider.lockActiveSuperAdmins).
func LockActiveAdmins(ctx context.Context, tx pgx.Tx, tenantID string) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT id::text FROM users
		  WHERE tenant_id = $1 AND role = 'admin' AND status = 'active'
		  ORDER BY id
		    FOR UPDATE`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("rbac: lock admins: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("rbac: scan admin: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GuardLastAdminTx locks the workspace's active Admins and returns
// ErrLastAdmin if userID is one of them and no other active Admin remains.
// It refuses only that case: a target that is not an active Admin passes, even
// in a workspace that has no Admin at all.
func GuardLastAdminTx(ctx context.Context, tx pgx.Tx, tenantID, userID string) error {
	id, ok := CanonicalID(userID)
	if !ok {
		return fmt.Errorf("rbac: invalid user id")
	}
	admins, err := LockActiveAdmins(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	return lastAdminCheck(admins, map[string]bool{id: true})
}

// GuardLastAdminsTx is GuardLastAdminTx for a set of users changed together
// (bulk suspend): it refuses if the set contains every active Admin.
func GuardLastAdminsTx(ctx context.Context, tx pgx.Tx, tenantID string, userIDs []string) error {
	admins, err := LockActiveAdmins(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	set := make(map[string]bool, len(userIDs))
	for _, raw := range userIDs {
		id, ok := CanonicalID(raw)
		if !ok {
			return fmt.Errorf("rbac: invalid user id")
		}
		set[id] = true
	}
	return lastAdminCheck(admins, set)
}

func lastAdminCheck(admins []string, removed map[string]bool) error {
	hit, remaining := false, 0
	for _, id := range admins {
		if removed[id] {
			hit = true
		} else {
			remaining++
		}
	}
	if hit && remaining == 0 {
		return ErrLastAdmin
	}
	return nil
}
