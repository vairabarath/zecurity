package resolvers

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/connector"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/resource"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// aclCoherenceFixture wires a mutationResolver against a real Postgres with a
// recording push hook. NotifyPolicyChange fires the hook synchronously, so
// fires.Load() tells us whether a resource mutation invalidated the per-workspace
// ACL snapshot. Set RESOURCE_TEST_DATABASE_URL to run; skipped otherwise.
//
// Sprint 20 Fix 02 (F35): the fixture creates its own migrated throwaway
// database (RESOURCE_TEST_DATABASE_URL is the admin DSN) and provisions a
// workspace, remote network, connector and active shield (lan_ip =
// aclFixtureShieldLAN) in it. It used to borrow the workspace
// of an existing shield named by RESOURCE_TEST_SHIELD_ID, so the tests ran inside
// whatever that workspace held (real devices changed posture decisions and added
// policy notifies; a revoked shield changed unprotect's path) and skipped in CI,
// which never set that variable.
type aclCoherenceFixture struct {
	mr        *mutationResolver
	notifier  *policy.Notifier
	pool      *pgxpool.Pool
	tenantID  string
	networkID string
	shieldID  string
	fires     *atomic.Int32
	ctx       context.Context
}

// aclFixtureShieldLAN is the fixture shield's LAN IP: protectResource only
// protects a resource whose host is an active shield's lan_ip.
const aclFixtureShieldLAN = "10.0.0.95"

func newACLCoherenceFixture(t *testing.T) *aclCoherenceFixture {
	t.Helper()
	dsn := os.Getenv("RESOURCE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("RESOURCE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	// Its own migrated throwaway database: CI's Postgres starts empty.
	pool := newMigratedTestPool(t, dsn, "resolvers_aclcoh")

	slug := fmt.Sprintf("aclcoh-%d", time.Now().UnixNano())
	var tenantID, networkID, connectorID, shieldID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $1, 'active', $2) RETURNING id`,
		slug, "ws-"+slug+".zecurity.in",
	).Scan(&tenantID); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id = $1`, tenantID)
	})
	if err := pool.QueryRow(ctx,
		`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'net', 'other') RETURNING id`,
		tenantID,
	).Scan(&networkID); err != nil {
		t.Fatalf("insert remote_network: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO connectors (tenant_id, remote_network_id, name, status)
		 VALUES ($1, $2, 'conn', 'active') RETURNING id`,
		tenantID, networkID,
	).Scan(&connectorID); err != nil {
		t.Fatalf("insert connector: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status, lan_ip)
		 VALUES ($1, $2, $3, 'shield', 'active', $4) RETURNING id`,
		tenantID, networkID, connectorID, aclFixtureShieldLAN,
	).Scan(&shieldID); err != nil {
		t.Fatalf("insert shield: %v", err)
	}

	fires := new(atomic.Int32)
	n := policy.NewNotifier(policy.NewSnapshotCache())
	n.RegisterPushHook(func(string) { fires.Add(1) })

	r := &Resolver{
		Pool:              pool,
		ResourceCfg:       resource.Config{DB: pool},
		PolicyStore:       policy.NewStore(pool),
		PolicyNotifier:    n,
		ConnectorRegistry: connector.NewConnectorRegistry(),
	}
	tctx := tenant.Set(ctx, tenant.TenantContext{
		TenantID: tenantID,
		UserID:   "00000000-0000-0000-0000-000000000000",
		Role:     "admin",
		Email:    "acl-coherence-test@example.com",
	})

	return &aclCoherenceFixture{
		mr: &mutationResolver{r}, notifier: n, pool: pool,
		tenantID: tenantID, networkID: networkID, shieldID: shieldID,
		fires: fires, ctx: tctx,
	}
}

// seedResource inserts a throwaway resource bound to the fixture's shield and
// registers cleanup. host must be unique enough to avoid clashes within a run.
func (f *aclCoherenceFixture) seedResource(t *testing.T, host, status string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO resources (tenant_id, remote_network_id, shield_id, name, host, protocol, port_from, port_to, status, pending_action)
		 VALUES ($1,$2,$3,'acl-coherence-test',$4,'tcp',8080,8080,$5,'apply')
		 RETURNING id`,
		f.tenantID, f.networkID, f.shieldID, host, status,
	).Scan(&id); err != nil {
		t.Fatalf("seed resource: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM resources WHERE id = $1`, id)
	})
	return id
}

