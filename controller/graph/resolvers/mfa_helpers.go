package resolvers

// Non-resolver helpers for workspace MFA (PENDING-06). Kept in its own file
// (no resolver methods) so gqlgen's codegen never relocates or drops it.

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/apperr"
	"github.com/yourorg/ztna/controller/internal/mfa"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// setConsoleRequireMfa is Admin Console Security (M-6). Turning the
// requirement on is refused unless the caller's own session has passed MFA:
// otherwise the admin who enables it (and everyone else) would be locked out
// of the console until they could enrol, with nobody able to turn it off.
func (r *mutationResolver) setConsoleRequireMfa(ctx context.Context, required bool) (*graph.ConsoleSecurity, error) {
	tc := tenant.MustGet(ctx)
	if required && tc.MFAAt.IsZero() {
		return nil, mfaRequiredError("verify your own authenticator code before requiring MFA for the console")
	}
	previous, err := r.ConsoleMFA.Set(ctx, tc.TenantID, required)
	if err != nil {
		return nil, fmt.Errorf("setConsoleRequireMfa: %w", err)
	}
	if previous != required {
		r.auditAdmin(ctx, "console_security.require_mfa", "workspace", tc.TenantID, map[string]any{
			"from": previous, "to": required,
		})
	}
	return &graph.ConsoleSecurity{RequireMfa: required}, nil
}

// resetUserMfa deletes another user's authenticator (M-8). The user's
// sessions and devices lose their MFA mark, so every policy that requires
// MFA asks them again, and they enrol a new authenticator at that prompt.
func (r *mutationResolver) resetUserMfa(ctx context.Context, userID string) (bool, error) {
	tc := tenant.MustGet(ctx)
	if userID == tc.UserID {
		return false, apperr.UserErrorf("you can't reset your own MFA; ask another admin")
	}
	var email string
	err := r.Pool.QueryRow(ctx,
		`SELECT email FROM users WHERE id::text = $1 AND tenant_id = $2`, userID, tc.TenantID).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, apperr.UserErrorf("user not found")
	}
	if err != nil {
		return false, fmt.Errorf("resetUserMfa: %w", err)
	}
	existed, err := r.MFA.Reset(ctx, tc.TenantID, userID)
	if err != nil {
		return false, fmt.Errorf("resetUserMfa: %w", err)
	}
	// The authenticator is gone; now withdraw what it vouched for. Both
	// steps are idempotent, so a retry after a partial failure is safe.
	if err := r.AuthService.ClearUserSessionMFA(ctx, userID); err != nil {
		return false, fmt.Errorf("resetUserMfa: clear session marks: %w", err)
	}
	if _, err := r.Pool.Exec(ctx,
		`UPDATE client_devices SET session_mfa_at = NULL
		  WHERE user_id::text = $1 AND workspace_id = $2 AND session_mfa_at IS NOT NULL`,
		userID, tc.TenantID); err != nil {
		return false, fmt.Errorf("resetUserMfa: clear device marks: %w", err)
	}
	r.auditAdmin(ctx, mfa.ActionReset, "user", userID, map[string]any{"email": email, "had_authenticator": existed})
	if err := r.PolicyNotifier.NotifyPolicyChange(ctx, tc.TenantID); err != nil {
		log.Printf("resetUserMfa: notify policy change: %v", err)
	}
	return existed, nil
}
