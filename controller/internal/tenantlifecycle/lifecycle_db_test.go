package tenantlifecycle

import (
	"bytes"
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
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/provider"
	"github.com/yourorg/ztna/controller/internal/tenantstate"
)

// Sprint 23 S1 (D-45) through the provider API over a real database:
// authorization, reason and typed slug, the state written (status stays
// 'active', D-34), sessions revoked (generation bump + invalidation after
// commit), the ACL push and relay publish after commit, the audit row with
// before/after, and resume.

func newTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("tenantlifecycle_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}
	parsed, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
		admin.Close()
	})
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list migrations: %v (%d files)", err, len(files))
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
	return pool
}

type fakeSessions struct {
	mu    sync.Mutex
	users []string
}

func (f *fakeSessions) InvalidateUserSessions(_ context.Context, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users = append(f.users, userID)
	return nil
}

type fakePolicy struct {
	mu         sync.Mutex
	workspaces []string
}

func (f *fakePolicy) NotifyPolicyChange(_ context.Context, ws string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.workspaces = append(f.workspaces, ws)
	return nil
}

type env struct {
	pool      *pgxpool.Pool
	h         *Handler
	sessions  *fakeSessions
	policy    *fakePolicy
	publishes int
	operator  map[string]string // role → provider user id
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := newTestDB(t)
	e := &env{pool: pool, sessions: &fakeSessions{}, policy: &fakePolicy{}, operator: map[string]string{}}
	e.h = &Handler{
		Store:         NewStore(pool),
		Authz:         provider.NewAuthz(),
		ProviderStore: provider.NewStore(pool),
		Sessions:      e.sessions,
		Policy:        e.policy,
		PublishRelayLists: func(context.Context) error {
			e.publishes++
			return nil
		},
	}
	for _, role := range []string{provider.RoleSuperAdmin, provider.RoleRelayOps} {
		var id string
		if err := pool.QueryRow(context.Background(),
			`INSERT INTO provider_users (email, role) VALUES ($1, $2) RETURNING id::text`,
			role+"@provider.test", role).Scan(&id); err != nil {
			t.Fatal(err)
		}
		e.operator[role] = id
	}
	return e
}

func (e *env) scalar(t *testing.T, dst any, sql string, args ...any) {
	t.Helper()
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(dst); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
}

// tenant seeds a workspace with two users; returns its id.
func (e *env) tenant(t *testing.T, slug string) (string, []string) {
	t.Helper()
	var id string
	e.scalar(t, &id, `INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ($1, $1, 'active', $1 || '.zecurity.test') RETURNING id::text`, slug)
	var users []string
	for i := 0; i < 2; i++ {
		var u string
		e.scalar(t, &u, `INSERT INTO users (tenant_id, email, provider, provider_sub) VALUES ($1, $2, 'google', $2) RETURNING id::text`,
			id, fmt.Sprintf("u%d@%s.test", i, slug))
		users = append(users, u)
	}
	sort.Strings(users)
	return id, users
}

type resp struct {
	code int
	body string
}

func (e *env) do(t *testing.T, role, path string, body any) resp {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(provider.WithActor(r.Context(), provider.Actor{UserID: e.operator[role], Email: role + "@provider.test", Role: role}))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /provider/tenants/{id}/suspend", e.h.Suspend)
	mux.HandleFunc("POST /provider/tenants/{id}/resume", e.h.Resume)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return resp{w.Code, w.Body.String()}
}

func suspendPath(id string) string { return "/provider/tenants/" + id + "/suspend" }
func resumePath(id string) string  { return "/provider/tenants/" + id + "/resume" }

