package plans

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// DB-backed tests for plans (Sprint 23 B1, D-43). They run against a
// throwaway database with every migration applied (PKI_TEST_DATABASE_URL).

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
	dbName := fmt.Sprintf("plans_test_%d", time.Now().UnixNano())
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
	if err := pool.QueryRow(ctx, `INSERT INTO provider_users (email, role) VALUES ('admin@provider.test', 'super-admin') RETURNING id::text`).Scan(&e.superID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO provider_users (email, role) VALUES ('ops@provider.test', 'relay-ops') RETURNING id::text`).Scan(&e.opsID); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Store: e.store, Authz: provider.NewAuthz(), ProviderStore: provider.NewStore(pool)}
	e.mux = http.NewServeMux()
	e.mux.HandleFunc("POST /provider/plans", h.Create)
	e.mux.HandleFunc("PATCH /provider/plans/{id}", h.Update)
	e.mux.HandleFunc("DELETE /provider/plans/{id}", h.Delete)
	e.mux.HandleFunc("POST /provider/plans/{id}/default", h.SetDefault)
	e.mux.HandleFunc("PUT /provider/tenants/{id}/plan", h.Assign)
	return e
}

func (e *env) workspace(t *testing.T, slug string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(),
		`INSERT INTO workspaces (slug, name, trust_domain, status) VALUES ($1, $1, $2, 'active') RETURNING id::text`,
		slug, "ws-"+slug+".zecurity.in").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) planOf(t *testing.T, ws string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `SELECT plan_id::text FROM workspaces WHERE id = $1`, ws).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) defaults(t *testing.T) []string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT id::text FROM plans WHERE is_default ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func n(v int64) *int64 { return &v }

func limits(users, resources, networks, connectors, shields, devices *int64) Limits {
	return Limits{KeyUsers: users, KeyResources: resources, KeyRemoteNetworks: networks,
		KeyConnectors: connectors, KeyShields: shields, KeyClientDevices: devices}
}

func value(l Limits, k string) string {
	if l[k] == nil {
		return "unlimited"
	}
	return fmt.Sprint(*l[k])
}

// The seeded plans and the default-plan trigger (D-43).
func TestPlansDB_SeedsAndCurrentDefault(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	free, err := Get(ctx, e.pool, FreePlanID)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{KeyUsers: "5", KeyResources: "50", KeyRemoteNetworks: "3",
		KeyConnectors: "10", KeyShields: "10", KeyClientDevices: "25"}
	for k, v := range want {
		if got := value(free.Limits, k); got != v {
			t.Errorf("Free %s = %s, want %s", k, got, v)
		}
	}
	if !free.IsDefault || free.Name != "Free" {
		t.Fatalf("Free must be the default: %+v", free)
	}
	legacy, err := Get(ctx, e.pool, LegacyPlanID)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range Keys {
		if legacy.Limits[k] != nil {
			t.Errorf("Legacy %s = %d, want unlimited", k, *legacy.Limits[k])
		}
	}
	if legacy.IsDefault {
		t.Fatal("Legacy must not be the default")
	}

	if got := e.planOf(t, e.workspace(t, "first")); got != FreePlanID {
		t.Fatalf("new workspace plan %s, want Free", got)
	}
	// The trigger follows a changed default (a column default couldn't).
	pro, err := e.store.Create(ctx, "Pro", "", limits(n(50), n(500), n(20), n(40), n(40), n(250)), nil, CardMeta{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.SetDefault(ctx, pro.ID, nil); err != nil {
		t.Fatal(err)
	}
	if got := e.planOf(t, e.workspace(t, "second")); got != pro.ID {
		t.Fatalf("after switching the default, new workspace plan %s, want Pro", got)
	}
	// An insert that names a plan keeps it.
	var named string
	if err := e.pool.QueryRow(ctx, `INSERT INTO workspaces (slug, name, trust_domain, status, plan_id)
		VALUES ('named', 'named', 'ws-named.zecurity.in', 'active', $1) RETURNING plan_id::text`, LegacyPlanID).Scan(&named); err != nil {
		t.Fatal(err)
	}
	if named != LegacyPlanID {
		t.Fatalf("named plan %s, want Legacy", named)
	}
}

// All six keys are required; a missing key never means unlimited (D-43).
func TestPlansDB_LimitsValidation(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	full := limits(n(1), n(1), n(1), n(1), n(1), nil)
	missing := full.Clone()
	delete(missing, KeyResources)
	extra := full.Clone()
	extra["sessions"] = n(1)
	negative := full.Clone()
	negative[KeyUsers] = n(-1)
	for name, l := range map[string]Limits{"missing key": missing, "extra key": extra, "negative": negative, "nil map": nil} {
		if _, err := e.store.Create(ctx, "p-"+strings.ReplaceAll(name, " ", "-"), "", l, nil, CardMeta{}, nil); err != ErrInvalidLimits {
			t.Errorf("%s: err %v, want ErrInvalidLimits", name, err)
		}
	}
	// The database refuses it too, whatever the application does.
	if _, err := e.pool.Exec(ctx, `INSERT INTO plans (name, limits) VALUES ('raw', '{"users": 5}')`); err == nil {
		t.Fatal("the plans CHECK must refuse a plan without all six keys")
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO plans (name, limits) VALUES ('frac',
		'{"users": 1.5, "resources": 1, "remote_networks": 1, "connectors": 1, "shields": 1, "client_devices": 1}')`); err == nil {
		t.Fatal("the plans CHECK must refuse a fractional limit")
	}
	if _, err := e.store.Create(ctx, "ok", "", full, nil, CardMeta{}, nil); err != nil {
		t.Fatalf("a full plan with an explicit null: %v", err)
	}
	if _, err := e.store.Create(ctx, "ok", "", full, nil, CardMeta{}, nil); err != ErrNameTaken {
		t.Fatalf("duplicate name: %v, want ErrNameTaken", err)
	}
}

