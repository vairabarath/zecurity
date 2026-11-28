package alerting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/health"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// DB-backed tests for F26 alert rules (Sprint 24 A1). They run against a
// throwaway database with every migration applied (PKI_TEST_DATABASE_URL).

const (
	ruleRelayOffline     = "00000000-0000-0000-0002-000000000001"
	ruleOverCapacity     = "00000000-0000-0000-0002-000000000002"
	rulePool             = "00000000-0000-0000-0002-000000000003"
	ruleConnectorOffline = "00000000-0000-0000-0002-000000000006"
)

type env struct {
	pool    *pgxpool.Pool
	store   *Store
	mux     *http.ServeMux
	superID string
	opsID   string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("alerting_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)"); err != nil {
			t.Logf("drop test database: %v", err)
		}
		admin.Close()
	})
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	e := &env{pool: pool, store: NewStore(pool)}
	e.exec(t, `INSERT INTO provider_users (email, role) VALUES ('admin@provider.test', 'super-admin')`)
	e.exec(t, `INSERT INTO provider_users (email, role) VALUES ('ops@provider.test', 'relay-ops')`)
	e.superID = e.one(t, `SELECT id::text FROM provider_users WHERE role = 'super-admin'`)
	e.opsID = e.one(t, `SELECT id::text FROM provider_users WHERE role = 'relay-ops'`)
	h := &Handler{Store: e.store, Authz: provider.NewAuthz(), ProviderStore: provider.NewStore(pool)}
	e.mux = http.NewServeMux()
	e.mux.HandleFunc("GET /provider/alerts", h.ListActive)
	e.mux.HandleFunc("GET /provider/alert-events", h.ListEvents)
	e.mux.HandleFunc("GET /provider/alert-rules", h.ListRules)
	e.mux.HandleFunc("POST /provider/alert-rules", h.CreateRule)
	e.mux.HandleFunc("PATCH /provider/alert-rules/{id}", h.UpdateRule)
	e.mux.HandleFunc("DELETE /provider/alert-rules/{id}", h.DeleteRule)
	return e
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func (e *env) one(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return s
}

func (e *env) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func (e *env) workspace(t *testing.T, slug string) string {
	t.Helper()
	return e.one(t, `INSERT INTO workspaces (slug, name, trust_domain, status) VALUES ($1, $1, $2, 'active') RETURNING id::text`,
		slug, "ws-"+slug+".zecurity.in")
}

func (e *env) connector(t *testing.T, ws, name, status string) string {
	t.Helper()
	rn := e.one(t, `INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, $2, 'office') RETURNING id::text`, ws, "rn-"+name)
	return e.one(t, `INSERT INTO connectors (tenant_id, remote_network_id, name, status) VALUES ($1, $2, $3, $4) RETURNING id::text`,
		ws, rn, name, status)
}

// onlyRule enables exactly one rule.
func (e *env) onlyRule(t *testing.T, id string) {
	t.Helper()
	e.exec(t, `UPDATE alert_rules SET enabled = (id = $1)`, id)
}

type resp struct {
	code int
	body string
}

func (e *env) do(t *testing.T, role, method, path string, body any) resp {
	t.Helper()
	raw := ""
	if body != nil {
		j, _ := json.Marshal(body)
		raw = string(j)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(raw))
	req.RemoteAddr = "192.0.2.10:54321"
	actor := provider.Actor{UserID: e.superID, Email: "admin@provider.test", Role: provider.RoleSuperAdmin}
	if role != provider.RoleSuperAdmin {
		// relay-ops and the Sprint 27 roles share the second operator row.
		actor = provider.Actor{UserID: e.opsID, Email: "ops@provider.test", Role: role}
	}
	req = req.WithContext(provider.WithActor(req.Context(), actor))
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return resp{rec.Code, rec.Body.String()}
}

// fakeObserver returns fixed observations per kind, or an error.
type fakeObserver struct {
	obs map[string][]Observation
	err error
}

