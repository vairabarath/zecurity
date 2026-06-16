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
)

// TestCompileACLSnapshot_RelayDiscovery verifies the five contract bullets
// from Sprint 10.2 M2 Phase 1: relay-disabled, relay-enabled, active-connector
// populates ConnectorId+ConnectorSpiffe, no-active-connector leaves them
// empty, and backwards compatibility of the non-relay fields.
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
		wsID := mustInsertWorkspace(t, ctx, testPool, "ws-disabled")
		store := NewStore(testPool, "", "")
		snap, err := CompileACLSnapshot(ctx, store, notifier, testPool, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if snap.RelayAddr != "" || snap.RelaySpiffeId != "" {
			t.Fatalf("relay disabled: want both empty, got addr=%q spiffe=%q",
				snap.RelayAddr, snap.RelaySpiffeId)
		}
	})

	t.Run("relay enabled", func(t *testing.T) {
		wsID := mustInsertWorkspace(t, ctx, testPool, "ws-enabled")
		const addr, spi = "relay.x:9093", "spiffe://td/relay/r1"
		store := NewStore(testPool, addr, spi)
		snap, err := CompileACLSnapshot(ctx, store, notifier, testPool, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if snap.RelayAddr != addr || snap.RelaySpiffeId != spi {
			t.Fatalf("relay enabled: want (%q, %q), got (%q, %q)",
				addr, spi, snap.RelayAddr, snap.RelaySpiffeId)
		}
	})

	t.Run("active connector present", func(t *testing.T) {
		wsID := mustInsertWorkspace(t, ctx, testPool, "ws-conn")
		connID := mustInsertActiveConnector(t, ctx, testPool, wsID, "td-a", "10.0.0.5")
		store := NewStore(testPool, "", "")
		snap, err := CompileACLSnapshot(ctx, store, notifier, testPool, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if snap.ConnectorId != connID {
			t.Fatalf("ConnectorId: want %q, got %q", connID, snap.ConnectorId)
		}
		wantSPIFFE := "spiffe://td-a/connector/" + connID
		if snap.ConnectorSpiffe != wantSPIFFE {
			t.Fatalf("ConnectorSpiffe: want %q, got %q", wantSPIFFE, snap.ConnectorSpiffe)
		}
		if snap.ConnectorTunnelAddr != "10.0.0.5:9092" {
			t.Fatalf("ConnectorTunnelAddr: want %q, got %q", "10.0.0.5:9092", snap.ConnectorTunnelAddr)
		}
	})

	t.Run("no active connector", func(t *testing.T) {
		wsID := mustInsertWorkspace(t, ctx, testPool, "ws-no-conn")
		store := NewStore(testPool, "", "")
		snap, err := CompileACLSnapshot(ctx, store, notifier, testPool, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if snap.ConnectorId != "" || snap.ConnectorSpiffe != "" {
			t.Fatalf("no active connector: want both empty, got id=%q spiffe=%q",
				snap.ConnectorId, snap.ConnectorSpiffe)
		}
	})

	t.Run("backwards compatibility", func(t *testing.T) {
		wsID := mustInsertWorkspace(t, ctx, testPool, "ws-compat")
		_ = mustInsertActiveConnector(t, ctx, testPool, wsID, "td-c", "10.0.0.50")

		noRelay := NewStore(testPool, "", "")
		withRelay := NewStore(testPool, "relay.x:9093", "spiffe://td/relay/r1")

		s1, err := CompileACLSnapshot(ctx, noRelay, notifier, testPool, wsID)
		if err != nil {
			t.Fatalf("noRelay compile: %v", err)
		}
		s2, err := CompileACLSnapshot(ctx, withRelay, notifier, testPool, wsID)
		if err != nil {
			t.Fatalf("withRelay compile: %v", err)
		}

		if s1.ConnectorTunnelAddr != s2.ConnectorTunnelAddr {
			t.Fatalf("ConnectorTunnelAddr drift: %q vs %q",
				s1.ConnectorTunnelAddr, s2.ConnectorTunnelAddr)
		}
		if len(s1.Entries) != len(s2.Entries) {
			t.Fatalf("Entries len drift: %d vs %d", len(s1.Entries), len(s2.Entries))
		}
		for i := range s1.Entries {
			if s1.Entries[i].String() != s2.Entries[i].String() {
				t.Fatalf("Entries[%d] drift: %v vs %v", i, s1.Entries[i], s2.Entries[i])
			}
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
func mustInsertWorkspace(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status)
		 VALUES ($1, $1, 'active')
		 RETURNING id::text`,
		slug,
	).Scan(&id)
	if err != nil {
		t.Fatalf("insert workspace %q: %v", slug, err)
	}
	return id
}

// mustInsertActiveConnector creates a remote_networks row + a connectors row
// with status='active' and the given trust_domain + lan_addr. Returns the
// connector UUID. The compiler's "active connector lookup" query targets
// exactly this shape.
func mustInsertActiveConnector(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID, trustDomain, lanAddr string) string {
	t.Helper()
	var rnID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO remote_networks (tenant_id, name, location)
		 VALUES ($1, 'test-network', 'home')
		 RETURNING id::text`,
		tenantID,
	).Scan(&rnID); err != nil {
		t.Fatalf("insert remote_networks: %v", err)
	}
	var connID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO connectors (tenant_id, remote_network_id, name, status, trust_domain, lan_addr, last_heartbeat_at)
		 VALUES ($1, $2, 'test-connector', 'active', $3, $4, NOW())
		 RETURNING id::text`,
		tenantID, rnID, trustDomain, lanAddr,
	).Scan(&connID); err != nil {
		t.Fatalf("insert connector: %v", err)
	}
	return connID
}