// Only one default; switching is atomic, also under concurrency.
func TestPlansDB_SetDefault(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a, _ := e.store.Create(ctx, "A", "", limits(n(1), n(1), n(1), n(1), n(1), n(1)), nil, CardMeta{}, nil)
	b, _ := e.store.Create(ctx, "B", "", limits(n(1), n(1), n(1), n(1), n(1), n(1)), nil, CardMeta{}, nil)
	var prev, next Plan
	changed, err := e.store.SetDefault(ctx, a.ID, func(_ context.Context, _ pgx.Tx, p, nx Plan) error {
		prev, next = p, nx
		return nil
	})
	if err != nil || !changed || prev.ID != FreePlanID || next.ID != a.ID {
		t.Fatalf("set default: changed=%v err=%v prev=%s next=%s", changed, err, prev.ID, next.ID)
	}
	if changed, err := e.store.SetDefault(ctx, a.ID, nil); err != nil || changed {
		t.Fatalf("setting the current default again: changed=%v err=%v", changed, err)
	}
	// Many concurrent switches between three plans: the lock set must not
	// depend on the (changing) default, or callers deadlock.
	targets := []string{b.ID, FreePlanID, a.ID}
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		target := targets[i%len(targets)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.store.SetDefault(ctx, target, nil); err != nil {
				t.Errorf("concurrent set default: %v", err)
			}
		}()
	}
	wg.Wait()
	if d := e.defaults(t); len(d) != 1 {
		t.Fatalf("defaults after concurrent switches: %v, want exactly one", d)
	}
	if _, err := e.store.SetDefault(ctx, "00000000-0000-0000-0000-0000000000ff", nil); err != ErrNotFound {
		t.Fatalf("unknown plan: %v", err)
	}
}

// An assigned plan or the default can't be deleted (D-43).
func TestPlansDB_DeleteGuards(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := e.store.Delete(ctx, FreePlanID, nil); err != ErrIsDefault {
		t.Fatalf("delete default: %v, want ErrIsDefault", err)
	}
	ws := e.workspace(t, "acme")
	if _, err := e.store.Assign(ctx, ws, LegacyPlanID, nil); err != nil {
		t.Fatal(err)
	}
	if err := e.store.Delete(ctx, LegacyPlanID, nil); err != ErrInUse {
		t.Fatalf("delete assigned: %v, want ErrInUse", err)
	}
	spare, _ := e.store.Create(ctx, "Spare", "", limits(nil, nil, nil, nil, nil, nil), nil, CardMeta{}, nil)
	audited := false
	if err := e.store.Delete(ctx, spare.ID, func(context.Context, pgx.Tx, Plan) error { audited = true; return nil }); err != nil || !audited {
		t.Fatalf("delete unused plan: err=%v audited=%v", err, audited)
	}
	if _, err := Get(ctx, e.pool, spare.ID); err != ErrNotFound {
		t.Fatalf("deleted plan still readable: %v", err)
	}
}

