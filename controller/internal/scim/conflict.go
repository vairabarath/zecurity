package scim

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/audit"
	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/permission"
)

// ── Conflict lifecycle (explicit state machine, ADR-025 §4.1/§9) ────────────
//
//	collision
//	   |
//	   v
//	PENDING
//	 /    \
//	accept   reject
//	  |        |
//	  v        v
//	LINKED   REJECTED
//	           |
//	         reopen
//	           |
//	           v
//	        PENDING
//
// Invariants enforced below:
//   - PENDING collision → 409 identity_conflict; the SCIM op must not mutate the
//     unrelated JIT/manual identity.
//   - An existing PENDING conflict is reused on retry (no duplicate pending row).
//   - REJECTED never auto-approves on a later SCIM retry; it blocks SCIM mutation
//     until an explicitly authorized Reopen.
//   - Invalid transitions (e.g. reject a LINKED conflict, accept a REJECTED one
//     without reopen, reopen a PENDING) fail safely and mutate nothing.
const (
	conflictPending  = "pending"
	conflictApproved = "approved"
	conflictRejected = "rejected"
)

// Audit action verbs for the conflict workflow.
const (
	actionConflictApproved    = "scim.user.conflict_approved"
	actionConflictRejected    = "scim.user.conflict_rejected"
	actionConflictReopened    = "scim.user.conflict_reopened"
	actionConflictPersistFail = "scim.user.conflict_persist_failed"
)

// ConflictRecord is a single pending/resolved identity-conflict row.
type ConflictRecord struct {
	ID               string
	WorkspaceID      string
	ConnectionID     string
	UserID           string // the unrelated JIT/manual identity occupying the key
	CanonicalKey     string
	Status           string
	ScimExternalID   string
	ResolutionReason string
	CreatedAt        string
	ResolvedAt       string
}

// conflictScope builds a scope with only the workspace/connection bound. The
// conflict engine never derives scope from a payload, and the conflict queries
// only need (workspace_id, connection_id); the other scope fields are unused here.
func (s *DirectoryService) conflictScope(workspaceID, connectionID string) *scope {
	return &scope{workspaceID: workspaceID, connectionID: connectionID}
}

