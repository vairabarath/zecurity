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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/posture"
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
		policyStore := NewStore(testPool)
		postureStore := posture.NewStore(testPool)

		compiled, err := CompileACLSnapshot(ctx, policyStore, postureStore, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		snap := compiled.Snapshot
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
		policyStore := NewStore(testPool)
		postureStore := posture.NewStore(testPool)

		compiled, err := CompileACLSnapshot(ctx, policyStore, postureStore, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		snap := compiled.Snapshot
		wantSPIFFE := appmeta.RelaySPIFFEID(relayID)
		if snap.RelayAddr != "relay.x:9093" || snap.RelaySpiffeId != wantSPIFFE {
			t.Fatalf("relay enabled: want (%q, %q), got (%q, %q)",
				"relay.x:9093", wantSPIFFE, snap.RelayAddr, snap.RelaySpiffeId)
		}
	})

	t.Run("relay enabled via public observed ip", func(t *testing.T) {
		wsID := mustInsertWorkspace(t, ctx, testPool, "ws-public-observed")
		relayID := mustInsertActiveRelay(t, ctx, testPool, "", "8.8.8.8", "public")
		policyStore := NewStore(testPool)
		postureStore := posture.NewStore(testPool)

		compiled, err := CompileACLSnapshot(ctx, policyStore, postureStore, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		snap := compiled.Snapshot
		wantSPIFFE := appmeta.RelaySPIFFEID(relayID)
		if snap.RelayAddr != "8.8.8.8:9093" || snap.RelaySpiffeId != wantSPIFFE {
			t.Fatalf("relay observed public: want (%q, %q), got (%q, %q)",
				"8.8.8.8:9093", wantSPIFFE, snap.RelayAddr, snap.RelaySpiffeId)
		}
	})

	t.Run("relay private observed ip is not discoverable", func(t *testing.T) {
		wsID := mustInsertWorkspace(t, ctx, testPool, "ws-private-observed")
		_ = mustInsertActiveRelay(t, ctx, testPool, "", "192.168.1.71", "private")
		policyStore := NewStore(testPool)
		postureStore := posture.NewStore(testPool)

		compiled, err := CompileACLSnapshot(ctx, policyStore, postureStore, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		snap := compiled.Snapshot
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

		policyStore := NewStore(testPool)
		postureStore := posture.NewStore(testPool)

		compiled, err := CompileACLSnapshot(ctx, policyStore, postureStore, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		snap := compiled.Snapshot

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

		policyStore := NewStore(testPool)
		postureStore := posture.NewStore(testPool)

		compiled, err := CompileACLSnapshot(ctx, policyStore, postureStore, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		snap := compiled.Snapshot

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

		policyStore := NewStore(testPool)
		postureStore := posture.NewStore(testPool)

		compiled, err := CompileACLSnapshot(ctx, policyStore, postureStore, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		snap := compiled.Snapshot
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

		policyStore := NewStore(testPool)
		postureStore := posture.NewStore(testPool)

		compiled, err := CompileACLSnapshot(ctx, policyStore, postureStore, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		snap := compiled.Snapshot

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

		policyStore := NewStore(testPool)
		postureStore := posture.NewStore(testPool)

		compiled, err := CompileACLSnapshot(ctx, policyStore, postureStore, notifier, wsID)

		if err != nil {
			t.Fatalf("first compile: %v", err)
		}
		s1 := compiled.Snapshot
		if s1.RelayAddr != "" || s1.RelaySpiffeId != "" {
			t.Fatalf("first compile (no relay): want empty relay fields, got addr=%q spiffe=%q",
				s1.RelayAddr, s1.RelaySpiffeId)
		}

		_ = mustInsertActiveRelay(t, ctx, testPool, "relay.compat:9093", "", "public")

		compiled, err = CompileACLSnapshot(ctx, policyStore, postureStore, notifier, wsID)

		if err != nil {
			t.Fatalf("second compile: %v", err)
		}
		s2 := compiled.Snapshot
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

// ── PENDING-16 Phase 5: posture gating through the Resource Policy ───────────

// TestCompileACLSnapshot_ResourcePolicyGating proves the Phase 5 verification
// matrix survives full ACL snapshot generation, not just a direct applyPosture
// call.
//
// Before Phase 5 this file had no posture seeding at all: every subtest compiled
// with zero device profiles, so applyPosture always took the ungated branch and
// the gated path was never exercised end to end. These cases close that gap.
//
// Each case builds a complete authorization chain -- workspace, group, user,
// device, remote network + connector, resource, access rule -- then attaches a
// Resource Policy and asserts on the compiled snapshot's AllowedSpiffeIds.
func TestCompileACLSnapshot_ResourcePolicyGating(t *testing.T) {
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
		t.Fatalf("build test DSN: %v", err)
	}
	testPool := mustConnectTestPool(t, ctx, testDBDSN)
	defer testPool.Close()
	applyAllMigrations(ctx, testPool)

	notifier := NewNotifier(NewSnapshotCache())

	tests := []struct {
		name string
		// profileSatisfied is nil for "attach no profiles at all".
		profileSatisfied  *bool
		secondSatisfied   *bool
		wantDeviceAllowed bool
	}{
		{
			name:              "policy with zero profiles is Any Device",
			profileSatisfied:  nil,
			wantDeviceAllowed: true,
		},
		{
			name:              "one profile satisfied allows the device",
			profileSatisfied:  boolPtr(true),
			wantDeviceAllowed: true,
		},
		{
			name:              "one profile unsatisfied denies the device",
			profileSatisfied:  boolPtr(false),
			wantDeviceAllowed: false,
		},
		{
			name:              "two profiles, second satisfied allows (OR)",
			profileSatisfied:  boolPtr(false),
			secondSatisfied:   boolPtr(true),
			wantDeviceAllowed: true,
		},
		{
			name:              "two profiles, both unsatisfied denies",
			profileSatisfied:  boolPtr(false),
			secondSatisfied:   boolPtr(false),
			wantDeviceAllowed: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Each case gets its own workspace so the compiled snapshots cannot
			// interfere with one another.
			slug := fmt.Sprintf("gating-%d", time.Now().UnixNano())
			wsID := mustInsertWorkspace(t, ctx, testPool, slug)
			groupID := mustInsertGroup(t, ctx, testPool, wsID, "gating-group")
			userID := mustInsertUser(t, ctx, testPool, wsID)
			mustAddGroupMember(t, ctx, testPool, groupID, userID)

			spiffeID := "spiffe://gating.test/client/" + userID
			deviceID := mustInsertClientDevice(t, ctx, testPool, wsID, userID, spiffeID)

			rnID, _ := mustInsertRNWithConnector(t, ctx, testPool, wsID, "gating-rn", slug+".test", "10.9.0.10:9092")
			resourceID := mustInsertResource(t, ctx, testPool, wsID, rnID, "gated-app", "10.9.0.1", 8443)
			mustAssignResourceToGroup(t, ctx, testPool, wsID, resourceID, groupID)

			postureStore := posture.NewStore(testPool)
			wsUUID := mustParseUUID(t, wsID)
			resourceUUID := mustParseUUID(t, resourceID)
			deviceUUID := mustParseUUID(t, deviceID)

			policy, err := postureStore.CreateResourcePolicy(ctx, wsUUID, "Gating Policy")
			if err != nil {
				t.Fatalf("create resource policy: %v", err)
			}
			if err := postureStore.AssignResourcePolicy(ctx, wsUUID, resourceUUID, policy.ID); err != nil {
				t.Fatalf("assign resource policy: %v", err)
			}

			// A posture report is the provenance every evaluation needs; without a
			// fresh received_at applyPosture fails closed regardless of Satisfied.
			reportID := mustInsertPostureReport(t, ctx, testPool, wsID, deviceID)

			attach := func(name string, satisfied bool) {
				profile, err := postureStore.CreateProfile(ctx, wsUUID, name, true)
				if err != nil {
					t.Fatalf("create profile %s: %v", name, err)
				}
				if err := postureStore.AddProfileToPolicy(ctx, wsUUID, policy.ID, profile.ID); err != nil {
					t.Fatalf("attach profile %s: %v", name, err)
				}
				mustUpsertEvaluation(t, ctx, testPool, wsID, deviceID, profile.ID.String(), reportID, satisfied)
				_ = deviceUUID
			}

			if tt.profileSatisfied != nil {
				attach("Gating Profile A", *tt.profileSatisfied)
			}
			if tt.secondSatisfied != nil {
				attach("Gating Profile B", *tt.secondSatisfied)
			}

			policyStore := NewStore(testPool)
			compiled, err := CompileACLSnapshot(ctx, policyStore, postureStore, notifier, wsID)
			if err != nil {
				t.Fatalf("CompileACLSnapshot: %v", err)
			}

			var entry *clientv1.ACLEntry
			for _, e := range compiled.Snapshot.Entries {
				if e.ResourceId == resourceID {
					entry = e
				}
			}
			if entry == nil {
				t.Fatalf("resource %s missing from snapshot entries %+v", resourceID, compiled.Snapshot.Entries)
			}

			allowed := false
			for _, id := range entry.AllowedSpiffeIds {
				if id == spiffeID {
					allowed = true
				}
			}
			if allowed != tt.wantDeviceAllowed {
				t.Fatalf("device allowed = %v, want %v (allowed_spiffe_ids = %v)",
					allowed, tt.wantDeviceAllowed, entry.AllowedSpiffeIds)
			}

			// Routing must survive posture gating untouched: the entry is still
			// present and still carries its route, even when the device is denied.
			if entry.RouteType != "connector" {
				t.Fatalf("route_type = %q, want connector", entry.RouteType)
			}
			if entry.RemoteNetworkId != rnID {
				t.Fatalf("remote_network_id = %q, want %q", entry.RemoteNetworkId, rnID)
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

// mustInsertPostureReport seeds a posture report received just now, so
// evaluations built from it are inside posture.MaxReportAge.
func mustInsertPostureReport(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, deviceID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO device_posture_reports (
		     report_id, device_id, workspace_id, client_version, os_info, reported_at, received_at
		 )
		 VALUES (gen_random_uuid()::text, $1, $2, 'test-client', '{"name":"linux"}'::jsonb, NOW(), NOW())
		 RETURNING id::text`,
		deviceID, workspaceID,
	).Scan(&id); err != nil {
		t.Fatalf("insert posture report: %v", err)
	}
	return id
}

// mustUpsertEvaluation seeds a latest-revision evaluation for (device, profile).
// profile_revision is read from the profile so the freshness/revision checks in
// applyPosture see a current result.
func mustUpsertEvaluation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, deviceID, profileID, reportID string, satisfied bool) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO device_profile_evaluations (
		     device_id, profile_id, workspace_id, satisfied, profile_revision, report_id
		 )
		 SELECT $1, p.id, $2, $3, p.revision, $5
		   FROM device_profiles p
		  WHERE p.id = $4
		 ON CONFLICT (device_id, profile_id) DO UPDATE
		    SET satisfied = EXCLUDED.satisfied,
		        profile_revision = EXCLUDED.profile_revision,
		        report_id = EXCLUDED.report_id`,
		deviceID, workspaceID, satisfied, profileID, reportID,
	); err != nil {
		t.Fatalf("upsert evaluation: %v", err)
	}
}

// mustParseUUID converts a ::text id from the seed helpers into a uuid.UUID for
// the posture store, which is uuid-typed throughout.
func mustParseUUID(t *testing.T, id string) uuid.UUID {
	t.Helper()
	parsed, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("parse uuid %q: %v", id, err)
	}
	return parsed
}

// TestCompileACLSnapshot_LegacyBindingNoLongerGates documents the one accepted
// behaviour change of the PENDING-16 Phase 5 cutover.
//
// A resource carrying a legacy enforce-mode resource_profile_bindings row but no
// Resource Policy is now UNGATED. Before the cutover that row gated the resource;
// the compiler no longer reads the legacy table, and a resource with no policy
// resolves to zero profiles, which applyPosture treats as Any Device.
//
// This is a deliberate widening, safe here only because the project is
// pre-production and no such row exists in any real database. The test exists so
// the consequence is asserted rather than discovered: if it ever starts mattering,
// this is the test that explains why.
func TestCompileACLSnapshot_LegacyBindingNoLongerGates(t *testing.T) {
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
		t.Fatalf("build test DSN: %v", err)
	}
	testPool := mustConnectTestPool(t, ctx, testDBDSN)
	defer testPool.Close()
	applyAllMigrations(ctx, testPool)

	slug := fmt.Sprintf("legacy-gate-%d", time.Now().UnixNano())
	wsID := mustInsertWorkspace(t, ctx, testPool, slug)
	groupID := mustInsertGroup(t, ctx, testPool, wsID, "legacy-group")
	userID := mustInsertUser(t, ctx, testPool, wsID)
	mustAddGroupMember(t, ctx, testPool, groupID, userID)

	spiffeID := "spiffe://legacy.test/client/" + userID
	deviceID := mustInsertClientDevice(t, ctx, testPool, wsID, userID, spiffeID)

	rnID, _ := mustInsertRNWithConnector(t, ctx, testPool, wsID, "legacy-rn", slug+".test", "10.9.1.10:9092")
	resourceID := mustInsertResource(t, ctx, testPool, wsID, rnID, "legacy-app", "10.9.1.1", 8443)
	mustAssignResourceToGroup(t, ctx, testPool, wsID, resourceID, groupID)

	postureStore := posture.NewStore(testPool)
	wsUUID := mustParseUUID(t, wsID)

	// An enforce-mode profile the device does NOT satisfy, bound the legacy way
	// and NOT attached to any Resource Policy. The resource keeps a NULL policy.
	profile, err := postureStore.CreateProfile(ctx, wsUUID, "Legacy Enforce", true)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO device_profile_requirements (profile_id, check_id, allow_unsupported)
		 VALUES ($1, 'legacy.synthetic.check', FALSE)`,
		profile.ID,
	); err != nil {
		t.Fatalf("add requirement: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE device_profiles SET mode = 'enforce' WHERE id = $1`, profile.ID,
	); err != nil {
		t.Fatalf("set enforce mode: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO resource_profile_bindings (resource_id, profile_id, workspace_id)
		 VALUES ($1, $2, $3)`,
		resourceID, profile.ID, wsID,
	); err != nil {
		t.Fatalf("insert legacy binding: %v", err)
	}

	reportID := mustInsertPostureReport(t, ctx, testPool, wsID, deviceID)
	mustUpsertEvaluation(t, ctx, testPool, wsID, deviceID, profile.ID.String(), reportID, false)

	// Sanity: the legacy row really is there and the resource really has no policy.
	var legacyCount int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM resource_profile_bindings WHERE resource_id = $1`, resourceID,
	).Scan(&legacyCount); err != nil {
		t.Fatalf("count legacy bindings: %v", err)
	}
	var hasPolicy bool
	if err := testPool.QueryRow(ctx,
		`SELECT device_resource_policy_id IS NOT NULL FROM resources WHERE id = $1`, resourceID,
	).Scan(&hasPolicy); err != nil {
		t.Fatalf("read policy assignment: %v", err)
	}
	if legacyCount != 1 || hasPolicy {
		t.Fatalf("fixture wrong: legacyCount=%d hasPolicy=%v, want 1 and false", legacyCount, hasPolicy)
	}

	compiled, err := CompileACLSnapshot(ctx, NewStore(testPool), postureStore, NewNotifier(NewSnapshotCache()), wsID)
	if err != nil {
		t.Fatalf("CompileACLSnapshot: %v", err)
	}

	var entry *clientv1.ACLEntry
	for _, e := range compiled.Snapshot.Entries {
		if e.ResourceId == resourceID {
			entry = e
		}
	}
	if entry == nil {
		t.Fatalf("resource missing from snapshot: %+v", compiled.Snapshot.Entries)
	}

	allowed := false
	for _, id := range entry.AllowedSpiffeIds {
		if id == spiffeID {
			allowed = true
		}
	}
	if !allowed {
		t.Fatalf("device denied (allowed=%v) -- the compiler is still honouring the legacy binding; "+
			"after Phase 5 a resource with no Resource Policy must be Any Device", entry.AllowedSpiffeIds)
	}

	// The snapshot must also carry no posture-driven expiry, since nothing gates.
	if !compiled.ValidUntil.IsZero() {
		t.Fatalf("ValidUntil = %v, want zero (no posture-gated entry)", compiled.ValidUntil)
	}
}