// Assignment and plan edits take effect at once on EffectiveLimits (D-43);
// EffectiveLimits fails closed.
func TestPlansDB_AssignAndEffectiveLimits(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ws := e.workspace(t, "acme")
	l, err := EffectiveLimits(ctx, e.pool, ws)
	if err != nil || value(l, KeyUsers) != "5" {
		t.Fatalf("Free limits: %v %v", l, err)
	}
	var before, after Plan
	changed, err := e.store.Assign(ctx, ws, LegacyPlanID, func(_ context.Context, _ pgx.Tx, b, a Plan) error {
		before, after = b, a
		return nil
	})
	if err != nil || !changed || before.ID != FreePlanID || after.ID != LegacyPlanID {
		t.Fatalf("assign: changed=%v err=%v before=%s after=%s", changed, err, before.ID, after.ID)
	}
	if l, _ := EffectiveLimits(ctx, e.pool, ws); value(l, KeyUsers) != "unlimited" {
		t.Fatalf("Legacy limits must be unlimited at once: %v", l)
	}
	if changed, err := e.store.Assign(ctx, ws, LegacyPlanID, nil); err != nil || changed {
		t.Fatalf("assigning the same plan: changed=%v err=%v", changed, err)
	}
	// Editing a plan's limits changes every tenant on it at once.
	if _, err := e.store.Assign(ctx, ws, FreePlanID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Update(ctx, FreePlanID, Change{Limits: limits(n(7), n(50), n(3), n(10), n(10), n(25))}, CardMeta{Reason: "test"}, nil); err != nil {
		t.Fatal(err)
	}
	if l, _ := EffectiveLimits(ctx, e.pool, ws); value(l, KeyUsers) != "7" {
		t.Fatalf("edited Free limits must apply at once: %v", l)
	}
	if _, err := e.store.Assign(ctx, ws, "00000000-0000-0000-0000-0000000000ff", nil); err != ErrNotFound {
		t.Fatalf("unknown plan: %v", err)
	}
	if _, err := e.store.Assign(ctx, "00000000-0000-0000-0000-0000000000ee", LegacyPlanID, nil); err != ErrWorkspaceNotFound {
		t.Fatalf("unknown workspace: %v", err)
	}
	if _, err := EffectiveLimits(ctx, e.pool, "00000000-0000-0000-0000-0000000000ee"); err != ErrWorkspaceNotFound {
		t.Fatalf("EffectiveLimits for an unknown workspace must fail closed: %v", err)
	}
}

// A stored plan that somehow lacks a key fails closed instead of reading
// as unlimited.
func TestParseLimits_MissingKeyFailsClosed(t *testing.T) {
	if _, err := parseLimits([]byte(`{"users": 5}`)); err != ErrIncompleteLimits {
		t.Fatalf("missing keys: %v, want ErrIncompleteLimits", err)
	}
	l, err := parseLimits([]byte(`{"users": null, "resources": 0, "remote_networks": 1, "connectors": 1, "shields": 1, "client_devices": 1}`))
	if err != nil || l[KeyUsers] != nil || *l[KeyResources] != 0 {
		t.Fatalf("explicit null and zero: %v %v", l, err)
	}
}

// ── HTTP ─────────────────────────────────────────────────────────────────────

type resp struct {
	code int
	body string
}

func (e *env) do(t *testing.T, role, method, path string, body any) resp {
	t.Helper()
	raw := ""
	switch b := body.(type) {
	case nil:
	case string:
		raw = b
	default:
		j, _ := json.Marshal(b)
		raw = string(j)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(raw))
	req.RemoteAddr = "192.0.2.10:54321"
	actor := provider.Actor{UserID: e.superID, Email: "admin@provider.test", Role: provider.RoleSuperAdmin}
	if role == provider.RoleRelayOps {
		actor = provider.Actor{UserID: e.opsID, Email: "ops@provider.test", Role: provider.RoleRelayOps}
	}
	req = req.WithContext(provider.WithActor(req.Context(), actor))
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return resp{code: rec.Code, body: rec.Body.String()}
}

func errCode(t *testing.T, r resp) string {
	t.Helper()
	var v struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(r.body), &v)
	return v.Error
}

