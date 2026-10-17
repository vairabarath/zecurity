package policy

import (
	"context"
	"os"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/posture"
)

// Certificate audit bugs 4/5: a device authorizes access only while its
// user is active (users.status = 'active') and, if the user has a workspace
// membership row, that membership is active too. A missing membership row
// does not deny on its own — SCIM-provisioned and first-login users have
// none. Suspension stays reversible: a reactivated user's devices come back
// only if they are otherwise valid (not revoked).

func lifecycleTestPool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := uniqueTestDBName(t)
	adminPool := mustConnectTestPool(t, ctx, adminDSN)
	t.Cleanup(adminPool.Close)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	})
	dsn, err := withTestDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build dsn: %v", err)
	}
	pool := mustConnectTestPool(t, ctx, dsn)
	t.Cleanup(pool.Close)
	if err := applyAllMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return pool, ctx
}

type lifecycleFixture struct {
	pool  *pgxpool.Pool
	ctx   context.Context
	wsID  string
	grpID string
}

// device adds a user in the group with one enrolled device; status is the
// user's status and membership the workspace_members status ("" = no row).
func (f *lifecycleFixture) device(t *testing.T, status, membership string) (userID, spiffe string) {
	t.Helper()
	userID = mustInsertUser(t, f.ctx, f.pool, f.wsID)
	mustExec(t, f.ctx, f.pool, `UPDATE users SET status = $2 WHERE id = $1`, userID, status)
	if membership != "" {
		mustExec(t, f.ctx, f.pool,
			`INSERT INTO workspace_members (workspace_id, user_id, email, status)
			 SELECT $1, id, email, $3 FROM users WHERE id = $2`,
			f.wsID, userID, membership)
	}
	mustAddGroupMember(t, f.ctx, f.pool, f.grpID, userID)
	spiffe = "spiffe://td/client/" + userID
	_ = mustInsertClientDevice(t, f.ctx, f.pool, f.wsID, userID, spiffe)
	return userID, spiffe
}

