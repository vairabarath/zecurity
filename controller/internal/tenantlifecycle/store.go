// Package tenantlifecycle implements the provider's tenant lifecycle writes:
// suspend and resume (Sprint 23 S1, Decision Record D-45; amends D-07,
// preserves D-08, consistent with D-34).
//
// Suspension is workspaces.suspended_at (with suspended_by and
// suspension_reason); workspaces.status stays 'active'. What a suspended
// tenant may do is decided only by internal/tenantstate. This package writes
// the state and runs what must happen at the transition:
//
//   - in the suspend transaction: the suspension columns, a bump of every
//     tenant user's identity_generation (so every access and refresh token
//     issued before is dead at its next refresh), new policy versions for
//     every relay pool the workspace is assigned to (D-40: residency relays
//     drop the workspace at their next heartbeat), and the provider audit
//     row with before/after (F17);
//   - right after commit: each user's refresh sessions are deleted and the
//     client devices enrolled under them are revoked (the existing
//     session-end path, PENDING-09 C-1), the ACL is recompiled and pushed
//     (deny-all for a suspended tenant), and the relay lists are published
//     (an empty list for a suspended tenant).
//
// Connectors and shields are never touched (D-08): their certificates,
// renewals and control streams continue, and they get the deny-all ACL.
//
// Resume clears the columns in one transaction (new pool policy versions and
// the audit row too), then pushes the normal ACL and relay lists. Agents need
// no re-enrollment; users sign in again.
package tenantlifecycle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/relay"
)

var (
	// ErrNotFound means no such workspace.
	ErrNotFound = errors.New("tenant not found")
	// ErrAlreadySuspended: suspend of a suspended tenant.
	ErrAlreadySuspended = errors.New("tenant already suspended")
	// ErrNotSuspended: resume of a tenant that isn't suspended.
	ErrNotSuspended = errors.New("tenant not suspended")
	// ErrConfirmationMismatch: the typed slug isn't the tenant's (D-17).
	ErrConfirmationMismatch = errors.New("typed tenant slug does not match")
)

// Suspension is a tenant's suspension state. SuspendedAt nil = not suspended.
type Suspension struct {
	SuspendedAt *time.Time
	By          *string
	Reason      *string
}

// Audit is the before/after shape written to the provider audit (F17).
func (s Suspension) Audit() map[string]any {
	out := map[string]any{"suspended": s.SuspendedAt != nil}
	if s.SuspendedAt != nil {
		out["suspended_at"] = s.SuspendedAt.UTC()
		out["suspended_by"] = s.By
		out["suspension_reason"] = s.Reason
	}
	return out
}

// InTx runs inside the transaction (the audit row): an error rolls it back.
type InTx func(ctx context.Context, tx pgx.Tx, before, after Suspension) error

// SuspendResult is what a committed suspension changed.
type SuspendResult struct {
	Before, After Suspension
	// UserIDs are the tenant's users whose sessions are now revoked; the
	// caller ends their server-side sessions after commit.
	UserIDs []string
}

// Store writes tenant suspension state.
type Store struct {
	pool *pgxpool.Pool
}

// NewStore returns a Store.
func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// lockWorkspace locks the workspace row and reads its slug and suspension.
func lockWorkspace(ctx context.Context, tx pgx.Tx, workspaceID string) (string, Suspension, error) {
	var slug string
	var s Suspension
	err := tx.QueryRow(ctx, `
		SELECT slug, suspended_at, suspended_by, suspension_reason
		  FROM workspaces WHERE id = $1 FOR UPDATE`, workspaceID).
		Scan(&slug, &s.SuspendedAt, &s.By, &s.Reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", Suspension{}, ErrNotFound
	}
	if err != nil {
		return "", Suspension{}, fmt.Errorf("lock workspace: %w", err)
	}
	return slug, s, nil
}

// Suspend suspends the tenant. confirmSlug must equal its slug (D-17
// extended); by is the provider operator's email and reason is mandatory
// (the handler validates both). inTx (the audit row) runs in the same
// transaction.
func (st *Store) Suspend(ctx context.Context, workspaceID, confirmSlug, by, reason string, inTx InTx) (SuspendResult, error) {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return SuspendResult{}, fmt.Errorf("begin suspend: %w", err)
	}
	defer tx.Rollback(ctx)

	slug, before, err := lockWorkspace(ctx, tx, workspaceID)
	if err != nil {
		return SuspendResult{}, err
	}
	if confirmSlug != slug {
		return SuspendResult{}, ErrConfirmationMismatch
	}
	if before.SuspendedAt != nil {
		return SuspendResult{}, ErrAlreadySuspended
	}

	var after Suspension
	if err := tx.QueryRow(ctx, `
		UPDATE workspaces
		   SET suspended_at = NOW(), suspended_by = $2, suspension_reason = $3, updated_at = NOW()
		 WHERE id = $1
		RETURNING suspended_at, suspended_by, suspension_reason`, workspaceID, by, reason).
		Scan(&after.SuspendedAt, &after.By, &after.Reason); err != nil {
		return SuspendResult{}, fmt.Errorf("suspend workspace: %w", err)
	}

	// Revoke every session: a generation bump makes every token issued
	// before it dead at its next refresh (identity.CheckGeneration), the
	// existing mechanism (no new token type).
	rows, err := tx.Query(ctx, `
		UPDATE users SET identity_generation = identity_generation + 1, updated_at = NOW()
		 WHERE tenant_id = $1
		RETURNING id::text`, workspaceID)
	if err != nil {
		return SuspendResult{}, fmt.Errorf("revoke tenant sessions: %w", err)
	}
	userIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return SuspendResult{}, fmt.Errorf("revoke tenant sessions: %w", err)
	}

	// D-40: residency relays drop the workspace at their next heartbeat.
	if err := relay.BumpWorkspacePoolPoliciesTx(ctx, tx, workspaceID); err != nil {
		return SuspendResult{}, err
	}
	if inTx != nil {
		if err := inTx(ctx, tx, before, after); err != nil {
			return SuspendResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return SuspendResult{}, fmt.Errorf("commit suspend: %w", err)
	}
	return SuspendResult{Before: before, After: after, UserIDs: userIDs}, nil
}

// Resume lifts the tenant's suspension. inTx (the audit row) runs in the
// same transaction.
func (st *Store) Resume(ctx context.Context, workspaceID string, inTx InTx) (before, after Suspension, err error) {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return Suspension{}, Suspension{}, fmt.Errorf("begin resume: %w", err)
	}
	defer tx.Rollback(ctx)

	_, before, err = lockWorkspace(ctx, tx, workspaceID)
	if err != nil {
		return Suspension{}, Suspension{}, err
	}
	if before.SuspendedAt == nil {
		return Suspension{}, Suspension{}, ErrNotSuspended
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workspaces
		   SET suspended_at = NULL, suspended_by = NULL, suspension_reason = NULL, updated_at = NOW()
		 WHERE id = $1`, workspaceID); err != nil {
		return Suspension{}, Suspension{}, fmt.Errorf("resume workspace: %w", err)
	}
	if err := relay.BumpWorkspacePoolPoliciesTx(ctx, tx, workspaceID); err != nil {
		return Suspension{}, Suspension{}, err
	}
	if inTx != nil {
		if err := inTx(ctx, tx, before, after); err != nil {
			return Suspension{}, Suspension{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Suspension{}, Suspension{}, fmt.Errorf("commit resume: %w", err)
	}
	return before, after, nil
}