func (f *fakeObserver) Observe(_ context.Context, kind string, _ Params, _ time.Time) ([]Observation, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.obs[kind], nil
}

// D-50: the scopes are separate in the schema, whatever the code does.
func TestAlertsDB_SchemaScopes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// 069 seeds nine rules; 072 (Sprint 25 U4) adds tenant.usage_over.
	if n := e.count(t, `SELECT count(*) FROM alert_rules`); n != 10 {
		t.Fatalf("seeded rules = %d, want 10", n)
	}
	if n := e.count(t, `SELECT count(*) FROM alert_rules WHERE scope = 'platform' AND enabled`); n != 5 {
		t.Fatalf("enabled platform rules = %d, want 5", n)
	}
	if n := e.count(t, `SELECT count(*) FROM alert_rules WHERE scope = 'tenant' AND enabled`); n != 0 {
		t.Fatalf("tenant rules must start disabled, %d enabled", n)
	}
	ws := e.workspace(t, "acme")
	refused := map[string]string{
		"tenant state without tenant_id": `INSERT INTO alert_states (rule_id, target_type, target_id, scope, tenant_id, target_name, status, severity, summary, pending_since)
			VALUES ('` + ruleConnectorOffline + `', 'connector', 'c1', 'tenant', NULL, 'c1', 'pending', 'warning', 's', NOW())`,
		"platform state with tenant_id": `INSERT INTO alert_states (rule_id, target_type, target_id, scope, tenant_id, target_name, status, severity, summary, pending_since)
			VALUES ('` + ruleRelayOffline + `', 'relay', 'r1', 'platform', '` + ws + `', 'r1', 'pending', 'warning', 's', NOW())`,
		"tenant event without tenant_id": `INSERT INTO alert_events (rule_id, kind, scope, tenant_id, target_type, target_id, target_name, transition, status, severity, summary, started_at)
			VALUES ('` + ruleConnectorOffline + `', 'connector.offline', 'tenant', NULL, 'connector', 'c1', 'c1', 'fired', 'firing', 'warning', 's', NOW())`,
		"kind in the wrong scope": `INSERT INTO alert_rules (kind, scope, name, severity) VALUES ('connector.offline', 'platform', 'bad', 'warning')`,
		"firing without firing_since": `INSERT INTO alert_states (rule_id, target_type, target_id, scope, target_name, status, severity, summary, pending_since)
			VALUES ('` + ruleRelayOffline + `', 'relay', 'r2', 'platform', 'r2', 'firing', 'warning', 's', NOW())`,
		"resolved transition with firing status": `INSERT INTO alert_events (rule_id, kind, scope, target_type, target_id, target_name, transition, status, severity, summary, started_at)
			VALUES ('` + ruleRelayOffline + `', 'relay.offline', 'platform', 'relay', 'r1', 'r1', 'resolved', 'firing', 'warning', 's', NOW())`,
	}
	for name, sql := range refused {
		if _, err := e.pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s: accepted, want a CHECK violation", name)
		}
	}
}

