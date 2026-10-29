package resolvers

import (
	"context"

	"github.com/yourorg/ztna/controller/internal/audit"
	"github.com/yourorg/ztna/controller/internal/resource"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// auditAdmin writes a durable audit_logs row for a workspace admin change
// (networks, connectors, shields, resources, groups, access rules, device
// profiles, invitations, scans). Called once the change has committed and
// before its notifications, so a notify error can't skip the record.
// Best-effort, like auditIdp: a failed write is logged, not fatal. Details
// never carry tokens or secrets.
func (r *Resolver) auditAdmin(ctx context.Context, action, targetType, targetID string, details map[string]any) {
	tc := tenant.MustGet(ctx)
	_ = audit.Record(ctx, r.Pool, audit.Entry{
		TenantID:    tc.TenantID,
		ActorUserID: tc.UserID,
		ActorEmail:  tc.Email,
		Action:      action,
		TargetType:  targetType,
		TargetID:    targetID,
		Details:     details,
	})
}

// resourceAuditDetails is a resource's audited state: what it exposes.
func resourceAuditDetails(row *resource.Row) map[string]any {
	if row == nil {
		return nil
	}
	return map[string]any{
		"name":      row.Name,
		"host":      row.Host,
		"protocol":  row.Protocol,
		"port_from": row.PortFrom,
		"port_to":   row.PortTo,
	}
}
