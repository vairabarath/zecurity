package resolvers

import (
	"context"
	"testing"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/db"
)

// remoteNetworks { shields { lanIp } } returned the shield's public IP: the
// batch query selected public_ip into the column scanShield reads as lan_ip.
// The per-network `shields` query was right, so the two disagreed.
func TestRemoteNetworks_ShieldLanIPIsTheLANAddress(t *testing.T) {
	f := newIsoFixture(t)
	ctx := context.Background()
	var connID, shieldID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO connectors (tenant_id, remote_network_id, name, status) VALUES ($1, $2, 'c', 'active') RETURNING id`,
		f.a.id, f.a.rn).Scan(&connID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status, lan_ip, public_ip)
		 VALUES ($1, $2, $3, 's', 'active', '10.80.0.7', '203.0.113.9') RETURNING id`,
		f.a.id, f.a.rn, connID).Scan(&shieldID); err != nil {
		t.Fatal(err)
	}
	qr := &queryResolver{&Resolver{Pool: f.pool, TenantDB: db.NewTenantDB(f.pool)}}

	nets, err := qr.RemoteNetworks(f.a.ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got *string
	for _, n := range nets {
		for _, s := range n.Shields {
			if s.ID == shieldID {
				got = s.LanIP
			}
		}
	}
	if got == nil || *got != "10.80.0.7" {
		v := "<nil>"
		if got != nil {
			v = *got
		}
		t.Fatalf("remoteNetworks shield lanIp = %s, want 10.80.0.7 (not the public IP)", v)
	}

	direct, err := qr.Shields(f.a.ctx, f.a.rn)
	if err != nil {
		t.Fatal(err)
	}
	if len(direct) != 1 || direct[0].LanIP == nil || *direct[0].LanIP != *got {
		t.Fatalf("shields(remoteNetworkId) disagrees with remoteNetworks: %+v", direct)
	}
}

// A missing, foreign or malformed resource id used to reach the console as
// INTERNAL ("an unexpected error occurred"); each action now gives its own
// user-safe answer, the same for all three cases.
func TestResourceActions_NotFoundIsUserSafe(t *testing.T) {
	f := newIsoFixture(t)
	name := "x"
	for _, c := range []struct {
		action string
		want   string
		call   func(id string) error
	}{
		{"updateResource", "resource not found", func(id string) error {
			_, err := f.mr.UpdateResource(f.a.ctx, id, graph.UpdateResourceInput{Name: &name})
			return err
		}},
		{"protectResource", "protect resource: resource not found, invalid state, or no matching active shield in its remote network", func(id string) error {
			_, err := f.mr.ProtectResource(f.a.ctx, id, nil)
			return err
		}},
		{"unprotectResource", "unprotect resource: resource not found or not in protected state", func(id string) error {
			_, err := f.mr.UnprotectResource(f.a.ctx, id)
			return err
		}},
		{"deleteResource", "resource not found", func(id string) error {
			_, err := f.mr.DeleteResource(f.a.ctx, id)
			return err
		}},
	} {
		for _, id := range []struct{ what, id string }{
			{"missing", missingID},
			{"foreign", f.b.res},
			{"malformed", "not-a-uuid"},
		} {
			err := c.call(id.id)
			if err == nil {
				t.Errorf("%s on %s id: no error", c.action, id.what)
				continue
			}
			msg := presented(f.a.ctx, err)
			if msg != c.want {
				t.Errorf("%s on %s id: client sees %q, want %q", c.action, id.what, msg, c.want)
			}
		}
	}
}
