package resolvers

// Helpers for the identity-provider admin resolvers (PENDING-04 Phase 6/7).
// Kept out of idp.resolvers.go so gqlgen's codegen (which relocates non-resolver
// code) never touches them.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/vektah/gqlparser/v2/gqlerror"
	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/apperr"
	"github.com/yourorg/ztna/controller/internal/audit"
	"github.com/yourorg/ztna/controller/internal/auth/providers"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/permission"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// idpConnToGQL maps a store connection to its GraphQL view. The client secret is
// NEVER mapped — it must not leave the server (ADR-024). Identity Health and
// LastSyncAt are derived for SCIM-capable connections via the SCIM engine when
// it is wired (nil-safe: a nil engine yields an empty health label).
func (r *Resolver) idpConnToGQL(c idp.Connection) *graph.WorkspaceIdpConnection {
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
		LastSyncAt:  c.LastSyncAt,
		// Identity mapping (ADR-025 §3). Non-null in the DB (migration 034
		// defaults 'sub' / 'externalId'), so these are plain values.
		SubjectClaim:   c.SubjectClaim,
		ScimIdentifier: c.ScimIdentifier,
		ScimEnabled:    c.ScimEnabled,
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
	// Identity Health is a SCIM concept derived from last_sync_at + status
	// (ADR-025 §12). Only meaningful for SCIM-enabled connections; for other
	// connections we leave the label empty rather than fabricating a state.
	if c.ScimEnabled && r.ScimStore != nil && r.ScimStore.DirectoryService() != nil {
		if h, err := r.ScimStore.DirectoryService().IdentityHealth(context.Background(), c.TenantIDOrEmpty(), c.ID); err == nil {
			out.IdentityHealth = string(h)
		}
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

// scimEnableRefusedError builds the user-actionable error returned when
// UpdateScimConfig(scimEnabled:true) cannot enable SCIM because the live
// mapping round-trip proof failed (or was never run). It is returned as a
// *gqlerror.Error so the fail-closed ErrorPresenter (graph/resolvers/presenter.go)
// passes it through verbatim with a branchable extensions.code, letting the
// admin UI offer the enableScimBreakGlass path instead of guessing from a
// generic failure. The message points at the dedicated identity.mapping.break_glass
// permission required to override.
func scimEnableRefusedError(reason string) error {
	gerr := gqlerror.Errorf("cannot enable SCIM — %s. Enabling despite an unproven mapping "+
		"requires the %q permission via enableScimBreakGlass (a mandatory reason is audited)",
		reason, permission.BreakGlassMapping)
	if gerr.Extensions == nil {
		gerr.Extensions = map[string]any{}
	}
	// Branchable code so the admin UI can offer the break-glass flow instead of
	// guessing from a generic failure. SCIM mapping not proven is a client-side
	// precondition failure, not a 409 identity_conflict, so it is not CONFLICT.
	gerr.Extensions["code"] = "SCIM_MAPPING_UNPROVEN"
	return gerr
}

// oidcDiscoveryProbeTimeout bounds the create-time discovery check. The
// provider's own HTTP client allows 10s, which is too long to hold a UI dialog
// on a mistyped domain; this keeps a bad host from stalling the mutation.
const oidcDiscoveryProbeTimeout = 5 * time.Second

// validateOIDCDiscovery verifies an operator-supplied OIDC configuration before
// it is persisted, by reusing the adapter layer's own discovery client
// (providers.OIDCProvider.ProbeFresh) rather than adding a second one.
//
// SCOPE — what a successful return proves, and nothing beyond it:
//
//   - the configured issuer (or explicit discoveryURL) is reachable, and
//   - it serves a valid OIDC discovery document whose `issuer` matches the
//     configured issuer and which advertises authorization/token/JWKS endpoints.
//
// It does NOT prove the OAuth client ID or client secret is valid, that the
// redirect URI is registered, or that any user can log in. Discovery is a
// public, unauthenticated endpoint: the provider is deliberately constructed
// with EMPTY client credentials here, so no secret can be sent, logged by a
// transport, or embedded in a returned error. Never report a successful return
// as "credentials verified".
//
// ProbeFresh (not Probe) is required: discoveryCache is keyed on the issuer
// alone and shared process-wide, so a cache-consulting check could pass without
// a request — including on another workspace's already-warm issuer.
func validateOIDCDiscovery(ctx context.Context, provider, issuer, discoveryURL, scopes string) error {
	ctx, cancel := context.WithTimeout(ctx, oidcDiscoveryProbeTimeout)
	defer cancel()

	p := providers.NewOIDCProvider(provider, issuer, "", "", discoveryURL, scopes)
	if _, err := p.ProbeFresh(ctx); err != nil {
		// User-safe by construction: every error the discovery path returns is
		// built in internal/auth/providers from the operator's OWN configured
		// URL plus a transport/parse condition (status code, decode failure,
		// issuer mismatch, missing endpoints). No DB text, no credential, and
		// Go errors carry no stack trace. Surfaced through apperr so the
		// fail-closed ErrorPresenter passes it to the admin instead of masking
		// it to "an unexpected error occurred" — the same reason
		// testIdpConnection reports its probe failure verbatim.
		return apperr.UserErrorf(
			"OIDC discovery failed for issuer %q: %s. The connection was NOT created. "+
				"Check that the domain is correct and reachable from the controller. "+
				"Note that this check does not validate the client ID or client secret.",
			issuer, err.Error())
	}
	return nil
}
