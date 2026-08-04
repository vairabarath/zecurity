package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/yourorg/ztna/controller/internal/auth/providers"
)

// ── Fakes (no database) ─────────────────────────────────────────────────────

type fakeResolver struct {
	core  *PrincipalCore
	found bool
	err   error

	gotConn, gotSubject, gotTenant string
}

func (f *fakeResolver) Resolve(_ context.Context, connectionID, subject, tenantID string) (*PrincipalCore, bool, error) {
	f.gotConn, f.gotSubject, f.gotTenant = connectionID, subject, tenantID
	return f.core, f.found, f.err
}

type fakeProvisioner struct {
	core   *PrincipalCore
	err    error
	called bool
	got    ProvisionInput
}

func (f *fakeProvisioner) Provision(_ context.Context, in ProvisionInput) (*PrincipalCore, error) {
	f.called = true
	f.got = in
	return f.core, f.err
}

type capturePublisher struct{ events []Event }

func (c *capturePublisher) Publish(_ context.Context, e Event) error {
	c.events = append(c.events, e)
	return nil
}

func newTestService(res identityResolver, prov Provisioner, pub EventPublisher) *Service {
	if pub == nil {
		pub = NopPublisher{}
	}
	return &Service{resolver: res, linker: NewLinker(prov), publisher: pub, pool: nil}
}

func testAuthCtx() *providers.AuthenticationContext {
	return &providers.AuthenticationContext{
		Provider: "okta",
		Issuer:   "https://acme.okta.com",
		Subject:  "okta-sub-1",
		Email:    "alice@example.com",
		Name:     "Alice",
	}
}

// ── Tests ───────────────────────────────────────────────────────────────────

// Returning user: the Resolver hits, the pipeline returns that user and never
// provisions. Resolution is keyed on (connection, subject) — never email.
func TestAuthenticate_ReturningUser(t *testing.T) {
	res := &fakeResolver{
		found: true,
		core:  &PrincipalCore{UserID: "u1", TenantID: "t1", Role: "member", Email: "alice@example.com", Status: "active", Generation: 2},
	}
	prov := &fakeProvisioner{}
	svc := newTestService(res, prov, nil)

	p, err := svc.Authenticate(context.Background(), testAuthCtx(), "conn-1", "")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Core.UserID != "u1" || p.Core.Generation != 2 {
		t.Fatalf("unexpected principal: %+v", p.Core)
	}
	if prov.called {
		t.Fatal("must not provision a returning user")
	}
	if res.gotConn != "conn-1" || res.gotSubject != "okta-sub-1" {
		t.Fatalf("resolver keyed wrong: conn=%q subject=%q", res.gotConn, res.gotSubject)
	}
}

// A resolved-but-inactive user is rejected (fail closed), and no provisioning
// is attempted.
func TestAuthenticate_InactiveUserRejected(t *testing.T) {
	res := &fakeResolver{
		found: true,
		core:  &PrincipalCore{UserID: "u1", Status: "suspended"},
	}
	prov := &fakeProvisioner{}
	svc := newTestService(res, prov, nil)

	_, err := svc.Authenticate(context.Background(), testAuthCtx(), "conn-1", "")
	if !errors.Is(err, ErrUserNotActive) {
		t.Fatalf("expected ErrUserNotActive, got %v", err)
	}
	if prov.called {
		t.Fatal("must not provision when a resolved user is inactive")
	}
}

// First-seen identity: the Resolver misses, the Linker JIT-creates via the
// Provisioner. The identity key (connection, subject, issuer) is threaded to
// provisioning; email travels only as a hint. A provisioning event is emitted.
func TestAuthenticate_FirstSeenJITCreates(t *testing.T) {
	res := &fakeResolver{found: false}
	prov := &fakeProvisioner{
		core: &PrincipalCore{UserID: "u2", TenantID: "t2", Role: "admin", Email: "alice@example.com", Status: "active", Generation: 1},
	}
	pub := &capturePublisher{}
	svc := newTestService(res, prov, pub)

	p, err := svc.Authenticate(context.Background(), testAuthCtx(), "conn-1", "Acme Inc")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !prov.called {
		t.Fatal("expected provisioning for a first-seen identity")
	}
	if p.Core.UserID != "u2" || p.Core.Generation != 1 {
		t.Fatalf("unexpected principal: %+v", p.Core)
	}
	if prov.got.ConnectionID != "conn-1" || prov.got.Subject != "okta-sub-1" || prov.got.Issuer != "https://acme.okta.com" {
		t.Fatalf("identity key not threaded to provision: %+v", prov.got)
	}
	if prov.got.WorkspaceName != "Acme Inc" {
		t.Fatalf("workspace name not passed: %q", prov.got.WorkspaceName)
	}
	if prov.got.Email != "alice@example.com" {
		t.Fatalf("email hint not passed: %q", prov.got.Email)
	}
	if len(pub.events) != 1 || pub.events[0].Action != ActionUserProvisioned || pub.events[0].TenantID != "t2" {
		t.Fatalf("expected one provisioned event for tenant t2, got %+v", pub.events)
	}
}

// A resolver error surfaces and never falls through to provisioning.
func TestAuthenticate_ResolverErrorSurfaces(t *testing.T) {
	res := &fakeResolver{err: errors.New("db down")}
	prov := &fakeProvisioner{}
	svc := newTestService(res, prov, nil)

	if _, err := svc.Authenticate(context.Background(), testAuthCtx(), "conn-1", ""); err == nil {
		t.Fatal("expected resolver error to surface")
	}
	if prov.called {
		t.Fatal("must not provision when resolution errored")
	}
}
