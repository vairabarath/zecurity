package bootstrap

import "context"

// Result holds the output of the bootstrap transaction.
// Fields are used by: CallbackHandler() in auth/callback.go (Step 7)
// to build the JWT claims (TenantID, UserID, Role).
// Implemented by: Member 3 (real transaction with DB).
// Current: stub returning test data.
type Result struct {
	TenantID string
	UserID   string
	Role     string
}

// Bootstrap finds or creates the user+tenant for a given identity.
// Called by: CallbackHandler() in auth/callback.go (Step 7).
//
// Parameters:
//   - email: user's verified email from Google id_token
//   - provider: identity provider name, e.g. "google"
//   - providerSub: immutable subject ID from Google (used as provider_sub in DB)
//   - name: user's display name from Google id_token
//
// TODO: Member 3 replaces this stub with the real DB transaction that:
//   - Looks up user by (provider, provider_sub)
//   - Creates tenant + workspace + user if first login
//   - Returns existing tenant/user if returning user
func Bootstrap(ctx context.Context, email, provider, providerSub, name string) (*Result, error) {
	// Stub — returns hardcoded test data until Member 3 ships.
	return &Result{
		TenantID: "00000000-0000-0000-0000-000000000001",
		UserID:   "00000000-0000-0000-0000-000000000002",
		Role:     "admin",
	}, nil
}
