package scim

// PENDING-19 R-11: SCIM may not leave a workspace with no active Admin.
// DELETE, PATCH active=false and connection disable are refused (409, audited)
// only when the target is the last active Admin; with another active Admin
// they go through. DB-backed: must run, not skip, in the gate.

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/policy"
)

type recordingRoleCache struct {
	mu      sync.Mutex
	users   []string
	tenants []string
}

func (r *recordingRoleCache) Invalidate(_, userID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users = append(r.users, userID)
}

func (r *recordingRoleCache) InvalidateTenant(tenantID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tenants = append(r.tenants, tenantID)
}

type lastAdminEnv struct {
	ctx   context.Context
	pool  *pgxpool.Pool
	ds    *DirectoryService
	idp   *idp.Store
	ws    string
	conn  string
	cache *recordingRoleCache
}

func newLastAdminEnv(t *testing.T, name string) *lastAdminEnv {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := "scim_lastadmin_" + name + "_" + itoa(os.Getpid())
	adminPool := mustConnectPool(t, ctx, adminDSN)
	t.Cleanup(adminPool.Close)
	_, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() { _, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName) })
	testDSN, err := withDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	pool := mustConnectPool(t, ctx, testDSN)
	t.Cleanup(pool.Close)
	if err := applyAllMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	e := &lastAdminEnv{ctx: ctx, pool: pool, cache: &recordingRoleCache{}}
	e.ws = seedWorkspace(ctx, t, pool, "la"+name)
	e.conn = seedSCIMConnection(ctx, t, pool, e.ws, "okta", "sub", "externalId")
	e.idp = idp.NewStore(pool, nil)
	e.ds = NewDirectoryService(pool, e.idp, identity.NewAuditSink(pool),
		policy.NewNotifier(policy.NewSnapshotCache()), &fakeSink{},
		identity.NewRevoker(pool, &fakeInvalidator{}, identity.NewAuditSink(pool))).
		WithRoleInvalidator(e.cache)
	return e
}

// scimUser provisions a SCIM-owned user and sets its role.
func (e *lastAdminEnv) scimUser(t *testing.T, email, role string) string {
	t.Helper()
	res, serr := e.ds.Provision(e.ctx, scopeFor(t, e.ctx, e.ds, e.ws, e.conn), map[string]any{
		"userName":   email,
		"externalId": "ext-" + email,
	}, "")
	if serr != nil {
		t.Fatalf("provision %s: %v", email, serr)
	}
	if _, err := e.pool.Exec(e.ctx, `UPDATE users SET role = $2 WHERE id = $1`, res.user.ID, role); err != nil {
		t.Fatalf("set role: %v", err)
	}
	return res.user.ID
}

// localAdmin inserts an active, non-SCIM Admin.
func (e *lastAdminEnv) localAdmin(t *testing.T, email string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(e.ctx,
		`INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		 VALUES ($1, $2, 'google', $2, 'admin', 'active') RETURNING id::text`,
		e.ws, email).Scan(&id); err != nil {
		t.Fatalf("insert local admin: %v", err)
	}
	return id
}

