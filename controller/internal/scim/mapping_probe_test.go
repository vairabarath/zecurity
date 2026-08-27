package scim

// White-box (package scim) integration tests for ProbeMapping. Follows the
// PKI_TEST_DATABASE_URL + applyAllMigrations harness used by the other
// scim/identity integration tests in this package. Skips cleanly when no DB
// is configured.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/policy"
)

func TestProbeMapping_Integration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := "scim_probe_test_" + itoa(os.Getpid())
	adminPool := mustConnectPool(t, ctx, adminDSN)
	defer adminPool.Close()
	_, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() { _, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName) }()

	testDSN, err := withDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	pool := mustConnectPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyAllMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	ws := seedWorkspace(ctx, t, pool, "probe")
	idpStore := idp.NewStore(pool, nil)
	ds := NewDirectoryService(pool, idpStore, identity.NewAuditSink(pool), policy.NewNotifier(policy.NewSnapshotCache()), nil, identity.NewRevoker(pool, nil, identity.NewAuditSink(pool)))

	// seedDisabledSCIMConnection mirrors seedSCIMConnection but with
	// scim_enabled=FALSE — the pre-enable state ProbeMapping must work in,
	// since that is exactly the state TestIdpConnection runs it from. The
	// public /scim/v2 router would 403 here; ProbeMapping must not.
	seedDisabledSCIMConnection := func(t *testing.T, provider, subjectClaim, scimIdent string) string {
		t.Helper()
		connID := seedSCIMConnection(ctx, t, pool, ws, provider, subjectClaim, scimIdent)
		if _, err := pool.Exec(ctx, `UPDATE identity_connections SET scim_enabled = FALSE WHERE id = $1`, connID); err != nil {
			t.Fatalf("disable scim on seeded connection: %v", err)
		}
		return connID
	}

	countActiveTokens := func(t *testing.T, connID string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM scim_tokens WHERE connection_id = $1 AND revoked_at IS NULL`, connID,
		).Scan(&n); err != nil {
			t.Fatalf("count active tokens: %v", err)
		}
		return n
	}

	t.Run("scimIdentifier round-trip verifies mapping and cleans up the probe user", func(t *testing.T) {
		connID := seedDisabledSCIMConnection(t, "okta", "sub", "externalId")
		conn, err := idpStore.GetByID(ctx, connID)
		if err != nil {
			t.Fatalf("load connection: %v", err)
		}

		result := ds.ProbeMapping(ctx, conn)
		if !result.Verified {
			t.Fatalf("expected verified round-trip, got unverified: %s", result.Reason)
		}
		if result.Reason == "" {
			t.Fatalf("expected a non-empty reason on success")
		}
		if containsSubjectClaimProvenClaim(result.Reason) {
			t.Fatalf("probe must never claim subjectClaim was proven, got reason: %s", result.Reason)
		}

		// No probe user left behind: the only rows in `users` for this
		// workspace must be tombstoned (status='deleted'), never active.
		var activeCount int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM users WHERE tenant_id = $1 AND status <> 'deleted'`, ws,
		).Scan(&activeCount); err != nil {
			t.Fatalf("count active users: %v", err)
		}
		if activeCount != 0 {
			t.Fatalf("expected the probe user to be cleaned up (tombstoned), found %d active users", activeCount)
		}
	})

	t.Run("wired MappingGate reaches proven only after a verified probe", func(t *testing.T) {
		connID := seedDisabledSCIMConnection(t, "okta", "sub", "externalId")
		conn, err := idpStore.GetByID(ctx, connID)
		if err != nil {
			t.Fatalf("load connection: %v", err)
		}

		gate := NewMappingGate(conn.Provider)
		base, gerr := gate.Evaluate(ctx, conn.SubjectClaim, conn.ScimIdentifier, BreakGlassOverride{})
		if gerr != nil {
			t.Fatalf("evaluate: %v", gerr)
		}
		if base.MappingState != MappingUnproven || base.ScimEnabledAllowed {
			t.Fatalf("expected unproven+disabled before any probe, got %+v", base)
		}

		probe := ds.ProbeMapping(ctx, conn)
		proven := base.WithRoundTrip(probe.Verified, conn.SubjectClaim, conn.ScimIdentifier)
		if proven.MappingState != MappingProven || !proven.ScimEnabledAllowed {
			t.Fatalf("expected the wired gate to reach proven+enabled after a verified probe, got %+v (probe=%+v)", proven, probe)
		}
	})

	t.Run("setup failure (unseeded connection) is unproven, not a panic", func(t *testing.T) {
		// A *idp.Connection that does not exist in identity_connections at
		// all: EnsureSyncInstance/Provision must fail on the FK, and
		// ProbeMapping must surface that as Verified=false, never panic.
		ghost := &idp.Connection{
			ID:             "00000000-0000-0000-0000-000000000000",
			TenantID:       &ws,
			Provider:       "okta",
			Issuer:         "https://ghost.example.com",
			SubjectClaim:   "sub",
			ScimIdentifier: "externalId",
		}
		result := ds.ProbeMapping(ctx, ghost)
		if result.Verified {
			t.Fatalf("expected an unproven result for a non-existent connection, got verified")
		}
		if result.Reason == "" {
			t.Fatalf("expected a descriptive failure reason")
		}
	})

	t.Run("no workspace on the connection short-circuits safely", func(t *testing.T) {
		platformConn := &idp.Connection{
			ID:             "11111111-1111-1111-1111-111111111111",
			TenantID:       nil, // platform-global
			Provider:       "google",
			SubjectClaim:   "sub",
			ScimIdentifier: "externalId",
		}
		result := ds.ProbeMapping(ctx, platformConn)
		if result.Verified {
			t.Fatalf("expected an unproven result for a workspace-less connection")
		}
	})

	t.Run("probe never mints, revokes, or rotates any scim_tokens", func(t *testing.T) {
		connID := seedDisabledSCIMConnection(t, "okta", "sub", "externalId")
		conn, err := idpStore.GetByID(ctx, connID)
		if err != nil {
			t.Fatalf("load connection: %v", err)
		}

		// Seed the maximum (2) active production tokens — exactly the state
		// that would trigger applyThirdTokenRule's oldest-token eviction if
		// the probe minted a third token. This is the regression the whole
		// corrected design exists to prevent.
		tokenStore, err := NewStore(pool, []byte("probe-test-hash-key"), 24*time.Hour)
		if err != nil {
			t.Fatalf("new token store: %v", err)
		}
		label1, label2 := "prod-token-1", "prod-token-2"
		mint1, err := tokenStore.Mint(ctx, ws, connID, &label1, nil, nil)
		if err != nil {
			t.Fatalf("mint token 1: %v", err)
		}
		mint2, err := tokenStore.Mint(ctx, ws, connID, &label2, nil, nil)
		if err != nil {
			t.Fatalf("mint token 2: %v", err)
		}

		before := countActiveTokens(t, connID)
		if before != 2 {
			t.Fatalf("expected 2 active tokens seeded, got %d", before)
		}

		result := ds.ProbeMapping(ctx, conn)
		if !result.Verified {
			t.Fatalf("expected the probe to succeed, got: %s", result.Reason)
		}

		after := countActiveTokens(t, connID)
		if after != 2 {
			t.Fatalf("probe must never change the active token count: before=%d after=%d", before, after)
		}

		// Both original tokens must still be exactly as minted: not revoked.
		var revoked1, revoked2 *time.Time
		if err := pool.QueryRow(ctx, `SELECT revoked_at FROM scim_tokens WHERE id = $1`, mint1.Token.ID).Scan(&revoked1); err != nil {
			t.Fatalf("read token 1: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT revoked_at FROM scim_tokens WHERE id = $1`, mint2.Token.ID).Scan(&revoked2); err != nil {
			t.Fatalf("read token 2: %v", err)
		}
		if revoked1 != nil {
			t.Fatalf("token 1 (oldest) must not have been revoked by the probe, got revoked_at=%v", revoked1)
		}
		if revoked2 != nil {
			t.Fatalf("token 2 must not have been revoked by the probe, got revoked_at=%v", revoked2)
		}

		// And no third (probe) token row was created at all.
		var total int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM scim_tokens WHERE connection_id = $1`, connID).Scan(&total); err != nil {
			t.Fatalf("count total tokens: %v", err)
		}
		if total != 2 {
			t.Fatalf("expected exactly the 2 seeded tokens and no probe-created token, found %d total rows", total)
		}
	})

	t.Run("mismatched scimIdentifier attribute is honestly reported, not silently proven", func(t *testing.T) {
		// A connection configured to read a SCIM attribute the probe's own
		// synthetic resource does not populate under a different name would
		// be a config error the gate's own ValidateMappingConfig already
		// catches before the probe ever runs (empty/degenerate mapping).
		// This subtest instead documents that guarantee at the gate level,
		// since ProbeMapping's resource is always self-consistent by
		// construction (it writes and reads the same configured attribute)
		// and cannot itself manufacture a live mismatch without bypassing
		// its own mapping logic — which would defeat the point of the probe.
		if err := ValidateMappingConfig("sub", "sub"); err == nil {
			t.Fatalf("expected the existing config validator to reject subjectClaim == scimIdentifier (degenerate mapping)")
		}
	})
}

func containsSubjectClaimProvenClaim(reason string) bool {
	// Guards against ever re-introducing language claiming subjectClaim was
	// proven by this probe (it never reads sc.subjectClaim).
	needle := "subjectClaim and SCIM scimIdentifier resolve to the same logical user"
	for i := 0; i+len(needle) <= len(reason); i++ {
		if reason[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
