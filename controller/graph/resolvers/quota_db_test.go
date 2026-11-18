package resolvers

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/db"
	"github.com/yourorg/ztna/controller/internal/invitation"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/quota"
	"github.com/yourorg/ztna/controller/internal/rbac"
	"github.com/yourorg/ztna/controller/internal/resource"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// Sprint 23 S3 (D-47): every GraphQL creation path refuses at the tenant's
// limit with quota_exceeded {key, limit, used}, allows below it, and creates
// nothing when it refuses. The presenter shows the refusal as
// QUOTA_EXCEEDED with the key, limit and usage.

type quotaFixture struct {
	pool *pgxpool.Pool
	ctx  context.Context
	mr   *mutationResolver
	ws   string
	rn   string
	plan string
}

func newQuotaFixture(t *testing.T) *quotaFixture {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	pool := newMigratedTestPool(t, adminDSN, "resolvers_quota")
	ctx := context.Background()
	f := &quotaFixture{pool: pool, ctx: ctx}
	// Every key at 1 except remote networks (2: one exists already).
	f.plan = f.scalar(t, `INSERT INTO plans (name, limits) VALUES ('tiny',
		'{"users": 1, "resources": 1, "remote_networks": 2, "connectors": 1, "shields": 1, "client_devices": 1}') RETURNING id::text`)
	f.ws = f.scalar(t, `INSERT INTO workspaces (slug, name, status, trust_domain, plan_id)
		VALUES ('quota', 'quota', 'active', 'quota.test', $1) RETURNING id::text`, f.plan)
	f.rn = f.scalar(t, `INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'rn', 'office') RETURNING id::text`, f.ws)
	user := f.scalar(t, `INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		VALUES ($1, 'admin@quota.test', 'google', 'sub-admin', 'admin', 'active') RETURNING id::text`, f.ws)
	f.mr = &mutationResolver{&Resolver{
		TenantDB:          db.NewTenantDB(pool),
		Pool:              pool,
		PolicyNotifier:    policy.NewNotifier(policy.NewSnapshotCache()),
		ResourceCfg:       resource.Config{DB: pool},
		InvitationStore:   invitation.NewStore(pool),
		InvitationEmailer: invitation.NewEmailer("", "", "", "", "http://test"),
	}}
	f.ctx = rbac.WithRole(tenant.Set(ctx, tenant.TenantContext{TenantID: f.ws, UserID: user, Role: "admin", Email: "admin@quota.test"}), rbac.RoleAdmin)
	return f
}

func (f *quotaFixture) scalar(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var v string
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return v
}

func (f *quotaFixture) count(t *testing.T, sql string) string {
	t.Helper()
	return f.scalar(t, sql, f.ws)
}

// refused asserts err is quota_exceeded for key and that the presenter
// shows it with the QUOTA_EXCEEDED extension.
func (f *quotaFixture) refused(t *testing.T, err error, key string, limit, used int64) {
	t.Helper()
	qe, ok := quota.AsExceeded(err)
	if !ok {
		t.Fatalf("want quota_exceeded for %s, got %v", key, err)
	}
	if qe.Key != key || qe.Limit != limit || qe.Used != used {
		t.Fatalf("quota_exceeded = %+v, want {%s %d %d}", qe, key, limit, used)
	}
	gerr := ErrorPresenter(context.Background(), err)
	if gerr.Extensions["code"] != quota.Code || gerr.Extensions["key"] != key || gerr.Extensions["limit"] != limit || gerr.Extensions["used"] != used {
		t.Fatalf("presented as %v %v", gerr.Message, gerr.Extensions)
	}
	if gerr.Message == "an unexpected error occurred" {
		t.Fatal("a quota refusal must not be masked as internal")
	}
}

func TestQuota_RemoteNetworks(t *testing.T) {
	f := newQuotaFixture(t)
	if _, err := f.mr.CreateRemoteNetwork(f.ctx, "second", graph.NetworkLocationOffice); err != nil {
		t.Fatalf("below the limit: %v", err)
	}
	_, err := f.mr.CreateRemoteNetwork(f.ctx, "third", graph.NetworkLocationOffice)
	f.refused(t, err, quota.KeyRemoteNetworks, 2, 2)
	if n := f.count(t, `SELECT count(*)::text FROM remote_networks WHERE tenant_id = $1`); n != "2" {
		t.Fatalf("networks = %s, want 2 (nothing created when refused)", n)
	}
}

