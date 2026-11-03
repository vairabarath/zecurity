package provider

import (
	"context"
	"errors"
	"testing"
)

type fakeBootstrapStore struct {
	hasAdmin     bool
	createCalled bool
	gotHash      string
}

func (f *fakeBootstrapStore) HasSuperAdmin(context.Context) (bool, error) { return f.hasAdmin, nil }

func (f *fakeBootstrapStore) CreateBootstrapSuperAdminIfNone(_ context.Context, email, hash string) (BootstrapResult, *ProviderUser, error) {
	f.createCalled, f.gotHash = true, hash
	return BootstrapCreated, &ProviderUser{ID: "b1", Email: email, Role: RoleSuperAdmin, MustChangePassword: true}, nil
}

func TestBootstrap_Outcomes(t *testing.T) {
	ctx := context.Background()

	// A super-admin exists: bootstrap env ignored, nothing hashed or created,
	// even with an invalid password configured.
	s := &fakeBootstrapStore{hasAdmin: true}
	if res, _, err := Bootstrap(ctx, s, "boot@inkyank.com", "x"); err != nil || res != BootstrapSkippedAdminExists || s.createCalled {
		t.Fatalf("admin exists: res=%v err=%v created=%v", res, err, s.createCalled)
	}

	s = &fakeBootstrapStore{}
	if res, _, err := Bootstrap(ctx, s, "  ", ""); err != nil || res != BootstrapNotConfigured || s.createCalled {
		t.Fatalf("not configured: res=%v err=%v", res, err)
	}
	if _, _, err := Bootstrap(ctx, s, "boot@inkyank.com", ""); err == nil {
		t.Fatal("missing password must be an error (refuse to start)")
	}
	if _, _, err := Bootstrap(ctx, s, "boot@inkyank.com", "short"); !errors.Is(err, ErrPasswordPolicy) {
		t.Fatalf("weak password: %v, want ErrPasswordPolicy", err)
	}
	if s.createCalled {
		t.Fatal("store called despite an invalid bootstrap password")
	}

	res, u, err := Bootstrap(ctx, s, "boot@inkyank.com", "temporary-pass-1")
	if err != nil || res != BootstrapCreated || u == nil || !s.createCalled {
		t.Fatalf("create: res=%v err=%v", res, err)
	}
	if ok, _ := VerifyPassword(ctx, "temporary-pass-1", s.gotHash); !ok {
		t.Fatal("store received a hash that does not verify the bootstrap password")
	}
}
