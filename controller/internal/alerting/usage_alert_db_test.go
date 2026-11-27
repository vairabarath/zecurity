package alerting

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"
)

// Sprint 25 U4 (D-61): tenant.usage_over over the real usage ledger and the
// real evaluator: fires once per tenant per month at the threshold, only for
// watched tenants, and resolves at the UTC month rollover.

const ruleUsageOver = "00000000-0000-0000-0002-000000000010"

func TestAlertsDB_UsageOver(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if n := e.count(t, `SELECT count(*) FROM alert_rules WHERE id = $1 AND kind = 'tenant.usage_over' AND scope = 'tenant' AND NOT enabled`, ruleUsageOver); n != 1 {
		t.Fatal("seeded usage rule missing, or enabled by default")
	}
	e.onlyRule(t, ruleUsageOver)
	e.exec(t, `UPDATE alert_rules SET params = '{"meter": "relay_bytes_to_clients", "threshold": 1000}' WHERE id = $1`, ruleUsageOver)

	acme, beta := e.workspace(t, "acme"), e.workspace(t, "beta")
	suspended := e.workspace(t, "susp")
	e.exec(t, `UPDATE workspaces SET suspended_at = now(), suspended_by = 'admin@provider.test', suspension_reason = 'unpaid' WHERE id = $1`, suspended)
	purged := "dddddddd-0000-0000-0000-0000000000d1"
	e.exec(t, `INSERT INTO usage_tenants (tenant_id, name, slug, purged_at) VALUES ($1, 'Gone', 'gone', now())`, purged)
	nov := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	use := func(tenant string, at time.Time, n int64) {
		e.exec(t, `INSERT INTO usage_hourly (tenant_id, meter, hour, quantity) VALUES ($1, 'relay_bytes_to_clients', $2, $3)
			ON CONFLICT (tenant_id, meter, hour) DO UPDATE SET quantity = usage_hourly.quantity + EXCLUDED.quantity`, tenant, at.Truncate(time.Hour), n)
	}
	use(acme, nov, 1500)
	use(beta, nov, 999)
	use(suspended, nov, 1_000_000)         // suspended tenants aren't watched
	use(purged, nov, 1_000_000)            // nor purged ones
	use(beta, nov.AddDate(0, -1, 0), 5000) // October doesn't count in November

	now := nov.Add(30 * time.Minute)
	ev := &Evaluator{Store: e.store, Observer: Sources{DB: e.pool}, Now: func() time.Time { return now }}
	tick := func(at time.Time, want ...string) []Event {
		t.Helper()
		now = at
		events := ev.Tick(ctx)
		var got []string
		for _, x := range events {
			got = append(got, x.Transition+":"+x.TargetName)
		}
		sort.Strings(got) // the evaluator doesn't order a tick's events
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("at %s: events %v, want %v", at.Format(time.RFC3339), got, want)
		}
		return events
	}

	fired := tick(now, TransitionFired+":acme")
	f := fired[0]
	if f.Scope != ScopeTenant || f.TenantID == nil || *f.TenantID != acme || f.Severity != SeverityWarning {
		t.Fatalf("fired event %+v", f)
	}
	if f.Summary != "Tenant acme used 1.5 kB relayed to clients in November 2026 (threshold 1.0 kB)" {
		t.Fatalf("summary %q", f.Summary)
	}
	tick(now.Add(time.Hour)) // still over: no second alert this month
	use(acme, nov.Add(2*time.Hour), 10_000)
	tick(now.Add(3 * time.Hour))       // more usage: still one alert
	use(beta, nov.Add(4*time.Hour), 1) // beta reaches exactly the threshold
	tick(now.Add(5*time.Hour), TransitionFired+":beta")

	// UTC month rollover: December usage starts at 0 → both resolve.
	dec := time.Date(2026, 12, 1, 0, 0, 30, 0, time.UTC)
	tick(dec, TransitionResolved+":acme", TransitionResolved+":beta") // sorted
	tick(dec.Add(time.Hour))
	if n := e.count(t, `SELECT count(*) FROM alert_events WHERE rule_id = $1 AND transition = 'fired'`, ruleUsageOver); n != 2 {
		t.Fatalf("%d fired events, want 2 (one per tenant per month)", n)
	}
	// A new month over the threshold fires again.
	use(acme, dec.Add(time.Hour), 2000)
	tick(dec.Add(2*time.Hour), TransitionFired+":acme")
}

// The rule is managed like every rule (D-53): super-admin only, the params
// validated by kind.
func TestAlertsDB_UsageOverRuleUpdate(t *testing.T) {
	e := newEnv(t)
	ok := e.do(t, "super-admin", "PATCH", "/provider/alert-rules/"+ruleUsageOver,
		map[string]any{"enabled": true, "params": map[string]any{"meter": "active_users", "threshold": 50}, "reason": "warn at 50 users"})
	if ok.code != 200 {
		t.Fatalf("update = %d %s", ok.code, ok.body)
	}
	if got := e.one(t, `SELECT params->>'meter' || ':' || (params->>'threshold') FROM alert_rules WHERE id = $1`, ruleUsageOver); got != "active_users:50" {
		t.Fatalf("stored params %s", got)
	}
	bad := e.do(t, "super-admin", "PATCH", "/provider/alert-rules/"+ruleUsageOver,
		map[string]any{"params": map[string]any{"meter": "bytes", "threshold": 5}, "reason": "typo"})
	if bad.code != 400 {
		t.Fatalf("bad meter = %d %s", bad.code, bad.body)
	}
	if r := e.do(t, "relay-ops", "PATCH", "/provider/alert-rules/"+ruleUsageOver,
		map[string]any{"enabled": false, "reason": "x"}); r.code != 403 {
		t.Fatalf("relay-ops update = %d", r.code)
	}
}
