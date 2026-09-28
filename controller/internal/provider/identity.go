package provider

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Provider Identity Service (Sprint 21 Phase H; Decision Record amendment
// 2026-09-26, D-24/D-29; ADR-029).
//
// Authentication ("who are you?") goes through an Authenticator. Authorization
// (role), the provider JWT and session_generation belong to the identity
// service and never depend on the authentication method: every Authenticator
// yields the same *ProviderUser row, and IssueSession — the only place a
// provider JWT is minted — reads role, email and generation from that row.

const (
	// AMRPassword is the RFC 8176 "amr" value for local password login.
	AMRPassword = "pwd"

	// ProviderSessionTTL is the lifetime of a full provider token (no refresh
	// tokens in the first release, D-28).
	ProviderSessionTTL = 15 * time.Minute
	// PasswordChangeTokenTTL is the lifetime of a password-change-only token.
	PasswordChangeTokenTTL = 10 * time.Minute

	minProviderKeyBytes = 32
)

// ErrInvalidCredentials is the single error every failed login maps to —
// unknown email, disabled account, no local password, wrong password — so the
// response never reveals which accounts exist.
var ErrInvalidCredentials = errors.New("invalid credentials")

// Credentials is the input to an Authenticator.
type Credentials struct {
	Email    string
	Password string
}

// Authenticator is one way to prove "who you are". Sprint 21 ships only
// LocalPasswordAuthenticator; optional external IdPs (OIDC, Google Workspace,
// Azure AD, Okta) plug in here later without touching roles, tokens or
// revocation (D-29).
type Authenticator interface {
	Method() string // RFC 8176 amr value, e.g. "pwd"
	Authenticate(ctx context.Context, in Credentials) (*ProviderUser, error)
}

// loginUserStore is what LocalPasswordAuthenticator needs from the store. It
// must return disabled rows too (with DisabledAt set) and
// ErrProviderUserNotFound when no row matches.
type loginUserStore interface {
	GetByEmailForLogin(ctx context.Context, email string) (*ProviderUser, error)
}

// LocalPasswordAuthenticator verifies email + password against the Argon2id
// hash in provider_users.
type LocalPasswordAuthenticator struct {
	users loginUserStore
}

func NewLocalPasswordAuthenticator(users loginUserStore) *LocalPasswordAuthenticator {
	return &LocalPasswordAuthenticator{users: users}
}

func (a *LocalPasswordAuthenticator) Method() string { return AMRPassword }

// Authenticate returns the provider user on success and ErrInvalidCredentials
// for every credential failure. Wherever there is no real hash to verify it
// still spends one dummy verification, so timing matches a wrong password.
// Capacity/context errors (ErrHashBusy, ctx.Err()) and store errors are
// returned as-is.
func (a *LocalPasswordAuthenticator) Authenticate(ctx context.Context, in Credentials) (*ProviderUser, error) {
	u, err := a.users.GetByEmailForLogin(ctx, in.Email)
	if errors.Is(err, ErrProviderUserNotFound) {
		return nil, a.reject(ctx, in.Password)
	}
	if err != nil {
		return nil, err
	}
	if u.DisabledAt != nil || u.PasswordHash == "" {
		return nil, a.reject(ctx, in.Password)
	}
	ok, err := VerifyPassword(ctx, in.Password, u.PasswordHash)
	if errors.Is(err, errMalformedHash) {
		return nil, a.reject(ctx, in.Password)
	}
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrInvalidCredentials
	}
	return u, nil
}

func (a *LocalPasswordAuthenticator) reject(ctx context.Context, password string) error {
	if err := VerifyDummy(ctx, password); err != nil {
		return err
	}
	return ErrInvalidCredentials
}

// IdentityService holds the provider signing key and mints/verifies provider
// sessions.
type IdentityService struct {
	key     []byte
	fullTTL time.Duration
	pwcTTL  time.Duration
	now     func() time.Time
}

// NewIdentityService validates the provider signing key (see
// ValidateProviderSigningKey for the reuse check against the tenant secret).
func NewIdentityService(providerKey string) (*IdentityService, error) {
	if len(providerKey) < minProviderKeyBytes {
		return nil, fmt.Errorf("provider signing key must be at least %d bytes", minProviderKeyBytes)
	}
	return &IdentityService{
		key:     []byte(providerKey),
		fullTTL: ProviderSessionTTL,
		pwcTTL:  PasswordChangeTokenTTL,
		now:     time.Now,
	}, nil
}

// ValidateProviderSigningKey enforces D-25 at startup: the provider key is
// present, long enough, and not the tenant JWT secret.
func ValidateProviderSigningKey(providerKey, tenantSecret string) error {
	if providerKey == "" {
		return errors.New("PROVIDER_JWT_SECRET is not set")
	}
	if len(providerKey) < minProviderKeyBytes {
		return fmt.Errorf("PROVIDER_JWT_SECRET must be at least %d bytes", minProviderKeyBytes)
	}
	if providerKey == tenantSecret {
		return errors.New("PROVIDER_JWT_SECRET must differ from JWT_SECRET")
	}
	return nil
}

// IssueSession is the ONLY place a provider JWT is minted. u must be the row
// the caller just loaded from provider_users: role, email and
// session_generation come from it, never from an Authenticator's input.
// pwcOnly issues a short-lived password-change-only token.
func (s *IdentityService) IssueSession(u *ProviderUser, amr []string, pwcOnly bool) (token string, expiresIn int64, err error) {
	if u == nil || u.ID == "" || u.Email == "" || u.Role == "" {
		return "", 0, errors.New("issue provider session: incomplete provider user")
	}
	if len(amr) == 0 {
		return "", 0, errors.New("issue provider session: amr required")
	}
	ttl := s.fullTTL
	if pwcOnly {
		ttl = s.pwcTTL
	}
	token, err = issueProviderToken(s.key, u, amr, pwcOnly, ttl, s.now())
	if err != nil {
		return "", 0, err
	}
	return token, int64(ttl / time.Second), nil
}

// Verify validates a provider token with this service's key.
func (s *IdentityService) Verify(token string) (*ProviderClaims, error) {
	return VerifyProviderToken(s.key, token)
}
