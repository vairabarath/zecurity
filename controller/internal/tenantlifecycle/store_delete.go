package tenantlifecycle

// Tenant deletion state (Sprint 23 S4, D-46; D-10: suspended tenants only,
// 30-day grace, cancellable; legal hold pauses the purge). The purge itself
// is purge.go; the tenant's own export and download are internal/tenantexport.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yourorg/ztna/controller/internal/tenantexport"
)

var (
	// ErrDeleteScheduled: resume (or a second schedule) while a deletion is
	// scheduled; cancel it first.
	ErrDeleteScheduled = errors.New("tenant deletion scheduled")
	// ErrDeleteNotScheduled: cancel without a scheduled deletion.
	ErrDeleteNotScheduled = errors.New("tenant deletion not scheduled")
	// ErrPurgeStarted: cancel after the purge began (its export may already
	// be written and agents revoked); the purge finishes.
	ErrPurgeStarted = errors.New("tenant purge already started")
	// ErrLegalHoldUnchanged: legal hold set to the state it already has.
	ErrLegalHoldUnchanged = errors.New("legal hold unchanged")
)

// DefaultDeleteGrace is the grace between scheduling and the purge (D-10).
const DefaultDeleteGrace = 30 * 24 * time.Hour

// Deletion is a tenant's deletion and legal-hold state.
type Deletion struct {
	ScheduledAt     *time.Time
	DeleteAfter     *time.Time
	By              *string
	Reason          *string
	LegalHold       bool
	LegalHoldReason *string
	LegalHoldBy     *string
	LegalHoldAt     *time.Time
}

// Audit is the before/after shape written to the provider audit (F17).
func (d Deletion) Audit() map[string]any {
	out := map[string]any{"delete_scheduled": d.ScheduledAt != nil, "legal_hold": d.LegalHold}
	if d.ScheduledAt != nil {
		out["delete_scheduled_at"] = d.ScheduledAt.UTC()
		out["delete_after"] = d.DeleteAfter.UTC()
		out["delete_by"] = d.By
		out["delete_reason"] = d.Reason
	}
	if d.LegalHold {
		out["legal_hold_reason"] = d.LegalHoldReason
		out["legal_hold_by"] = d.LegalHoldBy
	}
	return out
}

// DeleteInTx runs inside the transaction (the audit row).
type DeleteInTx func(ctx context.Context, tx pgx.Tx, before, after Deletion) error

const deletionColumns = `delete_scheduled_at, delete_after, delete_by, delete_reason,
	legal_hold, legal_hold_reason, legal_hold_by, legal_hold_at`

func scanDeletion(row pgx.Row, extra ...any) (Deletion, error) {
	var d Deletion
	err := row.Scan(append([]any{&d.ScheduledAt, &d.DeleteAfter, &d.By, &d.Reason,
		&d.LegalHold, &d.LegalHoldReason, &d.LegalHoldBy, &d.LegalHoldAt}, extra...)...)
	return d, err
}

// lockForDeletion locks the workspace row and reads slug, suspension and
// deletion state.
func lockForDeletion(ctx context.Context, tx pgx.Tx, workspaceID string) (slug string, suspended bool, d Deletion, err error) {
	var suspendedAt *time.Time
	d, err = scanDeletion(tx.QueryRow(ctx, `SELECT `+deletionColumns+`, slug, suspended_at
		  FROM workspaces WHERE id = $1 FOR UPDATE`, workspaceID), &slug, &suspendedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, Deletion{}, ErrNotFound
	}
	if err != nil {
		return "", false, Deletion{}, fmt.Errorf("lock workspace: %w", err)
	}
	return slug, suspendedAt != nil, d, nil
}

// ScheduleResult is what a committed schedule changed. Links are the
// emailed download tokens for the tenant's admins: the caller emails them
// after commit and never logs them.
type ScheduleResult struct {
	Before, After Deletion
	Links         []tenantexport.IssuedToken
}