func (f *lifecycleFixture) authorized(t *testing.T) []string {
	t.Helper()
	got, err := NewStore(f.pool).ListActiveDeviceSPIFFEsForGroups(f.ctx, f.wsID, []string{f.grpID})
	if err != nil {
		t.Fatalf("list devices: %v", err)
	}
	var ids []string
	for _, d := range got[f.grpID] {
		ids = append(ids, d.SPIFFEID)
	}
	single, err := NewStore(f.pool).ListActiveDeviceSPIFFEsForGroup(f.ctx, f.wsID, f.grpID)
	if err != nil {
		t.Fatalf("list devices (single group): %v", err)
	}
	sort.Strings(ids)
	sort.Strings(single)
	if len(ids) != len(single) {
		t.Fatalf("the two device queries disagree: %v vs %v", ids, single)
	}
	for i := range ids {
		if ids[i] != single[i] {
			t.Fatalf("the two device queries disagree: %v vs %v", ids, single)
		}
	}
	return ids
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

func newLifecycleFixture(t *testing.T, pool *pgxpool.Pool, ctx context.Context, slug string) *lifecycleFixture {
	wsID := mustInsertWorkspace(t, ctx, pool, slug)
	return &lifecycleFixture{pool: pool, ctx: ctx, wsID: wsID, grpID: mustInsertGroup(t, ctx, pool, wsID, "grp-"+slug)}
}

func TestDeviceAuthorization_FollowsUserAndMembershipLifecycle(t *testing.T) {
	pool, ctx := lifecycleTestPool(t)
	f := newLifecycleFixture(t, pool, ctx, "lifecycle")

	_, activeNoMembership := f.device(t, "active", "")
	_, activeMember := f.device(t, "active", "active")
	_, suspended := f.device(t, "suspended", "")
	_, locked := f.device(t, "locked", "")
	_, deleted := f.device(t, "deleted", "")
	_, invited := f.device(t, "active", "invited")
	_, suspendedMembership := f.device(t, "active", "suspended")

	got := f.authorized(t)
	for name, id := range map[string]string{
		"active user without a membership row (SCIM / first-login)": activeNoMembership,
		"active user with an active membership":                     activeMember,
	} {
		if !contains(got, id) {
			t.Errorf("%s must be authorized", name)
		}
	}
	for name, id := range map[string]string{
		"suspended user":              suspended,
		"locked user":                 locked,
		"deleted user":                deleted,
		"invited (not active) member": invited,
		"suspended membership":        suspendedMembership,
	} {
		if contains(got, id) {
			t.Errorf("%s must not be authorized", name)
		}
	}
}

// Suspension is reversible; revocation is not.
func TestDeviceAuthorization_ReactivationRestoresOnlyValidDevices(t *testing.T) {
	pool, ctx := lifecycleTestPool(t)
	f := newLifecycleFixture(t, pool, ctx, "reactivate")
	keptUser, kept := f.device(t, "active", "")
	revokedUser, revoked := f.device(t, "active", "")

	mustExec(t, ctx, pool, `UPDATE users SET status = 'suspended' WHERE id = ANY($1::uuid[])`, []string{keptUser, revokedUser})
	mustExec(t, ctx, pool, `UPDATE client_devices SET revoked_at = NOW() WHERE user_id = $1`, revokedUser)
	if got := f.authorized(t); contains(got, kept) || contains(got, revoked) {
		t.Fatalf("suspended users' devices authorized: %v", got)
	}

	mustExec(t, ctx, pool, `UPDATE users SET status = 'active' WHERE id = ANY($1::uuid[])`, []string{keptUser, revokedUser})
	got := f.authorized(t)
	if !contains(got, kept) {
		t.Error("a reactivated user's unrevoked device must be authorized again")
	}
	if contains(got, revoked) {
		t.Error("a revoked device must stay unauthorized after its user is reactivated")
	}
}

// A user recreated after deletion is a new identity: it cannot reach the
// deleted user's devices, and the deleted user's devices stay out even if
// that row were revived (SCIM delete revokes them).
func TestDeviceAuthorization_RecreatedUserCannotReuseDeletedUsersDevices(t *testing.T) {
	pool, ctx := lifecycleTestPool(t)
	f := newLifecycleFixture(t, pool, ctx, "recreate")
	oldUser, oldDevice := f.device(t, "active", "")
	// SCIM delete: soft delete + device revocation (device-trust consumer).
	mustExec(t, ctx, pool, `UPDATE users SET status = 'deleted' WHERE id = $1`, oldUser)
	mustExec(t, ctx, pool, `UPDATE client_devices SET revoked_at = NOW() WHERE user_id = $1`, oldUser)

	// The person comes back as a new user with the same email.
	var newUser string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (tenant_id, email, provider, provider_sub, role)
		 SELECT tenant_id, email || '.new', provider, gen_random_uuid()::text, role FROM users WHERE id = $1
		 RETURNING id::text`, oldUser).Scan(&newUser); err != nil {
		t.Fatalf("recreate user: %v", err)
	}
	mustAddGroupMember(t, ctx, pool, f.grpID, newUser)
	if newUser == oldUser {
		t.Fatal("the recreated user must have a new identity")
	}
	if contains(f.authorized(t), oldDevice) {
		t.Fatal("the deleted user's device is authorized after the user was recreated")
	}
	mustExec(t, ctx, pool, `UPDATE users SET status = 'active' WHERE id = $1`, oldUser)
	if contains(f.authorized(t), oldDevice) {
		t.Fatal("a revoked device came back when the deleted user row was revived")
	}
}

// The full compile excludes a suspended user's device from every entry.
func TestCompileACLSnapshot_ExcludesSuspendedUsersDevices(t *testing.T) {
	pool, ctx := lifecycleTestPool(t)
	f := newLifecycleFixture(t, pool, ctx, "compile")
	rnID, _ := mustInsertRNWithConnector(t, ctx, pool, f.wsID, "rn-compile", "td-compile", "10.40.0.1")
	rID := mustInsertResource(t, ctx, pool, f.wsID, rnID, "res-compile", "10.40.0.10", 443)
	mustAssignResourceToGroup(t, ctx, pool, f.wsID, rID, f.grpID)
	user, spiffe := f.device(t, "active", "")
	_, other := f.device(t, "active", "")

	allowed := func() []string {
		compiled, err := CompileACLSnapshot(ctx, NewStore(pool), posture.NewStore(pool), NewNotifier(NewSnapshotCache()), f.wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		var ids []string
		for _, e := range compiled.Snapshot.Entries {
			ids = append(ids, e.AllowedSpiffeIds...)
		}
		return ids
	}
	if got := allowed(); !contains(got, spiffe) || !contains(got, other) {
		t.Fatalf("active devices missing from the ACL: %v", got)
	}
	mustExec(t, ctx, pool, `UPDATE users SET status = 'suspended' WHERE id = $1`, user)
	got := allowed()
	if contains(got, spiffe) {
		t.Fatal("a suspended user's device is still in the compiled ACL")
	}
	if !contains(got, other) {
		t.Fatal("another user's device was dropped")
	}
}
