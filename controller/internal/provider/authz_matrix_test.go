package provider

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// actionConstants parses authz.go and returns every Action* constant's
// value (name → value), so a new constant can't escape the tests below.
func actionConstants(t *testing.T) map[string]string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), "authz.go", nil, 0)
	if err != nil {
		t.Fatalf("parse authz.go: %v", err)
	}
	out := map[string]string{}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, spec := range g.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if !strings.HasPrefix(n.Name, "Action") || i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok {
					t.Fatalf("%s is not a string literal", n.Name)
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("%s: %v", n.Name, err)
				}
				out[n.Name] = v
			}
		}
	}
	if len(out) < 60 {
		t.Fatalf("found only %d Action constants; parser out of date?", len(out))
	}
	return out
}

// D-72: every action constant is classified once (read, write or audit-row
// name), and the table holds nothing else.
func TestActionClass_EveryConstant(t *testing.T) {
	consts := actionConstants(t)
	values := map[string]bool{}
	for name, v := range consts {
		values[v] = true
		if actionClasses[v] == 0 {
			t.Errorf("%s (%q) has no entry in actionClasses: classify it, then decide which roles get it", name, v)
		}
	}
	for v := range actionClasses {
		if !values[v] {
			t.Errorf("actionClasses has %q, which is not an Action constant", v)
		}
	}
}

// D-72: the matrix lists only classified actions, and never an audit-row
// name except relay-ops' legacy relay.placement_rejected (kept so the
// relay-ops row equals the pre-O1 prefix outcome).
func TestRoleActions_OnlyKnownActions(t *testing.T) {
	for role, set := range roleActions {
		if !ValidRole(role) || role == RoleSuperAdmin {
			t.Errorf("roleActions has role %q", role)
		}
		for a := range set {
			switch actionClasses[a] {
			case classRead, classWrite:
			case classAuditOnly:
				if !(role == RoleRelayOps && a == ActionRelayPlacementRejected) {
					t.Errorf("%s: audit-row name %q in the matrix", role, a)
				}
			default:
				t.Errorf("%s: unclassified action %q in the matrix", role, a)
			}
		}
	}
	for _, role := range Roles {
		if role != RoleSuperAdmin && roleActions[role] == nil {
			t.Errorf("role %q has no matrix row", role)
		}
	}
}

// preO1RelayOps is the relay-ops outcome of decide() before Sprint 27 (the
// D-05 prefix rule with the D-31/D-41 exceptions plus alert.read and
// timeline.relay_audit), generated from the old code on 2026-11-27. The new
// matrix must give exactly this.
var preO1RelayOps = map[string]bool{
	"relay.create":             true,
	"relay.issue_token":        true,
	"relay.drain":              true,
	"relay.resume":             true,
	"relay.read":               true,
	"relay.metrics.read":       true,
	"relay.move":               true,
	"relay.capacity.set":       true,
	"relay.pool.create":        true,
	"relay.pool.update":        true,
	"relay.pool.delete":        true,
	"relay.placement_rejected": true,
	"alert.read":               true,
	"timeline.relay_audit":     true,
}

// expectedAllowed is the full matrix for the new roles (D-73), written out
// independently of roleActions so a change to either side fails here.
var expectedAllowed = map[string]map[string]bool{
	RoleRelayOps: preO1RelayOps,
	RoleAuditor: {
		"relay.read": true, "relay.metrics.read": true,
		"tenant.read": true, "tenant.list.read": true, "tenant.metrics.read": true, "tenant.usage.read": true,
		"plan.read": true, "invoice.read": true,
		"alert.read": true, "alert.rule.read": true, "alert.channel.read": true, "timeline.relay_audit": true,
		"audit.view": true, "cert.read": true, "health.read": true,
	},
	RoleBilling: {
		"plan.read": true, "plan.create": true, "plan.update": true,
		"tenant.plan.assign": true, "billing.settings.write": true,
		"invoice.read": true, "invoice.write": true,
		"tenant.usage.read": true, "tenant.list.read": true,
	},
	RoleSupport: {
		"tenant.read": true, "tenant.list.read": true, "alert.read": true, "health.read": true,
	},
}

// TestDecide_FullTable: every action constant × every role (and some
// non-roles) → the expected allow/deny. super-admin is allowed everything.
func TestDecide_FullTable(t *testing.T) {
	consts := actionConstants(t)
	names := make([]string, 0, len(consts))
	for n := range consts {
		names = append(names, n)
	}
	sort.Strings(names)
	roles := append(append([]string{}, Roles...), "", "admin", "Auditor", "billing ", "api_key")
	for _, role := range roles {
		for _, n := range names {
			action := consts[n]
			want := role == RoleSuperAdmin || expectedAllowed[role][action]
			err := decide(Actor{Role: role}, action, Target{})
			if want && err != nil {
				t.Errorf("%q %s (%s): want allow, got %v", role, n, action, err)
			}
			if !want && !errors.Is(err, ErrForbidden) {
				t.Errorf("%q %s (%s): want ErrForbidden, got %v", role, n, action, err)
			}
		}
	}
	// The expectation lists only real actions (a typo would pass silently).
	values := map[string]bool{}
	for _, v := range consts {
		values[v] = true
	}
	for role, set := range expectedAllowed {
		for a := range set {
			if !values[a] {
				t.Errorf("expectedAllowed[%s] has unknown action %q", role, a)
			}
		}
	}
}

