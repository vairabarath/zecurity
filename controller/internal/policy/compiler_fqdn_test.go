package policy

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
)

// TestParseResolver covers the JSON → proto conversion without a database.
//
// The load-bearing property is that nothing here returns an error: parseResolver
// is called inside CompileACLSnapshot, and an error there makes the caller
// default-deny the whole workspace. A single malformed resolver must degrade one
// resource, not take every user offline.
func TestParseResolver(t *testing.T) {
	t.Run("empty yields nil", func(t *testing.T) {
		if got := parseResolver(""); got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})
	t.Run("malformed json yields nil, not a panic or error", func(t *testing.T) {
		if got := parseResolver(`{"type":`); got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})
	t.Run("non-object yields nil", func(t *testing.T) {
		if got := parseResolver(`["dns"]`); got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})
	t.Run("missing type yields nil", func(t *testing.T) {
		if got := parseResolver(`{"config":{"server":"1.1.1.1"}}`); got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})
	t.Run("empty type yields nil", func(t *testing.T) {
		if got := parseResolver(`{"type":""}`); got != nil {
			t.Fatalf("want nil, got %+v", got)
		}
	})
	t.Run("type only", func(t *testing.T) {
		got := parseResolver(`{"type":"dns"}`)
		if got == nil || got.Type != "dns" {
			t.Fatalf("want type=dns, got %+v", got)
		}
		if len(got.Config) != 0 {
			t.Fatalf("want empty config, got %+v", got.Config)
		}
	})
	t.Run("type and config", func(t *testing.T) {
		got := parseResolver(`{"type":"dns","config":{"name":"backend.svc.internal","port":"53"}}`)
		if got == nil || got.Type != "dns" {
			t.Fatalf("want type=dns, got %+v", got)
		}
		if got.Config["name"] != "backend.svc.internal" || got.Config["port"] != "53" {
			t.Fatalf("config mismatch: %+v", got.Config)
		}
	})
	t.Run("unknown keys are ignored", func(t *testing.T) {
		got := parseResolver(`{"type":"static","address":"1.2.3.4","future":{"x":1}}`)
		if got == nil || got.Type != "static" {
			t.Fatalf("want type=static, got %+v", got)
		}
	})
}

