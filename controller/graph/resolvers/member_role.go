package resolvers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/apperr"
	"github.com/yourorg/ztna/controller/internal/audit"
	"github.com/yourorg/ztna/controller/internal/models"
	"github.com/yourorg/ztna/controller/internal/rbac"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// maxRoleReasonLen caps the audited reason for a role change (R-6).
const maxRoleReasonLen = 500

// actionMemberRoleChanged is the audit_logs action for setMemberRole.
const actionMemberRoleChanged = "member.role_changed"

// setMemberRole changes a workspace member's console role (PENDING-19 §7).
//
// Guarded by @requires(MEMBERS_ADMIN). In one transaction it locks every
// active Admin of the workspace (id order) and then the target, refuses a
// change of your own role and a change that would leave no active Admin,
// updates users.role (the source of truth) and workspace_members.role, and
// writes the audit row. The target's cached role is dropped after commit, so
// the change applies on their next request.
func (r *Resolver) setMemberRole(ctx context.Context, userID string, role graph.Role, reason string) (*models.User, error) {
	tc := tenant.MustGet(ctx)

	newRole := strings.ToLower(string(role))
	if !role.IsValid() || !rbac.ValidRole(newRole) {
		return nil, apperr.UserErrorf("invalid role")
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, apperr.UserErrorf("reason is required")
	}
	if utf8.RuneCountInString(reason) > maxRoleReasonLen {
		return nil, apperr.UserErrorf("reason is too long (max %d characters)", maxRoleReasonLen)
	}
	// Compare canonical ids: Postgres accepts any uuid spelling, so a raw
	// string compare would let a re-spelled own id past the self check.
	userID, ok := rbac.CanonicalID(userID)
	if !ok {
		return nil, apperr.UserErrorf("user not found")
	}
	if self, _ := rbac.CanonicalID(tc.UserID); userID == self {
		return nil, apperr.UserErrorf("you can't change your own role")
	}

	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("set member role: begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Lock order: active Admins first, then the target (same as every SCIM
	// disable path), so concurrent changes can't deadlock or both pass.
	admins, err := rbac.LockActiveAdmins(ctx, tx, tc.TenantID)
	if err != nil {
		return nil, err
	}

	var oldRole, status string
	err = tx.QueryRow(ctx,
		`SELECT role, status FROM users WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
		userID, tc.TenantID,
	).Scan(&oldRole, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, apperr.UserErrorf("user not found")
	}
	if err != nil {
		return nil, fmt.Errorf("set member role: lock target: %w", err)
	}
	if status == "deleted" {
		return nil, apperr.UserErrorf("user not found")
	}

	if oldRole == newRole {
		// No-op: succeeds without an audit row (as on the provider side).
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("set member role: commit: %w", err)
		}
		return r.loadMember(ctx, tc.TenantID, userID)
	}

	if oldRole == rbac.RoleAdmin && newRole != rbac.RoleAdmin {
		removed := false
		remaining := 0
		for _, id := range admins {
			if id == userID {
				removed = true
			} else {
				remaining++
			}
		}
		if removed && remaining == 0 {
			return nil, apperr.UserErrorf("can't remove the last active admin; make someone else Admin first")
		}
	}

	if _, err := tx.Exec(ctx,
		`UPDATE users SET role = $3, updated_at = NOW() WHERE id = $1 AND tenant_id = $2`,
		userID, tc.TenantID, newRole,
	); err != nil {
		return nil, fmt.Errorf("set member role: update user: %w", err)
	}
	// users.role is the source of truth. SCIM- and JIT-created users may have
	// no workspace_members row, so zero rows here is fine.
	if _, err := tx.Exec(ctx,
		`UPDATE workspace_members SET role = $3 WHERE workspace_id = $1 AND user_id = $2`,
		tc.TenantID, userID, newRole,
	); err != nil {
		return nil, fmt.Errorf("set member role: update membership: %w", err)
	}
	// The audit row commits with the change: a role change without a record is
	// not acceptable, so this is not the best-effort auditAdmin.
	if err := audit.RecordTx(ctx, tx, audit.Entry{
		TenantID:    tc.TenantID,
		ActorUserID: tc.UserID,
		ActorEmail:  tc.Email,
		Action:      actionMemberRoleChanged,
		TargetType:  "user",
		TargetID:    userID,
		Details: map[string]any{
			"from_role": oldRole,
			"to_role":   newRole,
			"reason":    reason,
		},
	}); err != nil {
		return nil, fmt.Errorf("set member role: audit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("set member role: commit: %w", err)
	}
	r.Roles.Invalidate(tc.TenantID, userID)
	return r.loadMember(ctx, tc.TenantID, userID)
}

// loadMember reads one user of the caller's workspace for a mutation result.
func (r *Resolver) loadMember(ctx context.Context, tenantID, userID string) (*models.User, error) {
	var u models.User
	err := r.Pool.QueryRow(ctx,
		`SELECT id, tenant_id, email, provider, provider_sub,
		        role, status, last_login_at, created_at, updated_at,
		        provisioned_by, provisioning_owner
		   FROM users
		  WHERE id = $1 AND tenant_id = $2`,
		userID, tenantID,
	).Scan(
		&u.ID, &u.TenantID, &u.Email,
		&u.Provider, &u.ProviderSub,
		&u.Role, &u.Status, &u.LastLoginAt,
		&u.CreatedAt, &u.UpdatedAt,
		&u.ProvisionedBy, &u.ProvisioningOwner,
	)
	if err != nil {
		return nil, fmt.Errorf("load member: %w", err)
	}
	return &u, nil
}

// refuseSelfGroupChange enforces R-9: only an Admin may add or remove
// themselves from a group, since group membership grants network access. It
// reads the live role that @requires put in the context, never the JWT claim,
// and fails closed if none is there.
func refuseSelfGroupChange(ctx context.Context, callerID, targetUserID string) error {
	target, ok := rbac.CanonicalID(targetUserID)
	if !ok {
		return errGroupOrUserNotFound
	}
	if self, _ := rbac.CanonicalID(callerID); self != target {
		return nil
	}
	if role, ok := rbac.RoleFrom(ctx); ok && role == rbac.RoleAdmin {
		return nil
	}
	return apperr.UserErrorf("only an Admin can change their own group membership")
}

// refuseOwnGroupAccessChange enforces O-6: only an Admin may assign or
// unassign a resource on a group they are a member of, since that changes
// their own network access as surely as joining the group (R-9).
func (r *Resolver) refuseOwnGroupAccessChange(ctx context.Context, tenantID, callerID, groupID string) error {
	if role, ok := rbac.RoleFrom(ctx); ok && role == rbac.RoleAdmin {
		return nil
	}
	gid, ok := rbac.CanonicalID(groupID)
	if !ok {
		return nil // the store rejects an invalid group id as not found
	}
	caller, ok := rbac.CanonicalID(callerID)
	if !ok {
		return apperr.UserErrorf("forbidden") // fail closed: no usable caller id
	}
	var member bool
	if err := r.Pool.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM group_members gm JOIN groups g ON g.id = gm.group_id
		    WHERE gm.group_id = $1 AND gm.user_id = $2 AND g.workspace_id = $3)`,
		gid, caller, tenantID,
	).Scan(&member); err != nil {
		return fmt.Errorf("check own group membership: %w", err)
	}
	if member {
		return apperr.UserErrorf("only an Admin can change resource access for a group they belong to")
	}
	return nil
}
