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
