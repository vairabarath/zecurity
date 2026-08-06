package resource

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Phase 4 (PENDING-14 Stage 2) addressing tests: a resource may now be addressed
// either by a pinned IP (host) or by a name the connector resolves (hostname +
// resolver). Verifies the two behaviours that migration 030 + Create introduce:
//
//  1. IP-hosted resource  → shield auto-match still works AND the new
//     resource_shields join row is written alongside the singular shield_id.
//  2. FQDN resource       → inserts with host NULL, no shield auto-match is
//     attempted (it keys on shields.lan_ip = host, which cannot match), and no
//     join row is created.
//
// Requires a database with migrations 001–030 applied. Set
// RESOURCE_TEST_DATABASE_URL to run; skipped otherwise.
func TestCreateAddressingModes(t *testing.T) {
	dsn := os.Getenv("RESOURCE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("RESOURCE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	// Throwaway workspace; everything else cascades from it.
	_, _ = db.Exec(ctx, `DELETE FROM workspaces WHERE slug = 'p4-addressing'`)
	var tenantID string
	if err := db.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, trust_domain, status)
		 VALUES ('p4-addressing','p4','p4.example','active') RETURNING id`,
	).Scan(&tenantID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DELETE FROM workspaces WHERE id = $1`, tenantID)
	})

	var networkID, connectorID, shieldID string
	if err := db.QueryRow(ctx,
		`INSERT INTO remote_networks (tenant_id, name, location)
		 VALUES ($1,'rn','office') RETURNING id`, tenantID).Scan(&networkID); err != nil {
		t.Fatalf("seed network: %v", err)
	}
	if err := db.QueryRow(ctx,
		`INSERT INTO connectors (tenant_id, remote_network_id, name, status)
		 VALUES ($1,$2,'c','active') RETURNING id`, tenantID, networkID).Scan(&connectorID); err != nil {
		t.Fatalf("seed connector: %v", err)
	}
	if err := db.QueryRow(ctx,
		`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status, lan_ip)
		 VALUES ($1,$2,$3,'sh','active','10.0.0.5') RETURNING id`,
		tenantID, networkID, connectorID).Scan(&shieldID); err != nil {
		t.Fatalf("seed shield: %v", err)
	}

	linkCount := func(resourceID string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(ctx,
			`SELECT count(*) FROM resource_shields WHERE resource_id = $1`, resourceID,
		).Scan(&n); err != nil {
			t.Fatalf("count resource_shields: %v", err)
		}
		return n
	}

	t.Run("ip hosted resource auto-matches its shield and writes the join row", func(t *testing.T) {
		host := "10.0.0.5" // matches the shield's lan_ip
		row, err := Create(ctx, db, tenantID, CreateInput{
			RemoteNetworkID: networkID,
			Name:            "ip-res",
			Host:            &host,
			Protocol:        "tcp",
			PortFrom:        5432,
			PortTo:          5432,
		})
		if err != nil {
			t.Fatalf("create ip resource: %v", err)
		}
		if row.Host != "10.0.0.5" {
			t.Errorf("host = %q, want 10.0.0.5", row.Host)
		}
		if row.Hostname != nil {
			t.Errorf("hostname = %v, want nil for an IP resource", *row.Hostname)
		}
		if row.ShieldID != shieldID {
			t.Errorf("shield_id = %q, want %q (auto-match should still work)", row.ShieldID, shieldID)
		}
		if got := linkCount(row.ID); got != 1 {
			t.Errorf("resource_shields rows = %d, want 1 — the join table must stay in "+
				"step with shield_id while both exist", got)
		}
	})

	t.Run("fqdn resource inserts with no host and no shield", func(t *testing.T) {
		hostname := "db.internal"
		resolver := `{"type":"dns","name":"db.internal"}`
		row, err := Create(ctx, db, tenantID, CreateInput{
			RemoteNetworkID: networkID,
			Name:            "fqdn-res",
			Hostname:        &hostname,
			Resolver:        &resolver,
			Protocol:        "tcp",
			PortFrom:        5432,
			PortTo:          5432,
		})
		if err != nil {
			t.Fatalf("create fqdn resource: %v", err)
		}
		// host is NULL in the DB; the select COALESCEs it so readers never see NULL.
		if row.Host != "" {
			t.Errorf("host = %q, want empty for an FQDN resource", row.Host)
		}
		if row.Hostname == nil || *row.Hostname != "db.internal" {
			t.Errorf("hostname = %v, want db.internal", row.Hostname)
		}
		if row.Resolver == nil {
			t.Fatal("resolver is nil — the resolver config must round-trip")
		}
		if row.ShieldID != "" {
			t.Errorf("shield_id = %q, want empty — auto-match keys on lan_ip = host "+
				"and must not run for an FQDN resource", row.ShieldID)
		}
		if got := linkCount(row.ID); got != 0 {
			t.Errorf("resource_shields rows = %d, want 0", got)
		}
	})

	t.Run("resource with neither host nor hostname is rejected", func(t *testing.T) {
		_, err := Create(ctx, db, tenantID, CreateInput{
			RemoteNetworkID: networkID,
			Name:            "no-address",
			Protocol:        "tcp",
			PortFrom:        80,
			PortTo:          80,
		})
		if err == nil {
			t.Fatal("expected an error — resources_addressable_check must reject a " +
				"resource with no address at all")
		}
	})
}