// The state machine: pending → firing after for_seconds, no repeat, renotify,
// resolve; a read error changes nothing; disabling resolves.
func TestAlertsDB_StateMachine(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	e.onlyRule(t, ruleRelayOffline) // for_seconds 300
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	now := t0
	obsv := &fakeObserver{obs: map[string][]Observation{}}
	ev := &Evaluator{Store: e.store, Observer: obsv, Now: func() time.Time { return now }}
	relayID := e.one(t, `INSERT INTO relays (name, status) VALUES ('relay-a', 'inactive') RETURNING id::text`)
	relayA := Observation{TargetType: "relay", TargetID: relayID, TargetName: "relay-a", Summary: "Relay relay-a is offline"}
	step := func(at time.Duration, obs []Observation, err error, want ...string) []Event {
		t.Helper()
		now = t0.Add(at)
		obsv.obs[KindRelayOffline], obsv.err = obs, err
		events := ev.Tick(ctx)
		var got []string
		for _, x := range events {
			got = append(got, x.Transition)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("at %s: events %v, want %v", at, got, want)
		}
		return events
	}
	state := func() string {
		var s string
		err := e.pool.QueryRow(ctx, `SELECT status FROM alert_states WHERE rule_id = $1`, ruleRelayOffline).Scan(&s)
		if err != nil {
			return "none"
		}
		return s
	}

	step(0, []Observation{relayA}, nil)
	if state() != "pending" {
		t.Fatalf("after first observation: %s, want pending", state())
	}
	step(299*time.Second, []Observation{relayA}, nil)
	fired := step(300*time.Second, []Observation{relayA}, nil, TransitionFired)
	if fired[0].Scope != ScopePlatform || fired[0].TenantID != nil || fired[0].Severity != SeverityCritical ||
		!fired[0].StartedAt.Equal(t0.Add(300*time.Second)) || fired[0].TargetName != "relay-a" {
		t.Fatalf("fired event: %+v", fired[0])
	}
	step(400*time.Second, []Observation{relayA}, nil) // no repeat while firing
	step(500*time.Second, nil, errors.New("db down")) // unknown: nothing changes
	if state() != "firing" {
		t.Fatalf("a read error changed the state to %s", state())
	}
	resolved := step(600*time.Second, nil, nil, TransitionResolved)
	if !strings.HasPrefix(resolved[0].Summary, "Resolved: ") || resolved[0].Status != "resolved" {
		t.Fatalf("resolved event: %+v", resolved[0])
	}
	if state() != "none" {
		t.Fatalf("resolved alert left state %s", state())
	}
	// A pending condition that clears before for_seconds leaves no event.
	step(700*time.Second, []Observation{relayA}, nil)
	step(800*time.Second, nil, nil)
	if state() != "none" {
		t.Fatalf("cleared pending left state %s", state())
	}

	// Renotify, then disable while firing → resolved "(rule disabled)".
	e.exec(t, `UPDATE alert_rules SET for_seconds = 0, renotify_seconds = 300 WHERE id = $1`, ruleRelayOffline)
	step(1000*time.Second, []Observation{relayA}, nil, TransitionFired)
	step(1200*time.Second, []Observation{relayA}, nil)
	step(1300*time.Second, []Observation{relayA}, nil, TransitionRenotified)
	step(1400*time.Second, []Observation{relayA}, nil)
	e.exec(t, `UPDATE alert_rules SET enabled = FALSE WHERE id = $1`, ruleRelayOffline)
	off := step(1500*time.Second, []Observation{relayA}, nil, TransitionResolved)
	if !strings.HasPrefix(off[0].Summary, "Resolved (rule disabled): ") {
		t.Fatalf("disable summary: %q", off[0].Summary)
	}
	if n := e.count(t, `SELECT count(*) FROM alert_events WHERE rule_id = $1`, ruleRelayOffline); n != 5 {
		t.Fatalf("events recorded = %d, want 5 (fired, resolved, fired, renotified, resolved)", n)
	}
}

type fakeHealth struct{ report *health.DiagnosticReport }

func (f fakeHealth) Evaluate(context.Context) *health.DiagnosticReport { return f.report }

func targets(obs []Observation) string {
	var s []string
	for _, o := range obs {
		s = append(s, o.TargetType+":"+o.TargetName)
	}
	sort.Strings(s)
	return strings.Join(s, ",")
}

