package scim

// White-box integration test (package scim) for Phase 9 — Connection Lifecycle,
// Identity Health, and Sync Instances (ADR-025 §12 / PENDING-05 P9). Uses the
// PKI_TEST_DATABASE_URL + applyAllMigrations harness; skips cleanly without a DB.

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/policy"
)

func TestLifecycle_Integration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := "scim_phase9_test_" + itoa(os.Getpid())
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

	ws := seedWorkspace(ctx, t, pool, "phase9")
	conn := seedSCIMConnection(ctx, t, pool, ws, "okta", "sub", "externalId")
	idpStore := idp.NewStore(pool, nil)
	ds := NewDirectoryService(pool, idpStore, nil, policy.NewNotifier(policy.NewSnapshotCache()), nil, nil)

	sc := scopeFor(t, ctx, ds, ws, conn)

	// ── Sync instance: Ensure opens one, then Current returns it ──────────────
	t.Run("EnsureSyncInstance opens exactly one instance", func(t *testing.T) {
		inst, serr := ds.EnsureSyncInstance(ctx, sc)
		if serr != nil {
			t.Fatalf("ensure sync instance: %v", serr)
		}
		if inst == "" {
			t.Fatalf("expected non-empty sync instance id")
		}
		if cur := ds.CurrentSyncInstance(ctx, sc); cur != inst {
			t.Fatalf("CurrentSyncInstance = %q, want %q", cur, inst)
		}
		// Idempotent: a second Ensure returns the SAME instance (no second row).
		again, serr := ds.EnsureSyncInstance(ctx, sc)
		if serr != nil {
			t.Fatalf("ensure again: %v", serr)
		}
		if again != inst {
			t.Fatalf("EnsureSyncInstance reopened; got %q want %q", again, inst)
		}
		// Exactly one row for the connection.
		var n int
		_ = pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM scim_sync_instances WHERE workspace_id = $1 AND connection_id = $2`,
			ws, conn).Scan(&n)
		if n != 1 {
			t.Fatalf("expected 1 sync instance row, got %d", n)
		}
	})

	// ── Provisioned users/groups record the sync_instance_id ──────────────────
	t.Run("provision stamps sync_instance_id + connection last_sync_at", func(t *testing.T) {
		res, serr := ds.Provision(ctx, sc, map[string]any{
			"userName":   "life@phase9.example.com",
			"externalId": "okta-life-1",
		}, ds.CurrentSyncInstance(ctx, sc))
		if serr != nil {
			t.Fatalf("provision: %v", serr)
		}
		// user.sync_instance_id set
		var uInst string
		_ = pool.QueryRow(ctx, `SELECT sync_instance_id::text FROM users WHERE id = $1`, res.user.ID).Scan(&uInst)
		if uInst == "" {
			t.Fatalf("expected user sync_instance_id to be stamped")
		}
		// connection.last_sync_at moved by the touch
		var ls *time.Time
		_ = pool.QueryRow(ctx, `SELECT last_sync_at FROM identity_connections WHERE id = $1`, conn).Scan(&ls)
		if ls == nil {
			t.Fatalf("expected connection last_sync_at to be stamped after provision")
		}
		// group stamps too
		g, serr := ds.CreateGroup(ctx, sc, "okta-grp-1", "Phase9 Group")
		if serr != nil {
			t.Fatalf("create group: %v", serr)
		}
		var gInst string
		_ = pool.QueryRow(ctx, `SELECT COALESCE(sync_instance_id::text,'') FROM groups WHERE id = $1`, g.ID).Scan(&gInst)
		if gInst == "" {
			t.Fatalf("expected group sync_instance_id to be stamped")
		}
	})

	// ── Identity Health derives Healthy from a fresh last_sync_at ─────────────
	t.Run("IdentityHealth Healthy after recent sync", func(t *testing.T) {
		// last_sync_at was just stamped by the provision/group writes.
		h, serr := ds.IdentityHealth(ctx, ws, conn)
		if serr != nil {
			t.Fatalf("identity health: %v", serr)
		}
		if h != HealthHealthy {
			t.Fatalf("expected Healthy, got %q", h)
		}
	})

	// ── Health Degrades with staleness ───────────────────────────────────────
	t.Run("IdentityHealth degrades with staleness", func(t *testing.T) {
		// Backdate last_sync_at beyond Disconnected threshold.
		if _, err := pool.Exec(ctx,
			`UPDATE identity_connections SET last_sync_at = NOW() - INTERVAL '100 hours' WHERE id = $1`, conn); err != nil {
			t.Fatalf("backdate sync: %v", err)
		}
		h, serr := ds.IdentityHealth(ctx, ws, conn)
		if serr != nil {
			t.Fatalf("identity health: %v", serr)
		}
		if h != HealthDisconnected {
			t.Fatalf("expected Disconnected after 100h staleness, got %q", h)
		}
		// Delayed band.
		if _, err := pool.Exec(ctx,
			`UPDATE identity_connections SET last_sync_at = NOW() - INTERVAL '48 hours' WHERE id = $1`, conn); err != nil {
			t.Fatalf("backdate sync 2: %v", err)
		}
		h, _ = ds.IdentityHealth(ctx, ws, conn)
		if h != HealthDelayed {
			t.Fatalf("expected Delayed at 48h, got %q", h)
		}
	})

	// ── Disabled connection reports HealthDisabled (not a sync-state) ─────────
	t.Run("IdentityHealth Disabled when connection disabled", func(t *testing.T) {
		if err := idpStore.SetStatus(ctx, ws, conn, "disabled"); err != nil {
			t.Fatalf("disable: %v", err)
		}
		defer idpStore.SetStatus(ctx, ws, conn, "active") // restore for later subtests
		h, serr := ds.IdentityHealth(ctx, ws, conn)
		if serr != nil {
			t.Fatalf("identity health: %v", serr)
		}
		if h != HealthDisabled {
			t.Fatalf("expected Disabled, got %q", h)
		}
	})

	// ── DISABLE stops SCIM operation (resolveScope refuses non-active) ───────
	t.Run("disable stops SCIM operation until re-enabled", func(t *testing.T) {
		if err := idpStore.SetStatus(ctx, ws, conn, "disabled"); err != nil {
			t.Fatalf("disable: %v", err)
		}
		if _, serr := ds.resolveScope(ctx, ws, conn); serr == nil {
			t.Fatalf("expected SCIM write to be refused on a disabled connection")
		}
		// Re-enable: SCIM operation resumes (status gate clears; ownership is a
		// separate concern and is NOT auto-restored — covered by the store test).
		if err := idpStore.SetStatus(ctx, ws, conn, "active"); err != nil {
			t.Fatalf("re-enable: %v", err)
		}
		if _, serr := ds.resolveScope(ctx, ws, conn); serr != nil {
			t.Fatalf("expected SCIM writes to resume after re-enable, got %+v", serr)
		}
	})

	// ── ReconcileStaleUsers identifies objects from a prior instance ─────────
	t.Run("ReconcileStaleUsers after reconnect (new instance)", func(t *testing.T) {
		cur := ds.CurrentSyncInstance(ctx, sc)
		// Open a NEW sync instance (simulating a disable→re-enable reconnect).
		inst, serr := ds.OpenSyncInstance(ctx, sc, "", "")
		if serr != nil {
			t.Fatalf("open new instance: %v", serr)
		}
		if inst == cur {
			t.Fatalf("expected a fresh instance id")
		}
		stale, serr := ds.ReconcileStaleUsers(ctx, sc, inst)
		if serr != nil {
			t.Fatalf("reconcile stale users: %v", serr)
		}
		if len(stale) == 0 {
			t.Fatalf("expected at least one stale user (provisioned under prior instance)")
		}
	})
}

func TestConnectionLifecycleStore_Integration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := "scim_phase9_lc_" + itoa(os.Getpid())
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

	ws := seedWorkspace(ctx, t, pool, "lc9")
	conn := seedSCIMConnection(ctx, t, pool, ws, "okta", "sub", "externalId")
	idpStore := idp.NewStore(pool, nil)
	ds := NewDirectoryService(pool, idpStore, nil, policy.NewNotifier(policy.NewSnapshotCache()), nil, nil)
	sc := scopeFor(t, ctx, ds, ws, conn)

	// Provision a SCIM user so disable/delete have something to act on.
	res, serr := ds.Provision(ctx, sc, map[string]any{
		"userName":   "lc@phase9.example.com",
		"externalId": "okta-lc-1",
	}, ds.CurrentSyncInstance(ctx, sc))
	if serr != nil {
		t.Fatalf("provision: %v", serr)
	}
	userID := res.user.ID

	readStatus := func() string {
		var s string
		_ = pool.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, userID).Scan(&s)
		return s
	}
	readOwner := func() string {
		var o string
		_ = pool.QueryRow(ctx, `SELECT provisioning_owner FROM users WHERE id = $1`, userID).Scan(&o)
		return o
	}

	// ── DISABLE: suspend + ownership unmanage, status untouched ───────────────
	t.Run("disable suspends scim users and flips provisioning_owner", func(t *testing.T) {
		if _, err := idpStore.SuspendSCIMUsersForConnection(ctx, ws, conn); err != nil {
			t.Fatalf("suspend: %v", err)
		}
		if _, err := idpStore.SetSCIMUsersUnmanaged(ctx, ws, conn); err != nil {
			t.Fatalf("unmanage: %v", err)
		}
		if got := readStatus(); got != "suspended" {
			t.Fatalf("expected suspended after disable, got %q", got)
		}
		if got := readOwner(); got != "unmanaged" {
			t.Fatalf("expected provisioning_owner=unmanaged, got %q", got)
		}
		// provisioned_by MUST stay immutable scim.
		var pb string
		_ = pool.QueryRow(ctx, `SELECT provisioned_by FROM users WHERE id = $1`, userID).Scan(&pb)
		if pb != "scim" {
			t.Fatalf("provisioned_by must stay 'scim', got %q", pb)
		}
	})

	// ── RE-ENABLE does NOT restore ownership (explicit re-enroll required) ────
	t.Run("re-enable does not restore scim ownership", func(t *testing.T) {
		if err := idpStore.SetStatus(ctx, ws, conn, "active"); err != nil {
			t.Fatalf("re-enable: %v", err)
		}
		if got := readOwner(); got != "unmanaged" {
			t.Fatalf("re-enable must NOT auto-restore provisioning_owner; got %q", got)
		}
	})

	// ── DELETE guard: refuse without force when users linked ─────────────────
	t.Run("delete refuses when users linked without force", func(t *testing.T) {
		linked, err := idpStore.LinkedUserCount(ctx, ws, conn)
		if err != nil {
			t.Fatalf("linked count: %v", err)
		}
		if linked == 0 {
			t.Fatalf("precondition: expected linked users for delete-guard test")
		}
		err = deleteViaStore(t, ctx, idpStore, ws, conn, false)
		if err == nil {
			t.Fatalf("expected delete to be refused without force")
		}
		// Connection still present and active (not removed).
		var st string
		_ = pool.QueryRow(ctx, `SELECT status FROM identity_connections WHERE id = $1`, conn).Scan(&st)
		if st != "active" {
			t.Fatalf("expected connection still active after refused delete, got %q", st)
		}
	})

	// ── DELETE with force: soft-delete (status=deleted, users preserved) ─────
	t.Run("delete force soft-deletes preserving users", func(t *testing.T) {
		if err := deleteViaStoreForce(t, ctx, idpStore, ws, conn); err != nil {
			t.Fatalf("soft-delete: %v", err)
		}
		var st string
		_ = pool.QueryRow(ctx, `SELECT status FROM identity_connections WHERE id = $1`, conn).Scan(&st)
		if st != "deleted" {
			t.Fatalf("expected status='deleted', got %q", st)
		}
		// User row still exists (never deleted), ownership unmanaged.
		if got := readStatus(); got == "" {
			t.Fatalf("user row must be preserved after soft-delete")
		}
		if got := readOwner(); got != "unmanaged" {
			t.Fatalf("expected provisioning_owner=unmanaged after soft-delete, got %q", got)
		}
	})
}

// deleteViaStore simulates the resolver's hard/soft-delete decision for the
// non-force path (refuses when linked).
func deleteViaStore(t *testing.T, ctx context.Context, store *idp.Store, ws, conn string, force bool) error {
	t.Helper()
	linked, err := store.LinkedUserCount(ctx, ws, conn)
	if err != nil {
		return err
	}
	if linked > 0 && !force {
		return errDeleteGuarded // refuse, as the resolver does
	}
	if linked > 0 {
		if _, err := store.SetSCIMUsersUnmanaged(ctx, ws, conn); err != nil {
			return err
		}
		return store.SoftDeleteConnection(ctx, ws, conn)
	}
	return store.DeleteWorkspaceConnection(ctx, ws, conn)
}

var errDeleteGuarded = errStub("delete guarded: linked users require force")

type errStub string

func (e errStub) Error() string { return string(e) }

func deleteViaStoreForce(t *testing.T, ctx context.Context, store *idp.Store, ws, conn string) error {
	t.Helper()
	linked, err := store.LinkedUserCount(ctx, ws, conn)
	if err != nil {
		return err
	}
	if linked > 0 {
		if _, err := store.SetSCIMUsersUnmanaged(ctx, ws, conn); err != nil {
			return err
		}
		return store.SoftDeleteConnection(ctx, ws, conn)
	}
	return store.DeleteWorkspaceConnection(ctx, ws, conn)
}
