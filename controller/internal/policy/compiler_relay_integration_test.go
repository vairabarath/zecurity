package policy

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// TestCompileACLSnapshot_RelayDiscovery verifies the five contract bullets
// from Sprint 10.2 M2 Phase 1: relay-disabled, relay-enabled, active-connector
// populates ConnectorId+ConnectorSpiffe, no-active-connector leaves them
// empty, and backwards compatibility of the non-relay fields.
//
// Note: the relay query now uses a per-connector join on
// connector_relay_placement → relays instead of a global ORDER BY/LIMIT 1.
// Tests that expect relay fields to be populated must also insert a placement
// row linking the connector to the relay.
//
// Requires PKI_TEST_DATABASE_URL pointing at a Postgres role with CREATE
// DATABASE privilege; otherwise skips.
func TestCompileACLSnapshot_RelayDiscovery(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	dbName := uniqueTestDBName(t)

	adminPool := mustConnectTestPool(t, ctx, adminDSN)
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

	testDBDSN, err := withTestDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test database dsn: %v", err)
	}

	testPool := mustConnectTestPool(t, ctx, testDBDSN)
	defer testPool.Close()

	if err := applyAllMigrations(ctx, testPool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	notifier := NewNotifier(NewSnapshotCache())

	t.Run("relay disabled", func(t *testing.T) {
		// No row in the relays table → ACL snapshot's relay fields stay empty.
		wsID := mustInsertWorkspace(t, ctx, testPool, "ws-disabled")
		store := NewStore(testPool)
		snap, err := CompileACLSnapshot(ctx, store, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if snap.RelayAddr != "" || snap.RelaySpiffeId != "" {
			t.Fatalf("relay disabled: want both empty, got addr=%q spiffe=%q",
				snap.RelayAddr, snap.RelaySpiffeId)
		}
	})

	t.Run("relay enabled via public_addr", func(t *testing.T) {
		// Active relay row with public_addr set → ACL emits that address and a
		// SPIFFE ID derived from the row's UUID.
		wsID := mustInsertWorkspace(t, ctx, testPool, "ws-enabled")
		relayID := mustInsertActiveRelay(t, ctx, testPool, "relay.x:9093", "", "public")
		store := NewStore(testPool)
		snap, err := CompileACLSnapshot(ctx, store, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		wantSPIFFE := appmeta.RelaySPIFFEID(relayID)
		if snap.RelayAddr != "relay.x:9093" || snap.RelaySpiffeId != wantSPIFFE {
			t.Fatalf("relay enabled: want (%q, %q), got (%q, %q)",
				"relay.x:9093", wantSPIFFE, snap.RelayAddr, snap.RelaySpiffeId)
		}
	})

	t.Run("relay enabled via public observed ip", func(t *testing.T) {
		wsID := mustInsertWorkspace(t, ctx, testPool, "ws-public-observed")
		relayID := mustInsertActiveRelay(t, ctx, testPool, "", "8.8.8.8", "public")
		store := NewStore(testPool)
		snap, err := CompileACLSnapshot(ctx, store, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		wantSPIFFE := appmeta.RelaySPIFFEID(relayID)
		if snap.RelayAddr != "8.8.8.8:9093" || snap.RelaySpiffeId != wantSPIFFE {
			t.Fatalf("relay observed public: want (%q, %q), got (%q, %q)",
				"8.8.8.8:9093", wantSPIFFE, snap.RelayAddr, snap.RelaySpiffeId)
		}
	})

	t.Run("relay private observed ip is not discoverable", func(t *testing.T) {
		wsID := mustInsertWorkspace(t, ctx, testPool, "ws-private-observed")
		_ = mustInsertActiveRelay(t, ctx, testPool, "", "192.168.1.71", "private")
		store := NewStore(testPool)
		snap, err := CompileACLSnapshot(ctx, store, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if snap.RelayAddr != "" || snap.RelaySpiffeId != "" {
			t.Fatalf("private observed relay should not be discoverable, got addr=%q spiffe=%q",
				snap.RelayAddr, snap.RelaySpiffeId)
		}
	})

	t.Run("multi-RN: each entry routes to its own connector", func(t *testing.T) {
		// Two remote networks, two connectors, two resources (one per RN).
		// Snapshot must emit two ACLRemoteNetwork entries each referencing the
		// correct connector, and each ACLEntry must carry the correct remote_network_id.
		wsID := mustInsertWorkspace(t, ctx, testPool, "ws-multi-rn")
		grpID := mustInsertGroup(t, ctx, testPool, wsID, "grp-multi")
		userID := mustInsertUser(t, ctx, testPool, wsID)
		mustAddGroupMember(t, ctx, testPool, grpID, userID)
		devID := mustInsertClientDevice(t, ctx, testPool, wsID, userID, "spiffe://td/client/dev1")

		rnID1, connID1 := mustInsertRNWithConnector(t, ctx, testPool, wsID, "rn-one", "td-one", "10.1.0.1")
		rnID2, connID2 := mustInsertRNWithConnector(t, ctx, testPool, wsID, "rn-two", "td-two", "10.2.0.1")
		r1ID := mustInsertResource(t, ctx, testPool, wsID, rnID1, "res-one", "10.1.0.10", 80)
		r2ID := mustInsertResource(t, ctx, testPool, wsID, rnID2, "res-two", "10.2.0.10", 80)
		mustAssignResourceToGroup(t, ctx, testPool, wsID, r1ID, grpID)
		mustAssignResourceToGroup(t, ctx, testPool, wsID, r2ID, grpID)
		_ = devID

		store := NewStore(testPool)
		snap, err := CompileACLSnapshot(ctx, store, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}

		if len(snap.RemoteNetworks) != 2 {
			t.Fatalf("want 2 remote_networks, got %d", len(snap.RemoteNetworks))
		}

		rnByID := make(map[string]*clientv1.ACLRemoteNetwork)
		for _, rn := range snap.RemoteNetworks {
			rnByID[rn.RemoteNetworkId] = rn
		}

		rn1 := rnByID[rnID1]
		if rn1 == nil || len(rn1.Connectors) != 1 || rn1.Connectors[0].ConnectorId != connID1 {
			t.Fatalf("rn1 connector mismatch: %+v", rn1)
		}
		if rn1.Connectors[0].ConnectorTunnelAddr != "10.1.0.1:9092" {
			t.Fatalf("rn1 tunnel addr: want 10.1.0.1:9092, got %q", rn1.Connectors[0].ConnectorTunnelAddr)
		}

		rn2 := rnByID[rnID2]
		if rn2 == nil || len(rn2.Connectors) != 1 || rn2.Connectors[0].ConnectorId != connID2 {
			t.Fatalf("rn2 connector mismatch: %+v", rn2)
		}
		if rn2.Connectors[0].ConnectorTunnelAddr != "10.2.0.1:9092" {
			t.Fatalf("rn2 tunnel addr: want 10.2.0.1:9092, got %q", rn2.Connectors[0].ConnectorTunnelAddr)
		}

		entryByResource := make(map[string]*clientv1.ACLEntry)
		for _, e := range snap.Entries {
			entryByResource[e.ResourceId] = e
		}
		if entryByResource[r1ID].RemoteNetworkId != rnID1 {
			t.Fatalf("r1 remote_network_id: want %q, got %q", rnID1, entryByResource[r1ID].RemoteNetworkId)
		}
		if entryByResource[r2ID].RemoteNetworkId != rnID2 {
			t.Fatalf("r2 remote_network_id: want %q, got %q", rnID2, entryByResource[r2ID].RemoteNetworkId)
		}
	})

	t.Run("RN with no active connector: entry present, connectors empty", func(t *testing.T) {
		// A resource belongs to a remote network that has no active connector.
		// Compilation must not fail. The ACLRemoteNetwork entry must be present
		// with an empty connectors list so clients can report "unavailable".
		wsID := mustInsertWorkspace(t, ctx, testPool, "ws-no-conn-rn")
		grpID := mustInsertGroup(t, ctx, testPool, wsID, "grp-no-conn")
		userID := mustInsertUser(t, ctx, testPool, wsID)
		mustAddGroupMember(t, ctx, testPool, grpID, userID)
		_ = mustInsertClientDevice(t, ctx, testPool, wsID, userID, "spiffe://td/client/dev2")

		rnID := mustInsertRemoteNetwork(t, ctx, testPool, wsID, "rn-offline")
		// No connector inserted for this RN.
		rID := mustInsertResource(t, ctx, testPool, wsID, rnID, "res-offline", "10.3.0.1", 443)
		mustAssignResourceToGroup(t, ctx, testPool, wsID, rID, grpID)

		store := NewStore(testPool)
		snap, err := CompileACLSnapshot(ctx, store, notifier, wsID)
		if err != nil {
			t.Fatalf("compile must not fail when connector absent: %v", err)
		}

		if len(snap.RemoteNetworks) != 1 {
			t.Fatalf("want 1 remote_network, got %d", len(snap.RemoteNetworks))
		}
		rn := snap.RemoteNetworks[0]
		if rn.RemoteNetworkId != rnID {
			t.Fatalf("remote_network_id: want %q, got %q", rnID, rn.RemoteNetworkId)
		}
		if len(rn.Connectors) != 0 {
			t.Fatalf("connectors must be empty, got %d", len(rn.Connectors))
		}
		if len(snap.Entries) != 1 || snap.Entries[0].ResourceId != rID {
			t.Fatalf("entry must still be present: %+v", snap.Entries)
		}
	})

	// ── Gap 1 regression tests ──────────────────────────────────────────────

	t.Run("per-connector: connector with placement gets relay coords", func(t *testing.T) {
		// A connector linked to a relay via connector_relay_placement must carry
		// that relay's addr and SPIFFE ID on its ACLConnector entry.
		// Old code (global relay lookup) would return the workspace relay for ALL
		// connectors; new code reads the per-connector placement JOIN.
		wsID := mustInsertWorkspace(t, ctx, testPool, "ws-per-conn-relay")
		grpID := mustInsertGroup(t, ctx, testPool, wsID, "grp-per-conn")
		userID := mustInsertUser(t, ctx, testPool, wsID)
		mustAddGroupMember(t, ctx, testPool, grpID, userID)
		_ = mustInsertClientDevice(t, ctx, testPool, wsID, userID, "spiffe://td/client/dev-pcr")

		rnID, connID := mustInsertRNWithConnector(t, ctx, testPool, wsID, "rn-per-conn", "td-per-conn", "10.10.0.1")
		rID := mustInsertResource(t, ctx, testPool, wsID, rnID, "res-per-conn", "10.10.0.10", 80)
		mustAssignResourceToGroup(t, ctx, testPool, wsID, rID, grpID)

		relayID := mustInsertActiveRelay(t, ctx, testPool, "relay.per:9093", "", "public")
		mustInsertConnectorRelayPlacement(t, ctx, testPool, connID, relayID)

		store := NewStore(testPool)
		snap, err := CompileACLSnapshot(ctx, store, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if len(snap.RemoteNetworks) != 1 || len(snap.RemoteNetworks[0].Connectors) != 1 {
			t.Fatalf("unexpected remote_networks: %+v", snap.RemoteNetworks)
		}
		conn := snap.RemoteNetworks[0].Connectors[0]
		if conn.ConnectorId != connID {
			t.Fatalf("connector_id mismatch: want %q, got %q", connID, conn.ConnectorId)
		}
		if conn.RelayAddr != "relay.per:9093" {
			t.Fatalf("relay_addr: want %q, got %q", "relay.per:9093", conn.RelayAddr)
		}
		wantRelaySpiffe := appmeta.RelaySPIFFEID(relayID)
		if conn.RelaySpiffeId != wantRelaySpiffe {
			t.Fatalf("relay_spiffe_id: want %q, got %q", wantRelaySpiffe, conn.RelaySpiffeId)
		}
	})

	t.Run("per-connector: connector without placement gets empty relay coords", func(t *testing.T) {
		// Two connectors in separate remote networks. Only one has a placement.
		// The unplaced connector must get empty relay fields even though an active
		// relay exists in the system. Old global-relay code would give BOTH connectors
		// the same relay addr; new per-connector JOIN gives each only its own.
		wsID := mustInsertWorkspace(t, ctx, testPool, "ws-mixed-relay")
		grpID := mustInsertGroup(t, ctx, testPool, wsID, "grp-mixed")
		userID := mustInsertUser(t, ctx, testPool, wsID)
		mustAddGroupMember(t, ctx, testPool, grpID, userID)
		_ = mustInsertClientDevice(t, ctx, testPool, wsID, userID, "spiffe://td/client/dev-mr")

		rnID1, connID1 := mustInsertRNWithConnector(t, ctx, testPool, wsID, "rn-with-relay", "td-with", "10.20.0.1")
		rnID2, _ := mustInsertRNWithConnector(t, ctx, testPool, wsID, "rn-no-relay", "td-without", "10.20.0.2")
		r1ID := mustInsertResource(t, ctx, testPool, wsID, rnID1, "res-with-relay", "10.20.0.10", 80)
		r2ID := mustInsertResource(t, ctx, testPool, wsID, rnID2, "res-no-relay", "10.20.0.20", 80)
		mustAssignResourceToGroup(t, ctx, testPool, wsID, r1ID, grpID)
		mustAssignResourceToGroup(t, ctx, testPool, wsID, r2ID, grpID)

		relayID := mustInsertActiveRelay(t, ctx, testPool, "relay.mixed:9093", "", "public")
		mustInsertConnectorRelayPlacement(t, ctx, testPool, connID1, relayID)

		store := NewStore(testPool)
		snap, err := CompileACLSnapshot(ctx, store, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}

		byRN := make(map[string]*clientv1.ACLConnector)
		for _, rn := range snap.RemoteNetworks {
			if len(rn.Connectors) == 1 {
				byRN[rn.RemoteNetworkId] = rn.Connectors[0]
			}
		}

		connWith := byRN[rnID1]
		if connWith == nil {
			t.Fatalf("connector for rn %q missing from snapshot", rnID1)
		}
		if connWith.RelayAddr == "" || connWith.RelaySpiffeId == "" {
			t.Fatalf("conn with placement: want non-empty relay fields, got addr=%q spiffe=%q",
				connWith.RelayAddr, connWith.RelaySpiffeId)
		}

		connWithout := byRN[rnID2]
		if connWithout == nil {
			t.Fatalf("connector for rn %q missing from snapshot", rnID2)
		}
		if connWithout.RelayAddr != "" || connWithout.RelaySpiffeId != "" {
			t.Fatalf("conn without placement: want empty relay fields, got addr=%q spiffe=%q",
				connWithout.RelayAddr, connWithout.RelaySpiffeId)
		}
	})

	t.Run("entries unaffected by relay presence", func(t *testing.T) {
		// Confirm entries and remote_networks don't drift when a relay row is added.
		wsID := mustInsertWorkspace(t, ctx, testPool, "ws-compat")
		grpID := mustInsertGroup(t, ctx, testPool, wsID, "grp-compat")
		userID := mustInsertUser(t, ctx, testPool, wsID)
		mustAddGroupMember(t, ctx, testPool, grpID, userID)
		_ = mustInsertClientDevice(t, ctx, testPool, wsID, userID, "spiffe://td/client/dev3")

		rnID, _ := mustInsertRNWithConnector(t, ctx, testPool, wsID, "rn-compat", "td-c", "10.0.0.50")
		rID := mustInsertResource(t, ctx, testPool, wsID, rnID, "res-compat", "10.0.0.60", 8080)
		mustAssignResourceToGroup(t, ctx, testPool, wsID, rID, grpID)

		store := NewStore(testPool)
		s1, err := CompileACLSnapshot(ctx, store, notifier, wsID)
		if err != nil {
			t.Fatalf("first compile: %v", err)
		}
		if s1.RelayAddr != "" || s1.RelaySpiffeId != "" {
			t.Fatalf("first compile (no relay): want empty relay fields, got addr=%q spiffe=%q",
				s1.RelayAddr, s1.RelaySpiffeId)
		}

		_ = mustInsertActiveRelay(t, ctx, testPool, "relay.compat:9093", "", "public")

		s2, err := CompileACLSnapshot(ctx, store, notifier, wsID)
		if err != nil {
			t.Fatalf("second compile: %v", err)
		}

		if len(s1.Entries) != len(s2.Entries) {
			t.Fatalf("Entries len drift: %d vs %d", len(s1.Entries), len(s2.Entries))
		}
		if len(s1.RemoteNetworks) != len(s2.RemoteNetworks) {
			t.Fatalf("RemoteNetworks len drift: %d vs %d", len(s1.RemoteNetworks), len(s2.RemoteNetworks))
		}
		if s2.RelayAddr == "" || s2.RelaySpiffeId == "" {
			t.Fatalf("second compile should have relay fields, got addr=%q spiffe=%q",
				s2.RelayAddr, s2.RelaySpiffeId)
		}
		if s1.RelayAddr != "" {
			t.Fatalf("first compile should have no relay, got %q", s1.RelayAddr)
		}
	})
}

// ── Test helpers ────────────────────────────────────────────────────────────

func mustConnectTestPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
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

func uniqueTestDBName(t *testing.T) string {
	t.Helper()
	name := strings.ToLower(t.Name())
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, " ", "_")
	return fmt.Sprintf("%s_%d_%d", name, os.Getpid(), time.Now().UnixNano())
}

func withTestDBName(dsn, dbName string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

// applyAllMigrations executes every controller/migrations/*.sql in numeric
// order. The compiler queries hit tables across migrations 001..012; applying
// the full set is the simplest robust approach.
func applyAllMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	migrationsDir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		return fmt.Errorf("resolve migrations dir: %w", err)
	}
	files, err := filepath.Glob(filepath.Join(migrationsDir, "*.sql"))
	if err != nil {
		return fmt.Errorf("glob migrations: %w", err)
	}
	sort.Strings(files)
	for _, f := range files {
		sqlBytes, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("execute %s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

// mustInsertWorkspace creates a workspace row keyed by slug; returns the id.
// trust_domain is derived from the slug so it stays unique across test cases.
func mustInsertWorkspace(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $1, 'active', $1 || '.test')
		 RETURNING id::text`,
		slug,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert workspace %q: %v", slug, err)
	}
	return id
}

// mustInsertActiveRelay creates a relays row with status='active', a recent
// heartbeat, and the supplied address metadata. Returns the relay UUID.
// observedIP may be empty when only public_addr is set; addressScope must be
// one of 'public'/'private' (or empty when no observed_ip is provided).
// The relay row is deleted in t.Cleanup so it does not contaminate later subtests.
func mustInsertActiveRelay(t *testing.T, ctx context.Context, pool *pgxpool.Pool, publicAddr, observedIP, addressScope string) string {
	t.Helper()
	var (
		pubArg   any
		ipArg    any
		scopeArg any
		portArg  any
	)
	if publicAddr != "" {
		pubArg = publicAddr
	}
	if observedIP != "" {
		ipArg = observedIP
		portArg = 9093
		if addressScope != "" {
			scopeArg = addressScope
		}
	}
	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO relays (name, status, public_addr, observed_ip, observed_port, address_scope, last_heartbeat_at)
		 VALUES ('test-relay', 'active', $1, $2::inet, $3, $4, NOW())
		 RETURNING id::text`,
		pubArg, ipArg, portArg, scopeArg,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert relay: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM relays WHERE id = $1`, id); err != nil {
			t.Logf("cleanup relay %s: %v", id, err)
		}
	})
	return id
}

// mustInsertRemoteNetwork creates a remote_networks row and returns its UUID.
func mustInsertRemoteNetwork(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO remote_networks (tenant_id, name, location)
		 VALUES ($1, $2, 'home')
		 RETURNING id::text`,
		tenantID, name,
	).Scan(&id); err != nil {
		t.Fatalf("insert remote_network %q: %v", name, err)
	}
	return id
}

// mustInsertRNWithConnector creates a remote_network + active connector in one call.
// Returns (remoteNetworkID, connectorID).
func mustInsertRNWithConnector(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID, rnName, trustDomain, lanAddr string) (string, string) {
	t.Helper()
	rnID := mustInsertRemoteNetwork(t, ctx, pool, tenantID, rnName)
	var connID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO connectors (tenant_id, remote_network_id, name, status, trust_domain, lan_addr, last_heartbeat_at)
		 VALUES ($1, $2, 'test-connector', 'active', $3, $4, NOW())
		 RETURNING id::text`,
		tenantID, rnID, trustDomain, lanAddr,
	).Scan(&connID); err != nil {
		t.Fatalf("insert connector for rn %q: %v", rnName, err)
	}
	return rnID, connID
}

// mustInsertGroup creates a groups row and returns its UUID.
func mustInsertGroup(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO groups (workspace_id, name)
		 VALUES ($1, $2)
		 RETURNING id::text`,
		workspaceID, name,
	).Scan(&id); err != nil {
		t.Fatalf("insert group %q: %v", name, err)
	}
	return id
}

// mustInsertUser creates a users row and returns its UUID.
func mustInsertUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (tenant_id, email, provider, provider_sub, role)
		 VALUES ($1, gen_random_uuid()::text || '@test.example', 'test', gen_random_uuid()::text, 'member')
		 RETURNING id::text`,
		workspaceID,
	).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// mustAddGroupMember adds a user to a group.
func mustAddGroupMember(t *testing.T, ctx context.Context, pool *pgxpool.Pool, groupID, userID string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`,
		groupID, userID,
	); err != nil {
		t.Fatalf("add group member: %v", err)
	}
}

// mustInsertClientDevice creates a client_devices row with a SPIFFE ID.
func mustInsertClientDevice(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, userID, spiffeID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO client_devices (workspace_id, user_id, name, os, spiffe_id)
		 VALUES ($1, $2, 'test-device', 'linux', $3)
		 RETURNING id::text`,
		workspaceID, userID, spiffeID,
	).Scan(&id); err != nil {
		t.Fatalf("insert client_device: %v", err)
	}
	return id
}

// mustInsertResource creates a resources row in the given remote network.
// Status 'unprotected' maps to route_type 'connector' without requiring a shield.
func mustInsertResource(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, remoteNetworkID, name, host string, port int) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO resources (tenant_id, remote_network_id, name, host, port_from, protocol, status)
		 VALUES ($1, $2, $3, $4, $5, 'tcp', 'unprotected')
		 RETURNING id::text`,
		workspaceID, remoteNetworkID, name, host, port,
	).Scan(&id); err != nil {
		t.Fatalf("insert resource %q: %v", name, err)
	}
	return id
}

// mustInsertConnectorRelayPlacement links a connector to a relay in
// connector_relay_placement. Required for Gap 1 per-connector relay tests.
func mustInsertConnectorRelayPlacement(t *testing.T, ctx context.Context, pool *pgxpool.Pool, connectorID, relayID string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO connector_relay_placement (connector_id, relay_id, attached_at, last_confirmed, source)
		 VALUES ($1, $2, NOW(), NOW(), 'heartbeat')
		 ON CONFLICT (connector_id) DO UPDATE SET relay_id = EXCLUDED.relay_id`,
		connectorID, relayID,
	); err != nil {
		t.Fatalf("insert connector_relay_placement: %v", err)
	}
}

// mustAssignResourceToGroup creates an enabled access_rules row.
func mustAssignResourceToGroup(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, resourceID, groupID string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO access_rules (workspace_id, resource_id, group_id, enabled)
		 VALUES ($1, $2, $3, TRUE)
		 ON CONFLICT (resource_id, group_id) DO UPDATE SET enabled = TRUE`,
		workspaceID, resourceID, groupID,
	); err != nil {
		t.Fatalf("assign resource to group: %v", err)
	}
}
