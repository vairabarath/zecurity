package resolvers

// Non-resolver helpers for the explicit fine-grained permission primitive
// (Sprint 17 / ADR-025 Phase 3). Kept in its own file (no resolver methods) so
// gqlgen's codegen never relocates or drops it.

import (
	"context"
	"time"

	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/yourorg/ztna/controller/internal/permission"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// BreakGlassMFAWindow is how recent the console session's MFA must be for a
// break-glass action (PENDING-06 M-7).
const BreakGlassMFAWindow = 15 * time.Minute

// CodeMFARequired is the GraphQL error code the admin console reacts to by
// asking for an MFA code (the same code the console-wide requirement uses).
const CodeMFARequired = "MFA_REQUIRED"

// RequireBreakGlassMFA is the break-glass step-up (PENDING-06 M-7): the
// caller's console session must have passed MFA within BreakGlassMFAWindow.
// It applies whatever the workspace's console-MFA setting is: break-glass
// actions always need a fresh second factor.
//
// Call sites that need break-glass authorization MUST route through this
// hook, after their permission check.
func RequireBreakGlassMFA(_ context.Context, tc *tenant.TenantContext) error {
	return requireRecentMFA(tc, time.Now())
}

func requireRecentMFA(tc *tenant.TenantContext, now time.Time) error {
	if tc != nil && !tc.MFAAt.IsZero() && now.Sub(tc.MFAAt) < BreakGlassMFAWindow && !tc.MFAAt.After(now.Add(time.Minute)) {
		return nil
	}
	return mfaRequiredError("this action needs a fresh second factor: verify an authenticator code and try again")
}

// mfaRequiredError is a client-actionable error carrying CodeMFARequired; the
// fail-closed ErrorPresenter passes a *gqlerror.Error through verbatim.
func mfaRequiredError(msg string) error {
	gerr := gqlerror.Errorf("%s", msg)
	gerr.Extensions = map[string]any{"code": CodeMFARequired}
	return gerr
}

// BreakGlassMapping is re-exported here for resolver call sites that need the
// canonical permission string without importing the permission package directly.
const BreakGlassMapping = permission.BreakGlassMapping
