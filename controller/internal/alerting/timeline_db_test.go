package alerting

import (
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/ztna/controller/internal/provider"
	"github.com/yourorg/ztna/controller/internal/providerquery"
	"github.com/yourorg/ztna/controller/internal/relay"
)

// DB-backed tests for the incident timeline (Sprint 24 T1, F27).

type timelineFixture struct {
	relayInPool, relayDefault, pool, tenant string
	t0                                      time.Time
}

// seedTimeline writes a small incident: relay-1 (in pool-east) is drained,
// goes offline, the pool fails over, relay-2 (default pool) goes offline,
// tenant acme's connector goes offline, relay-1 recovers, acme is suspended.
func seedTimeline(t *testing.T, e *env) timelineFixture {
	t.Helper()
	f := timelineFixture{t0: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)}
	f.pool = e.one(t, `INSERT INTO relay_pools (name) VALUES ('pool-east') RETURNING id::text`)
	f.relayInPool = e.one(t, `INSERT INTO relays (name, status, pool_id) VALUES ('relay-1', 'active', $1) RETURNING id::text`, f.pool)
	f.relayDefault = e.one(t, `INSERT INTO relays (name, status) VALUES ('relay-2', 'active') RETURNING id::text`)
	f.tenant = e.workspace(t, "acme")
	at := func(sec int) time.Time { return f.t0.Add(time.Duration(sec) * time.Second) }

	alert := func(sec int, rule, kind, scope string, tenant any, ttype, tid, tname, transition string) {
		status := "firing"
		if transition == TransitionResolved {
			status = "resolved"
		}
		e.exec(t, `INSERT INTO alert_events (rule_id, kind, scope, tenant_id, target_type, target_id, target_name,
		               transition, status, severity, summary, started_at, occurred_at)
		           VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'critical', $7 || ' ' || $8, $10, $10)`,
			rule, kind, scope, tenant, ttype, tid, tname, transition, status, at(sec))
	}
	audit := func(sec int, email, action, ttype, tid string, details string) {
		e.exec(t, `INSERT INTO provider_audit_logs (provider_email, action, target_type, target_id, details, created_at)
		           VALUES ($1, $2, $3, $4, $5::jsonb, $6)`, email, action, ttype, tid, details, at(sec))
	}

	audit(0, "admin@provider.test", "relay.drain", "relay", f.relayInPool, `{"name":"relay-1","reason":"kernel upgrade"}`)
	// No name recorded: the timeline shows the relay's current name.
	audit(30, "admin@provider.test", "relay.capacity.set", "relay", f.relayInPool, `{"reason":"1 Gbps"}`)
	alert(60, ruleRelayOffline, KindRelayOffline, ScopePlatform, nil, "relay", f.relayInPool, "relay-1", TransitionFired)
	alert(120, rulePool, KindPoolNoEligibleRelay, ScopePlatform, nil, "pool", f.pool, "pool-east", TransitionFired)
	// Same instant as the pool alert: the keyset must order across sources.
	audit(120, relayPoolFailoverActor, "relay_pool.failover", "relay_pool", f.pool, `{"pool_name":"pool-east","tenants":1}`)
	alert(180, ruleRelayOffline, KindRelayOffline, ScopePlatform, nil, "relay", f.relayDefault, "relay-2", TransitionFired)
	alert(240, ruleConnectorOffline, KindConnectorOffline, ScopeTenant, f.tenant, "connector", "c-1", "acme-connector", TransitionFired)
	alert(300, ruleRelayOffline, KindRelayOffline, ScopePlatform, nil, "relay", f.relayInPool, "relay-1", TransitionResolved)
	audit(360, "admin@provider.test", "tenant.suspend", "tenant", f.tenant, `{"reason":"unpaid","name":"acme"}`)
	// Not incident context: operator-account rows and reads stay on the audit page.
	audit(400, "admin@provider.test", "tenant.read", "tenant", f.tenant, `{}`)
	audit(420, "admin@provider.test", "provider_user.create", "provider_user", e.opsID, `{"email":"ops@provider.test"}`)
	return f
}

