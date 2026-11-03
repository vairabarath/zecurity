package resource

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// PENDING-14 F1: DNS-name resources in the controller. A name is an alias for
// a host; when protected, its shield is chosen by the admin and every
// instruction to the shield carries the shield's own LAN IP (F-5).
func TestDNSNameResources(t *testing.T) {
	db := newResourceTestDB(t)
	ctx := context.Background()
	slug := fmt.Sprintf("f1-%d", time.Now().UnixNano())

	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	var tenantID, otherTenantID, netA, netB, otherNet, connA, connB, otherConn string
	must("workspace", db.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ($1, $1, 'active', $2) RETURNING id`,
		slug, "ws-"+slug+".zecurity.in").Scan(&tenantID))
	must("other workspace", db.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ($1, $1, 'active', $2) RETURNING id`,
		slug+"-o", "ws-"+slug+"-o.zecurity.in").Scan(&otherTenantID))
	network := func(tenant, name string) string {
		var id string
		must("network "+name, db.QueryRow(ctx,
			`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, $2, 'other') RETURNING id`, tenant, name).Scan(&id))
		return id
	}
	netA, netB, otherNet = network(tenantID, "a"), network(tenantID, "b"), network(otherTenantID, "o")
	conn := func(tenant, net, name string) string {
		var id string
		must("connector "+name, db.QueryRow(ctx,
			`INSERT INTO connectors (tenant_id, remote_network_id, name, status) VALUES ($1, $2, $3, 'active') RETURNING id`,
			tenant, net, name).Scan(&id))
		return id
	}
	connA, connB, otherConn = conn(tenantID, netA, "ca"), conn(tenantID, netB, "cb"), conn(otherTenantID, otherNet, "co")
	shield := func(tenant, net, connector, name, lan, status string) string {
		var id string
		must("shield "+name, db.QueryRow(ctx,
			`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status, lan_ip)
			 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			tenant, net, connector, name, status, lan).Scan(&id))
		return id
	}
	sA := shield(tenantID, netA, connA, "sa", "10.1.0.10", "active")
	sA2 := shield(tenantID, netA, connA, "sa2", "10.1.0.11", "active")
	sAoff := shield(tenantID, netA, connA, "sa-off", "10.1.0.12", "disconnected")
	sB := shield(tenantID, netB, connB, "sb", "10.2.0.10", "active")
	sOther := shield(otherTenantID, otherNet, otherConn, "so", "10.9.0.10", "active")

	port := 7000
	create := func(net, host, proto string) (*Row, error) {
		port++
		return Create(ctx, db, tenantID, CreateInput{
			RemoteNetworkID: net, Name: fmt.Sprintf("r-%d", port), Host: host, Protocol: proto, PortFrom: port, PortTo: port,
		})
	}
	desiredHost := func(shieldID, resourceID string) (string, bool) {
		t.Helper()
		rows, err := GetDesiredForShield(ctx, db, shieldID)
		must("desired", err)
		for _, r := range rows {
			if r.ID == resourceID {
				return r.Host, true
			}
		}
		return "", false
	}
	str := func(s string) *string { return &s }

	t.Run("the Go and SQL host rules agree", func(t *testing.T) {
		for _, h := range []string{"10.0.0.5", "127.0.0.1", "db.internal", "123.example", "x.y2k", "10-0-0-5.nip.io"} {
			var sqlIPv4 bool
			must("sql", db.QueryRow(ctx, `SELECT $1 ~ '`+ipv4HostPattern+`'`, h).Scan(&sqlIPv4))
			if sqlIPv4 == IsNameHost(h) {
				t.Errorf("%q: SQL says ipv4=%v, Go says name=%v", h, sqlIPv4, IsNameHost(h))
			}
		}
	})

	t.Run("a name must use tcp", func(t *testing.T) {
		for _, p := range []string{"udp", "any"} {
			if _, err := create(netA, "db.acme.example", p); !errors.Is(err, ErrNameNeedsTCP) {
				t.Fatalf("protocol %s: err = %v, want ErrNameNeedsTCP", p, err)
			}
		}
		row, err := create(netA, "db.acme.example", "tcp")
		must("tcp name", err)
		if _, err := Update(ctx, db, tenantID, row.ID, UpdateInput{Protocol: str("udp")}); !errors.Is(err, ErrNameNeedsTCP) {
			t.Fatalf("protocol-only edit to udp: err = %v, want ErrNameNeedsTCP", err)
		}
		// An IPv4 resource on udp can't become a name either.
		ip, err := create(netA, "10.1.0.50", "udp")
		must("udp ip", err)
		if _, err := Update(ctx, db, tenantID, ip.ID, UpdateInput{Host: str("db2.acme.example")}); !errors.Is(err, ErrNameNeedsTCP) {
			t.Fatalf("udp ip -> name: err = %v, want ErrNameNeedsTCP", err)
		}
	})

	t.Run("a name is never auto-matched to a shield", func(t *testing.T) {
		row, err := create(netA, "auto.acme.example", "tcp")
		must("create", err)
		if row.ShieldID != "" || row.Status != "unprotected" {
			t.Fatalf("shield=%q status=%q, want none/unprotected", row.ShieldID, row.Status)
		}
	})

	t.Run("collision: same name, overlapping ports, another network", func(t *testing.T) {
		mk := func(net string, from, to int) error {
			_, err := Create(ctx, db, tenantID, CreateInput{RemoteNetworkID: net, Name: fmt.Sprintf("c-%s-%d-%d", net[:4], from, to),
				Host: "shared.acme.example", Protocol: "tcp", PortFrom: from, PortTo: to})
			return err
		}
		must("first", mk(netA, 100, 200))
		if err := mk(netB, 150, 150); !errors.Is(err, ErrNameCollision) {
			t.Fatalf("overlap in other network: err = %v, want ErrNameCollision", err)
		}
		if err := mk(netB, 200, 300); !errors.Is(err, ErrNameCollision) {
			t.Fatalf("edge overlap: err = %v, want ErrNameCollision", err)
		}
		must("no overlap in other network", mk(netB, 201, 300))
		must("same network, overlapping", mk(netA, 150, 160))
		// IPv4 hosts legitimately repeat across networks.
		_, err := create(netA, "10.5.5.5", "tcp")
		must("ip a", err)
		port--
		_, err = create(netB, "10.5.5.5", "tcp")
		must("same ip in b", err)
		// Update into a collision is refused too.
		row, err := Create(ctx, db, tenantID, CreateInput{RemoteNetworkID: netB, Name: "c-move", Host: "other.acme.example", Protocol: "tcp", PortFrom: 120, PortTo: 120})
		must("other", err)
		if _, err := Update(ctx, db, tenantID, row.ID, UpdateInput{Host: str("shared.acme.example")}); !errors.Is(err, ErrNameCollision) {
			t.Fatalf("update into collision: err = %v, want ErrNameCollision", err)
		}
		// Editing a resource doesn't collide with itself.
		if _, err := Update(ctx, db, tenantID, row.ID, UpdateInput{Name: str("c-move-2"), Host: str("other.acme.example")}); err != nil {
			t.Fatalf("self edit: %v", err)
		}
	})

	t.Run("collision check is race-safe", func(t *testing.T) {
		// Pause each save between its check and its INSERT: without the lock
		// every save would pass the check before any of them commits.
		afterNameCollisionCheck = func() { time.Sleep(100 * time.Millisecond) }
		defer func() { afterNameCollisionCheck = nil }()
		var wg sync.WaitGroup
		errs := make([]error, 8)
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				net := netA
				if i%2 == 1 {
					net = netB
				}
				_, errs[i] = Create(ctx, db, tenantID, CreateInput{RemoteNetworkID: net, Name: fmt.Sprintf("race-%d", i),
					Host: "race.acme.example", Protocol: "tcp", PortFrom: 443, PortTo: 443})
			}(i)
		}
		wg.Wait()
		var n int
		must("count", db.QueryRow(ctx,
			`SELECT count(DISTINCT remote_network_id) FROM resources WHERE tenant_id = $1 AND host = 'race.acme.example'`, tenantID).Scan(&n))
		for _, e := range errs {
			if e != nil && !errors.Is(e, ErrNameCollision) {
				t.Fatalf("unexpected error: %v", e)
			}
		}
		if n != 1 {
			t.Fatalf("race.acme.example exists in %d networks, want 1", n)
		}
	})

	t.Run("protect: a name needs an active shield of its own network", func(t *testing.T) {
		row, err := create(netA, "web.acme.example", "tcp")
		must("create", err)
		if _, err := MarkProtecting(ctx, db, tenantID, row.ID, ""); !errors.Is(err, ErrProtectNameNeedsShield) {
			t.Fatalf("no shield: err = %v, want ErrProtectNameNeedsShield", err)
		}
		for name, sid := range map[string]string{"other network": sB, "other workspace": sOther, "offline": sAoff, "not a uuid": "x", "unknown": "00000000-0000-0000-0000-000000000000"} {
			if _, err := MarkProtecting(ctx, db, tenantID, row.ID, sid); !errors.Is(err, ErrProtectNoShield) {
				t.Fatalf("%s: err = %v, want ErrProtectNoShield", name, err)
			}
		}
		got, err := MarkProtecting(ctx, db, tenantID, row.ID, sA2)
		must("protect", err)
		if got.ShieldID != sA2 || got.Status != "protecting" || got.Host != "web.acme.example" || got.ShieldHost != "10.1.0.11" {
			t.Fatalf("after protect: shield=%q status=%q host=%q shieldHost=%q", got.ShieldID, got.Status, got.Host, got.ShieldHost)
		}
		if h, ok := desiredHost(sA2, row.ID); !ok || h != "10.1.0.11" {
			t.Fatalf("desired host = %q (%v), want the shield's LAN IP", h, ok)
		}
	})

	t.Run("protect: an IPv4 host keeps auto-match; a given shield must be on that IP", func(t *testing.T) {
		row, err := create(netA, "10.1.0.10", "tcp")
		must("create", err)
		if _, err := MarkProtecting(ctx, db, tenantID, row.ID, sA2); !errors.Is(err, ErrProtectNoShield) {
			t.Fatalf("shield on another IP: err = %v, want ErrProtectNoShield", err)
		}
		got, err := MarkProtecting(ctx, db, tenantID, row.ID, "")
		must("auto", err)
		if got.ShieldID != sA || got.ShieldHost != "10.1.0.10" {
			t.Fatalf("shield=%q shieldHost=%q", got.ShieldID, got.ShieldHost)
		}
	})

	// A protected name, acked by its shield.
	protectedName := func(host string, shieldID string) *Row {
		t.Helper()
		row, err := create(netA, host, "tcp")
		must("create", err)
		_, err = MarkProtecting(ctx, db, tenantID, row.ID, shieldID)
		must("protect", err)
		got, err := GetByID(ctx, db, tenantID, row.ID)
		must("get", err)
		_, err = RecordAck(ctx, db, tenantID, Ack{ResourceID: row.ID, Status: "protected", ShieldID: shieldID,
			Host: got.ShieldHost, Protocol: "tcp", PortFrom: got.PortFrom, PortTo: got.PortTo, VerifiedAt: time.Now().Unix()})
		must("ack", err)
		got, err = GetByID(ctx, db, tenantID, row.ID)
		must("get", err)
		if got.Status != "protected" {
			t.Fatalf("status after ack = %q, want protected", got.Status)
		}
		return got
	}

	t.Run("ack: the shield's LAN IP counts, the name does not", func(t *testing.T) {
		row, err := create(netA, "ack.acme.example", "tcp")
		must("create", err)
		_, err = MarkProtecting(ctx, db, tenantID, row.ID, sA)
		must("protect", err)
		_, err = RecordAck(ctx, db, tenantID, Ack{ResourceID: row.ID, Status: "protected", ShieldID: sA,
			Host: "ack.acme.example", Protocol: "tcp", PortFrom: row.PortFrom, PortTo: row.PortTo, VerifiedAt: time.Now().Unix()})
		must("ack name", err)
		if got, _ := GetByID(ctx, db, tenantID, row.ID); got.Status != "protecting" {
			t.Fatalf("an ack for the name changed status to %q", got.Status)
		}
		_, err = RecordAck(ctx, db, tenantID, Ack{ResourceID: row.ID, Status: "protected", ShieldID: sA,
			Host: "10.1.0.10", Protocol: "tcp", PortFrom: row.PortFrom, PortTo: row.PortTo, VerifiedAt: time.Now().Unix()})
		must("ack ip", err)
		if got, _ := GetByID(ctx, db, tenantID, row.ID); got.Status != "protected" {
			t.Fatalf("status = %q, want protected", got.Status)
		}
	})

	t.Run("unprotect: the pending remove carries the LAN IP", func(t *testing.T) {
		row := protectedName("rm.acme.example", sA)
		_, err := MarkUnprotecting(ctx, db, tenantID, row.ID)
		must("unprotect", err)
		pending, err := GetPendingForShield(ctx, db, sA)
		must("pending", err)
		found := false
		for _, p := range pending {
			if p.ID == row.ID {
				found = true
				if p.Host != "10.1.0.10" || p.PendingAction != "remove" {
					t.Fatalf("pending = %+v", p)
				}
			}
		}
		if !found {
			t.Fatal("no pending remove")
		}
		got, _ := GetByID(ctx, db, tenantID, row.ID)
		if got.ShieldHost != "10.1.0.10" {
			t.Fatalf("row shield host = %q", got.ShieldHost)
		}
	})

	t.Run("update a protected name", func(t *testing.T) {
		row := protectedName("upd.acme.example", sA)
		// name -> name: still protected, nothing for the shield to do.
		res, err := Update(ctx, db, tenantID, row.ID, UpdateInput{Host: str("upd2.acme.example"), Name: str(row.Name)})
		must("rename", err)
		if res.Row.Status != "protected" || len(res.ShieldPushes) != 0 || res.Row.ShieldID != sA {
			t.Fatalf("name->name: status=%q pushes=%v shield=%q", res.Row.Status, res.ShieldPushes, res.Row.ShieldID)
		}
		// Another network: refused, nothing changes.
		if _, err := Update(ctx, db, tenantID, row.ID, UpdateInput{RemoteNetworkID: str(netB)}); !errors.Is(err, ErrNameProtectedMove) {
			t.Fatalf("network move: err = %v, want ErrNameProtectedMove", err)
		}
		// A port change re-protects on the same shield, with its LAN IP.
		res, err = Update(ctx, db, tenantID, row.ID, UpdateInput{PortFrom: num(9999), PortTo: num(9999)})
		must("port", err)
		if res.Row.Status != "protecting" || len(res.ShieldPushes) != 1 || res.ShieldPushes[0].ShieldID != sA {
			t.Fatalf("port edit: status=%q pushes=%v", res.Row.Status, res.ShieldPushes)
		}
		if h, ok := desiredHost(sA, row.ID); !ok || h != "10.1.0.10" {
			t.Fatalf("desired host = %q (%v)", h, ok)
		}
		// name -> IPv4 of another shield: moves there, as for any IP host.
		res, err = Update(ctx, db, tenantID, row.ID, UpdateInput{Host: str("10.1.0.11")})
		must("to ip", err)
		if res.Row.ShieldID != sA2 || !res.Moved {
			t.Fatalf("name->ip: shield=%q moved=%v", res.Row.ShieldID, res.Moved)
		}
	})

	t.Run("update a protected IPv4 resource to a name keeps its shield", func(t *testing.T) {
		row, err := create(netA, "10.1.0.11", "tcp")
		must("create", err)
		_, err = MarkProtecting(ctx, db, tenantID, row.ID, "")
		must("protect", err)
		_, err = RecordAck(ctx, db, tenantID, Ack{ResourceID: row.ID, Status: "protected", ShieldID: sA2,
			Host: "10.1.0.11", Protocol: "tcp", PortFrom: row.PortFrom, PortTo: row.PortTo, VerifiedAt: time.Now().Unix()})
		must("ack", err)
		res, err := Update(ctx, db, tenantID, row.ID, UpdateInput{Host: str("files.acme.example")})
		must("to name", err)
		if res.Row.ShieldID != sA2 || res.Row.Status != "protected" || len(res.ShieldPushes) != 0 || res.Row.ShieldHost != "10.1.0.11" {
			t.Fatalf("ip->name: shield=%q status=%q pushes=%v shieldHost=%q", res.Row.ShieldID, res.Row.Status, res.ShieldPushes, res.Row.ShieldHost)
		}
	})

	t.Run("a new shield LAN IP reaches the desired set", func(t *testing.T) {
		row := protectedName("move.acme.example", sA2)
		_, err := db.Exec(ctx, `UPDATE shields SET lan_ip = '10.1.0.99' WHERE id = $1`, sA2)
		must("lan ip", err)
		if h, ok := desiredHost(sA2, row.ID); !ok || h != "10.1.0.99" {
			t.Fatalf("desired host = %q (%v), want 10.1.0.99", h, ok)
		}
		_, err = db.Exec(ctx, `UPDATE shields SET lan_ip = '10.1.0.11' WHERE id = $1`, sA2)
		must("restore", err)
	})
}

func num(i int) *int { return &i }
