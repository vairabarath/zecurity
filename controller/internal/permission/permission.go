// Package permission implements the smallest fine-grained permission primitive
// needed by Sprint 17 / ADR-025: an explicit, per-(workspace,user) grant stored
// in the workspace_permissions table.
//
// Locked rule (Phase 3): possession is ALWAYS an explicit row. The store never
// consults a caller's role — an ADMIN without a row does NOT satisfy any
// permission check. Wider authorization (e.g. "ADMIN may grant") lives at the
// GraphQL layer via the @hasRole directive, not here.
package permission

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Canonical permission strings. Import these instead of typing the string
// literals at call sites so there is exactly one spelling of each permission.
const (
	// BreakGlassMapping gates directory-mapping overrides (e.g. fail-closed
	// mapping probes). Only an explicit grant satisfies it — never ADMIN alone.
	BreakGlassMapping = "identity.mapping.break_glass"
)

// ErrInvalidScope is returned when a required identifier is empty.
var ErrInvalidScope = errors.New("permission: invalid scope")

// Store reads and writes workspace_permissions.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore builds a Store from a pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// HasPermission reports whether userID explicitly holds permission in
// workspaceID. It performs an exact primary-key lookup and NEVER falls back to
// role checks — absence of a row (including for admins) is false.
func (s *Store) HasPermission(ctx context.Context, workspaceID, userID, permission string) (bool, error) {
	if workspaceID == "" || userID == "" || permission == "" {
		return false, ErrInvalidScope
	}
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM workspace_permissions
			WHERE workspace_id = $1 AND user_id = $2 AND permission = $3
		)`,
		workspaceID, userID, permission,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("permission.HasPermission: %w", err)
	}
	return exists, nil
}

// Grant records an explicit grant. It is idempotent: re-granting an existing
// (workspace, user, permission) row is a no-op rather than an error.
func (s *Store) Grant(ctx context.Context, workspaceID, userID, permission, grantedBy string) error {
	if workspaceID == "" || userID == "" || permission == "" {
		return ErrInvalidScope
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO workspace_permissions
		   (workspace_id, user_id, permission, granted_by)
		 VALUES ($1, $2, $3, NULLIF($4, '')::uuid)
		 ON CONFLICT (workspace_id, user_id, permission) DO NOTHING`,
		workspaceID, userID, permission, grantedBy,
	)
	if err != nil {
		return fmt.Errorf("permission.Grant: %w", err)
	}
	return nil
}

// Revoke deletes an explicit grant. Deleting a non-existent row is not an error
// (idempotent); use HasPermission to distinguish "never had it" from "had it".
func (s *Store) Revoke(ctx context.Context, workspaceID, userID, permission string) error {
	if workspaceID == "" || userID == "" || permission == "" {
		return ErrInvalidScope
	}
	_, err := s.pool.Exec(ctx,
		`DELETE FROM workspace_permissions
		 WHERE workspace_id = $1 AND user_id = $2 AND permission = $3`,
		workspaceID, userID, permission,
	)
	if err != nil {
		return fmt.Errorf("permission.Revoke: %w", err)
	}
	return nil
}
