package resolvers

// Helpers for the identity-provider admin resolvers (PENDING-04 Phase 6/7).
// Kept out of idp.resolvers.go so gqlgen's codegen (which relocates non-resolver
// code) never touches them.

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/audit"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// idpConnToGQL maps a store connection to its GraphQL view. The client secret is
// NEVER mapped — it must not leave the server (ADR-024).
func idpConnToGQL(c idp.Connection) *graph.WorkspaceIdpConnection {
	proto := graph.IdpProtocolOidc
	if strings.EqualFold(c.Protocol, "saml") {
		proto = graph.IdpProtocolSaml
	}
	out := &graph.WorkspaceIdpConnection{
		ID:          c.ID,
		Protocol:    proto,
		Provider:    c.Provider,
		DisplayName: c.DisplayName,
		Issuer:      c.Issuer,
		Scopes:      c.Scopes,
		Status:      c.Status,
		Managed:     c.Managed,
	}
	if c.ClientID != "" {
		id := c.ClientID
		out.ClientID = &id
	}
	if c.DiscoveryURL != "" {
		d := c.DiscoveryURL
		out.DiscoveryURL = &d
	}
	if c.DomainHint != "" {
		h := c.DomainHint
		out.DomainHint = &h
	}
	return out
}

// auditIdp writes a durable audit_logs row for an IdP admin action. Best-effort:
// the mutation has already committed, so a failed audit write is logged, not fatal.
func (r *Resolver) auditIdp(ctx context.Context, tc tenant.TenantContext, action, targetID string, details map[string]any) {
	_ = audit.Record(ctx, r.Pool, audit.Entry{
		TenantID:    tc.TenantID,
		ActorUserID: tc.UserID,
		ActorEmail:  tc.Email,
		Action:      action,
		TargetType:  "idp_connection",
		TargetID:    targetID,
		Details:     details,
	})
}

// revokeConnectionSessions bumps identity_generation for every user linked to
// the connection, killing their sessions.
func (r *Resolver) revokeConnectionSessions(ctx context.Context, tc tenant.TenantContext, connectionID string) {
	userIDs, err := r.IdpStore.UserIDsForConnection(ctx, tc.TenantID, connectionID)
	if err != nil {
		log.Printf("idp: users for connection %s: %v", connectionID, err)
		return
	}
	r.bumpUsers(ctx, tc, userIDs)
}

// bumpUsers revokes each user's sessions via the Revoker (best-effort per user).
func (r *Resolver) bumpUsers(ctx context.Context, tc tenant.TenantContext, userIDs []string) {
	if r.Revoker == nil {
		return
	}
	for _, uid := range userIDs {
		if _, err := r.Revoker.BumpGeneration(ctx, tc.TenantID, uid, tc.Email); err != nil {
			log.Printf("idp: bump generation for user %s: %v", uid, err)
		}
	}
}

// guardLastLoginPath enforces the no-lockout invariant (ADR-024 §5) before a
// connection is disabled/deleted. It consults the workspace's platform login
// toggle (Phase 7): while platform login is on, the shared IdP is always a way
// back in, so removing a BYO connection is safe.
func (r *Resolver) guardLastLoginPath(ctx context.Context, tc tenant.TenantContext) error {
	n, err := r.IdpStore.CountActiveWorkspaceConnections(ctx, tc.TenantID)
	if err != nil {
		return fmt.Errorf("no-lockout check: %w", err)
	}
	platformOK, err := r.IdpStore.PlatformLoginEnabled(ctx, tc.TenantID)
	if err != nil {
		return fmt.Errorf("no-lockout check: %w", err)
	}
	return lastLoginPathGuard(n, platformOK, r.BreakGlassEmails[tc.Email])
}

// lastLoginPathGuard is the pure no-lockout decision for REMOVING one active BYO
// connection: after removal at least one login path must remain. Refused only
// when the platform fallback is also unavailable and the actor is not a
// break-glass admin. (activeConnections is the count BEFORE removal, so >1 means
// one survives.)
func lastLoginPathGuard(activeConnections int, platformAvailable, actorIsBreakGlass bool) error {
	if activeConnections > 1 || platformAvailable || actorIsBreakGlass {
		return nil
	}
	return fmt.Errorf("refusing to remove the workspace's last active identity provider: " +
		"configure another provider or a break-glass admin (IDP_BREAK_GLASS_EMAILS) first")
}

// platformDisableGuard is the pure no-lockout decision for DISABLING platform
// login: the workspace must retain at least one active identity provider of its
// own, else the actor must be a break-glass admin — otherwise disabling the
// shared IdP would strand every member.
func platformDisableGuard(activeOwnConnections int, actorIsBreakGlass bool) error {
	if activeOwnConnections >= 1 || actorIsBreakGlass {
		return nil
	}
	return fmt.Errorf("refusing to disable platform login: the workspace has no active " +
		"identity provider of its own; add one or configure a break-glass admin first")
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
