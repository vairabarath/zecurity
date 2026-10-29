package resolvers

import (
	"context"
	"testing"
)

// Discovered services may only be read or promoted for the caller's own
// shields. Before the ownership check, getDiscoveredServices returned any
// shield's services (bound IPs, ports, service names), and
// promoteDiscoveredService answered differently depending on whether a foreign
// shield had discovered the requested protocol/port.
// Reuses the two-workspace fixture from policy_tenant_isolation_test.go.

// seedShield inserts a connector and an active shield in w's network, plus
// one discovered service (tcp/5432) on it, and returns the shield ID.
func (f *isoFixture) seedShield(t *testing.T, w isoWorkspace, tag string) string {
	t.Helper()
	ctx := context.Background()
	var conn, shield string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO connectors (tenant_id, remote_network_id, name, status) VALUES ($1, $2, $3, 'active') RETURNING id`,
		w.id, w.rn, "conn-"+tag).Scan(&conn); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status) VALUES ($1, $2, $3, $4, 'active') RETURNING id`,
		w.id, w.rn, conn, "shield-"+tag).Scan(&shield); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO shield_discovered_services (shield_id, protocol, port, bound_ip, service_name) VALUES ($1, 'tcp', 5432, $2, $3)`,
		shield, "10.90.0."+map[string]string{"a": "1", "b": "2"}[tag], "postgres-"+tag); err != nil {
		t.Fatal(err)
	}
	return shield
}

func TestDiscoveryIsolation_ListHidesForeignShield(t *testing.T) {
	f := newIsoFixture(t)
	shieldA, shieldB := f.seedShield(t, f.a, "a"), f.seedShield(t, f.b, "b")

	// Positive control: A reads its own shield's services.
	own, err := f.qr.GetDiscoveredServices(f.a.ctx, shieldA)
	if err != nil || len(own) != 1 || own[0].ServiceName != "postgres-a" {
		t.Fatalf("own shield: err=%v services=%+v", err, own)
	}

	for _, c := range []struct{ name, id string }{
		{"foreign shield", shieldB},
		{"missing shield", missingID},
		{"malformed id", "not-a-uuid"},
	} {
		svcs, err := f.qr.GetDiscoveredServices(f.a.ctx, c.id)
		if err == nil {
			t.Errorf("%s: returned %d services", c.name, len(svcs))
			continue
		}
		if msg := presented(f.a.ctx, err); msg != "shield not found" {
			t.Errorf("%s: client sees %q, want %q", c.name, msg, "shield not found")
		}
	}

	// A shield of A's with nothing discovered is an empty list, not an error.
	var bare string
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status)
		 SELECT tenant_id, remote_network_id, connector_id, 'shield-bare', 'active' FROM shields WHERE id = $1 RETURNING id`,
		shieldA).Scan(&bare); err != nil {
		t.Fatal(err)
	}
	empty, err := f.qr.GetDiscoveredServices(f.a.ctx, bare)
	if err != nil || len(empty) != 0 {
		t.Fatalf("own shield without services: err=%v services=%+v", err, empty)
	}
}

func TestDiscoveryIsolation_PromoteGivesNoForeignSignal(t *testing.T) {
	f := newIsoFixture(t)
	f.seedShield(t, f.a, "a")
	shieldB := f.seedShield(t, f.b, "b")
	before := f.resourceCount(t)
	vA, vB := f.notifier.Version(f.a.id), f.notifier.Version(f.b.id)

	// B's shield has tcp/5432 discovered and not udp/9; a missing shield has
	// nothing. All three must look identical to A.
	for _, c := range []struct {
		name, shield, proto string
		port                int
	}{
		{"foreign shield, discovered service", shieldB, "tcp", 5432},
		{"foreign shield, unknown service", shieldB, "udp", 9},
		{"missing shield", missingID, "tcp", 5432},
	} {
		res, err := f.mr.PromoteDiscoveredService(f.a.ctx, c.shield, c.proto, c.port)
		if err == nil {
			t.Errorf("%s: promoted to resource %s", c.name, res.ID)
			continue
		}
		if msg := presented(f.a.ctx, err); msg != "shield not found" {
			t.Errorf("%s: client sees %q, want %q", c.name, msg, "shield not found")
		}
	}
	if after := f.resourceCount(t); after != before {
		t.Errorf("rejected promotions changed the resource count: %d -> %d", before, after)
	}
	if f.notifier.Version(f.a.id) != vA || f.notifier.Version(f.b.id) != vB {
		t.Errorf("rejected promotions bumped a policy version")
	}
}