// ScheduleDelete schedules the tenant's deletion grace from now (D-10).
// Refused unless the tenant is suspended (ErrNotSuspended), with the typed
// slug (D-17) and no deletion already scheduled. Each active tenant admin
// gets a download token valid until the purge (emailed by the caller).
func (st *Store) ScheduleDelete(ctx context.Context, workspaceID, confirmSlug, by, reason string, grace time.Duration, inTx DeleteInTx) (ScheduleResult, error) {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return ScheduleResult{}, fmt.Errorf("begin schedule delete: %w", err)
	}
	defer tx.Rollback(ctx)

	slug, suspended, before, err := lockForDeletion(ctx, tx, workspaceID)
	if err != nil {
		return ScheduleResult{}, err
	}
	if confirmSlug != slug {
		return ScheduleResult{}, ErrConfirmationMismatch
	}
	if !suspended {
		return ScheduleResult{}, ErrNotSuspended
	}
	if before.ScheduledAt != nil {
		return ScheduleResult{}, ErrDeleteScheduled
	}
	after, err := scanDeletion(tx.QueryRow(ctx, `
		UPDATE workspaces
		   SET delete_scheduled_at = NOW(), delete_after = NOW() + make_interval(secs => $2),
		       delete_by = $3, delete_reason = $4, updated_at = NOW()
		 WHERE id = $1
		RETURNING `+deletionColumns, workspaceID, grace.Seconds(), by, reason))
	if err != nil {
		return ScheduleResult{}, fmt.Errorf("schedule delete: %w", err)
	}
	links, err := tenantexport.IssueEmailTokensTx(ctx, tx, workspaceID, *after.DeleteAfter)
	if err != nil {
		return ScheduleResult{}, err
	}
	if inTx != nil {
		if err := inTx(ctx, tx, before, after); err != nil {
			return ScheduleResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ScheduleResult{}, fmt.Errorf("commit schedule delete: %w", err)
	}
	return ScheduleResult{Before: before, After: after, Links: links}, nil
}

// CancelDelete cancels a scheduled deletion before the purge starts. The
// emailed download links stop working.
func (st *Store) CancelDelete(ctx context.Context, workspaceID string, inTx DeleteInTx) (before, after Deletion, err error) {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return Deletion{}, Deletion{}, fmt.Errorf("begin cancel delete: %w", err)
	}
	defer tx.Rollback(ctx)

	_, _, before, err = lockForDeletion(ctx, tx, workspaceID)
	if err != nil {
		return Deletion{}, Deletion{}, err
	}
	if before.ScheduledAt == nil {
		return Deletion{}, Deletion{}, ErrDeleteNotScheduled
	}
	var started bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenant_purges WHERE workspace_id = $1)`, workspaceID).Scan(&started); err != nil {
		return Deletion{}, Deletion{}, fmt.Errorf("check purge: %w", err)
	}
	if started {
		return Deletion{}, Deletion{}, ErrPurgeStarted
	}
	after, err = scanDeletion(tx.QueryRow(ctx, `
		UPDATE workspaces
		   SET delete_scheduled_at = NULL, delete_after = NULL, delete_by = NULL, delete_reason = NULL, updated_at = NOW()
		 WHERE id = $1
		RETURNING `+deletionColumns, workspaceID))
	if err != nil {
		return Deletion{}, Deletion{}, fmt.Errorf("cancel delete: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM tenant_export_tokens WHERE workspace_id = $1`, workspaceID); err != nil {
		return Deletion{}, Deletion{}, fmt.Errorf("drop download tokens: %w", err)
	}
	if inTx != nil {
		if err := inTx(ctx, tx, before, after); err != nil {
			return Deletion{}, Deletion{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Deletion{}, Deletion{}, fmt.Errorf("commit cancel delete: %w", err)
	}
	return before, after, nil
}

// SetLegalHold sets or clears the legal hold. A hold pauses the purge (it
// isn't started, and a started purge stops before destroying anything
// further) until cleared.
func (st *Store) SetLegalHold(ctx context.Context, workspaceID string, on bool, by, reason string, inTx DeleteInTx) (before, after Deletion, err error) {
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		return Deletion{}, Deletion{}, fmt.Errorf("begin legal hold: %w", err)
	}
	defer tx.Rollback(ctx)

	_, _, before, err = lockForDeletion(ctx, tx, workspaceID)
	if err != nil {
		return Deletion{}, Deletion{}, err
	}
	if before.LegalHold == on {
		return Deletion{}, Deletion{}, ErrLegalHoldUnchanged
	}
	q := `UPDATE workspaces SET legal_hold = true, legal_hold_reason = $2, legal_hold_by = $3, legal_hold_at = NOW(), updated_at = NOW()
		 WHERE id = $1 RETURNING ` + deletionColumns
	args := []any{workspaceID, reason, by}
	if !on {
		q = `UPDATE workspaces SET legal_hold = false, legal_hold_reason = NULL, legal_hold_by = NULL, legal_hold_at = NULL, updated_at = NOW()
		 WHERE id = $1 RETURNING ` + deletionColumns
		args = []any{workspaceID}
	}
	after, err = scanDeletion(tx.QueryRow(ctx, q, args...))
	if err != nil {
		return Deletion{}, Deletion{}, fmt.Errorf("set legal hold: %w", err)
	}
	if inTx != nil {
		if err := inTx(ctx, tx, before, after); err != nil {
			return Deletion{}, Deletion{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Deletion{}, Deletion{}, fmt.Errorf("commit legal hold: %w", err)
	}
	return before, after, nil
}
