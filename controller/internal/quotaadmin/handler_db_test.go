package quotaadmin

import (
	"bytes"
	"context"
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

	"github.com/yourorg/ztna/controller/internal/provider"
)

// Sprint 23 S3 (D-47): PUT /provider/tenants/{id}/quotas/{key} over a real
// database: super-admin only, reason required, body validation, the stored
// override and its audit row with before/after.

func newPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("quotaadmin_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	u, _ := url.Parse(adminDSN)
	u.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName)
		admin.Close()
	})
	files, _ := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	sort.Strings(files)
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	return pool
}

func TestQuotaAdminDB_Set(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()
	var ws, opID, relayOpsID string
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ('acme', 'acme', 'active', 'acme.test') RETURNING id::text`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO provider_users (email, role) VALUES ('sa@p.test', 'super-admin') RETURNING id::text`).Scan(&opID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO provider_users (email, role) VALUES ('ro@p.test', 'relay-ops') RETURNING id::text`).Scan(&relayOpsID); err != nil {
		t.Fatal(err)
	}
	h := &Handler{Pool: pool, Authz: provider.NewAuthz(), ProviderStore: provider.NewStore(pool)}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /provider/tenants/{id}/quotas/{key}", h.Set)
	do := func(actor provider.Actor, id, key, body string) (int, string) {
		r := httptest.NewRequest(http.MethodPut, "/provider/tenants/"+id+"/quotas/"+key, bytes.NewBufferString(body))
		r.Header.Set("Content-Type", "application/json")
		r = r.WithContext(provider.WithActor(r.Context(), actor))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	sa := provider.Actor{UserID: opID, Email: "sa@p.test", Role: provider.RoleSuperAdmin}
	ro := provider.Actor{UserID: relayOpsID, Email: "ro@p.test", Role: provider.RoleRelayOps}

	for _, c := range []struct {
		actor      provider.Actor
		id, key    string
		body       string
		code       int
		errContain string
	}{
		{ro, ws, "users", `{"limit": 3, "reason": "x"}`, 403, "forbidden"},
		{sa, "nope", "users", `{"limit": 3, "reason": "x"}`, 404, "not_found"},
		{sa, "00000000-0000-0000-0000-00000000dead", "users", `{"limit": 3, "reason": "x"}`, 404, "not_found"},
		{sa, ws, "seats", `{"limit": 3, "reason": "x"}`, 404, "unknown_key"},
		{sa, ws, "users", `{"reason": "x"}`, 400, "limit_required"},
		{sa, ws, "users", `{"limit": -1, "reason": "x"}`, 400, "invalid_limit"},
		{sa, ws, "users", `{"limit": 1.5, "reason": "x"}`, 400, "invalid_limit"},
		{sa, ws, "users", `{"limit": "3", "reason": "x"}`, 400, "invalid_limit"},
		{sa, ws, "users", `{"limit": 3}`, 400, "reason_required"},
		{sa, ws, "users", `{"limit": 3, "reason": "  "}`, 400, "reason_required"},
		{sa, ws, "users", `{"limit": 3, "reason": "` + strings.Repeat("r", 501) + `"}`, 400, "reason_too_long"},
		{sa, ws, "users", `{"inherit": true, "limit": 3, "reason": "x"}`, 400, "invalid_request"},
		{sa, ws, "users", `{"limit": 3, "reason": "x", "extra": 1}`, 400, "invalid_request"},
	} {
		code, body := do(c.actor, c.id, c.key, c.body)
		if code != c.code || !strings.Contains(body, c.errContain) {
			t.Fatalf("%s %s %s: %d %s, want %d %s", c.actor.Role, c.key, c.body, code, body, c.code, c.errContain)
		}
	}
	var overrides, audits int
	pool.QueryRow(ctx, `SELECT count(*) FROM workspace_quota_overrides`).Scan(&overrides)
	pool.QueryRow(ctx, `SELECT count(*) FROM provider_audit_logs WHERE action = 'tenant.quota.set'`).Scan(&audits)
	if overrides != 0 || audits != 0 {
		t.Fatalf("refused requests wrote %d overrides, %d audit rows", overrides, audits)
	}

	if code, body := do(sa, ws, "connectors", `{"limit": 3, "reason": "pilot"}`); code != 204 {
		t.Fatalf("set: %d %s", code, body)
	}
	if code, body := do(sa, ws, "connectors", `{"limit": null, "reason": "trial"}`); code != 204 {
		t.Fatalf("unlimited: %d %s", code, body)
	}
	if code, body := do(sa, ws, "connectors", `{"inherit": true, "reason": "back"}`); code != 204 {
		t.Fatalf("inherit: %d %s", code, body)
	}
	rows, err := pool.Query(ctx, `SELECT details, provider_user_id::text, target_id FROM provider_audit_logs WHERE action = 'tenant.quota.set' ORDER BY created_at, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var d map[string]any
		var by, target string
		if err := rows.Scan(&d, &by, &target); err != nil {
			t.Fatal(err)
		}
		if by != opID || target != ws || d["key"] != "connectors" {
			t.Fatalf("audit row by %s target %s details %v", by, target, d)
		}
		b, a := d["before"].(map[string]any), d["after"].(map[string]any)
		got = append(got, fmt.Sprintf("%v:%v/%v->%v/%v", d["reason"], b["source"], b["limit"], a["source"], a["limit"]))
	}
	want := []string{"pilot:plan/10->override/3", "trial:override/3->override/<nil>", "back:override/<nil>->plan/10"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("audit before/after %v, want %v", got, want)
	}
}
