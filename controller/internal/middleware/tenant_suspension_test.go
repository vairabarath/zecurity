package middleware

import (
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

	"github.com/yourorg/ztna/controller/internal/tenant"
	"github.com/yourorg/ztna/controller/internal/tenantstate"
)

func newSuspensionTestDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("middleware_susp_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	parsed, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName)
		admin.Close()
	})
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list migrations: %v", err)
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
	return pool, parsed.String()
}

// Sprint 23 S1 (D-45): every tenant console request (GraphQL and REST,
// mutations and reads) is refused with TENANT_SUSPENDED while the tenant is
// suspended, even with an access token issued before; a lookup error refuses
// (fail closed); the protected WorkspaceGuard is unchanged and still passes
// (status stays 'active', D-34).
func TestTenantEligibilityGuard(t *testing.T) {
	pool, dsn := newSuspensionTestDB(t)
	ctx := context.Background()
	var ws string
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ('g', 'g', 'active', 'g.test') RETURNING id::text`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	serve := func(p *pgxpool.Pool, tenantID string) (int, string, bool) {
		t.Helper()
		reached := false
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true })
		req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"mutation { x }"}`))
		req = req.WithContext(tenant.Set(req.Context(), tenant.TenantContext{TenantID: tenantID, UserID: "u", Role: "admin"}))
		rec := httptest.NewRecorder()
		WorkspaceGuard(p)(TenantEligibilityGuard(p)(next)).ServeHTTP(rec, req)
		return rec.Code, rec.Body.String(), reached
	}
	if code, body, reached := serve(pool, ws); code != http.StatusOK || !reached {
		t.Fatalf("active tenant: %d %s reached=%v", code, body, reached)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET suspended_at = NOW(), suspended_by = 'op@x', suspension_reason = 'test' WHERE id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	code, body, reached := serve(pool, ws)
	if code != http.StatusForbidden || reached || !strings.Contains(body, tenantstate.Code) || !strings.Contains(body, tenantstate.Message) {
		t.Fatalf("suspended tenant: %d %s reached=%v, want 403 %s", code, body, reached, tenantstate.Code)
	}
	if strings.Contains(body, "test") {
		t.Fatalf("refusal leaks the provider's reason: %s", body)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET suspended_at = NULL, suspended_by = NULL, suspension_reason = NULL WHERE id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if code, _, reached := serve(pool, ws); code != http.StatusOK || !reached {
		t.Fatalf("resumed tenant: %d reached=%v", code, reached)
	}

	// Fail closed on a lookup error (only the eligibility guard: the
	// WorkspaceGuard would refuse first with its own message).
	closed, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	reachedClosed := false
	req := httptest.NewRequest(http.MethodPost, "/graphql", nil)
	req = req.WithContext(tenant.Set(req.Context(), tenant.TenantContext{TenantID: ws}))
	rec := httptest.NewRecorder()
	TenantEligibilityGuard(closed)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reachedClosed = true })).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || reachedClosed {
		t.Fatalf("lookup error: %d reached=%v, want 403", rec.Code, reachedClosed)
	}
}
