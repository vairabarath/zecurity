package resolvers

import (
	"context"
	"errors"
	"fmt"

	"github.com/99designs/gqlgen/graphql"
	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/apperr"
	"github.com/yourorg/ztna/controller/internal/rbac"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// RoleSource resolves a caller's live workspace role (rbac.Checker in
// production). It returns rbac.ErrNoAccess for a missing or non-active user.
type RoleSource interface {
	Role(ctx context.Context, tenantID, userID string) (string, error)
}

// NewRequires returns the @requires(capability:) directive (PENDING-19 §4–5).
//
// It runs before the field's resolver and allows the call only if the caller's
// LIVE role (users.role via roles, not the JWT claim) holds the capability.
// Authentication is enforced upstream by AuthMiddleware; this only authorizes.
// It fails closed: no tenant context, a missing or non-active user, or a failed
// lookup all deny. The resolved role is put in the context (rbac.WithRole) for
// resolvers that need it (R-9).
//
// Denial is per field, never request-wide, so a Member keeps the self-service
// fields (me, myDevices) that carry no @requires.
func NewRequires(roles RoleSource) func(ctx context.Context, obj any, next graphql.Resolver, capability graph.Capability) (any, error) {
	return func(ctx context.Context, _ any, next graphql.Resolver, capability graph.Capability) (any, error) {
		tc, ok := tenant.Get(ctx)
		if !ok {
			return nil, apperr.UserErrorf("unauthenticated")
		}
		role, err := roles.Role(ctx, tc.TenantID, tc.UserID)
		if errors.Is(err, rbac.ErrNoAccess) {
			return nil, apperr.UserErrorf("forbidden: requires capability %s", capability)
		}
		if err != nil {
			// Masked by ErrorPresenter; the call is denied either way.
			return nil, fmt.Errorf("requires %s: %w", capability, err)
		}
		if !rbac.Allows(role, rbac.Capability(capability)) {
			return nil, apperr.UserErrorf("forbidden: requires capability %s", capability)
		}
		return next(rbac.WithRole(ctx, role))
	}
}
