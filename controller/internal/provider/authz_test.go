package provider

import (
	"errors"
	"testing"
)

func TestDecide(t *testing.T) {
	cases := []struct {
		role, action string
		wantAllow    bool
	}{
		{RoleSuperAdmin, ActionRelayCreate, true},
		{RoleSuperAdmin, ActionProviderUserManage, true},
		{RoleRelayOps, ActionRelayCreate, true},
		{RoleRelayOps, ActionRelayIssueToken, true},
		{RoleRelayOps, ActionProviderUserManage, false},
		{RoleRelayOps, ActionAuditView, false},
		{"bogus", ActionRelayCreate, false},
	}
	for _, c := range cases {
		err := decide(Actor{Role: c.role}, c.action, Target{})
		if c.wantAllow && err != nil {
			t.Errorf("%s/%s: want allow, got %v", c.role, c.action, err)
		}
		if !c.wantAllow && !errors.Is(err, ErrForbidden) {
			t.Errorf("%s/%s: want ErrForbidden, got %v", c.role, c.action, err)
		}
	}
}

// TestDecide_ReadMatrix pins the Phase R read matrix (D-05/D-06) for every
// read action and every Can… entry point, so a change to decide() or to an
// action name that would widen or narrow access fails here.
func TestDecide_ReadMatrix(t *testing.T) {
	a := NewAuthz()
	checks := []struct {
		name  string
		check func(Actor) error
	}{
		{ActionRelayRead, func(x Actor) error { return a.CanReadRelays(x, Target{Type: "relay", ID: "r1"}) }},
		{ActionTenantRead, func(x Actor) error { return a.CanReadTenants(x, Target{Type: "tenant", ID: "t1"}) }},
		{ActionAuditView, func(x Actor) error { return a.CanViewProviderAudit(x) }},
		{ActionCertRead, func(x Actor) error { return a.CanReadCertificates(x) }},
	}
	allowed := map[string]map[string]bool{
		RoleSuperAdmin: {ActionRelayRead: true, ActionTenantRead: true, ActionAuditView: true, ActionCertRead: true},
		RoleRelayOps:   {ActionRelayRead: true},
		// Unknown, empty, tenant-style and near-miss roles get nothing.
		"":             {},
		"admin":        {},
		"ADMIN":        {},
		"Super-Admin":  {},
		"super-admin ": {},
		"relay-ops-x":  {},
	}
	for role, want := range allowed {
		for _, c := range checks {
			err := c.check(Actor{UserID: "u", Email: "e@x", Role: role})
			switch {
			case want[c.name] && err != nil:
				t.Errorf("role %q, %s: want allow, got %v", role, c.name, err)
			case !want[c.name] && !errors.Is(err, ErrForbidden):
				t.Errorf("role %q, %s: want ErrForbidden, got %v", role, c.name, err)
			}
		}
	}
}

// The read action names are part of the audit and API vocabulary; keep them
// stable and in the namespaces the prefix rule depends on.
func TestReadActionNames(t *testing.T) {
	for got, want := range map[string]string{
		ActionRelayRead:  "relay.read",
		ActionTenantRead: "tenant.read",
		ActionCertRead:   "cert.read",
		ActionAuditView:  "audit.view",
	} {
		if got != want {
			t.Errorf("action = %q, want %q", got, want)
		}
	}
}
