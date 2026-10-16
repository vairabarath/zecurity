package resolvers

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/outbox"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// Certificate audit bugs 4/5: the GraphQL paths that disable or delete an
// identity connection must commit the change together with durable policy
// invalidation work — not notify best-effort after commit.

type idpPropagationFixture struct {
	pool   *pgxpool.Pool
	ctx    context.Context
	mr     *mutationResolver
	ws     string
	connID string
	scim   string
}

func newIdpPropagationFixture(t *testing.T) *idpPropagationFixture {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := "resolvers_idpprop_" + strconv.Itoa(os.Getpid()) + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(adminPool.Close)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() { _, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName) })
	parsed, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	parsed.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	files, _ := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}

	f := &idpPropagationFixture{pool: pool, ctx: ctx}
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ('idpprop', 'idpprop', 'active', 'idpprop.test') RETURNING id::text`).Scan(&f.ws); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	store := idp.NewStore(pool, testSecretEnc{})
	conn, err := store.CreateWorkspaceConnection(ctx, f.ws, idp.CreateInput{
		Provider: "okta", DisplayName: "Acme Okta", Issuer: "https://acme.okta.com",
		ClientID: "client-1", ClientSecret: "secret-1",
	})
	if err != nil {
		t.Fatalf("connection: %v", err)
	}
	f.connID = conn.ID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (tenant_id, email, provider, provider_sub, role, status, provisioning_owner)
		 VALUES ($1, 'scim-user@acme.test', 'okta', 'sub-1', 'member', 'active', 'scim') RETURNING id::text`,
		f.ws).Scan(&f.scim); err != nil {
		t.Fatalf("user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO external_identities (tenant_id, user_id, connection_id, issuer, subject)
		 VALUES ($1, $2, $3, 'https://acme.okta.com', 'sub-1')`, f.ws, f.scim, f.connID); err != nil {
		t.Fatalf("link user: %v", err)
	}
	f.mr = &mutationResolver{&Resolver{
		IdpStore:       store,
		Pool:           pool,
		Outbox:         outbox.NewOutbox(pool),
		PolicyNotifier: policy.NewNotifier(policy.NewSnapshotCache()),
	}}
	f.ctx = tenant.Set(ctx, tenant.TenantContext{TenantID: f.ws, UserID: f.scim, Role: "admin", Email: "admin@acme.test"})
	return f
}

func (f *idpPropagationFixture) scalar(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var v string
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return v
}

func (f *idpPropagationFixture) invalidations(t *testing.T) string {
	return f.scalar(t, `SELECT count(*)::text FROM outbox_events WHERE event_type = $1 AND workspace_id = $2`,
		policy.EventPolicyInvalidationRequested, f.ws)
}

func (f *idpPropagationFixture) failOutboxInserts(t *testing.T) {
	t.Helper()
	for _, sql := range []string{
		`CREATE FUNCTION fail_outbox() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'outbox unavailable'; END; $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER fail_outbox BEFORE INSERT ON outbox_events FOR EACH ROW EXECUTE FUNCTION fail_outbox()`,
	} {
		if _, err := f.pool.Exec(context.Background(), sql); err != nil {
			t.Fatalf("install failure trigger: %v", err)
		}
	}
}

func TestSetIdpConnectionStatus_DisableRecordsDurableInvalidation(t *testing.T) {
	t.Skip("bugs 4/5: demonstrates the gap; un-skip with the fix")
	f := newIdpPropagationFixture(t)
	if _, err := f.mr.SetIdpConnectionStatus(f.ctx, f.connID, "disabled"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if got := f.scalar(t, `SELECT status FROM users WHERE id = $1`, f.scim); got != "suspended" {
		t.Fatalf("SCIM user status = %s", got)
	}
	if got := f.invalidations(t); got != "1" {
		t.Fatalf("policy invalidation events = %s, want 1", got)
	}
}

func TestSetIdpConnectionStatus_DisableFailsClosedWithoutInvalidation(t *testing.T) {
	t.Skip("bugs 4/5: demonstrates the gap; un-skip with the fix")
	f := newIdpPropagationFixture(t)
	f.failOutboxInserts(t)
	if _, err := f.mr.SetIdpConnectionStatus(f.ctx, f.connID, "disabled"); err == nil {
		t.Fatal("disable succeeded although its invalidation could not be recorded")
	}
	if got := f.scalar(t, `SELECT status FROM identity_connections WHERE id = $1`, f.connID); got != "active" {
		t.Fatalf("connection status = %s", got)
	}
	if got := f.scalar(t, `SELECT status FROM users WHERE id = $1`, f.scim); got != "active" {
		t.Fatalf("SCIM user status = %s", got)
	}
}

func TestDeleteIdpConnection_RecordsDurableInvalidation(t *testing.T) {
	t.Skip("bugs 4/5: demonstrates the gap; un-skip with the fix")
	f := newIdpPropagationFixture(t)
	if _, err := f.mr.DeleteIdpConnection(f.ctx, f.connID, true); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := f.invalidations(t); got != "1" {
		t.Fatalf("policy invalidation events = %s, want 1", got)
	}
}

func TestDeleteIdpConnection_FailsClosedWithoutInvalidation(t *testing.T) {
	t.Skip("bugs 4/5: demonstrates the gap; un-skip with the fix")
	f := newIdpPropagationFixture(t)
	f.failOutboxInserts(t)
	if _, err := f.mr.DeleteIdpConnection(f.ctx, f.connID, true); err == nil {
		t.Fatal("delete succeeded although its invalidation could not be recorded")
	}
	if got := f.scalar(t, `SELECT status FROM identity_connections WHERE id = $1`, f.connID); got != "active" {
		t.Fatalf("connection status = %s", got)
	}
}