// TestCompileACLSnapshot_FQDNAddressing is the end-to-end proof that Phase 5's
// addressing fields survive the whole controller path: a row written by
// migration 030's columns, read by ListEnabledRulesWithResources, and emitted by
// CompileACLSnapshot into an ACLEntry.
//
// Before Phase 5 an FQDN resource compiled to an entry with an empty address and
// nothing else — the client drops entries whose address will not parse as an IP,
// so the resource silently vanished with no error anywhere.
//
// Requires PKI_TEST_DATABASE_URL pointing at a Postgres role with CREATE
// DATABASE privilege; otherwise skips.
func TestCompileACLSnapshot_FQDNAddressing(t *testing.T) {
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
	store := NewStore(testPool)

	// One workspace holding all three resources, so a single compile exercises
	// every addressing mode at once — including the malformed one, which must
	// not affect the other two.
	wsID := mustInsertWorkspace(t, ctx, testPool, "ws-fqdn")
	grpID := mustInsertGroup(t, ctx, testPool, wsID, "grp-fqdn")
	userID := mustInsertUser(t, ctx, testPool, wsID)
	mustAddGroupMember(t, ctx, testPool, grpID, userID)
	mustInsertClientDevice(t, ctx, testPool, wsID, userID, "spiffe://td/client/dev-fqdn")
	rnID := mustInsertRemoteNetwork(t, ctx, testPool, wsID, "rn-fqdn")

	ipID := mustInsertResource(t, ctx, testPool, wsID, rnID, "ip-res", "10.9.0.10", 80)
	fqdnID := mustInsertFQDNResource(t, ctx, testPool, wsID, rnID, "fqdn-res", "db.internal",
		`{"type":"dns","config":{"name":"backend.svc.internal"}}`, "127.0.0.1", 5432)
	badID := mustInsertFQDNResource(t, ctx, testPool, wsID, rnID, "bad-resolver-res", "broken.internal",
		`{"type":123}`, "", 6379)

	for _, id := range []string{ipID, fqdnID, badID} {
		mustAssignResourceToGroup(t, ctx, testPool, wsID, id, grpID)
	}

	snap, err := CompileACLSnapshot(ctx, store, notifier, wsID)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	byID := make(map[string]*clientv1.ACLEntry, len(snap.Entries))
	for _, e := range snap.Entries {
		byID[e.ResourceId] = e
	}
	if len(byID) != 3 {
		t.Fatalf("want 3 entries, got %d", len(byID))
	}

	t.Run("ip resource is unchanged by phase 5", func(t *testing.T) {
		e := byID[ipID]
		if e.Address != "10.9.0.10" {
			t.Errorf("address = %q, want 10.9.0.10", e.Address)
		}
		if e.Hostname != "" {
			t.Errorf("hostname = %q, want empty for an IP resource", e.Hostname)
		}
		if e.Resolver != nil {
			t.Errorf("resolver = %+v, want nil for an IP resource", e.Resolver)
		}
	})

	t.Run("fqdn resource carries hostname, resolver and local_target", func(t *testing.T) {
		e := byID[fqdnID]
		if e.Address != "" {
			t.Errorf("address = %q, want empty — an FQDN resource has no pinned IP", e.Address)
		}
		if e.Hostname != "db.internal" {
			t.Errorf("hostname = %q, want db.internal", e.Hostname)
		}
		if e.Resolver == nil {
			t.Fatal("resolver is nil — the whole point of phase 5 is that it reaches the wire")
		}
		if e.Resolver.Type != "dns" {
			t.Errorf("resolver.type = %q, want dns", e.Resolver.Type)
		}
		if got := e.Resolver.Config["name"]; got != "backend.svc.internal" {
			t.Errorf("resolver.config[name] = %q, want backend.svc.internal", got)
		}
		// Note what is NOT asserted: local_target. The row has it set in the DB
		// (see the seed below), but ACLEntry deliberately does not carry it —
		// only the Shield dials that address, and Shields receive
		// shield.v1.ResourceInstruction, never an ACLEntry. If a LocalTarget
		// field reappears on ACLEntry, this test will still pass; the guard for
		// that is TestACLRelevantUpdate's "local_target only" case.
		//
		// The classic fields must still be right, or the connector cannot match
		// the dial request to this entry at all.
		if e.Port != 5432 || e.Protocol != "tcp" || e.RouteType != "connector" {
			t.Errorf("port/protocol/route = %d/%s/%s, want 5432/tcp/connector",
				e.Port, e.Protocol, e.RouteType)
		}
	})

	t.Run("malformed resolver degrades one resource, not the workspace", func(t *testing.T) {
		// Compilation already succeeded above — that is half the assertion. The
		// other half is that the bad resource still appears, with hostname intact
		// and a nil resolver, so the connector denies exactly this one dial.
		e := byID[badID]
		if e == nil {
			t.Fatal("entry missing — a bad resolver must not remove the resource")
		}
		if e.Hostname != "broken.internal" {
			t.Errorf("hostname = %q, want broken.internal", e.Hostname)
		}
		if e.Resolver != nil {
			t.Errorf("resolver = %+v, want nil for a malformed value", e.Resolver)
		}
		// And the healthy FQDN resource in the same workspace is unaffected.
		if byID[fqdnID].Resolver == nil {
			t.Error("a sibling's bad resolver must not clear this one")
		}
	})
}

// mustInsertFQDNResource inserts a name-addressed resource: host NULL, hostname
// set. Pass an empty localTarget to leave that column NULL.
func mustInsertFQDNResource(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	workspaceID, remoteNetworkID, name, hostname, resolver, localTarget string, port int) string {
	t.Helper()
	var lt *string
	if localTarget != "" {
		lt = &localTarget
	}
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO resources (tenant_id, remote_network_id, name, hostname, resolver,
		                        local_target, port_from, protocol, status)
		 VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, 'tcp', 'unprotected')
		 RETURNING id::text`,
		workspaceID, remoteNetworkID, name, hostname, resolver, lt, port,
	).Scan(&id); err != nil {
		t.Fatalf("insert fqdn resource %q: %v", name, err)
	}
	return id
}