// D-72 deny by default: an action with no matrix entry is denied to every
// role except super-admin, including a new relay.* action for relay-ops
// (the old prefix rule would have allowed it).
func TestDecide_UnknownActionDenied(t *testing.T) {
	for _, action := range []string{"relay.new_verb", "relay.pool.new_verb", "tenant.new_verb", "plan.read.x", "", "*"} {
		if err := decide(Actor{Role: RoleSuperAdmin}, action, Target{}); err != nil {
			t.Errorf("super-admin %q: %v", action, err)
		}
		for _, role := range Roles[1:] {
			if err := decide(Actor{Role: role}, action, Target{}); !errors.Is(err, ErrForbidden) {
				t.Errorf("%s %q: want ErrForbidden, got %v", role, action, err)
			}
		}
	}
}

// D-73: auditor holds every read and nothing else; support and auditor
// never write; billing never touches relays, pools, tenant lifecycle,
// quotas, alerts or operators; support never reads billing, usage, plan
// cards or the audit log.
func TestRoleBoundaries(t *testing.T) {
	for a, c := range actionClasses {
		if c == classRead && !roleActions[RoleAuditor][a] {
			t.Errorf("auditor lacks read %q", a)
		}
	}
	for _, role := range []string{RoleAuditor, RoleSupport} {
		for a := range roleActions[role] {
			if IsWriteAction(a) {
				t.Errorf("%s may write: %q", role, a)
			}
		}
	}
	for a := range roleActions[RoleBilling] {
		if strings.HasPrefix(a, "relay.") || strings.HasPrefix(a, "alert.") || strings.HasPrefix(a, "provider_user.") {
			t.Errorf("billing has %q", a)
		}
	}
	for _, a := range []string{ActionTenantSuspend, ActionTenantResume, ActionTenantDelete, ActionTenantDeleteCancel,
		ActionTenantLegalHoldSet, ActionTenantQuotaSet, ActionTenantCreate, ActionTenantFirstAdminReinvite,
		ActionTenantRelayPoolAssign, ActionTenantRead, ActionAuditView, ActionPlanDelete, ActionPlanSetDefault} {
		if roleActions[RoleBilling][a] {
			t.Errorf("billing has %q", a)
		}
	}
	for _, a := range []string{ActionInvoiceRead, ActionInvoiceWrite, ActionTenantUsageRead, ActionPlanRead,
		ActionAuditView, ActionTimelineRelayAudit, ActionTenantMetricsRead, ActionRelayRead, ActionCertRead} {
		if roleActions[RoleSupport][a] {
			t.Errorf("support has %q", a)
		}
	}
}

func TestIsWriteAction(t *testing.T) {
	for a, c := range actionClasses {
		if IsWriteAction(a) != (c != classRead) {
			t.Errorf("IsWriteAction(%q) = %v, class %d", a, IsWriteAction(a), c)
		}
	}
	for _, a := range []string{"", "unknown", "relay.read.x", "invoice.issue"} {
		if !IsWriteAction(a) {
			t.Errorf("IsWriteAction(%q) = false; unknown and audit-only must count as writes", a)
		}
	}
}

// The Authz entry points use the expected actions: tenant list vs detail.
func TestAuthz_TenantListVsDetail(t *testing.T) {
	a := NewAuthz()
	for _, c := range []struct {
		role         string
		list, detail bool
	}{
		{RoleSuperAdmin, true, true},
		{RoleAuditor, true, true},
		{RoleSupport, true, true},
		{RoleBilling, true, false},
		{RoleRelayOps, false, false},
	} {
		if got := a.CanListTenants(Actor{Role: c.role}) == nil; got != c.list {
			t.Errorf("%s list: %v", c.role, got)
		}
		if got := a.CanReadTenants(Actor{Role: c.role}, Target{Type: "tenant", ID: "t"}) == nil; got != c.detail {
			t.Errorf("%s detail: %v", c.role, got)
		}
	}
}

// D-74: TOTP is mandatory for super-admin, auditor, billing and support;
// optional for relay-ops unless enabled.
func TestRequiresTOTP_Roles(t *testing.T) {
	for _, c := range []struct {
		role string
		want bool
	}{
		{RoleSuperAdmin, true}, {RoleAuditor, true}, {RoleBilling, true}, {RoleSupport, true},
		{RoleRelayOps, false}, {"", false}, {"admin", false},
	} {
		if got := RequiresTOTP(&ProviderUser{Role: c.role}); got != c.want {
			t.Errorf("RequiresTOTP(%q) = %v, want %v", c.role, got, c.want)
		}
	}
}

func TestValidRole_NewRoles(t *testing.T) {
	for _, r := range []string{RoleSuperAdmin, RoleRelayOps, RoleAuditor, RoleBilling, RoleSupport} {
		if !ValidRole(r) {
			t.Errorf("ValidRole(%q) = false", r)
		}
	}
	for _, r := range []string{"", "admin", "Auditor", "billing ", "api_key", "owner"} {
		if ValidRole(r) {
			t.Errorf("ValidRole(%q) = true", r)
		}
	}
}