// TestUpdateResource_InvalidatesWhenACLFieldChanges: changing protocol (a
// compiler-visible field) must bump the version and fire the push hook.
func TestUpdateResource_InvalidatesWhenACLFieldChanges(t *testing.T) {
	f := newACLCoherenceFixture(t)
	id := f.seedResource(t, "10.0.0.91", "unprotected")
	before := f.notifier.Version(f.tenantID)

	proto := "udp"
	if _, err := f.mr.UpdateResource(f.ctx, id, graph.UpdateResourceInput{Protocol: &proto}); err != nil {
		t.Fatalf("UpdateResource: %v", err)
	}

	if got := f.fires.Load(); got != 1 {
		t.Fatalf("push hook fired %d times, want 1", got)
	}
	if got := f.notifier.Version(f.tenantID); got <= before {
		t.Fatalf("version not bumped: %d -> %d", before, got)
	}
}

// TestUpdateResource_NoInvalidateForIrrelevantField: changing only fields the
// compiler ignores (description, port_to) must NOT invalidate.
func TestUpdateResource_NoInvalidateForIrrelevantField(t *testing.T) {
	f := newACLCoherenceFixture(t)
	id := f.seedResource(t, "10.0.0.92", "unprotected")
	before := f.notifier.Version(f.tenantID)

	desc, portTo := "noise", 9090
	if _, err := f.mr.UpdateResource(f.ctx, id, graph.UpdateResourceInput{Description: &desc, PortTo: &portTo}); err != nil {
		t.Fatalf("UpdateResource: %v", err)
	}

	if got := f.fires.Load(); got != 0 {
		t.Fatalf("push hook fired %d times for ACL-irrelevant update, want 0", got)
	}
	if got := f.notifier.Version(f.tenantID); got != before {
		t.Fatalf("version changed on irrelevant update: %d -> %d", before, got)
	}
}

// TestDeleteResource_HardPath_Invalidates: a hard delete (pending/unprotected)
// removes the row from compiler output and must invalidate.
func TestDeleteResource_HardPath_Invalidates(t *testing.T) {
	f := newACLCoherenceFixture(t)
	id := f.seedResource(t, "10.0.0.93", "unprotected")

	ok, err := f.mr.DeleteResource(f.ctx, id)
	if err != nil {
		t.Fatalf("DeleteResource: %v", err)
	}
	if !ok {
		t.Fatal("DeleteResource returned false")
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("push hook fired %d times, want 1", got)
	}
}

// TestForceDeleteResource_Invalidates: the break-glass hard delete removes the
// row and must invalidate.
func TestForceDeleteResource_Invalidates(t *testing.T) {
	f := newACLCoherenceFixture(t)
	id := f.seedResource(t, "10.0.0.94", "unprotected")

	ok, err := f.mr.ForceDeleteResource(f.ctx, id)
	if err != nil {
		t.Fatalf("ForceDeleteResource: %v", err)
	}
	if !ok {
		t.Fatal("ForceDeleteResource returned false")
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("push hook fired %d times, want 1", got)
	}
}

// TestProtectResource_Invalidates: protect moves the resource from a
// connector-routed status (unprotected) to a shield-routed one (protecting), and
// the ACL compiler routes by status (policy.routeTypeForResource), so the
// compiled output changes and the snapshot must be invalidated once.
// Sprint 20 Fix 02 (F35): this test used to expect 0 — written before the
// compiler started reading status (49c4732).
func TestProtectResource_Invalidates(t *testing.T) {
	f := newACLCoherenceFixture(t)
	id := f.seedResource(t, aclFixtureShieldLAN, "unprotected")

	if _, err := f.mr.ProtectResource(f.ctx, id); err != nil {
		t.Fatalf("ProtectResource: %v", err)
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("push hook fired %d times on protect, want 1", got)
	}
}

// TestUnprotectResource_DoesNotInvalidate: unprotect changes only status/pending_action
// — it must NOT invalidate (locks current behavior).
func TestUnprotectResource_DoesNotInvalidate(t *testing.T) {
	f := newACLCoherenceFixture(t)
	id := f.seedResource(t, "10.0.0.96", "protected")

	if _, err := f.mr.UnprotectResource(f.ctx, id); err != nil {
		t.Fatalf("UnprotectResource: %v", err)
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("push hook fired %d times on unprotect, want 0", got)
	}
}
