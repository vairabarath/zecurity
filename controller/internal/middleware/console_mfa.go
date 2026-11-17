package middleware

import (
	"context"
	"fmt"
	"net/http"

	"github.com/yourorg/ztna/controller/internal/tenant"
)

// ConsoleMFAPolicy says whether a workspace requires MFA on the admin
// console (mfa.ConsolePolicy).
type ConsoleMFAPolicy interface {
	Required(ctx context.Context, workspaceID string) (bool, error)
}

// CodeMFARequired is the error code the admin console reacts to by sending
// the user to its MFA page.
const CodeMFARequired = "MFA_REQUIRED"

// RequireConsoleMFA enforces Admin Console Security (PENDING-06 M-6): in a
// workspace that requires MFA on the console, a session that has not passed
// MFA is refused with MFA_REQUIRED. It must run after AuthMiddleware. A
// policy lookup failure refuses the request (fail closed).
//
// The MFA mark comes from the access token (mfa_at), which is minted from
// the session's mark; a reset (M-8) therefore takes effect within one
// access-token lifetime, like every other session revocation here.
func RequireConsoleMFA(policy ConsoleMFAPolicy) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tc, ok := tenant.Get(r.Context())
			if !ok {
				writeJSON401(w, "missing tenant context")
				return
			}
			required, err := policy.Required(r.Context(), tc.TenantID)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusServiceUnavailable)
				fmt.Fprint(w, `{"errors":[{"message":"console security policy unavailable","extensions":{"code":"UNAVAILABLE"}}]}`)
				return
			}
			if required && tc.MFAAt.IsZero() {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				fmt.Fprintf(w, `{"errors":[{"message":"multi-factor authentication is required for this workspace's console","extensions":{"code":%q}}]}`, CodeMFARequired)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