func (e *env) auditRows(t *testing.T, action string) []map[string]any {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT details FROM provider_audit_logs WHERE action = $1 ORDER BY created_at`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var d map[string]any
		if err := rows.Scan(&d); err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func TestLifecycleDB_SuspendAndResume(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	ws, users := e.tenant(t, "acme")
	other, _ := e.tenant(t, "other")
	var gen0 int
	e.scalar(t, &gen0, `SELECT identity_generation FROM users WHERE id = $1`, users[0])

	// Authorization (D-06): relay-ops can't suspend or resume.
	for _, p := range []string{suspendPath(ws), resumePath(ws)} {
		if r := e.do(t, provider.RoleRelayOps, p, map[string]any{"reason": "x", "confirm_slug": "acme"}); r.code != http.StatusForbidden {
			t.Fatalf("relay-ops %s: %d %s, want 403", p, r.code, r.body)
		}
	}
	// Reason and typed slug (D-17 extended), checked by the server.
	for _, c := range []struct {
		body map[string]any
		code int
		err  string
	}{
		{map[string]any{"confirm_slug": "acme"}, http.StatusBadRequest, "reason_required"},
		{map[string]any{"reason": "   ", "confirm_slug": "acme"}, http.StatusBadRequest, "reason_required"},
		{map[string]any{"reason": "non-payment"}, http.StatusBadRequest, "confirmation_mismatch"},
		{map[string]any{"reason": "non-payment", "confirm_slug": "Acme"}, http.StatusBadRequest, "confirmation_mismatch"},
		{map[string]any{"reason": "non-payment", "confirm_slug": "other"}, http.StatusBadRequest, "confirmation_mismatch"},
	} {
		r := e.do(t, provider.RoleSuperAdmin, suspendPath(ws), c.body)
		if r.code != c.code || !bytes.Contains([]byte(r.body), []byte(c.err)) {
			t.Fatalf("suspend %v: %d %s, want %d %s", c.body, r.code, r.body, c.code, c.err)
		}
	}
	if r := e.do(t, provider.RoleSuperAdmin, suspendPath("00000000-0000-0000-0000-00000000dead"), map[string]any{"reason": "x", "confirm_slug": "x"}); r.code != http.StatusNotFound {
		t.Fatalf("unknown tenant: %d %s", r.code, r.body)
	}
	if err := tenantstate.Check(ctx, e.pool, ws); err != nil {
		t.Fatalf("refused requests changed the state: %v", err)
	}
	if n := len(e.auditRows(t, provider.ActionTenantSuspend)); n != 0 {
		t.Fatalf("refused requests wrote %d audit rows", n)
	}

	// Suspend.
	if r := e.do(t, provider.RoleSuperAdmin, suspendPath(ws), map[string]any{"reason": "non-payment", "confirm_slug": "acme"}); r.code != http.StatusNoContent {
		t.Fatalf("suspend: %d %s", r.code, r.body)
	}
	if err := tenantstate.Check(ctx, e.pool, ws); !errors.Is(err, tenantstate.ErrSuspended) {
		t.Fatalf("after suspend Check = %v, want ErrSuspended", err)
	}
	if err := tenantstate.Check(ctx, e.pool, other); err != nil {
		t.Fatalf("another tenant affected: %v", err)
	}
	// D-34: status stays 'active'.
	var status, by, reason string
	e.scalar(t, &status, `SELECT status FROM workspaces WHERE id = $1`, ws)
	e.scalar(t, &by, `SELECT suspended_by FROM workspaces WHERE id = $1`, ws)
	e.scalar(t, &reason, `SELECT suspension_reason FROM workspaces WHERE id = $1`, ws)
	if status != "active" || by != "super-admin@provider.test" || reason != "non-payment" {
		t.Fatalf("status=%q by=%q reason=%q", status, by, reason)
	}
	// Sessions: every user's generation is bumped (in the transaction), and
	// their sessions are ended after commit.
	for _, u := range users {
		var gen int
		e.scalar(t, &gen, `SELECT identity_generation FROM users WHERE id = $1`, u)
		if gen != gen0+1 {
			t.Fatalf("user %s generation %d, want %d", u, gen, gen0+1)
		}
	}
	got := append([]string{}, e.sessions.users...)
	sort.Strings(got)
	if fmt.Sprint(got) != fmt.Sprint(users) {
		t.Fatalf("sessions ended for %v, want %v", got, users)
	}
	if fmt.Sprint(e.policy.workspaces) != fmt.Sprint([]string{ws}) || e.publishes != 1 {
		t.Fatalf("after suspend: ACL pushes %v, relay publishes %d", e.policy.workspaces, e.publishes)
	}
	rows := e.auditRows(t, provider.ActionTenantSuspend)
	if len(rows) != 1 {
		t.Fatalf("suspend audit rows %d, want 1", len(rows))
	}
	d := rows[0]
	before, _ := d["before"].(map[string]any)
	after, _ := d["after"].(map[string]any)
	if d["reason"] != "non-payment" || before["suspended"] != false || after["suspended"] != true || after["suspension_reason"] != "non-payment" {
		t.Fatalf("suspend audit details %v", d)
	}

	// Twice → already_suspended; nothing changes.
	if r := e.do(t, provider.RoleSuperAdmin, suspendPath(ws), map[string]any{"reason": "again", "confirm_slug": "acme"}); r.code != http.StatusConflict || !bytes.Contains([]byte(r.body), []byte("already_suspended")) {
		t.Fatalf("second suspend: %d %s", r.code, r.body)
	}

	// Resume: reason required; then clears the state.
	if r := e.do(t, provider.RoleSuperAdmin, resumePath(ws), map[string]any{}); r.code != http.StatusBadRequest || !bytes.Contains([]byte(r.body), []byte("reason_required")) {
		t.Fatalf("resume without reason: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, resumePath(ws), map[string]any{"reason": "paid"}); r.code != http.StatusNoContent {
		t.Fatalf("resume: %d %s", r.code, r.body)
	}
	if err := tenantstate.Check(ctx, e.pool, ws); err != nil {
		t.Fatalf("after resume Check = %v", err)
	}
	if fmt.Sprint(e.policy.workspaces) != fmt.Sprint([]string{ws, ws}) || e.publishes != 2 {
		t.Fatalf("after resume: ACL pushes %v, relay publishes %d", e.policy.workspaces, e.publishes)
	}
	rows = e.auditRows(t, provider.ActionTenantResume)
	if len(rows) != 1 {
		t.Fatalf("resume audit rows %d, want 1", len(rows))
	}
	before, _ = rows[0]["before"].(map[string]any)
	after, _ = rows[0]["after"].(map[string]any)
	if rows[0]["reason"] != "paid" || before["suspended"] != true || after["suspended"] != false {
		t.Fatalf("resume audit details %v", rows[0])
	}
	if r := e.do(t, provider.RoleSuperAdmin, resumePath(ws), map[string]any{"reason": "again"}); r.code != http.StatusConflict || !bytes.Contains([]byte(r.body), []byte("not_suspended")) {
		t.Fatalf("second resume: %d %s", r.code, r.body)
	}
}

// Fail closed: the eligibility check refuses on a lookup error and on an
// unknown workspace.
func TestEligibilityDB_FailClosed(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if err := tenantstate.Check(ctx, e.pool, "00000000-0000-0000-0000-00000000dead"); !errors.Is(err, tenantstate.ErrNotFound) {
		t.Fatalf("unknown workspace: %v, want ErrNotFound", err)
	}
	if err := tenantstate.Check(ctx, e.pool, ""); err == nil {
		t.Fatal("empty workspace id allowed")
	}
	closed := newTestDB(t)
	closed.Close()
	if err := tenantstate.Check(ctx, closed, "00000000-0000-0000-0000-000000000001"); err == nil || tenantstate.IsSuspended(err) {
		t.Fatalf("lookup error: %v, want a non-nil, non-suspended error (callers deny)", err)
	}
}