// ensurePendingConflict persists (or reuses) a pending scim_identity_conflicts
// row for (workspace, connection, canonical key). It runs in its OWN short
// transaction because the calling SCIM request ultimately returns 409 and cannot
// share the request transaction. It is BEST-EFFORT: if the row write fails we
// still return the conflict to the caller (the 409 must be returned regardless),
// but we do NOT silently swallow the error — we publish it through the audit
// publisher as a structured failure so it is observable via the event bus.
//
// The unique partial index idx_conflicts_uniq_pending guarantees at most one
// pending row per key; ON CONFLICT DO NOTHING reuses the existing pending row on
// retry rather than creating a duplicate.
func (s *DirectoryService) ensurePendingConflict(ctx context.Context, workspaceID, connectionID, canonicalKey, userID, scimExternalID string) {
	if canonicalKey == "" {
		return
	}
	// Reuse any existing conflict row for this key (pending, rejected, or
	// approved) rather than creating a duplicate. The unique partial index
	// already forbids two pending rows; this guard additionally forbids
	// re-inserting a pending row while a rejected/approved one exists, so a
	// rejected conflict stays rejected until an explicit Reopen and a retry
	// does not spawn a fresh pending row.
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT TRUE FROM scim_identity_conflicts
		  WHERE workspace_id = $1 AND connection_id = $2 AND canonical_identity_key = $3 LIMIT 1`,
		workspaceID, connectionID, canonicalKey,
	).Scan(&exists); err != nil && !isNotFound(err) {
		// Observability only: the SCIM 409 still returns regardless.
		_ = s.audit.Publish(ctx, identity.Event{
			TenantID:   workspaceID,
			Action:     actionConflictPersistFail,
			TargetType: "scim_identity_conflict",
			TargetID:   userID,
			Details: map[string]any{
				"connection_id":          connectionID,
				"canonical_identity_key": canonicalKey,
				"error":                  err.Error(),
			},
		})
		return
	}
	if exists {
		return
	}

	rowID := uuid.New().String()
	_, err := s.pool.Exec(ctx,
		`INSERT INTO scim_identity_conflicts
		   (id, workspace_id, connection_id, user_id, canonical_identity_key, scim_external_id, status)
		 VALUES ($1, $2, $3, $4, $5, $6, 'pending')
		 ON CONFLICT (workspace_id, connection_id, canonical_identity_key)
		   WHERE status = 'pending'
		   DO NOTHING`,
		rowID, workspaceID, connectionID, userID, canonicalKey, scimExternalID,
	)
	if err != nil {
		// Best-effort persistence: the SCIM 409 still returns. Record (do not
		// swallow) the failure through the audit/publisher seam so the gap is
		// observable.
		_ = s.audit.Publish(ctx, identity.Event{
			TenantID:   workspaceID,
			Action:     actionConflictPersistFail,
			TargetType: "scim_identity_conflict",
			TargetID:   userID,
			Details: map[string]any{
				"connection_id":          connectionID,
				"canonical_identity_key": canonicalKey,
				"error":                  err.Error(),
			},
		})
	}
}

// GetConflict returns the (any-state) conflict for a key, or nil if none.
// Scope is bound by the workspace/connection — never the payload.
func (s *DirectoryService) GetConflict(ctx context.Context, workspaceID, connectionID, canonicalKey string) (*ConflictRecord, error) {
	if canonicalKey == "" {
		return nil, nil
	}
	return s.lookupConflict(ctx, workspaceID, connectionID, canonicalKey)
}

func (s *DirectoryService) lookupConflict(ctx context.Context, workspaceID, connectionID, canonicalKey string) (*ConflictRecord, error) {
	var c ConflictRecord
	err := s.pool.QueryRow(ctx,
		`SELECT id, workspace_id, connection_id, user_id, canonical_identity_key,
		        scim_external_id, status,
		        TO_CHAR(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS created_at,
		        COALESCE(TO_CHAR(resolved_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '') AS resolved_at
		   FROM scim_identity_conflicts
		  WHERE workspace_id = $1 AND connection_id = $2 AND canonical_identity_key = $3
		  ORDER BY created_at DESC LIMIT 1`,
		workspaceID, connectionID, canonicalKey,
	).Scan(&c.ID, &c.WorkspaceID, &c.ConnectionID, &c.UserID, &c.CanonicalKey,
		&c.ScimExternalID, &c.Status, &c.CreatedAt, &c.ResolvedAt)
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("lookup conflict: %w", err)
	}
	return &c, nil
}

// ListConflicts returns all conflicts for the workspace/connection (most recent
// first). Admin-scoped query.
func (s *DirectoryService) ListConflicts(ctx context.Context, workspaceID, connectionID string) ([]ConflictRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, workspace_id, connection_id, user_id, canonical_identity_key,
		        scim_external_id, status,
		        TO_CHAR(created_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"') AS created_at,
		        COALESCE(TO_CHAR(resolved_at, 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '') AS resolved_at
		   FROM scim_identity_conflicts
		  WHERE workspace_id = $1 AND connection_id = $2
		  ORDER BY created_at DESC`,
		workspaceID, connectionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list conflicts: %w", err)
	}
	defer rows.Close()
	var out []ConflictRecord
	for rows.Next() {
		var c ConflictRecord
		if err := rows.Scan(&c.ID, &c.WorkspaceID, &c.ConnectionID, &c.UserID, &c.CanonicalKey,
			&c.ScimExternalID, &c.Status, &c.CreatedAt, &c.ResolvedAt); err != nil {
			return nil, fmt.Errorf("scan conflict: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AcceptLink atomically links the SCIM identity to the existing user and marks
// the conflict linked. It requires the explicit identity.mapping.break_glass
// permission (ADMIN role alone is insufficient) and runs entirely inside one
// transaction:
//
//	BEGIN
//	  verify conflict is pending (and matches the expected user/key/scope)
//	  verify caller holds identity.mapping.break_glass
//	  confirm/create the external_identities link (workspace, connection, key)
//	  set provisioning_owner = 'scim' (preserve immutable provisioned_by)
//	  publish scim.user.conflict_approved audit event
//	COMMIT
//
// Any failure rolls back everything; the conflict stays pending and no ownership
// or link is changed.
func (s *DirectoryService) AcceptLink(ctx context.Context, workspaceID, connectionID, actorUserID, actorEmail, canonicalKey, reason string) *SCIMError {
	c, err := s.lookupConflict(ctx, workspaceID, connectionID, canonicalKey)
	if err != nil {
		return newSCIMError(500, "", "lookup conflict: "+err.Error())
	}
	if c == nil {
		return newSCIMError(404, "", "no conflict for canonical key")
	}
	if c.Status != conflictPending {
		return newSCIMError(409, "identity_conflict",
			fmt.Sprintf("conflict is %q; only a pending conflict may be accepted (reopen a rejected one first)", c.Status))
	}

	// Explicit, authoritative permission check — never implied by ADMIN role.
	if s.perm == nil {
		return newSCIMError(500, "", "permission store unavailable")
	}
	held, perr := s.perm.HasPermission(ctx, workspaceID, actorUserID, permission.BreakGlassMapping)
	if perr != nil {
		return newSCIMError(500, "", "permission check: "+perr.Error())
	}
	if !held {
		return newSCIMError(403, "", "requires the "+permission.BreakGlassMapping+" permission (ADMIN role alone is insufficient)")
	}

	// Verify the target identity still exists and matches the conflict's scope.
	var owner string
	if err := s.pool.QueryRow(ctx,
		`SELECT provisioning_owner FROM users WHERE id = $1 AND tenant_id = $2`,
		c.UserID, workspaceID,
	).Scan(&owner); err != nil {
		if isNotFound(err) {
			return newSCIMError(404, "", "conflict target identity no longer exists")
		}
		return newSCIMError(500, "", "verify conflict target: "+err.Error())
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return newSCIMError(500, "", "begin tx: "+err.Error())
	}
	defer tx.Rollback(ctx)

	// 1) Confirm/create the external_identities link for the correct scope.
	if _, err := tx.Exec(ctx,
		`INSERT INTO external_identities
		   (tenant_id, user_id, connection_id, issuer, subject, sync_instance_id)
		 VALUES ($1, $2, $3,
		   COALESCE((SELECT issuer FROM identity_connections WHERE id = $3), ''),
		   $4, NULL)
		 ON CONFLICT (tenant_id, connection_id, subject)
		   DO NOTHING`,
		workspaceID, c.UserID, connectionID, canonicalKey,
	); err != nil {
		return newSCIMError(500, "", "link external identity: "+err.Error())
	}

	// 2) Mark the conflict approved (idempotent against re-accept).
	if _, err := tx.Exec(ctx,
		`UPDATE scim_identity_conflicts
		    SET status = 'approved', resolved_at = NOW(), resolved_by = $2
		  WHERE id = $1 AND status = 'pending'`,
		c.ID, nullIfText(actorUserID),
	); err != nil {
		return newSCIMError(500, "", "resolve conflict: "+err.Error())
	}

	// 3) Set provisioning_owner -> scim. provisioned_by is immutable by design
	//    (it is NOT touched here), so local roles/policies/devices are preserved.
	if _, err := tx.Exec(ctx,
		`UPDATE users SET provisioning_owner = 'scim', updated_at = NOW()
		  WHERE id = $1 AND tenant_id = $2`,
		c.UserID, workspaceID,
	); err != nil {
		return newSCIMError(500, "", "set provisioning_owner: "+err.Error())
	}

	// 4) Audit the approval inside the same tx.
	if err := s.auditTx(ctx, tx, identity.Event{
		TenantID:    workspaceID,
		ActorUserID: actorUserID,
		ActorEmail:  actorEmail,
		Action:      actionConflictApproved,
		TargetType:  "user",
		TargetID:    c.UserID,
		Details: map[string]any{
			"connection_id":          connectionID,
			"canonical_identity_key": canonicalKey,
			"permission":             permission.BreakGlassMapping,
			"reason":                 reason,
		},
	}); err != nil {
		return newSCIMError(500, "", "audit approval: "+err.Error())
	}

	if err := tx.Commit(ctx); err != nil {
		return newSCIMError(500, "", "commit accept-link: "+err.Error())
	}
	return nil
}

// Reject closes a pending conflict as REJECTED (no identity change). It is
// audited and fails safely if the conflict is not pending.
func (s *DirectoryService) Reject(ctx context.Context, workspaceID, connectionID, actorUserID, actorEmail, canonicalKey, reason string) *SCIMError {
	return s.transitionConflict(ctx, workspaceID, connectionID, actorUserID, actorEmail, canonicalKey,
		conflictPending, conflictRejected, actionConflictRejected, reason)
}

// Reopen moves a REJECTED conflict back to PENDING and emits
// scim.user.conflict_reopened. Invalid from any state other than REJECTED.
func (s *DirectoryService) Reopen(ctx context.Context, workspaceID, connectionID, actorUserID, actorEmail, canonicalKey, reason string) *SCIMError {
	return s.transitionConflict(ctx, workspaceID, connectionID, actorUserID, actorEmail, canonicalKey,
		conflictRejected, conflictPending, actionConflictReopened, reason)
}

// transitionConflict performs a validated (from -> to) status change in one tx
// and audits the transition. It mutates no identity — only the conflict row.
func (s *DirectoryService) transitionConflict(ctx context.Context, workspaceID, connectionID, actorUserID, actorEmail, canonicalKey, from, to, action, reason string) *SCIMError {
	c, err := s.lookupConflict(ctx, workspaceID, connectionID, canonicalKey)
	if err != nil {
		return newSCIMError(500, "", "lookup conflict: "+err.Error())
	}
	if c == nil {
		return newSCIMError(404, "", "no conflict for canonical key")
	}
	if c.Status != from {
		return newSCIMError(409, "identity_conflict",
			fmt.Sprintf("conflict is %q; cannot transition to %q from here", c.Status, to))
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return newSCIMError(500, "", "begin tx: "+err.Error())
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx,
		`UPDATE scim_identity_conflicts
		    SET status = $3, resolved_at = NOW(), resolved_by = $4
		  WHERE id = $1 AND status = $2`,
		c.ID, from, to, nullIfText(actorUserID),
	); err != nil {
		return newSCIMError(500, "", "transition conflict: "+err.Error())
	}

	if err := s.auditTx(ctx, tx, identity.Event{
		TenantID:    workspaceID,
		ActorUserID: actorUserID,
		ActorEmail:  actorEmail,
		Action:      action,
		TargetType:  "user",
		TargetID:    c.UserID,
		Details: map[string]any{
			"connection_id":          connectionID,
			"canonical_identity_key": canonicalKey,
			"from":                   from,
			"to":                     to,
			"reason":                 reason,
		},
	}); err != nil {
		return newSCIMError(500, "", "audit transition: "+err.Error())
	}

	if err := tx.Commit(ctx); err != nil {
		return newSCIMError(500, "", "commit transition: "+err.Error())
	}
	return nil
}

// auditTx publishes an identity.Event inside the supplied transaction, so the
// approval/transition audit row commits atomically with the identity change.
func (s *DirectoryService) auditTx(ctx context.Context, tx pgx.Tx, e identity.Event) error {
	return auditRecordTx(ctx, tx, e)
}

// auditRecordTx writes an identity.Event to audit_logs inside the supplied
// transaction so the event commits atomically with the identity mutation.
func auditRecordTx(ctx context.Context, tx pgx.Tx, e identity.Event) error {
	return audit.RecordTx(ctx, tx, audit.Entry{
		TenantID:    e.TenantID,
		ActorUserID: e.ActorUserID,
		ActorEmail:  e.ActorEmail,
		Action:      e.Action,
		TargetType:  e.TargetType,
		TargetID:    e.TargetID,
		Details:     e.Details,
	})
}

// isNotFound normalizes pgx "no rows" into a boolean without importing pgx here.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no rows") || strings.Contains(err.Error(), "pgx.ErrNoRows")
}

// compile-time guard: DirectoryService must provide a *pgxpool.Pool to conflict.go.
var _ = pgxpool.Pool{}
