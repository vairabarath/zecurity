package provider

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeLoginStore serves GetByEmailForLogin from a map; err overrides lookups.
type fakeLoginStore struct {
	users map[string]*ProviderUser
	err   error
}

func (f *fakeLoginStore) GetByEmailForLogin(_ context.Context, email string) (*ProviderUser, error) {
	if f.err != nil {
		return nil, f.err
	}
	u, ok := f.users[email]
	if !ok {
		return nil, ErrProviderUserNotFound
	}
	return u, nil
}

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := HashPassword(context.Background(), pw)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestLocalPasswordAuthenticator(t *testing.T) {
	ctx := context.Background()
	disabledAt := time.Now()
	good := mustHash(t, "correct-password-1")
	store := &fakeLoginStore{users: map[string]*ProviderUser{
		"ok@inkyank.com":       {ID: "u1", Email: "ok@inkyank.com", Role: RoleRelayOps, PasswordHash: good, SessionGeneration: 3},
		"disabled@inkyank.com": {ID: "u2", Email: "disabled@inkyank.com", Role: RoleSuperAdmin, PasswordHash: good, DisabledAt: &disabledAt},
		"nohash@inkyank.com":   {ID: "u3", Email: "nohash@inkyank.com", Role: RoleSuperAdmin},
		"corrupt@inkyank.com":  {ID: "u4", Email: "corrupt@inkyank.com", Role: RoleSuperAdmin, PasswordHash: "not-a-phc"},
	}}
	a := NewLocalPasswordAuthenticator(store)
	if a.Method() != AMRPassword {
		t.Fatalf("Method() = %q, want %q", a.Method(), AMRPassword)
	}

	u, err := a.Authenticate(ctx, Credentials{Email: "ok@inkyank.com", Password: "correct-password-1"})
	if err != nil || u.ID != "u1" {
		t.Fatalf("valid login: u=%v err=%v", u, err)
	}

	// Every credential failure is the SAME error (no account enumeration).
	for name, c := range map[string]Credentials{
		"unknown email":  {Email: "nobody@inkyank.com", Password: "correct-password-1"},
		"disabled":       {Email: "disabled@inkyank.com", Password: "correct-password-1"},
		"no local hash":  {Email: "nohash@inkyank.com", Password: "correct-password-1"},
		"corrupt hash":   {Email: "corrupt@inkyank.com", Password: "correct-password-1"},
		"wrong password": {Email: "ok@inkyank.com", Password: "wrong-password-1"},
	} {
		if u, err := a.Authenticate(ctx, c); u != nil || !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("%s: u=%v err=%v, want ErrInvalidCredentials", name, u, err)
		}
	}
}

func TestLocalPasswordAuthenticator_StoreErrorPropagates(t *testing.T) {
	boom := errors.New("db down")
	a := NewLocalPasswordAuthenticator(&fakeLoginStore{err: boom})
	if _, err := a.Authenticate(context.Background(), Credentials{Email: "x@y", Password: "p"}); !errors.Is(err, boom) {
		t.Fatalf("store error must propagate (not become invalid credentials): %v", err)
	}
}

// IssueSession is the single minting path: role, email and generation come
// from the row, pwc tokens are short-lived, and incomplete input is refused.
func TestIdentityService_SingleMintingPath(t *testing.T) {
	s := mustIdentity(t)
	u := &ProviderUser{ID: "u9", Email: "row@inkyank.com", Role: RoleRelayOps, SessionGeneration: 42}

	full, exp, err := s.IssueSession(u, []string{AMRPassword}, false)
	if err != nil || exp != int64(ProviderSessionTTL/time.Second) {
		t.Fatalf("full session: exp=%d err=%v", exp, err)
	}
	pwc, pexp, err := s.IssueSession(u, []string{AMRPassword}, true)
	if err != nil || pexp != int64(PasswordChangeTokenTTL/time.Second) {
		t.Fatalf("pwc session: exp=%d err=%v", pexp, err)
	}

	fc, err := s.Verify(full)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Role != RoleRelayOps || fc.Email != "row@inkyank.com" || fc.Gen != 42 || fc.PWC || fc.AMR[0] != AMRPassword {
		t.Fatalf("full claims not taken from the row: %+v", fc)
	}
	if got := fc.ExpiresAt.Sub(fc.IssuedAt.Time); got != ProviderSessionTTL {
		t.Fatalf("full token lifetime = %v, want %v", got, ProviderSessionTTL)
	}
	pc, err := s.Verify(pwc)
	if err != nil || !pc.PWC {
		t.Fatalf("pwc claims: %+v err=%v", pc, err)
	}
	if got := pc.ExpiresAt.Sub(pc.IssuedAt.Time); got != PasswordChangeTokenTTL {
		t.Fatalf("pwc token lifetime = %v, want %v", got, PasswordChangeTokenTTL)
	}

	for name, bad := range map[string]*ProviderUser{
		"nil":      nil,
		"no id":    {Email: "a@b", Role: RoleRelayOps},
		"no email": {ID: "x", Role: RoleRelayOps},
		"no role":  {ID: "x", Email: "a@b"},
	} {
		if _, _, err := s.IssueSession(bad, []string{AMRPassword}, false); err == nil {
			t.Errorf("%s: IssueSession accepted an incomplete user", name)
		}
	}
	if _, _, err := s.IssueSession(u, nil, false); err == nil {
		t.Error("IssueSession accepted an empty amr")
	}
}
