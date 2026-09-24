package scim

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/permission"
	"github.com/yourorg/ztna/controller/internal/policy"
)

// newCleanupTestPool spins up a throwaway database with every migration
// applied and returns a pool onto it. The database is dropped when the test
// finishes.
func newCleanupTestPool(ctx context.Context, t *testing.T, name string) *pgxpool.Pool {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	dbName := fmt.Sprintf("scim_%s_test_%d", name, os.Getpid())
	adminPool := mustConnectPool(t, ctx, adminDSN)
	t.Cleanup(adminPool.Close)
	_, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() { _, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName) })

	testDSN, err := withDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	pool := mustConnectPool(t, ctx, testDSN)
	t.Cleanup(pool.Close)
	if err := applyAllMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return pool
}

func countRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", sql, err)
	}
	return n
}

// TestGroupCleanupOnConnectionDelete_Integration exercises the full SCIM
// connection-deletion lifecycle against real store/provisioning code.
//
// The bug: groups.connection_id carries ON DELETE CASCADE, but that fires only
// on a physical DELETE of the identity_connections row. The forced deletion
// path for a connection with linked users is a SOFT delete (UPDATE status =
// 'deleted'), so the cascade never runs and the connection's SCIM-owned groups
// survive. Because groups carry a UNIQUE (workspace_id, connection_id,
// external_id) partial index scoped to the OLD connection_id, the orphans do
// not themselves collide — but they remain live ACL subjects attached to a dead
// connection, and they leave the directory in a state no IdP can reconcile.
//
// The lifecycle contract verified here:
//   - SCIM-owned groups (and their memberships) for the deleted connection go
//   - the connection's SCIM tokens are revoked and its sync instances purged
//   - users, external_identities and manually-created groups are preserved
//   - a replacement connection can provision the same Okta group and membership
func TestGroupCleanupOnConnectionDelete_Integration(t *testing.T) {
	ctx := context.Background()
	pool := newCleanupTestPool(ctx, t, "group_cleanup")

	ws := seedWorkspace(ctx, t, pool, "group-cleanup")
	oldConn := seedSCIMConnection(ctx, t, pool, ws, "okta", "sub", "externalId")

	idpStore := idp.NewStore(pool, nil)
	permStore := permission.NewStore(pool)
	notifier := policy.NewNotifier(policy.NewSnapshotCache())
	ds := NewDirectoryService(pool, idpStore, identity.NewAuditSink(pool), notifier, nil, nil).WithPermissionStore(permStore)
	sc := scopeFor(t, ctx, ds, ws, oldConn)

	// A user Okta had provisioned through the original connection.
	const subject = "00u16w7qu5saDn60H698"
	scimUser := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, provider, provider_sub, role, status, provisioned_by, provisioning_owner)
		 VALUES ($1,$2,$3,'okta',$4,'member','active','scim','scim')`,
		scimUser, ws, "xiyo@gomail.edu.pl", subject); err != nil {
		t.Fatalf("seed scim user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO external_identities (tenant_id, user_id, connection_id, issuer, subject)
		 VALUES ($1,$2,$3,'https://trial.okta.com',$4)`,
		ws, scimUser, oldConn, subject); err != nil {
		t.Fatalf("seed link: %v", err)
	}

	// A manually-created group holding the same user. This is the control: it
	// is not connection-owned and must survive the delete untouched.
	manualGroup := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO groups (id, workspace_id, name, origin) VALUES ($1,$2,'ops-oncall','manual')`,
		manualGroup, ws); err != nil {
		t.Fatalf("seed manual group: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO group_members (group_id, user_id) VALUES ($1,$2)`,
		manualGroup, scimUser); err != nil {
		t.Fatalf("seed manual group member: %v", err)
	}

	// A real SCIM bearer token minted for the connection through the token
	// store, not hand-inserted.
	tokenStore, err := NewStore(pool, []byte("group-cleanup-test-hash-key"), 0)
	if err != nil {
		t.Fatalf("new token store: %v", err)
	}
	minted, err := tokenStore.Mint(ctx, ws, oldConn, nil, nil, nil)
	if err != nil {
		t.Fatalf("mint scim token: %v", err)
	}

	// Provision the group and its membership through the real SCIM path.
	g, serr := ds.CreateGroup(ctx, sc, "eng-team", "Engineering Team")
	if serr != nil {
		t.Fatalf("create group: %v", serr)
	}
	if _, serr := ds.PatchGroup(ctx, sc, g.ID, &groupPatch{Ops: []patchOp{
		{Op: "add", Values: []string{scimUser}},
	}}); serr != nil {
		t.Fatalf("add user to group: %v", serr)
	}

	// CreateGroup opens a sync instance for the connection; capture it so the
	// purge assertion targets a row real provisioning created.
	syncInst := ds.CurrentSyncInstance(ctx, sc)
	if syncInst == "" {
		t.Fatal("expected provisioning to have opened a sync instance")
	}
	if n := countRows(ctx, t, pool,
		`SELECT COUNT(*) FROM group_members WHERE group_id = $1`, g.ID); n != 1 {
		t.Fatalf("precondition: expected 1 member on the scim group, got %d", n)
	}

	// --- The admin deletes the connection (soft-delete: it has linked users) ---
	if _, err := idpStore.SetSCIMUsersUnmanaged(ctx, ws, oldConn); err != nil {
		t.Fatalf("unmanage users: %v", err)
	}
	if err := idpStore.SoftDeleteConnection(ctx, ws, oldConn); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	// The connection row survives, marked terminal.
	var oldStatus string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM identity_connections WHERE id = $1`, oldConn).Scan(&oldStatus); err != nil {
		t.Fatalf("old connection row must survive the soft-delete: %v", err)
	}
	if oldStatus != "deleted" {
		t.Fatalf("expected old connection status 'deleted', got %q", oldStatus)
	}

	// The SCIM group is gone, addressed by the id captured before the delete.
	if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM groups WHERE id = $1`, g.ID); n != 0 {
		t.Fatalf("expected the scim group to be deleted, still found %d row(s)", n)
	}
	// ...and so are its memberships (via ON DELETE CASCADE on group_members).
	if n := countRows(ctx, t, pool,
		`SELECT COUNT(*) FROM group_members WHERE group_id = $1`, g.ID); n != 0 {
		t.Fatalf("expected group_members for the deleted group to be gone, got %d", n)
	}
	// No SCIM group of any kind remains for the dead connection.
	if n := countRows(ctx, t, pool,
		`SELECT COUNT(*) FROM groups WHERE connection_id = $1 AND origin = 'scim'`, oldConn); n != 0 {
		t.Fatalf("expected 0 scim groups for the deleted connection, got %d", n)
	}

	// The token is revoked.
	var revoked *string
	if err := pool.QueryRow(ctx,
		`SELECT revoked_at::text FROM scim_tokens WHERE id = $1`, minted.Token.ID).Scan(&revoked); err != nil {
		t.Fatalf("read scim token after delete: %v", err)
	}
	if revoked == nil {
		t.Fatal("expected the connection's scim token to be revoked")
	}

	// The sync instance is purged.
	if n := countRows(ctx, t, pool,
		`SELECT COUNT(*) FROM scim_sync_instances WHERE id = $1`, syncInst); n != 0 {
		t.Fatalf("expected the sync instance to be purged, got %d row(s)", n)
	}

	// --- Preservation: nothing outside the connection's SCIM metadata moved ---

	// The manual group and its membership are untouched.
	if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM groups WHERE id = $1`, manualGroup); n != 1 {
		t.Fatal("the manually-created group must survive the connection delete")
	}
	if n := countRows(ctx, t, pool,
		`SELECT COUNT(*) FROM group_members WHERE group_id = $1 AND user_id = $2`,
		manualGroup, scimUser); n != 1 {
		t.Fatal("membership of the manual group must survive the connection delete")
	}

	// The user survives, flipped to unmanaged (ADR-025 §12) rather than removed.
	var owner, provisionedBy, userStatus string
	if err := pool.QueryRow(ctx,
		`SELECT provisioning_owner, provisioned_by, status FROM users WHERE id = $1`,
		scimUser).Scan(&owner, &provisionedBy, &userStatus); err != nil {
		t.Fatalf("the scim user must survive the connection delete: %v", err)
	}
	if owner != "unmanaged" {
		t.Fatalf("expected user ownership 'unmanaged', got %q", owner)
	}
	if provisionedBy != "scim" {
		t.Fatalf("provisioned_by is immutable provenance; got %q", provisionedBy)
	}
	if userStatus != "active" {
		t.Fatalf("expected the preserved user to stay active, got %q", userStatus)
	}

	// The external identity survives.
	if n := countRows(ctx, t, pool,
		`SELECT COUNT(*) FROM external_identities WHERE user_id = $1 AND subject = $2`,
		scimUser, subject); n != 1 {
		t.Fatal("the external identity must survive the connection delete")
	}

	// --- The admin creates a fresh connection to the same Okta org ---
	newConn := seedSCIMConnection(ctx, t, pool, ws, "okta", "sub", "externalId")
	newSc := scopeFor(t, ctx, ds, ws, newConn)

	var newStatus string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM identity_connections WHERE id = $1`, newConn).Scan(&newStatus); err != nil {
		t.Fatalf("check new connection status: %v", err)
	}
	if newStatus != "active" {
		t.Fatalf("expected new connection status 'active', got %q", newStatus)
	}

	// --- Okta re-pushes the same user ---
	// The user survived as unmanaged, and unmanaged → scim requires an explicit
	// authorized admin action (ADR-025 §4/§4.1), so this is a 409, never a 500.
	resource := map[string]any{
		"schemas":    []any{"urn:ietf:params:scim:schemas:core:2.0:User"},
		"userName":   "xiyo@gomail.edu.pl",
		"externalId": subject,
		"emails": []any{
			map[string]any{"value": "xiyo@gomail.edu.pl", "primary": true},
		},
	}

	if _, serr := ds.Provision(ctx, newSc, resource, ""); serr == nil {
		t.Fatal("expected the re-push to be refused, got success")
	} else if serr.Status == 500 {
		t.Fatalf("re-push returned opaque 500 instead of 409 conflict: %s", serr.Detail)
	} else if serr.Status != 409 || serr.ScimType != "identity_conflict" {
		t.Fatalf("expected 409 identity_conflict, got %d %q: %s", serr.Status, serr.ScimType, serr.Detail)
	}

	// --- Okta pushes the same group through the real SCIM path ---
	// This is the regression the cleanup exists for: with the old group orphaned
	// the directory can never be reconciled against the new connection.
	g2, serr := ds.CreateGroup(ctx, newSc, "eng-team", "Engineering Team")
	if serr != nil {
		t.Fatalf("create group on new connection: %v", serr)
	}
	if g2.ID == g.ID {
		t.Fatal("the replacement group must be a fresh row, not the resurrected orphan")
	}

	// The new group must belong to the NEW connection.
	var gotConn string
	if err := pool.QueryRow(ctx,
		`SELECT connection_id::text FROM groups WHERE id = $1`, g2.ID).Scan(&gotConn); err != nil {
		t.Fatalf("read new group connection: %v", err)
	}
	if gotConn != newConn {
		t.Fatalf("new group belongs to connection %s, want the new connection %s", gotConn, newConn)
	}

	groups2, err := ds.ListGroups(ctx, newSc)
	if err != nil {
		t.Fatalf("list groups after recreation: %v", err)
	}
	if len(groups2) != 1 || groups2[0].ID != g2.ID {
		t.Fatalf("expected exactly the new group under the new connection, got %+v", groups2)
	}

	// --- Approve the link so the surviving user can be re-provisioned ---
	admin := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, provider, provider_sub, role, status, provisioned_by, provisioning_owner)
		 VALUES ($1,$2,$3,'okta','admin-sub','admin','active','manual','manual')`,
		admin, ws, "admin@group-cleanup.example.com"); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if err := permStore.Grant(ctx, ws, admin, permission.BreakGlassMapping, admin); err != nil {
		t.Fatalf("grant break_glass: %v", err)
	}
	if aerr := ds.AcceptLink(ctx, ws, newConn, admin, "admin@group-cleanup.example.com", subject, "group cleanup test"); aerr != nil {
		t.Fatalf("accept link: %v", aerr)
	}

	res2, serr2 := ds.Provision(ctx, newSc, resource, "")
	if serr2 != nil {
		t.Fatalf("provision after approval: %v", serr2)
	}
	if res2.created {
		t.Fatal("post-approval push created a new user; it must reuse the preserved one")
	}
	if res2.user.ID != scimUser {
		t.Fatalf("post-approval push resolved to %s, want the preserved user %s", res2.user.ID, scimUser)
	}

	// --- Membership provisioning works on the new connection ---
	if _, serr := ds.PatchGroup(ctx, newSc, g2.ID, &groupPatch{Ops: []patchOp{
		{Op: "add", Values: []string{scimUser}},
	}}); serr != nil {
		t.Fatalf("patch group to add user: %v", serr)
	}
	members, err := ds.ListGroupMembers(ctx, newSc, g2.ID)
	if err != nil {
		t.Fatalf("list members on new group: %v", err)
	}
	if len(members) != 1 || members[0] != scimUser {
		t.Fatalf("expected 1 member (user %s), got %v", scimUser, members)
	}
}

// TestOrphanedGroupDoesNotBlockReprovisioning_Integration pins down what an
// orphaned SCIM group actually does and does not do, because the reported
// symptom ("stale groups belonging to the old deleted connection make Okta
// provisioning fail") and the real mechanism are not the same thing.
//
// It marks a connection deleted with a raw UPDATE, bypassing the cleanup
// entirely, so the orphan is present exactly as it is in a database that
// pre-dates the fix. Group identity is scoped per connection — the unique
// index is (workspace_id, connection_id, external_id) WHERE origin='scim' —
// so the replacement connection can still create the same group.
//
// The orphan's real damage is that it is unreachable: GetGroup/ListGroups are
// connection-scoped so no live connection can address it, while the admin
// group list is workspace-scoped (policy.Store.ListGroups) so it stays visible,
// and deleteGroup refuses origin='scim'. It becomes a permanently undeletable
// ACL subject. That is what the cleanup prevents.
func TestOrphanedGroupDoesNotBlockReprovisioning_Integration(t *testing.T) {
	ctx := context.Background()
	pool := newCleanupTestPool(ctx, t, "orphan_premise")

	ws := seedWorkspace(ctx, t, pool, "orphan-premise")
	oldConn := seedSCIMConnection(ctx, t, pool, ws, "okta", "sub", "externalId")

	idpStore := idp.NewStore(pool, nil)
	ds := NewDirectoryService(pool, idpStore, identity.NewAuditSink(pool),
		policy.NewNotifier(policy.NewSnapshotCache()), nil, nil).
		WithPermissionStore(permission.NewStore(pool))

	orphan, serr := ds.CreateGroup(ctx, scopeFor(t, ctx, ds, ws, oldConn), "eng-team", "Engineering Team")
	if serr != nil {
		t.Fatalf("create group: %v", serr)
	}

	// Mark the connection deleted WITHOUT the cleanup, reproducing a database
	// in the pre-fix state.
	if _, err := pool.Exec(ctx,
		`UPDATE identity_connections SET status = 'deleted' WHERE id = $1`, oldConn); err != nil {
		t.Fatalf("raw soft-delete: %v", err)
	}
	if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM groups WHERE id = $1`, orphan.ID); n != 1 {
		t.Fatal("precondition: the orphan must still be present")
	}

	// A replacement connection provisions the same group through the real path.
	newConn := seedSCIMConnection(ctx, t, pool, ws, "okta", "sub", "externalId")
	newSc := scopeFor(t, ctx, ds, ws, newConn)

	g2, serr := ds.CreateGroup(ctx, newSc, "eng-team", "Engineering Team")
	if serr != nil {
		t.Fatalf("the orphan blocked re-provisioning after all — this changes the root cause: %v", serr)
	}
	if g2.ID == orphan.ID {
		t.Fatal("expected a fresh row under the new connection, not the orphan")
	}

	// The orphan survives and is invisible to every connection-scoped SCIM read
	// — no IdP can ever reconcile or remove it.
	groups, err := ds.ListGroups(ctx, newSc)
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	if len(groups) != 1 || groups[0].ID != g2.ID {
		t.Fatalf("the new connection must see only its own group, got %+v", groups)
	}
	if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM groups WHERE id = $1`, orphan.ID); n != 1 {
		t.Fatal("the orphan must still be stranded in the workspace")
	}
	// ...yet it is still workspace-visible, which is how it reaches the admin UI.
	if n := countRows(ctx, t, pool,
		`SELECT COUNT(*) FROM groups WHERE workspace_id = $1`, ws); n != 2 {
		t.Fatal("expected the orphan and the new group to both be workspace-visible")
	}
}

// TestSoftDeleteConnection_RollbackOnCleanupFailure_Integration proves the
// cleanup is atomic rather than merely written inside a Begin/Commit pair.
//
// A BEFORE DELETE trigger on scim_sync_instances raises, which fails the LAST
// statement of the transaction. If the transaction is real, the three
// statements that ran before it — the status flip, the group delete and the
// token revoke — must all be undone.
func TestSoftDeleteConnection_RollbackOnCleanupFailure_Integration(t *testing.T) {
	ctx := context.Background()
	pool := newCleanupTestPool(ctx, t, "rollback")

	ws := seedWorkspace(ctx, t, pool, "rollback")
	conn := seedSCIMConnection(ctx, t, pool, ws, "okta", "sub", "externalId")

	idpStore := idp.NewStore(pool, nil)
	ds := NewDirectoryService(pool, idpStore, identity.NewAuditSink(pool),
		policy.NewNotifier(policy.NewSnapshotCache()), nil, nil).
		WithPermissionStore(permission.NewStore(pool))
	sc := scopeFor(t, ctx, ds, ws, conn)

	tokenStore, err := NewStore(pool, []byte("rollback-test-hash-key"), 0)
	if err != nil {
		t.Fatalf("new token store: %v", err)
	}
	minted, err := tokenStore.Mint(ctx, ws, conn, nil, nil, nil)
	if err != nil {
		t.Fatalf("mint scim token: %v", err)
	}

	g, serr := ds.CreateGroup(ctx, sc, "eng-team", "Engineering Team")
	if serr != nil {
		t.Fatalf("create group: %v", serr)
	}

	// Make the final cleanup statement fail.
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION fail_sync_instance_delete() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'injected sync-instance delete failure';
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER t_fail_sync_instance_delete
			BEFORE DELETE ON scim_sync_instances
			FOR EACH ROW EXECUTE FUNCTION fail_sync_instance_delete();
	`); err != nil {
		t.Fatalf("install failure trigger: %v", err)
	}

	if err := idpStore.SoftDeleteConnection(ctx, ws, conn); err == nil {
		t.Fatal("expected SoftDeleteConnection to fail while the trigger is installed")
	}

	// Every earlier statement in the transaction must have rolled back.
	var status string
	if err := pool.QueryRow(ctx,
		`SELECT status FROM identity_connections WHERE id = $1`, conn).Scan(&status); err != nil {
		t.Fatalf("read connection after failed delete: %v", err)
	}
	if status != "active" {
		t.Fatalf("connection status must roll back to 'active', got %q", status)
	}
	if n := countRows(ctx, t, pool, `SELECT COUNT(*) FROM groups WHERE id = $1`, g.ID); n != 1 {
		t.Fatal("the scim group delete must roll back")
	}
	var revoked *string
	if err := pool.QueryRow(ctx,
		`SELECT revoked_at::text FROM scim_tokens WHERE id = $1`, minted.Token.ID).Scan(&revoked); err != nil {
		t.Fatalf("read token after failed delete: %v", err)
	}
	if revoked != nil {
		t.Fatalf("the token revoke must roll back, got revoked_at=%q", *revoked)
	}

	// With the fault removed, the same call must now succeed and clean up fully.
	if _, err := pool.Exec(ctx, `DROP TRIGGER t_fail_sync_instance_delete ON scim_sync_instances`); err != nil {
		t.Fatalf("drop failure trigger: %v", err)
	}
	if err := idpStore.SoftDeleteConnection(ctx, ws, conn); err != nil {
		t.Fatalf("soft-delete after removing the fault: %v", err)
	}
	if n := countRows(ctx, t, pool,
		`SELECT COUNT(*) FROM groups WHERE connection_id = $1 AND origin = 'scim'`, conn); n != 0 {
		t.Fatalf("expected the scim group to be cleaned up on the successful run, got %d", n)
	}
	if n := countRows(ctx, t, pool,
		`SELECT COUNT(*) FROM scim_sync_instances WHERE connection_id = $1`, conn); n != 0 {
		t.Fatalf("expected sync instances purged on the successful run, got %d", n)
	}
}