func (e *env) audits(t *testing.T, action string) []map[string]any {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT details FROM provider_audit_logs WHERE action = $1 ORDER BY created_at`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var d map[string]any
		_ = json.Unmarshal(raw, &d)
		out = append(out, d)
	}
	return out
}

var fullLimits = map[string]any{"users": 5, "resources": 50, "remote_networks": 3, "connectors": 10, "shields": 10, "client_devices": nil}

// Every plan write is super-admin only (D-06): relay-ops get 403 and nothing
// changes.
func TestPlansHTTP_SuperAdminOnly(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace(t, "acme")
	cases := []struct {
		method, path string
		body         any
	}{
		{"POST", "/provider/plans", map[string]any{"name": "Pro", "limits": fullLimits}},
		{"PATCH", "/provider/plans/" + FreePlanID, map[string]any{"name": "X", "reason": "r"}},
		{"DELETE", "/provider/plans/" + LegacyPlanID, map[string]any{"reason": "r"}},
		{"POST", "/provider/plans/" + LegacyPlanID + "/default", map[string]any{"reason": "r"}},
		{"PUT", "/provider/tenants/" + ws + "/plan", map[string]any{"plan_id": LegacyPlanID, "reason": "r"}},
	}
	for _, c := range cases {
		if r := e.do(t, provider.RoleRelayOps, c.method, c.path, c.body); r.code != http.StatusForbidden {
			t.Errorf("relay-ops %s %s = %d %s, want 403", c.method, c.path, r.code, r.body)
		}
	}
	if e.planOf(t, ws) != FreePlanID || len(e.defaults(t)) != 1 || e.defaults(t)[0] != FreePlanID {
		t.Fatal("a refused write changed state")
	}
	var count int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM provider_audit_logs`).Scan(&count)
	if count != 0 {
		t.Fatalf("refused writes wrote %d audit rows", count)
	}
}

