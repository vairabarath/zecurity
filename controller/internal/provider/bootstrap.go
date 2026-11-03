package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// BootstrapNotConfigured: no super-admin exists and PROVIDER_BOOTSTRAP_EMAIL
// is unset — nobody can sign in to the provider plane until one is set.
const BootstrapNotConfigured BootstrapResult = BootstrapEmailTaken + 1

// bootstrapStore is what Bootstrap needs from the provider store.
type bootstrapStore interface {
	HasSuperAdmin(ctx context.Context) (bool, error)
	CreateBootstrapSuperAdminIfNone(ctx context.Context, email, passwordHash string) (BootstrapResult, *ProviderUser, error)
}

// Bootstrap runs the create-only first-super-admin bootstrap (D-26, ADR-029).
//
//   - A super-admin already exists (any state) → BootstrapSkippedAdminExists;
//     the bootstrap environment is ignored and no password is hashed.
//   - No super-admin, no email → BootstrapNotConfigured.
//   - No super-admin, email set → the password must satisfy the policy (else an
//     error: the caller refuses to start), then the account is created with
//     must_change_password — race-safely inside the store.
//
// There is deliberately no reset or promotion path here; break-glass recovery
// is a future controller CLI command.
func Bootstrap(ctx context.Context, store bootstrapStore, email, password string) (BootstrapResult, *ProviderUser, error) {
	exists, err := store.HasSuperAdmin(ctx)
	if err != nil {
		return 0, nil, err
	}
	if exists {
		return BootstrapSkippedAdminExists, nil, nil
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return BootstrapNotConfigured, nil, nil
	}
	if password == "" {
		return 0, nil, errors.New("PROVIDER_BOOTSTRAP_PASSWORD is required to create the first provider super-admin")
	}
	if err := ValidateNewPassword(password, email, ""); err != nil {
		return 0, nil, fmt.Errorf("PROVIDER_BOOTSTRAP_PASSWORD: %w", err)
	}
	hash, err := HashPassword(ctx, password)
	if err != nil {
		return 0, nil, fmt.Errorf("hash bootstrap password: %w", err)
	}
	return store.CreateBootstrapSuperAdminIfNone(ctx, email, hash)
}