// FU-4: promoting a service on the caller's own shield creates a resource in
// that shield's network (before, it never set a network and always failed).
func TestDiscoveryIsolation_PromoteOwnShieldCreatesResource(t *testing.T) {
	f := newIsoFixture(t)
	shieldA := f.seedShield(t, f.a, "a")
	before := f.resourceCount(t)
	vA, vB := f.notifier.Version(f.a.id), f.notifier.Version(f.b.id)

	res, err := f.mr.PromoteDiscoveredService(f.a.ctx, shieldA, "tcp", 5432)
	if err != nil {
		t.Fatalf("own shield promotion: %v", err)
	}
	if res.Host != "10.90.0.1" || res.PortFrom != 5432 || res.PortTo != 5432 || res.Protocol != "tcp" {
		t.Errorf("promoted resource = %+v", res)
	}
	// F-5: named after the service and the shield's name, not its ID.
	if res.Name != "postgres-a on shield-a" {
		t.Errorf("promoted resource name = %q, want %q", res.Name, "postgres-a on shield-a")
	}
	var rn, tenant string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT remote_network_id::text, tenant_id::text FROM resources WHERE id = $1`, res.ID,
	).Scan(&rn, &tenant); err != nil {
		t.Fatal(err)
	}
	if rn != f.a.rn || tenant != f.a.id {
		t.Errorf("resource network/workspace = %s/%s, want %s/%s", rn, tenant, f.a.rn, f.a.id)
	}
	if after := f.resourceCount(t); after != before+1 {
		t.Errorf("resource count %d -> %d, want +1", before, after)
	}
	if f.notifier.Version(f.a.id) == vA {
		t.Error("promotion did not notify A's policy")
	}
	if f.notifier.Version(f.b.id) != vB {
		t.Error("promotion bumped another workspace's policy version")
	}

	// F-5: promoting it again is a clear user error, not a database error.
	if _, err := f.mr.PromoteDiscoveredService(f.a.ctx, shieldA, "tcp", 5432); err == nil {
		t.Error("the same service was promoted twice")
	} else if msg := presented(f.a.ctx, err); msg != `promoteDiscoveredService: tcp/5432 is already a resource ("postgres-a on shield-a")` {
		t.Errorf("repeated promotion: client sees %q", msg)
	}
	// F-5: a service the shield couldn't name is named by protocol and port.
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO shield_discovered_services (shield_id, protocol, port, bound_ip, service_name) VALUES ($1, 'tcp', 8813, '10.90.0.1', '')`,
		shieldA); err != nil {
		t.Fatal(err)
	}
	unnamed, err := f.mr.PromoteDiscoveredService(f.a.ctx, shieldA, "tcp", 8813)
	if err != nil {
		t.Fatalf("unnamed service promotion: %v", err)
	}
	if unnamed.Name != "tcp/8813 on shield-a" {
		t.Errorf("unnamed service resource name = %q, want %q", unnamed.Name, "tcp/8813 on shield-a")
	}
	before++

	// An undiscovered port on the same shield still creates nothing.
	if _, err := f.mr.PromoteDiscoveredService(f.a.ctx, shieldA, "udp", 9); err == nil {
		t.Error("undiscovered service was promoted")
	}
	if after := f.resourceCount(t); after != before+1 {
		t.Errorf("rejected promotion changed the resource count to %d", after)
	}
}
