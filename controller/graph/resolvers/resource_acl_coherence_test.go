package resolvers

import (
	"context"
	"os"
	"sync/atomic"
	"testing"

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
// ACL snapshot. Set RESOURCE_TEST_DATABASE_URL and RESOURCE_TEST_SHIELD_ID to run;
// skipped otherwise (matches internal/resource/snapshot_integration_test.go).
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

func newACLCoherenceFixture(t *testing.T) *aclCoherenceFixture {
	t.Helper()
	dsn := os.Getenv("RESOURCE_TEST_DATABASE_URL")
	shieldID := os.Getenv("RESOURCE_TEST_SHIELD_ID")
	if dsn == "" || shieldID == "" {
		t.Skip("RESOURCE_TEST_DATABASE_URL / RESOURCE_TEST_SHIELD_ID not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	var tenantID, networkID string
	if err := pool.QueryRow(ctx,
		`SELECT tenant_id, remote_network_id FROM shields WHERE id = $1`, shieldID,
	).Scan(&tenantID, &networkID); err != nil {
		t.Fatalf("load shield: %v", err)
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

// TestProtectResource_Invalidates: protect MUST invalidate.
//
// This test previously asserted the opposite, on the premise that protect "changes
// only status/pending_action, which the compiler never reads". That premise is
// false: the compiler selects r.status (internal/policy/store.go) and derives
// ACLEntry.route_type from it (routeTypeForResource) — 'unprotected' yields
// "connector", 'protecting' yields "shield". Protect therefore flips route_type,
// and the connector branches on route_type FIRST (Sprint 16 Phase 7.1).
//
// Not propagating that flip is an enforcement bypass, not a missed optimisation:
// connectors would keep direct-dialing a resource that is now shield-protected.
// The test had never executed — it is gated on RESOURCE_TEST_SHIELD_ID — so the
// wrong premise was never contradicted.
//
// Contrast TestUnprotectResource_DoesNotInvalidate below, where the asymmetry is
// real and correct.
func TestProtectResource_Invalidates(t *testing.T) {
	f := newACLCoherenceFixture(t)
	id := f.seedResource(t, "10.0.0.95", "unprotected")
	before := f.notifier.Version(f.tenantID)

	if _, err := f.mr.ProtectResource(f.ctx, id); err != nil {
		t.Fatalf("ProtectResource: %v (is RESOURCE_TEST_SHIELD_ID active, and does "+
			"its lan_ip match the seeded host? MarkProtecting requires "+
			"shields.lan_ip = resources.host)", err)
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("push hook fired %d times on protect, want 1 — route_type flips "+
			"connector→shield and every client must learn it", got)
	}
	if got := f.notifier.Version(f.tenantID); got <= before {
		t.Fatalf("version not bumped on protect: %d -> %d", before, got)
	}
}

// TestUnprotectResource_DoesNotInvalidate: unprotect must NOT invalidate — and
// unlike protect, that is correct rather than an oversight.
//
// Unprotect moves 'protected' → 'protecting' with pending_action='remove'. Both
// statuses map to route_type "shield" (routeTypeForResource), so the compiled ACL
// is byte-identical at this point and there is nothing for a client to learn. The
// flip to "connector" happens later, when the shield acks removal and the row
// reaches 'unprotected' — invalidation belongs on that path, not here.
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