func (e *lastAdminEnv) status(t *testing.T, id string) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(e.ctx, `SELECT status FROM users WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("read status: %v", err)
	}
	return s
}

func (e *lastAdminEnv) refusals(t *testing.T, id string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(e.ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
		e.ws, actionLastAdminRefused, id).Scan(&n); err != nil {
		t.Fatalf("count refusals: %v", err)
	}
	return n
}

func wantLastAdmin409(t *testing.T, serr *SCIMError) {
	t.Helper()
	if serr == nil || serr.Status != 409 {
		t.Fatalf("want SCIM 409 last-admin refusal, got %+v", serr)
	}
}

func TestSCIMLastAdmin_DeprovisionAndPatch(t *testing.T) {
	e := newLastAdminEnv(t, "dp")
	admin := e.scimUser(t, "ada@la.example.com", "admin")
	member := e.scimUser(t, "mo@la.example.com", "member")
	sc := scopeFor(t, e.ctx, e.ds, e.ws, e.conn)

	// A non-Admin target passes even though the workspace's only Admin stays.
	if serr := e.ds.Deprovision(e.ctx, sc, member, false, ""); serr != nil {
		t.Fatalf("suspend member: %v", serr)
	}

	for _, tc := range []struct {
		name string
		run  func() *SCIMError
	}{
		{"DELETE", func() *SCIMError { return e.ds.Deprovision(e.ctx, sc, admin, true, "") }},
		{"suspend", func() *SCIMError { return e.ds.Deprovision(e.ctx, sc, admin, false, "") }},
		{"PATCH active=false", func() *SCIMError {
			f := false
			_, serr := e.ds.Update(e.ctx, sc, admin, &userPatch{Active: &f}, "")
			return serr
		}},
	} {
		before := e.refusals(t, admin)
		wantLastAdmin409(t, tc.run())
		if got := e.status(t, admin); got != "active" {
			t.Fatalf("%s: last admin status = %q, want active", tc.name, got)
		}
		if e.refusals(t, admin) != before+1 {
			t.Fatalf("%s: refusal not audited", tc.name)
		}
	}

	// PATCH that doesn't deactivate (email only) is not guarded.
	if _, serr := e.ds.Update(e.ctx, sc, admin, &userPatch{Email: "ada2@la.example.com"}, ""); serr != nil {
		t.Fatalf("email patch on last admin: %v", serr)
	}

	// With another active Admin the same changes go through.
	e.localAdmin(t, "bo@la.example.com")
	f := false
	if _, serr := e.ds.Update(e.ctx, sc, admin, &userPatch{Active: &f}, ""); serr != nil {
		t.Fatalf("PATCH active=false with another admin: %v", serr)
	}
	if got := e.status(t, admin); got != "suspended" {
		t.Fatalf("status = %q, want suspended", got)
	}
	if !contains(e.cache.users, admin) {
		t.Fatalf("role cache not invalidated for %s: %v", admin, e.cache.users)
	}
	tr := true
	if _, serr := e.ds.Update(e.ctx, sc, admin, &userPatch{Active: &tr}, ""); serr != nil {
		t.Fatalf("reactivate: %v", serr)
	}
	if serr := e.ds.Deprovision(e.ctx, sc, admin, true, ""); serr != nil {
		t.Fatalf("DELETE with another admin: %v", serr)
	}
	if got := e.status(t, admin); got != "deleted" {
		t.Fatalf("status = %q, want deleted", got)
	}
}

// A suspended Admin is not an active Admin: it doesn't count as "another".
func TestSCIMLastAdmin_SuspendedAdminDoesNotCount(t *testing.T) {
	e := newLastAdminEnv(t, "sus")
	admin := e.scimUser(t, "ada@sus.example.com", "admin")
	other := e.localAdmin(t, "bo@sus.example.com")
	if _, err := e.pool.Exec(e.ctx, `UPDATE users SET status = 'suspended' WHERE id = $1`, other); err != nil {
		t.Fatal(err)
	}
	sc := scopeFor(t, e.ctx, e.ds, e.ws, e.conn)
	wantLastAdmin409(t, e.ds.Deprovision(e.ctx, sc, admin, false, ""))
}

func TestSCIMLastAdmin_ConnectionDisable(t *testing.T) {
	e := newLastAdminEnv(t, "cd")
	admin := e.scimUser(t, "ada@cd.example.com", "admin")
	member := e.scimUser(t, "mo@cd.example.com", "member")

	_, _, err := e.idp.DisableConnection(e.ctx, e.ws, e.conn, nil)
	if !errors.Is(err, idp.ErrLastAdmin) {
		t.Fatalf("disable with SCIM-only admin: err = %v, want idp.ErrLastAdmin", err)
	}
	if e.status(t, admin) != "active" || e.status(t, member) != "active" {
		t.Fatal("refused disable must change nothing")
	}
	var connStatus string
	if err := e.pool.QueryRow(e.ctx, `SELECT status FROM identity_connections WHERE id = $1`, e.conn).Scan(&connStatus); err != nil {
		t.Fatal(err)
	}
	if connStatus == "disabled" {
		t.Fatal("refused disable must leave the connection enabled")
	}
	if _, err := e.idp.SuspendSCIMUsersForConnection(e.ctx, e.ws, e.conn); !errors.Is(err, idp.ErrLastAdmin) {
		t.Fatalf("SuspendSCIMUsersForConnection: err = %v, want idp.ErrLastAdmin", err)
	}

	// A local Admin survives the disable, so it goes through.
	e.localAdmin(t, "bo@cd.example.com")
	suspended, _, err := e.idp.DisableConnection(e.ctx, e.ws, e.conn, nil)
	if err != nil {
		t.Fatalf("disable with another admin: %v", err)
	}
	if suspended != 2 || e.status(t, admin) != "suspended" {
		t.Fatalf("suspended=%d admin=%q, want 2 and suspended", suspended, e.status(t, admin))
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func TestSCIMLastAdmin_NonCanonicalID(t *testing.T) {
	e := newLastAdminEnv(t, "nc")
	admin := e.scimUser(t, "ada@nc.example.com", "admin")
	sc := scopeFor(t, e.ctx, e.ds, e.ws, e.conn)
	wantLastAdmin409(t, e.ds.Deprovision(e.ctx, sc, strings.ToUpper(admin), false, ""))
	f := false
	_, serr := e.ds.Update(e.ctx, sc, strings.ToUpper(admin), &userPatch{Active: &f}, "")
	wantLastAdmin409(t, serr)
	if e.status(t, admin) != "active" {
		t.Fatal("last admin suspended via non-canonical id")
	}
}
