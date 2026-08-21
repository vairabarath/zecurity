package resolvers

// Non-resolver helpers for the explicit fine-grained permission primitive
// (Sprint 17 / ADR-025 Phase 3). Kept in its own file (no resolver methods) so
// gqlgen's codegen never relocates or drops it.

import (
	"context"

	"github.com/yourorg/ztna/controller/internal/permission"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// RequireBreakGlassMFA is the break-glass MFA hook specified by the Phase 3 plan.
//
// It is a deliberate no-op stub: the auth infrastructure to enforce step-up MFA
// (PENDING-06) is not implemented yet. Call sites that need break-glass
// authorization MUST route through this hook so that, once PENDING-06 lands, the
// real enforcement can be plugged in here without touching every call site.
//
// Until then it returns nil (allowed).
func RequireBreakGlassMFA(_ context.Context, _ *tenant.TenantContext) error {
	return nil
}

// BreakGlassMapping is re-exported here for resolver call sites that need the
// canonical permission string without importing the permission package directly.
const BreakGlassMapping = permission.BreakGlassMapping