// Each signal against real rows.
func TestAlertsDB_Signals(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now()
	src := Sources{DB: e.pool}
	observe := func(kind string, raw string) []Observation {
		t.Helper()
		p, err := ParseParams(kind, []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		obs, err := src.Observe(ctx, kind, p, now)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		return obs
	}

	// Relays: one offline, one active over capacity, one active without capacity.
	e.exec(t, `INSERT INTO relays (name, status) VALUES ('relay-down', 'inactive')`)
	e.exec(t, `INSERT INTO relays (name, status, link_capacity_bps, bytes_sampled_at, bytes_rate_window_seconds,
	                               bytes_to_connector_per_sec, bytes_to_client_per_sec)
	           VALUES ('relay-busy', 'active', 1000, NOW(), 60, 500, 450)`)
	e.exec(t, `INSERT INTO relays (name, status, bytes_sampled_at, bytes_rate_window_seconds,
	                               bytes_to_connector_per_sec, bytes_to_client_per_sec)
	           VALUES ('relay-nocap', 'active', NOW(), 60, 5000, 5000)`)
	if got := targets(observe(KindRelayOffline, `{}`)); got != "relay:relay-down" {
		t.Errorf("relay.offline: %s", got)
	}
	if got := targets(observe(KindRelayOverCapacity, `{}`)); got != "relay:relay-busy" {
		t.Errorf("relay.over_capacity at 90%%: %s", got)
	}
	if got := targets(observe(KindRelayOverCapacity, `{"threshold_percent": 96}`)); got != "" {
		t.Errorf("relay.over_capacity at 96%% (relay at 95%%): %s", got)
	}

	// Tenants: acme (active) and frozen (suspended), both on the default
	// pool, which has no eligible relay.
	acme := e.workspace(t, "acme")
	frozen := e.workspace(t, "frozen")
	e.exec(t, `UPDATE workspaces SET suspended_at = NOW(), suspended_by = 't', suspension_reason = 't' WHERE id = $1`, frozen)
	e.connector(t, acme, "acme-c1", "disconnected")
	e.connector(t, acme, "acme-c2", "active")
	e.connector(t, frozen, "frozen-c1", "disconnected")

	conn := observe(KindConnectorOffline, `{}`)
	if targets(conn) != "connector:acme-c1" || conn[0].TenantID != acme {
		t.Errorf("connector.offline: %s (tenant %q)", targets(conn), conn[0].TenantID)
	}
	if got := targets(observe(KindPoolNoEligibleRelay, `{}`)); got != "pool:Default" && !strings.HasPrefix(got, "pool:") {
		t.Errorf("pool.no_eligible_relay: %s", got)
	}
	path := observe(KindTenantNoRelayPath, `{}`)
	if len(path) != 1 || path[0].TenantID != acme {
		t.Errorf("tenant.no_relay_path: %+v (the suspended tenant must not appear)", path)
	}
	// An eligible relay in the default pool clears both.
	e.exec(t, `INSERT INTO relays (name, status, capacity_label, public_addr) VALUES ('relay-ok', 'active', 'high', '203.0.113.5:443')`)
	if got := observe(KindPoolNoEligibleRelay, `{}`); len(got) != 0 {
		t.Errorf("pool.no_eligible_relay with an eligible relay: %s", targets(got))
	}
	if got := observe(KindTenantNoRelayPath, `{}`); len(got) != 0 {
		t.Errorf("tenant.no_relay_path with an eligible relay: %s", targets(got))
	}

	// Over quota: acme has 2 remote networks, limit lowered to 1.
	e.exec(t, `INSERT INTO workspace_quota_overrides (workspace_id, key, limit_value, reason, set_by) VALUES ($1, 'remote_networks', 1, 't', 'op')`, acme)
	q := observe(KindTenantOverQuota, `{}`)
	if len(q) != 1 || q[0].TenantID != acme || !strings.Contains(q[0].Summary, "remote networks 2/1") {
		t.Errorf("tenant.over_quota: %+v", q)
	}
	if got := observe(KindTenantOverQuota, `{"keys": ["users"]}`); len(got) != 0 {
		t.Errorf("tenant.over_quota filtered to users: %+v", got)
	}

	// Control plane: nil source is an error (unknown, never "resolved").
	if _, err := src.Observe(ctx, KindControlPlane, Params{}, now); !errors.Is(err, errNoSource) {
		t.Errorf("controlplane without a source: %v", err)
	}
	src.Health = fakeHealth{&health.DiagnosticReport{Components: map[string]health.ComponentStatus{
		"database": {Status: "up"}, "valkey": {Status: "down", Error: "connection refused"},
	}}}
	if got := targets(observe(KindControlPlane, `{}`)); got != "controller:valkey" {
		t.Errorf("controlplane.degraded: %s", got)
	}
	if got := observe(KindControlPlane, `{"components": ["database"]}`); len(got) != 0 {
		t.Errorf("controlplane.degraded limited to database: %s", targets(got))
	}

	// Certificates: a root CA expiring in 10 days fires at 30, not at 5.
	e.exec(t, `DELETE FROM ca_root`)
	e.exec(t, `INSERT INTO ca_root (encrypted_key, nonce, certificate_pem, not_before, not_after)
	           VALUES ('k', 'n', 'not a pem', NOW() - interval '1 year', NOW() + interval '10 days')`)
	if got := observe(KindCertificateExpiring, `{"days": 30}`); !strings.Contains(targets(got), "certificate:Root CA") {
		t.Errorf("certificate.expiring 30d: %s", targets(got))
	}
	if got := observe(KindCertificateExpiring, `{"days": 5}`); strings.Contains(targets(got), "Root CA") {
		t.Errorf("certificate.expiring 5d: %s", targets(got))
	}
}

// The API: relay-ops see relay and pool alerts only and can't manage rules;
// writes need a reason and are audited; deleting a firing rule resolves it;
// purging a tenant removes its alert rows (D-46).
func TestAlertsDB_API(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	acme := e.workspace(t, "acme")
	e.exec(t, `UPDATE alert_rules SET for_seconds = 0, enabled = TRUE WHERE id IN ($1, $2)`, ruleRelayOffline, ruleConnectorOffline)
	e.exec(t, `UPDATE alert_rules SET enabled = FALSE WHERE id NOT IN ($1, $2)`, ruleRelayOffline, ruleConnectorOffline)
	obsv := &fakeObserver{obs: map[string][]Observation{
		KindRelayOffline:     {{TargetType: "relay", TargetID: "r-a", TargetName: "relay-a", Summary: "Relay relay-a is offline"}},
		KindConnectorOffline: {{TargetType: "connector", TargetID: "c-1", TargetName: "acme-c1", TenantID: acme, Summary: "Connector acme-c1 of tenant acme is offline"}},
	}}
	ev := &Evaluator{Store: e.store, Observer: obsv}
	if n := len(ev.Tick(ctx)); n != 2 {
		t.Fatalf("fired %d, want 2", n)
	}

	var active struct {
		Items []ActiveAlert `json:"items"`
	}
	r := e.do(t, provider.RoleSuperAdmin, "GET", "/provider/alerts", nil)
	if r.code != 200 || json.Unmarshal([]byte(r.body), &active) != nil || len(active.Items) != 2 {
		t.Fatalf("super-admin alerts: %d %s", r.code, r.body)
	}
	r = e.do(t, provider.RoleSuperAdmin, "GET", "/provider/alerts?tenant_id="+acme, nil)
	if json.Unmarshal([]byte(r.body), &active) != nil || len(active.Items) != 1 || active.Items[0].Kind != KindConnectorOffline {
		t.Fatalf("tenant filter: %s", r.body)
	}
	r = e.do(t, provider.RoleRelayOps, "GET", "/provider/alerts", nil)
	if json.Unmarshal([]byte(r.body), &active) != nil || len(active.Items) != 1 || active.Items[0].Kind != KindRelayOffline {
		t.Fatalf("relay-ops alerts: %s", r.body)
	}
	if strings.Contains(r.body, "acme") {
		t.Fatalf("relay-ops saw a tenant name: %s", r.body)
	}
	for _, path := range []string{"/provider/alerts?scope=tenant", "/provider/alerts?tenant_id=" + acme, "/provider/alert-events?scope=tenant"} {
		if r := e.do(t, provider.RoleRelayOps, "GET", path, nil); r.code != 200 || strings.Contains(r.body, "acme") || strings.Contains(r.body, "connector") {
			t.Fatalf("relay-ops %s: %d %s", path, r.code, r.body)
		}
	}
	r = e.do(t, provider.RoleRelayOps, "GET", "/provider/alert-events", nil)
	if r.code != 200 || !strings.Contains(r.body, "relay.offline") || strings.Contains(r.body, "connector.offline") {
		t.Fatalf("relay-ops events: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, "GET", "/provider/alerts?scope=bogus", nil); r.code != 400 {
		t.Fatalf("bad scope: %d", r.code)
	}

	// Rule management is super-admin only.
	for _, c := range []struct{ method, path string }{
		{"GET", "/provider/alert-rules"},
		{"POST", "/provider/alert-rules"},
		{"PATCH", "/provider/alert-rules/" + ruleRelayOffline},
		{"DELETE", "/provider/alert-rules/" + ruleRelayOffline},
	} {
		if r := e.do(t, provider.RoleRelayOps, c.method, c.path, map[string]any{"reason": "x"}); r.code != 403 {
			t.Fatalf("relay-ops %s %s: %d", c.method, c.path, r.code)
		}
	}
	if r := e.do(t, provider.RoleSuperAdmin, "PATCH", "/provider/alert-rules/"+ruleOverCapacity, map[string]any{"enabled": true}); r.code != 400 || !strings.Contains(r.body, "reason_required") {
		t.Fatalf("patch without reason: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, "PATCH", "/provider/alert-rules/"+ruleOverCapacity,
		map[string]any{"params": map[string]any{"days": 3}, "reason": "x"}); r.code != 400 || !strings.Contains(r.body, "invalid_params") {
		t.Fatalf("patch bad params: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, "PATCH", "/provider/alert-rules/"+ruleOverCapacity,
		map[string]any{"renotify_seconds": 60, "reason": "x"}); r.code != 400 || !strings.Contains(r.body, "invalid_rule") {
		t.Fatalf("patch bad renotify: %d %s", r.code, r.body)
	}
	r = e.do(t, provider.RoleSuperAdmin, "PATCH", "/provider/alert-rules/"+ruleOverCapacity,
		map[string]any{"params": map[string]any{"threshold_percent": 80}, "severity": "critical", "reason": "tighter on the lab"})
	if r.code != 200 || !strings.Contains(r.body, `"threshold_percent":80`) {
		t.Fatalf("patch: %d %s", r.code, r.body)
	}
	var details string
	if err := e.pool.QueryRow(ctx, `SELECT details::text FROM provider_audit_logs WHERE action = 'alert.rule.update' AND target_id = $1`, ruleOverCapacity).Scan(&details); err != nil {
		t.Fatalf("update audit: %v", err)
	}
	if !strings.Contains(details, "tighter on the lab") || !strings.Contains(details, `"before"`) || !strings.Contains(details, `"after"`) {
		t.Fatalf("update audit details: %s", details)
	}

	r = e.do(t, provider.RoleSuperAdmin, "POST", "/provider/alert-rules", map[string]any{
		"kind": "shield.offline", "name": "Shield offline (fast)", "severity": "critical", "for_seconds": 60, "reason": "lab"})
	if r.code != 201 || !strings.Contains(r.body, `"scope":"tenant"`) {
		t.Fatalf("create: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, "POST", "/provider/alert-rules", map[string]any{
		"kind": "shield.offline", "name": "Shield offline (fast)", "severity": "critical", "reason": "lab"}); r.code != 409 {
		t.Fatalf("duplicate name: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, "POST", "/provider/alert-rules", map[string]any{
		"kind": "made.up", "name": "x", "severity": "critical", "reason": "lab"}); r.code != 400 {
		t.Fatalf("unknown kind: %d %s", r.code, r.body)
	}
	if n := e.count(t, `SELECT count(*) FROM provider_audit_logs WHERE action = 'alert.rule.create'`); n != 1 {
		t.Fatalf("create audits = %d", n)
	}

	// Deleting a rule with a firing alert resolves it in the same transaction.
	if r := e.do(t, provider.RoleSuperAdmin, "DELETE", "/provider/alert-rules/"+ruleRelayOffline, map[string]any{"reason": "replaced"}); r.code != 204 {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	if n := e.count(t, `SELECT count(*) FROM alert_events WHERE rule_id = $1 AND transition = 'resolved' AND summary LIKE 'Resolved (rule deleted)%'`, ruleRelayOffline); n != 1 {
		t.Fatalf("delete resolved events = %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM provider_audit_logs WHERE action = 'alert.rule.delete' AND target_id = $1`, ruleRelayOffline); n != 1 {
		t.Fatalf("delete audits = %d", n)
	}

	// D-46: a purged tenant's alert rows go with the workspace row.
	e.exec(t, `DELETE FROM workspaces WHERE id = $1`, acme)
	if n := e.count(t, `SELECT (SELECT count(*) FROM alert_states WHERE tenant_id IS NOT NULL) + (SELECT count(*) FROM alert_events WHERE tenant_id IS NOT NULL)`); n != 0 {
		t.Fatalf("tenant alert rows left after the workspace was deleted: %d", n)
	}
}

// Review fixes (PR #234): a rule changed mid-tick changes nothing; a target
// that leaves scope resolves with the reason; workspace CAs carry no tenant
// name; a pool used only by suspended tenants doesn't fire; one bad stored
// rule doesn't stop the others; the active list reports truncation.
func TestAlertsDB_ReviewFixes(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	now := time.Now()

	// Stale rule copy: Apply with an old UpdatedAt does nothing.
	e.onlyRule(t, ruleRelayOffline)
	e.exec(t, `UPDATE alert_rules SET for_seconds = 0 WHERE id = $1`, ruleRelayOffline)
	r, err := e.store.GetRule(ctx, ruleRelayOffline)
	if err != nil {
		t.Fatal(err)
	}
	stale := r
	stale.UpdatedAt = r.UpdatedAt.Add(-time.Minute)
	relayA := Observation{TargetType: "relay", TargetID: "r-a", TargetName: "relay-a", Summary: "Relay relay-a is offline"}
	if ev, err := e.store.Apply(ctx, stale, []Observation{relayA}, now, ""); err != nil || len(ev) != 0 {
		t.Fatalf("stale apply: %v %v", ev, err)
	}
	if n := e.count(t, `SELECT count(*) FROM alert_states`); n != 0 {
		t.Fatalf("stale apply wrote %d states", n)
	}
	disabledCopy := r
	disabledCopy.Enabled = false
	if ev, _ := e.store.Apply(ctx, r, []Observation{relayA}, now, ""); len(ev) != 1 {
		t.Fatalf("fresh apply: %v", ev)
	}
	if ev, _ := e.store.Apply(ctx, disabledCopy, nil, now, ""); len(ev) != 0 {
		t.Fatalf("an apply from a stale 'disabled' copy resolved alerts: %v", ev)
	}

	// Leaving scope: a suspended tenant's firing connector alert resolves
	// as "tenant suspended", not as fixed.
	acme := e.workspace(t, "acme")
	c1 := e.connector(t, acme, "acme-c1", "disconnected")
	e.onlyRule(t, ruleConnectorOffline)
	e.exec(t, `UPDATE alert_rules SET for_seconds = 0 WHERE id = $1`, ruleConnectorOffline)
	ev := &Evaluator{Store: e.store, Observer: Sources{DB: e.pool}}
	ofKind := func(events []Event, kind string) []Event {
		var out []Event
		for _, x := range events {
			if x.Kind == kind {
				out = append(out, x)
			}
		}
		return out
	}
	if got := ofKind(ev.Tick(ctx), KindConnectorOffline); len(got) != 1 || got[0].TargetID != c1 {
		t.Fatalf("connector alert: %+v", got)
	}
	e.exec(t, `UPDATE workspaces SET suspended_at = NOW(), suspended_by = 't', suspension_reason = 't' WHERE id = $1`, acme)
	got := ofKind(ev.Tick(ctx), KindConnectorOffline)
	if len(got) != 1 || !strings.HasPrefix(got[0].Summary, "Resolved (tenant suspended): ") {
		t.Fatalf("suspended tenant resolve: %+v", got)
	}

	// Pool used only by the suspended tenant: no alert.
	src := Sources{DB: e.pool}
	if obs, err := src.Observe(ctx, KindPoolNoEligibleRelay, Params{}, now); err != nil || len(obs) != 0 {
		t.Fatalf("pool used only by a suspended tenant: %v %v", targets(obs), err)
	}
	e.exec(t, `UPDATE workspaces SET suspended_at = NULL, suspended_by = NULL, suspension_reason = NULL WHERE id = $1`, acme)
	if obs, err := src.Observe(ctx, KindPoolNoEligibleRelay, Params{}, now); err != nil || len(obs) != 1 {
		t.Fatalf("pool used by an active tenant: %v %v", targets(obs), err)
	}

	// Workspace CA: platform alert without the tenant's name.
	e.exec(t, `UPDATE workspaces SET name = 'Acme Secret Corp' WHERE id = $1`, acme)
	if n := e.count(t, `SELECT count(*) FROM workspace_ca_keys WHERE tenant_id = $1`, acme); n > 0 {
		e.exec(t, `UPDATE workspace_ca_keys SET not_after = NOW() + interval '3 days' WHERE tenant_id = $1`, acme)
		p, _ := ParseParams(KindCertificateExpiring, nil)
		obs, err := src.Observe(ctx, KindCertificateExpiring, p, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range obs {
			if strings.Contains(o.TargetName+o.Summary, "Acme Secret Corp") {
				t.Fatalf("platform certificate alert names the tenant: %+v", o)
			}
		}
	} else {
		var keyID string
		err := e.pool.QueryRow(ctx, `INSERT INTO workspace_ca_keys (tenant_id, encrypted_private_key, nonce, certificate_pem, not_before, not_after)
			VALUES ($1, 'k', 'n', 'not a pem', NOW() - interval '1 day', NOW() + interval '3 days') RETURNING id::text`, acme).Scan(&keyID)
		if err != nil {
			t.Fatalf("seed workspace CA (adjust columns if the schema changed): %v", err)
		}
		p, _ := ParseParams(KindCertificateExpiring, nil)
		obs, err := src.Observe(ctx, KindCertificateExpiring, p, now)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, o := range obs {
			if strings.Contains(o.TargetName+o.Summary, "Acme Secret Corp") {
				t.Fatalf("platform certificate alert names the tenant: %+v", o)
			}
			if o.TargetName == "workspace CA "+keyID {
				found = true
			}
		}
		if !found {
			t.Fatalf("workspace CA not reported: %s", targets(obs))
		}
	}

	// A bad stored rule is skipped, the others still listed.
	e.exec(t, `UPDATE alert_rules SET params = '{"keys": ["sessions"]}' WHERE kind = 'tenant.over_quota'`)
	rules, err := e.store.ListRules(ctx)
	if err != nil || len(rules) != 9 { // ten seeded (072 adds one), one bad
		t.Fatalf("list with one bad rule: %d rules, %v", len(rules), err)
	}

	// Truncation flag on the active list.
	if r := e.do(t, provider.RoleSuperAdmin, "GET", "/provider/alerts", nil); r.code != 200 || !strings.Contains(r.body, `"truncated":false`) {
		t.Fatalf("truncated flag: %d %s", r.code, r.body)
	}
}
