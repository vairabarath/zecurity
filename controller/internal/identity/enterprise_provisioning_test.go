package identity

// Coverage for the tier split on FIRST-SEEN identities.
//
// The platform tier (shared IdP, nil owning workspace) is the signup path: a
// never-seen identity legitimately gets a workspace created for it.
//
// An ENTERPRISE connection is the opposite. It already belongs to exactly one
// workspace, so provisioning a NEW one would hand a fresh Zecurity workspace —
// with ADMIN — to anyone in the customer's IdP directory who merely signs in.
// Authentication proves identity; it does not grant access. Such a user joins
// the owning workspace only if invited or SCIM-provisioned, else ErrNotInvited.

import (
	"context"
	"errors"
	"testing"
)

func strptr(s string) *string { return &s }

// The regression that matters: an enterprise login must carry the owning
// workspace down to the provisioner, which is the only thing that lets it
// refuse instead of creating a workspace.
func TestAuthenticate_EnterpriseTierThreadsOwningWorkspace(t *testing.T) {
	res := &fakeResolver{found: false}
	prov := &fakeProvisioner{
		core: &PrincipalCore{
			UserID: "u1", TenantID: "ws-acme", Role: "member",
			Email: "alice@example.com", Status: "active", Generation: 1,
		},
	}
	svc := newTestService(res, prov, nil)

	if _, err := svc.Authenticate(
		context.Background(), testAuthCtx(), "conn-okta", "", strptr("ws-acme"),
	); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if prov.got.ConnectionTenantID == nil {
		t.Fatal("the owning workspace was not threaded to the provisioner; it cannot refuse without it")
	}
	if *prov.got.ConnectionTenantID != "ws-acme" {
		t.Fatalf("wrong owning workspace: %q", *prov.got.ConnectionTenantID)
	}
}

// The platform tier keeps its nil, preserving the signup path.
func TestAuthenticate_PlatformTierPassesNoOwningWorkspace(t *testing.T) {
	res := &fakeResolver{found: false}
	prov := &fakeProvisioner{
		core: &PrincipalCore{UserID: "u2", TenantID: "ws-new", Status: "active", Generation: 1},
	}
	svc := newTestService(res, prov, nil)

	if _, err := svc.Authenticate(
		context.Background(), testAuthCtx(), "conn-google", "Acme Inc", nil,
	); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if prov.got.ConnectionTenantID != nil {
		t.Fatalf("platform tier must pass no owning workspace, got %q", *prov.got.ConnectionTenantID)
	}
	if prov.got.WorkspaceName != "Acme Inc" {
		t.Fatalf("signup workspace name lost: %q", prov.got.WorkspaceName)
	}
}

// ErrNotInvited must reach the caller INTACT: the callback distinguishes it from
// a generic failure to tell the user their sign-in worked but access was not
// granted. Wrapping it would collapse that into "authentication_failed".
func TestAuthenticate_NotInvitedSurfacesUnwrapped(t *testing.T) {
	res := &fakeResolver{found: false}
	prov := &fakeProvisioner{err: ErrNotInvited}
	svc := newTestService(res, prov, nil)

	_, err := svc.Authenticate(
		context.Background(), testAuthCtx(), "conn-okta", "", strptr("ws-acme"),
	)
	if err == nil {
		t.Fatal("expected a refusal for an uninvited enterprise identity")
	}
	if !errors.Is(err, ErrNotInvited) {
		t.Fatalf("ErrNotInvited must remain identifiable, got %v", err)
	}
}

// A genuine provisioning failure must NOT masquerade as "not invited".
func TestAuthenticate_RealProvisionErrorIsNotNotInvited(t *testing.T) {
	res := &fakeResolver{found: false}
	prov := &fakeProvisioner{err: errors.New("insert user: connection reset")}
	svc := newTestService(res, prov, nil)

	_, err := svc.Authenticate(context.Background(), testAuthCtx(), "conn-google", "", nil)
	if err == nil {
		t.Fatal("expected the provisioning error to surface")
	}
	if errors.Is(err, ErrNotInvited) {
		t.Fatal("an internal provisioning failure must not be reported as not-invited")
	}
}

// An EXISTING member signs in normally on an enterprise connection — the refusal
// applies only to first-seen identities, so SCIM-provisioned and invited users
// (who already have an external_identities row) are unaffected.
func TestAuthenticate_EnterpriseExistingMemberIsUnaffected(t *testing.T) {
	res := &fakeResolver{
		found: true,
		core: &PrincipalCore{
			UserID: "u3", TenantID: "ws-acme", Role: "member",
			Email: "bob@example.com", Status: "active", Generation: 4,
		},
	}
	prov := &fakeProvisioner{}
	svc := newTestService(res, prov, nil)

	p, err := svc.Authenticate(
		context.Background(), testAuthCtx(), "conn-okta", "", strptr("ws-acme"),
	)
	if err != nil {
		t.Fatalf("an existing member must still sign in: %v", err)
	}
	if prov.called {
		t.Fatal("a resolved identity must never reach the provisioner")
	}
	if p.Core.UserID != "u3" || p.Core.TenantID != "ws-acme" {
		t.Fatalf("unexpected principal: %+v", p.Core)
	}
}