func TestQuota_ConnectorsAndShields(t *testing.T) {
	f := newQuotaFixture(t)
	tok, err := f.mr.GenerateConnectorToken(f.ctx, f.rn, "c1")
	if err != nil {
		t.Fatalf("first connector: %v", err)
	}
	_, err = f.mr.GenerateConnectorToken(f.ctx, f.rn, "c2")
	f.refused(t, err, quota.KeyConnectors, 1, 1) // a pending connector counts
	if n := f.count(t, `SELECT count(*)::text FROM connectors WHERE tenant_id = $1`); n != "1" {
		t.Fatalf("connectors = %s, want 1", n)
	}

	// Shields need an active connector in the network.
	if _, err := f.pool.Exec(f.ctx, `UPDATE connectors SET status = 'active' WHERE id = $1`, tok.ConnectorID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mr.GenerateShieldToken(f.ctx, f.rn, "s1"); err != nil {
		t.Fatalf("first shield: %v", err)
	}
	_, err = f.mr.GenerateShieldToken(f.ctx, f.rn, "s2")
	f.refused(t, err, quota.KeyShields, 1, 1)
	if n := f.count(t, `SELECT count(*)::text FROM shields WHERE tenant_id = $1`); n != "1" {
		t.Fatalf("shields = %s, want 1", n)
	}
	// A revoked connector frees its slot.
	if _, err := f.pool.Exec(f.ctx, `UPDATE connectors SET status = 'revoked', revoked_at = NOW() WHERE id = $1`, tok.ConnectorID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mr.GenerateConnectorToken(f.ctx, f.rn, "c3"); err != nil {
		t.Fatalf("after revoking: %v", err)
	}
}

func TestQuota_Resources(t *testing.T) {
	f := newQuotaFixture(t)
	in := func(name, host string) graph.CreateResourceInput {
		return graph.CreateResourceInput{RemoteNetworkID: f.rn, Name: name, Host: host, Protocol: "tcp", PortFrom: 22, PortTo: 22}
	}
	r1, err := f.mr.CreateResource(f.ctx, in("r1", "10.0.0.1"))
	if err != nil {
		t.Fatalf("first resource: %v", err)
	}
	_, err = f.mr.CreateResource(f.ctx, in("r2", "10.0.0.2"))
	f.refused(t, err, quota.KeyResources, 1, 1) // an unprotected resource counts
	if n := f.count(t, `SELECT count(*)::text FROM resources WHERE tenant_id = $1`); n != "1" {
		t.Fatalf("resources = %s, want 1", n)
	}
	// A resource being deleted frees its slot.
	if _, err := f.pool.Exec(f.ctx, `UPDATE resources SET status = 'deleting' WHERE id = $1`, r1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mr.CreateResource(f.ctx, in("r3", "10.0.0.3")); err != nil {
		t.Fatalf("after deleting: %v", err)
	}
}

func TestQuota_Invitations(t *testing.T) {
	f := newQuotaFixture(t)
	// The admin is the one user the plan allows.
	_, err := f.mr.CreateInvitation(f.ctx, "new@quota.test")
	f.refused(t, err, quota.KeyUsers, 1, 1)
	if n := f.count(t, `SELECT count(*)::text FROM invitations WHERE workspace_id = $1`); n != "0" {
		t.Fatalf("invitations = %s, want 0", n)
	}
	// An override raises the limit; a pending invitation then takes a slot.
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO workspace_quota_overrides (workspace_id, key, limit_value, reason, set_by)
		VALUES ($1, 'users', 2, 'r', 'op')`, f.ws); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mr.CreateInvitation(f.ctx, "new@quota.test"); err != nil {
		t.Fatalf("below the raised limit: %v", err)
	}
	_, err = f.mr.CreateInvitation(f.ctx, "other@quota.test")
	f.refused(t, err, quota.KeyUsers, 2, 2)
	// Re-inviting the same identity takes no new slot.
	if _, err := f.mr.CreateInvitation(f.ctx, "NEW@quota.test"); err != nil {
		t.Fatalf("re-invite: %v", err)
	}
}

// The admin console's read of its own limits and usage.
func TestQuota_WorkspaceQuotasQuery(t *testing.T) {
	f := newQuotaFixture(t)
	qr := &queryResolver{f.mr.Resolver}
	got, err := qr.WorkspaceQuotas(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(quota.Keys) {
		t.Fatalf("%d quotas, want %d", len(got), len(quota.Keys))
	}
	for i, q := range got {
		if q.Key != quota.Keys[i] || q.Limit == nil {
			t.Fatalf("quota %d = %+v", i, q)
		}
	}
	if got[0].Used != 1 || *got[0].Limit != 1 || got[2].Used != 1 || *got[2].Limit != 2 {
		t.Fatalf("users %+v, remote networks %+v", got[0], got[2])
	}
}