const relayPoolFailoverActor = "system:relay-pool-failover"

func timelineEnv(t *testing.T) *env {
	e := newEnv(t)
	h := &TimelineHandler{DB: e.pool, Authz: provider.NewAuthz()}
	e.mux.HandleFunc("GET /provider/timeline", h.List)
	return e
}

func getTimeline(t *testing.T, e *env, role string, q url.Values) providerquery.Page[providerquery.TimelineItem] {
	t.Helper()
	r := e.do(t, role, "GET", "/provider/timeline?"+q.Encode(), nil)
	if r.code != 200 {
		t.Fatalf("timeline %v as %s: %d %s", q, role, r.code, r.body)
	}
	var p providerquery.Page[providerquery.TimelineItem]
	if err := json.Unmarshal([]byte(r.body), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// label is "source:what@seconds" for compact order assertions.
func labels(f timelineFixture, items []providerquery.TimelineItem) string {
	var out []string
	for _, it := range items {
		what := it.TargetName
		if it.Source == providerquery.TimelineSourceAlert {
			what += "/" + *it.Transition
		} else {
			what = *it.Action
		}
		out = append(out, it.Source+":"+what+"@"+strconv.Itoa(int(it.At.Sub(f.t0).Seconds())))
	}
	return strings.Join(out, " ")
}

func TestTimelineDB_SuperAdmin(t *testing.T) {
	e := timelineEnv(t)
	f := seedTimeline(t, e)
	sa := provider.RoleSuperAdmin

	all := getTimeline(t, e, sa, url.Values{})
	want := "audit:tenant.suspend@360 alert:relay-1/resolved@300 alert:acme-connector/fired@240 alert:relay-2/fired@180 " +
		"%s %s alert:relay-1/fired@60 audit:relay.capacity.set@30 audit:relay.drain@0"
	// The two rows at 120 s are ordered by id (random): accept either order.
	a := strings.Fields(labels(f, all.Items))
	if len(a) != 9 || all.NextCursor != nil {
		t.Fatalf("all: %v (next %v)", a, all.NextCursor)
	}
	mid := map[string]bool{a[4]: true, a[5]: true}
	if !mid["alert:pool-east/fired@120"] || !mid["audit:relay_pool.failover@120"] {
		t.Fatalf("rows at 120 s: %v", a[4:6])
	}
	a[4], a[5] = "%s", "%s"
	if got := strings.Join(a, " "); got != want {
		t.Fatalf("all:\n got %s\nwant %s", got, want)
	}
	for _, it := range all.Items {
		if it.Action != nil && *it.Action == "relay.drain" {
			if it.Reason == nil || *it.Reason != "kernel upgrade" || it.Actor == nil || *it.Actor != "admin@provider.test" || it.TargetName != "relay-1" {
				t.Fatalf("drain row: %+v", it)
			}
		}
		if it.Action != nil && *it.Action == "relay.capacity.set" && it.TargetName != "relay-1" {
			t.Fatalf("capacity row name (current relay name): %q", it.TargetName)
		}
		if it.Action != nil && *it.Action == "relay_pool.failover" && it.TargetName != "pool-east" {
			t.Fatalf("failover row name: %q", it.TargetName)
		}
		if it.Action != nil && *it.Action == "tenant.suspend" && (it.TenantID == nil || *it.TenantID != f.tenant) {
			t.Fatalf("suspend row tenant: %+v", it)
		}
	}

	if got := labels(f, getTimeline(t, e, sa, url.Values{"relay_id": {f.relayInPool}}).Items); got !=
		"alert:relay-1/resolved@300 alert:relay-1/fired@60 audit:relay.capacity.set@30 audit:relay.drain@0" {
		t.Fatalf("relay filter: %s", got)
	}
	// pool_id: the pool's rows and those of the relays now in it, not relay-2.
	if got := getTimeline(t, e, sa, url.Values{"pool_id": {f.pool}}); len(got.Items) != 6 || strings.Contains(labels(f, got.Items), "relay-2") {
		t.Fatalf("pool filter: %s", labels(f, got.Items))
	}
	if got := labels(f, getTimeline(t, e, sa, url.Values{"tenant_id": {f.tenant}}).Items); got !=
		"audit:tenant.suspend@360 alert:acme-connector/fired@240" {
		t.Fatalf("tenant filter: %s", got)
	}
	if got := getTimeline(t, e, sa, url.Values{"source": {"audit"}}); len(got.Items) != 4 {
		t.Fatalf("source=audit: %s", labels(f, got.Items))
	}
	if got := getTimeline(t, e, sa, url.Values{"source": {"alert"}}); len(got.Items) != 5 {
		t.Fatalf("source=alert: %s", labels(f, got.Items))
	}
	win := url.Values{"since": {f.t0.Add(60 * time.Second).Format(time.RFC3339)}, "until": {f.t0.Add(180 * time.Second).Format(time.RFC3339)}}
	if got := getTimeline(t, e, sa, win); len(got.Items) != 3 {
		t.Fatalf("window [60,180): %s", labels(f, got.Items))
	}
}

// Paging walks the union without gaps or repeats, across the tie at 120 s.
func TestTimelineDB_Paging(t *testing.T) {
	e := timelineEnv(t)
	f := seedTimeline(t, e)
	full := labels(f, getTimeline(t, e, provider.RoleSuperAdmin, url.Values{}).Items)
	var got []string
	cursor := ""
	for i := 0; i < 10; i++ {
		q := url.Values{"limit": {"3"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		p := getTimeline(t, e, provider.RoleSuperAdmin, q)
		got = append(got, labels(f, p.Items))
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
	}
	if strings.Join(got, " ") != full {
		t.Fatalf("paged:\n got %s\nwant %s", strings.Join(got, " "), full)
	}
}

// D-40 extended, D-06, D-55: relay-ops get relay and pool alert rows and the
// audit rows of relay and pool operations (actor and reason, no details) —
// no tenant rows or names, no tenant or operator-account audit rows.
func TestTimelineDB_RelayOps(t *testing.T) {
	e := timelineEnv(t)
	f := seedTimeline(t, e)
	ops := provider.RoleRelayOps
	// A platform alert that is not a relay or pool kind: hidden from relay-ops
	// as in A1's reads (not only tenant-scoped ones).
	e.exec(t, `INSERT INTO alert_events (rule_id, kind, scope, target_type, target_id, target_name,
	               transition, status, severity, summary, started_at, occurred_at)
	           VALUES ('00000000-0000-0000-0002-000000000004', $1, 'platform', 'controller', 'valkey', 'valkey',
	                   'fired', 'firing', 'critical', 'valkey down', $2, $2)`, KindControlPlane, f.t0.Add(90*time.Second))
	// Relay- and pool-related rows that are tenant events: hidden from relay-ops.
	e.exec(t, `INSERT INTO provider_audit_logs (provider_email, action, target_type, target_id, details, created_at)
	           VALUES ('system:relay-placement', 'relay.placement_rejected', 'relay', $1,
	                   jsonb_build_object('tenant_id', $2::text, 'connector_id', 'c-1', 'reason', 'outside pools'), $3),
	                  ('admin@provider.test', 'tenant.relay_pool.assign', 'tenant', $2,
	                   jsonb_build_object('reason', 'move acme', 'after', jsonb_build_object('relay_pool_id', $4::text)), $3)`,
		f.relayInPool, f.tenant, f.t0.Add(150*time.Second), f.pool)

	r := e.do(t, ops, "GET", "/provider/timeline", nil)
	if r.code != 200 {
		t.Fatalf("relay-ops: %d %s", r.code, r.body)
	}
	for _, leak := range []string{"acme", f.tenant, "tenant.", "unpaid", "placement_rejected", "outside pools",
		"provider_user", "ops@provider.test", "valkey", `"details"`} {
		if strings.Contains(r.body, leak) {
			t.Fatalf("relay-ops body contains %q: %s", leak, r.body)
		}
	}
	all := getTimeline(t, e, ops, url.Values{})
	want := "alert:relay-1/resolved@300 alert:relay-2/fired@180 %s %s alert:relay-1/fired@60 audit:relay.capacity.set@30 audit:relay.drain@0"
	a := strings.Fields(labels(f, all.Items))
	if len(a) != 7 {
		t.Fatalf("relay-ops rows: %v", a)
	}
	a[2], a[3] = "%s", "%s" // pool alert and failover at the same instant
	if got := strings.Join(a, " "); got != want {
		t.Fatalf("relay-ops:\n got %s\nwant %s", got, want)
	}
	for _, it := range all.Items {
		if it.TenantID != nil || it.Details != nil {
			t.Fatalf("relay-ops row: %+v", it)
		}
		if it.Action != nil && *it.Action == "relay.drain" &&
			(it.Actor == nil || *it.Actor != "admin@provider.test" || it.Reason == nil || *it.Reason != "kernel upgrade") {
			t.Fatalf("relay-ops drain row lacks actor or reason: %+v", it)
		}
	}
	if got := getTimeline(t, e, ops, url.Values{"tenant_id": {f.tenant}}); len(got.Items) != 0 {
		t.Fatalf("relay-ops tenant filter: %s", labels(f, got.Items))
	}
	if got := getTimeline(t, e, ops, url.Values{"source": {"audit"}}); len(got.Items) != 3 {
		t.Fatalf("relay-ops audit source: %s", labels(f, got.Items))
	}
	if got := getTimeline(t, e, ops, url.Values{"pool_id": {f.pool}}); len(got.Items) != 6 {
		t.Fatalf("relay-ops pool filter: %s", labels(f, got.Items))
	}
	// The full provider audit log stays super-admin only: relay-ops lack
	// audit.view (D-55 grants timeline.relay_audit, nothing more).
	if provider.NewAuthz().CanViewProviderAudit(provider.Actor{Role: ops}) == nil {
		t.Fatal("relay-ops may read the full provider audit log")
	}
	// Super-admins still see the tenant-related relay rows.
	if got := labels(f, getTimeline(t, e, provider.RoleSuperAdmin, url.Values{"relay_id": {f.relayInPool}}).Items); !strings.Contains(got, "relay.placement_rejected") {
		t.Fatalf("super-admin relay view lacks placement_rejected: %s", got)
	}
}

func TestTimelineDB_BadRequests(t *testing.T) {
	e := timelineEnv(t)
	f := seedTimeline(t, e)
	for name, q := range map[string]string{
		"two entities":  "relay_id=" + f.relayInPool + "&pool_id=" + f.pool,
		"bad relay id":  "relay_id=relay-1",
		"bad tenant id": "tenant_id=acme",
		"bad source":    "source=metrics",
		"bad since":     "since=yesterday",
		"bad range":     "since=2026-11-21T00:00:00Z&until=2026-11-20T00:00:00Z",
		"bad limit":     "limit=-1",
		"bad cursor":    "cursor=%21%21",
	} {
		if r := e.do(t, provider.RoleSuperAdmin, "GET", "/provider/timeline?"+q, nil); r.code != 400 {
			t.Errorf("%s: %d %s", name, r.code, r.body)
		}
	}
}

// The allowlist spells the system failover actions as literals (provider
// can't import relay); keep them equal to relay's constants.
func TestRelayOpsTimelineAuditActions(t *testing.T) {
	have := map[string]bool{}
	for _, a := range provider.RelayOpsTimelineAuditActions {
		have[a] = true
	}
	for _, a := range []string{relay.ActionPoolFailover, relay.ActionPoolFailback, provider.ActionRelayDrain, provider.ActionRelayMove} {
		if !have[a] {
			t.Errorf("%s missing from RelayOpsTimelineAuditActions", a)
		}
	}
	for _, a := range []string{provider.ActionRelayPlacementRejected, provider.ActionTenantRelayPoolAssign, provider.ActionRelayRead} {
		if have[a] {
			t.Errorf("%s must not be visible to relay-ops (tenant event or read)", a)
		}
	}
}

// Sprint 27 O1 (D-73): auditor sees what a super-admin sees; support sees
// tenant alerts only (never usage alerts, platform alerts or audit rows);
// billing has no alert.read.
func TestAlertsDB_NewRoleViews(t *testing.T) {
	e := timelineEnv(t)
	f := seedTimeline(t, e)
	e.exec(t, `INSERT INTO alert_events (rule_id, kind, scope, tenant_id, target_type, target_id, target_name,
	               transition, status, severity, summary, started_at, occurred_at)
	           VALUES ($1, $2, 'tenant', $3::uuid, 'tenant', $3::text, 'acme', 'fired', 'firing', 'warning', 'acme usage 600 GB', $4, $4)`,
		ruleUsageOver, KindTenantUsageOver, f.tenant, f.t0.Add(270*time.Second))

	// auditor = super-admin, row for row.
	for _, q := range []url.Values{{}, {"tenant_id": {f.tenant}}, {"source": {"audit"}}} {
		sa := labels(f, getTimeline(t, e, provider.RoleSuperAdmin, q).Items)
		if au := labels(f, getTimeline(t, e, provider.RoleAuditor, q).Items); au != sa {
			t.Fatalf("auditor timeline %v:\n got %s\nwant %s", q, au, sa)
		}
	}
	sa := e.do(t, provider.RoleSuperAdmin, "GET", "/provider/alert-events", nil)
	if au := e.do(t, provider.RoleAuditor, "GET", "/provider/alert-events", nil); au.code != 200 || au.body != sa.body {
		t.Fatalf("auditor events differ: %d %s", au.code, au.body)
	}

	// support: the tenant's connector alert only.
	sup := provider.RoleSupport
	for _, q := range []url.Values{{}, {"tenant_id": {f.tenant}}} {
		if got := labels(f, getTimeline(t, e, sup, q).Items); got != "alert:acme-connector/fired@240" {
			t.Fatalf("support timeline %v: %s", q, got)
		}
	}
	if got := getTimeline(t, e, sup, url.Values{"source": {"audit"}}); len(got.Items) != 0 {
		t.Fatalf("support saw audit rows: %s", labels(f, got.Items))
	}
	r := e.do(t, sup, "GET", "/provider/alert-events", nil)
	if r.code != 200 || !strings.Contains(r.body, KindConnectorOffline) {
		t.Fatalf("support events: %d %s", r.code, r.body)
	}
	for _, leak := range []string{KindRelayOffline, KindPoolNoEligibleRelay, KindTenantUsageOver, "600 GB"} {
		if strings.Contains(r.body, leak) {
			t.Fatalf("support events contain %q: %s", leak, r.body)
		}
	}
	for _, path := range []string{"/provider/alert-events?scope=platform", "/provider/alerts?scope=platform"} {
		if r := e.do(t, sup, "GET", path, nil); r.code != 200 || strings.Contains(r.body, "relay") {
			t.Fatalf("support %s: %d %s", path, r.code, r.body)
		}
	}

	// billing: no alerts, no timeline. Rule management stays super-admin
	// only for every new role.
	for _, path := range []string{"/provider/alerts", "/provider/alert-events", "/provider/timeline"} {
		if r := e.do(t, provider.RoleBilling, "GET", path, nil); r.code != 403 {
			t.Fatalf("billing %s: %d", path, r.code)
		}
	}
	for _, role := range []string{provider.RoleAuditor, provider.RoleBilling, provider.RoleSupport} {
		if r := e.do(t, role, "PATCH", "/provider/alert-rules/"+ruleRelayOffline, map[string]any{"enabled": true, "reason": "x"}); r.code != 403 {
			t.Fatalf("%s rule update: %d", role, r.code)
		}
	}
	if r := e.do(t, provider.RoleAuditor, "GET", "/provider/alert-rules", nil); r.code != 200 {
		t.Fatalf("auditor rule read: %d", r.code)
	}
}