func TestPlansHTTP_CreateUpdateAssignAudited(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace(t, "acme")

	r := e.do(t, provider.RoleSuperAdmin, "POST", "/provider/plans", map[string]any{"name": "Pro", "description": "Paid", "limits": fullLimits})
	if r.code != http.StatusCreated {
		t.Fatalf("create = %d %s", r.code, r.body)
	}
	var created PlanJSON
	_ = json.Unmarshal([]byte(r.body), &created)
	if created.IsDefault || created.Limits[KeyClientDevices] != nil || *created.Limits[KeyUsers] != 5 {
		t.Fatalf("created plan %+v", created)
	}
	if a := e.audits(t, provider.ActionPlanCreate); len(a) != 1 || a[0]["after"] == nil {
		t.Fatalf("create audit %v", a)
	}

	// Validation.
	bad := map[string]any{"users": 5}
	if r := e.do(t, provider.RoleSuperAdmin, "POST", "/provider/plans", map[string]any{"name": "Bad", "limits": bad}); errCode(t, r) != "invalid_limits" {
		t.Fatalf("missing keys = %d %s", r.code, r.body)
	}
	frac := `{"name":"Frac","limits":{"users":1.5,"resources":1,"remote_networks":1,"connectors":1,"shields":1,"client_devices":1}}`
	if r := e.do(t, provider.RoleSuperAdmin, "POST", "/provider/plans", frac); errCode(t, r) != "invalid_limits" {
		t.Fatalf("fraction = %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, "POST", "/provider/plans", map[string]any{"name": "Pro", "limits": fullLimits}); errCode(t, r) != "plan_name_taken" {
		t.Fatalf("duplicate = %d %s", r.code, r.body)
	}

	// Update needs a reason and records before/after.
	if r := e.do(t, provider.RoleSuperAdmin, "PATCH", "/provider/plans/"+created.ID, map[string]any{"name": "Pro 2"}); errCode(t, r) != "reason_required" {
		t.Fatalf("update without reason = %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, "PATCH", "/provider/plans/"+created.ID, map[string]any{"name": "Pro 2", "reason": "rename"}); r.code != http.StatusNoContent {
		t.Fatalf("update = %d %s", r.code, r.body)
	}
	if a := e.audits(t, provider.ActionPlanUpdate); len(a) != 1 || a[0]["reason"] != "rename" ||
		a[0]["before"].(map[string]any)["name"] != "Pro" || a[0]["after"].(map[string]any)["name"] != "Pro 2" {
		t.Fatalf("update audit %v", a)
	}

	// Assign: reason required, unknown plan, before/after.
	if r := e.do(t, provider.RoleSuperAdmin, "PUT", "/provider/tenants/"+ws+"/plan", map[string]any{"plan_id": created.ID}); errCode(t, r) != "reason_required" {
		t.Fatalf("assign without reason = %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, "PUT", "/provider/tenants/"+ws+"/plan", map[string]any{"plan_id": "00000000-0000-0000-0000-0000000000ff", "reason": "r"}); errCode(t, r) != "plan_not_found" {
		t.Fatalf("assign unknown plan = %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, "PUT", "/provider/tenants/"+ws+"/plan", map[string]any{"plan_id": created.ID, "reason": "upgrade"}); r.code != http.StatusNoContent {
		t.Fatalf("assign = %d %s", r.code, r.body)
	}
	a := e.audits(t, provider.ActionTenantPlanAssign)
	if len(a) != 1 || a[0]["reason"] != "upgrade" || a[0]["before"].(map[string]any)["plan_name"] != "Free" ||
		a[0]["after"].(map[string]any)["plan_name"] != "Pro 2" {
		t.Fatalf("assign audit %v", a)
	}
	if e.planOf(t, ws) != created.ID {
		t.Fatal("assignment not applied")
	}

	// Delete: refused while assigned or default; allowed once unused.
	if r := e.do(t, provider.RoleSuperAdmin, "DELETE", "/provider/plans/"+created.ID, map[string]any{"reason": "r"}); errCode(t, r) != "plan_in_use" {
		t.Fatalf("delete assigned = %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, "DELETE", "/provider/plans/"+FreePlanID, map[string]any{"reason": "r"}); errCode(t, r) != "plan_is_default" {
		t.Fatalf("delete default = %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, "PUT", "/provider/tenants/"+ws+"/plan", map[string]any{"plan_id": FreePlanID, "reason": "back"}); r.code != http.StatusNoContent {
		t.Fatalf("reassign = %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, "DELETE", "/provider/plans/"+created.ID, map[string]any{"reason": "retire"}); r.code != http.StatusNoContent {
		t.Fatalf("delete unused = %d %s", r.code, r.body)
	}
	if a := e.audits(t, provider.ActionPlanDelete); len(a) != 1 || a[0]["before"] == nil || a[0]["reason"] != "retire" {
		t.Fatalf("delete audit %v", a)
	}

	// Set default with a reason; audited with previous and new default.
	if r := e.do(t, provider.RoleSuperAdmin, "POST", "/provider/plans/"+LegacyPlanID+"/default", map[string]any{"reason": "promo"}); r.code != http.StatusNoContent {
		t.Fatalf("set default = %d %s", r.code, r.body)
	}
	if a := e.audits(t, provider.ActionPlanSetDefault); len(a) != 1 ||
		a[0]["before"].(map[string]any)["default_plan_name"] != "Free" || a[0]["after"].(map[string]any)["default_plan_name"] != "Legacy" {
		t.Fatalf("set default audit %v", a)
	}
	if r := e.do(t, provider.RoleSuperAdmin, "PATCH", "/provider/plans/not-a-uuid", map[string]any{"reason": "r"}); r.code != http.StatusNotFound {
		t.Fatalf("bad id = %d", r.code)
	}
}
