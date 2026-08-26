package scim

// White-box test (package scim) so it can drive DirectoryService.Deprovision /
// Reactivate directly and inject a fake SideEffectSink. Follows the
// PKI_TEST_DATABASE_URL + applyAllMigrations harness used by other scim/identity
// tests. Skips cleanly when no DB is configured.

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/policy"
)

func TestDeprovision_Integration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := "scim_phase6_test_" + itoa(os.Getpid())
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

	ws := seedWorkspace(ctx, t, pool, "phase6")
	conn := seedSCIMConnection(ctx, t, pool, ws, "okta", "sub", "externalId")
	idpStore := idp.NewStore(pool, nil)

	fakeSink := &fakeSink{}
	fakeInvalidator := &fakeInvalidator{}
	ds := NewDirectoryService(pool, idpStore, identity.NewAuditSink(pool), policy.NewNotifier(policy.NewSnapshotCache()), fakeSink, identity.NewRevoker(pool, fakeInvalidator, identity.NewAuditSink(pool)))

	// Provision a SCIM-owned user to deprovision/reactivate.
	res, serr := ds.Provision(ctx, scopeFor(t, ctx, ds, ws, conn), map[string]any{
		"userName":   "erin@phase6.example.com",
		"externalId": "okta-erin-1",
	}, "")
	if serr != nil {
		t.Fatalf("provision erin: %v", serr)
	}
	userID := res.user.ID

	readStatus := func(id string) string {
		var s string
		_ = pool.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, id).Scan(&s)
		return s
	}
	readGen := func(id string) int {
		var g int
		_ = pool.QueryRow(ctx, `SELECT identity_generation FROM users WHERE id = $1`, id).Scan(&g)
		return g
	}

	t.Run("suspend (active=false) flips status, bumps generation, enqueues revoke", func(t *testing.T) {
		fakeSink.arm(nil)
		fakeInvalidator.arm(nil)
		sc := scopeFor(t, ctx, ds, ws, conn)
		if serr := ds.Deprovision(ctx, sc, userID, false, ""); serr != nil {
			t.Fatalf("suspend: %v", serr)
		}
		if got := readStatus(userID); got != "suspended" {
			t.Fatalf("expected suspended, got %q", got)
		}
		if g := readGen(userID); g < 1 {
			t.Fatalf("expected generation bump >=1, got %d", g)
		}
		if len(fakeSink.events) != 1 {
			t.Fatalf("expected 1 enqueued event, got %d", len(fakeSink.events))
		}
		if fakeSink.events[0].Reason != identity.DeviceTrustReasonSuspended {
			t.Fatalf("expected suspended reason, got %q", fakeSink.events[0].Reason)
		}
		if len(fakeInvalidator.calls) != 1 || fakeInvalidator.calls[0] != userID {
			t.Fatalf("expected exactly 1 session invalidation for %s, got %v", userID, fakeInvalidator.calls)
		}
	})

	t.Run("reactivate flips status active and enqueues re-enrollment (no gen bump)", func(t *testing.T) {
		fakeSink.events = nil
		fakeSink.arm(nil)
		fakeInvalidator.arm(nil)
		genBefore := readGen(userID)
		sc := scopeFor(t, ctx, ds, ws, conn)
		if serr := ds.Reactivate(ctx, sc, userID, ""); serr != nil {
			t.Fatalf("reactivate: %v", serr)
		}
		if got := readStatus(userID); got != "active" {
			t.Fatalf("expected active, got %q", got)
		}
		if g := readGen(userID); g != genBefore {
			t.Fatalf("reactivate must NOT bump generation (got %d, want %d)", g, genBefore)
		}
		if len(fakeSink.events) != 1 || fakeSink.events[0].Reason != "" {
			t.Fatalf("expected 1 re-enrollment event (empty reason), got %+v", fakeSink.events)
		}
		if len(fakeInvalidator.calls) != 0 {
			t.Fatalf("reactivate must NOT invalidate sessions, got %v", fakeInvalidator.calls)
		}
	})

	t.Run("soft-delete (DELETE) sets tombstone + reason deleted", func(t *testing.T) {
		fakeSink.events = nil
		fakeSink.arm(nil)
		fakeInvalidator.arm(nil)
		sc := scopeFor(t, ctx, ds, ws, conn)
		if serr := ds.Deprovision(ctx, sc, userID, true, ""); serr != nil {
			t.Fatalf("delete: %v", serr)
		}
		if got := readStatus(userID); got != "deleted" {
			t.Fatalf("expected deleted tombstone, got %q", got)
		}
		if len(fakeSink.events) != 1 || fakeSink.events[0].Reason != identity.DeviceTrustReasonDeleted {
			t.Fatalf("expected deleted reason, got %+v", fakeSink.events)
		}
		if len(fakeInvalidator.calls) != 1 || fakeInvalidator.calls[0] != userID {
			t.Fatalf("expected exactly 1 session invalidation for %s, got %v", userID, fakeInvalidator.calls)
		}
		// Tombstone hidden from Get (404).
		if _, serr := ds.Get(ctx, sc, userID); serr == nil || serr.Status != 404 {
			t.Fatalf("expected 404 for tombstoned user, got %+v", serr)
		}
	})

	// Regression: a token scoped to one workspace must not be able to touch a
	// user in another by supplying that user's UUID. Before the fix the ownership
	// guard was an unscoped `SELECT provisioning_owner FROM users WHERE id = $1`,
	// the scoped status UPDATE silently matched 0 rows (not an error), and
	// execution continued into a generation bump that itself carried no tenant
	// predicate — cross-tenant session revocation plus a device-trust event
	// naming this workspace but a foreign user.
	//
	// This lives here rather than in users_integration_test.go because only this
	// file wires a real Revoker and sink; with those nil the assertions below
	// would pass vacuously.
	t.Run("cross-tenant deprovision is refused and mutates nothing", func(t *testing.T) {
		otherWS := seedWorkspace(ctx, t, pool, "victim-tenant")
		otherConn := seedSCIMConnection(ctx, t, pool, otherWS, "okta", "sub", "externalId")
		otherSC := scopeFor(t, ctx, ds, otherWS, otherConn)

		fakeSink.arm(nil)
		fakeInvalidator.arm(nil)
		vres, serr := ds.Provision(ctx, otherSC, map[string]any{
			"userName":   "victim@victim-tenant.example.com",
			"externalId": "okta-victim-1",
		}, "")
		if serr != nil {
			t.Fatalf("provision victim: %v", serr)
		}
		victim := vres.user.ID

		genBefore, statusBefore := readGen(victim), readStatus(victim)
		fakeSink.arm(nil)
		fakeInvalidator.arm(nil)

		// This workspace's scope, the other workspace's user.
		sc := scopeFor(t, ctx, ds, ws, conn)
		if serr := ds.Deprovision(ctx, sc, victim, true, ""); serr == nil || serr.Status != 404 {
			t.Fatalf("expected 404 for cross-tenant deprovision, got %+v", serr)
		}
		if got := readGen(victim); got != genBefore {
			t.Fatalf("cross-tenant generation bump: %d -> %d (foreign sessions would be revoked)", genBefore, got)
		}
		if got := readStatus(victim); got != statusBefore {
			t.Fatalf("cross-tenant status change: %q -> %q", statusBefore, got)
		}
		if len(fakeSink.events) != 0 {
			t.Fatalf("cross-tenant device-trust event enqueued for a foreign user: %+v", fakeSink.events)
		}
		if len(fakeInvalidator.calls) != 0 {
			t.Fatalf("cross-tenant invalidator called for a foreign user: %v", fakeInvalidator.calls)
		}

		// Positive control: through the victim's OWN scope the same call must
		// succeed — otherwise the assertions above would pass even if
		// Deprovision were simply broken for every caller.
		fakeSink.arm(nil)
		fakeInvalidator.arm(nil)
		if serr := ds.Deprovision(ctx, otherSC, victim, true, ""); serr != nil {
			t.Fatalf("in-scope deprovision should succeed, got %+v", serr)
		}
		if got := readGen(victim); got != genBefore+1 {
			t.Fatalf("in-scope deprovision did not bump generation: %d -> %d", genBefore, got)
		}
		if got := readStatus(victim); got != "deleted" {
			t.Fatalf("in-scope deprovision status = %q, want \"deleted\"", got)
		}
		if len(fakeSink.events) != 1 {
			t.Fatalf("in-scope deprovision should enqueue 1 event, got %d", len(fakeSink.events))
		}
		if len(fakeInvalidator.calls) != 1 || fakeInvalidator.calls[0] != victim {
			t.Fatalf("in-scope deprovision should invalidate %s, got %v", victim, fakeInvalidator.calls)
		}
	})

	t.Run("forced enqueue failure aborts the whole deprovision tx", func(t *testing.T) {
		// Re-provision a fresh user (previous one is tombstoned).
		res2, serr := ds.Provision(ctx, scopeFor(t, ctx, ds, ws, conn), map[string]any{
			"userName":   "finn@phase6.example.com",
			"externalId": "okta-finn-1",
		}, "")
		if serr != nil {
			t.Fatalf("provision finn: %v", serr)
		}
		finn := res2.user.ID
		genBefore := readGen(finn)
		fakeSink.arm(errWantFail) // sink fails → tx must roll back
		fakeInvalidator.arm(nil)
		sc := scopeFor(t, ctx, ds, ws, conn)
		_ = ds.Deprovision(ctx, sc, finn, false, "")
		// User must remain active (status unchanged) and generation unbumped.
		if got := readStatus(finn); got != "active" {
			t.Fatalf("abort invariant broken: status=%q (should be active)", got)
		}
		if g := readGen(finn); g != genBefore {
			t.Fatalf("abort invariant broken: generation bumped to %d (should be %d)", g, genBefore)
		}
		if len(fakeInvalidator.calls) != 0 {
			t.Fatalf("aborted tx must not invalidate sessions, got %v", fakeInvalidator.calls)
		}
	})

	t.Run("invalidation is best-effort post-commit and does not fail deprovision", func(t *testing.T) {
		res2, serr := ds.Provision(ctx, scopeFor(t, ctx, ds, ws, conn), map[string]any{
			"userName":   "gale@phase6.example.com",
			"externalId": "okta-gale-1",
		}, "")
		if serr != nil {
			t.Fatalf("provision gale: %v", serr)
		}
		gale := res2.user.ID
		genBefore := readGen(gale)
		fakeSink.arm(nil)
		fakeInvalidator.arm(errWantFail) // invalidator fails — must not roll back deprovision
		sc := scopeFor(t, ctx, ds, ws, conn)
		if serr := ds.Deprovision(ctx, sc, gale, false, ""); serr != nil {
			t.Fatalf("deprovision with failing invalidator should still succeed, got %v", serr)
		}
		if got := readStatus(gale); got != "suspended" {
			t.Fatalf("expected suspended despite invalidator error, got %q", got)
		}
		if g := readGen(gale); g != genBefore+1 {
			t.Fatalf("expected generation bump despite invalidator error, got %d want %d", g, genBefore+1)
		}
		if len(fakeSink.events) != 1 {
			t.Fatalf("expected 1 enqueued event despite invalidator error, got %d", len(fakeSink.events))
		}
		if len(fakeInvalidator.calls) != 1 || fakeInvalidator.calls[0] != gale {
			t.Fatalf("expected 1 invalidation attempt for %s despite error, got %v", gale, fakeInvalidator.calls)
		}
	})
}

// ── fake sink ─────────────────────────────────────────────────────────────────

var errWantFail = context.Canceled // arbitrary non-nil error

type fakeSink struct {
	events []identity.DeviceTrustEvent
	armErr error
}

func (f *fakeSink) arm(e error) {
	f.armErr = e
	f.events = nil
}

func (f *fakeSink) Enqueue(ctx context.Context, tx pgx.Tx, evt identity.DeviceTrustEvent) error {
	if f.armErr != nil {
		return f.armErr
	}
	f.events = append(f.events, evt)
	return nil
}

type fakeInvalidator struct {
	calls []string
	err   error
}

func (f *fakeInvalidator) arm(e error) {
	f.err = e
	f.calls = nil
}

func (f *fakeInvalidator) InvalidateUserSessions(ctx context.Context, userID string) error {
	f.calls = append(f.calls, userID)
	return f.err
}

// ── helpers (mirror other scim test helpers) ─────────────────────────────────

func itoa(n int) string {
	// small int→string to avoid importing strconv in this test file's hot path
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
