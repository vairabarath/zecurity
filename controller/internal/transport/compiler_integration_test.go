package transport

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// TestCompileTransportSnapshot verifies the transport compiler groups active
// connectors by remote_network_id and resolves tunnel/relay/SPIFFE coordinates
// identically to the ACL compiler. Requires PKI_TEST_DATABASE_URL; otherwise skips.
func TestCompileTransportSnapshot(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := uniqueTransportTestDBName(t)

	adminPool := mustConnectTransportPool(t, ctx, adminDSN)
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

	testDSN, err := withTransportTestDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	pool := mustConnectTransportPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyTransportMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	store := NewStore(pool)
	notifier := NewNotifier(NewSnapshotCache())

	t.Run("connector with placement carries relay coords", func(t *testing.T) {
		wsID := mustInsertWorkspace(t, ctx, pool, "ws-tp-1")
		rnID := mustInsertRemoteNetwork(t, ctx, pool, wsID, "rn-1")
		connID := mustInsertConnector(t, ctx, pool, wsID, rnID, "td-1", "10.1.0.1")
		relayID := mustInsertActiveRelay(t, ctx, pool, "relay.x:9093")
		mustInsertPlacement(t, ctx, pool, connID, relayID)

		snap, err := CompileTransportSnapshot(ctx, store, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if len(snap.RemoteNetworks) != 1 {
			t.Fatalf("want 1 remote_network, got %d", len(snap.RemoteNetworks))
		}
		rn := snap.RemoteNetworks[0]
		if rn.RemoteNetworkId != rnID {
			t.Fatalf("remote_network_id: want %q got %q", rnID, rn.RemoteNetworkId)
		}
		if len(rn.Connectors) != 1 {
			t.Fatalf("want 1 connector, got %d", len(rn.Connectors))
		}
		c := rn.Connectors[0]
		if c.ConnectorId != connID {
			t.Fatalf("connector_id: want %q got %q", connID, c.ConnectorId)
		}
		if c.ConnectorTunnelAddr != "10.1.0.1:9092" {
			t.Fatalf("tunnel_addr: want 10.1.0.1:9092 got %q", c.ConnectorTunnelAddr)
		}
		if c.RelayAddr != "relay.x:9093" {
			t.Fatalf("relay_addr: want relay.x:9093 got %q", c.RelayAddr)
		}
		if c.ConnectorSpiffe != appmeta.ConnectorSPIFFEID("td-1", connID) {
			t.Fatalf("connector_spiffe mismatch: got %q", c.ConnectorSpiffe)
		}
		if c.RelaySpiffeId != appmeta.RelaySPIFFEID(relayID) {
			t.Fatalf("relay_spiffe_id mismatch: got %q", c.RelaySpiffeId)
		}
	})

	t.Run("connector without placement is direct-only (empty relay)", func(t *testing.T) {
		wsID := mustInsertWorkspace(t, ctx, pool, "ws-tp-2")
		rnID := mustInsertRemoteNetwork(t, ctx, pool, wsID, "rn-2")
		_ = mustInsertConnector(t, ctx, pool, wsID, rnID, "td-2", "10.2.0.1")

		snap, err := CompileTransportSnapshot(ctx, store, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if len(snap.RemoteNetworks) != 1 || len(snap.RemoteNetworks[0].Connectors) != 1 {
			t.Fatalf("unexpected shape: %+v", snap.RemoteNetworks)
		}
		c := snap.RemoteNetworks[0].Connectors[0]
		if c.RelayAddr != "" || c.RelaySpiffeId != "" {
			t.Fatalf("no-placement connector must have empty relay, got addr=%q spiffe=%q", c.RelayAddr, c.RelaySpiffeId)
		}
		if c.ConnectorTunnelAddr != "10.2.0.1:9092" {
			t.Fatalf("tunnel_addr: want 10.2.0.1:9092 got %q", c.ConnectorTunnelAddr)
		}
	})

	// ── Empty-network delivery (Fix05-Empty-Network-Transport-Delivery) ──────

	rnsByID := func(t *testing.T, wsID string) map[string]int {
		t.Helper()
		snap, err := CompileTransportSnapshot(ctx, store, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		out := make(map[string]int, len(snap.RemoteNetworks))
		for _, rn := range snap.RemoteNetworks {
			if _, dup := out[rn.RemoteNetworkId]; dup {
				t.Fatalf("remote network %q emitted twice", rn.RemoteNetworkId)
			}
			out[rn.RemoteNetworkId] = len(rn.Connectors)
		}
		return out
	}

	t.Run("remote network with no active connector is emitted empty", func(t *testing.T) {
		wsID := mustInsertWorkspace(t, ctx, pool, "ws-tp-3")
		rnNone := mustInsertRemoteNetwork(t, ctx, pool, wsID, "rn-3-none")
		rnDisc := mustInsertRemoteNetwork(t, ctx, pool, wsID, "rn-3-disc")
		c := mustInsertConnector(t, ctx, pool, wsID, rnDisc, "td-3", "10.3.0.1")
		mustSetConnectorStatus(t, ctx, pool, c, "disconnected")

		got := rnsByID(t, wsID)
		if n, ok := got[rnNone]; !ok || n != 0 {
			t.Fatalf("RN with no connectors: want present+empty, got present=%v n=%d", ok, n)
		}
		if n, ok := got[rnDisc]; !ok || n != 0 {
			t.Fatalf("RN with only a disconnected connector: want present+empty, got present=%v n=%d", ok, n)
		}
	})

	t.Run("multiple remote networks are independent", func(t *testing.T) {
		wsID := mustInsertWorkspace(t, ctx, pool, "ws-tp-4")
		rnA := mustInsertRemoteNetwork(t, ctx, pool, wsID, "rn-4-a")
		rnB := mustInsertRemoteNetwork(t, ctx, pool, wsID, "rn-4-b")
		_ = mustInsertConnector(t, ctx, pool, wsID, rnB, "td-4", "10.4.0.1")
		_ = mustInsertConnector(t, ctx, pool, wsID, rnB, "td-4", "10.4.0.2")

		got := rnsByID(t, wsID)
		if len(got) != 2 || got[rnA] != 0 || got[rnB] != 2 {
			t.Fatalf("want {A:0, B:2}, got %v", got)
		}
	})

	t.Run("last connector leaving empties only its network; return repopulates", func(t *testing.T) {
		wsID := mustInsertWorkspace(t, ctx, pool, "ws-tp-5")
		rnA := mustInsertRemoteNetwork(t, ctx, pool, wsID, "rn-5-a")
		rnB := mustInsertRemoteNetwork(t, ctx, pool, wsID, "rn-5-b")
		cA := mustInsertConnector(t, ctx, pool, wsID, rnA, "td-5", "10.5.0.1")
		_ = mustInsertConnector(t, ctx, pool, wsID, rnB, "td-5", "10.5.0.2")

		if got := rnsByID(t, wsID); got[rnA] != 1 || got[rnB] != 1 {
			t.Fatalf("before: want {A:1, B:1}, got %v", got)
		}

		mustSetConnectorStatus(t, ctx, pool, cA, "disconnected")
		got := rnsByID(t, wsID)
		if n, ok := got[rnA]; !ok || n != 0 {
			t.Fatalf("after last connector left: A must be present+empty, got present=%v n=%d", ok, n)
		}
		if got[rnB] != 1 {
			t.Fatalf("unrelated B must keep its connector, got %d", got[rnB])
		}

		mustSetConnectorStatus(t, ctx, pool, cA, "active")
		if got := rnsByID(t, wsID); got[rnA] != 1 || got[rnB] != 1 {
			t.Fatalf("after return: want {A:1, B:1}, got %v", got)
		}
	})

	t.Run("deleted remote network without connectors stays absent", func(t *testing.T) {
		wsID := mustInsertWorkspace(t, ctx, pool, "ws-tp-6")
		rnDel := mustInsertRemoteNetwork(t, ctx, pool, wsID, "rn-6-del")
		if _, err := pool.Exec(ctx, `UPDATE remote_networks SET status = 'deleted' WHERE id = $1`, rnDel); err != nil {
			t.Fatalf("mark rn deleted: %v", err)
		}
		if got := rnsByID(t, wsID); len(got) != 0 {
			t.Fatalf("deleted RN must not be emitted, got %v", got)
		}
	})

	t.Run("other workspaces' networks are not emitted", func(t *testing.T) {
		wsX := mustInsertWorkspace(t, ctx, pool, "ws-tp-7x")
		wsY := mustInsertWorkspace(t, ctx, pool, "ws-tp-7y")
		rnX := mustInsertRemoteNetwork(t, ctx, pool, wsX, "rn-7-x")
		_ = mustInsertRemoteNetwork(t, ctx, pool, wsY, "rn-7-y")

		got := rnsByID(t, wsX)
		if len(got) != 1 || got[rnX] != 0 {
			t.Fatalf("want only {X:0}, got %v", got)
		}
	})
}

func mustSetConnectorStatus(t *testing.T, ctx context.Context, pool *pgxpool.Pool, connectorID, status string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE connectors SET status = $1 WHERE id = $2`, status, connectorID); err != nil {
		t.Fatalf("set connector status: %v", err)
	}
}

// ── minimal DB harness (mirrors policy integration helpers) ──────────────────

func mustConnectTransportPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func uniqueTransportTestDBName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("transport_test_%d", os.Getpid())
}

func withTransportTestDBName(dsn, dbName string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func applyTransportMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			return fmt.Errorf("execute %s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

func mustInsertWorkspace(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $1, 'active', $1 || '.test') RETURNING id::text`, slug,
	).Scan(&id); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	return id
}

func mustInsertRemoteNetwork(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO remote_networks (tenant_id, name, location)
		 VALUES ($1, $2, 'home') RETURNING id::text`, tenantID, name,
	).Scan(&id); err != nil {
		t.Fatalf("insert remote_network: %v", err)
	}
	return id
}

func mustInsertConnector(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID, rnID, trustDomain, lanAddr string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO connectors (tenant_id, remote_network_id, name, status, trust_domain, lan_addr, last_heartbeat_at)
		 VALUES ($1, $2, 'test-connector', 'active', $3, $4, NOW()) RETURNING id::text`,
		tenantID, rnID, trustDomain, lanAddr,
	).Scan(&id); err != nil {
		t.Fatalf("insert connector: %v", err)
	}
	return id
}

func mustInsertActiveRelay(t *testing.T, ctx context.Context, pool *pgxpool.Pool, publicAddr string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO relays (name, status, public_addr, last_heartbeat_at)
		 VALUES ('test-relay', 'active', $1, NOW()) RETURNING id::text`, publicAddr,
	).Scan(&id); err != nil {
		t.Fatalf("insert relay: %v", err)
	}
	return id
}

func mustInsertPlacement(t *testing.T, ctx context.Context, pool *pgxpool.Pool, connectorID, relayID string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO connector_relay_placement (connector_id, relay_id, attached_at, last_confirmed, source)
		 VALUES ($1, $2, NOW(), NOW(), 'heartbeat')
		 ON CONFLICT (connector_id) DO UPDATE SET relay_id = EXCLUDED.relay_id`,
		connectorID, relayID,
	); err != nil {
		t.Fatalf("insert placement: %v", err)
	}
}
