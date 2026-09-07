package scim

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/permission"
	"github.com/yourorg/ztna/controller/internal/policy"
)

// TestReprovisionAfterConnectionDelete_Integration reproduces the live failure
// reported 2026-09-05: an Okta connection was deleted (soft-delete, because it
// had linked users), a fresh connection was created for the SAME Okta org, and
// Okta's re-push failed for every user.
//
// The users survived the delete by design (ADR-025 §12) as
// provisioning_owner='unmanaged', still holding their canonical keys in the
// TENANT-scoped users.UNIQUE(tenant_id, provider_sub). Resolve is
// CONNECTION-scoped, so the new connection missed and fell through to
// JIT-create, where the INSERT died on users_tenant_id_provider_sub_key. That
// surfaced to Okta as an opaque 500.
//
// Per ADR-025 §4/§4.1 the correct outcome is 409 identity_conflict plus a
// pending conflict record — `unmanaged → scim` requires an explicit authorized
// admin action, so the identity must never be silently reclaimed.
func TestReprovisionAfterConnectionDelete_Integration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("scim_reconnect_test_%d", os.Getpid())
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

	ws := seedWorkspace(ctx, t, pool, "reconnect")
	oldConn := seedSCIMConnection(ctx, t, pool, ws, "okta", "sub", "externalId")

	idpStore := idp.NewStore(pool, nil)
	permStore := permission.NewStore(pool)
	ds := NewDirectoryService(pool, idpStore, identity.NewAuditSink(pool),
		policy.NewNotifier(policy.NewSnapshotCache()), nil, nil).WithPermissionStore(permStore)

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

	// --- The admin deletes the connection (soft-delete: it has linked users) ---
	if _, err := idpStore.SetSCIMUsersUnmanaged(ctx, ws, oldConn); err != nil {
		t.Fatalf("unmanage users: %v", err)
	}
	if err := idpStore.SoftDeleteConnection(ctx, ws, oldConn); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}

	// --- The admin creates a fresh connection to the same Okta org ---
	newConn := seedSCIMConnection(ctx, t, pool, ws, "okta", "sub", "externalId")
	sc := scopeFor(t, ctx, ds, ws, newConn)

	// --- Okta re-pushes the same user ---
	resource := map[string]any{
		"schemas":  []any{"urn:ietf:params:scim:schemas:core:2.0:User"},
		"userName": "xiyo@gomail.edu.pl",
		"externalId": subject,
		"emails": []any{
			map[string]any{"value": "xiyo@gomail.edu.pl", "primary": true},
		},
	}

	res, serr := ds.Provision(ctx, sc, resource, "")
	if serr == nil {
		t.Fatal("expected the re-push to be refused, got success")
	}
	// The regression: this used to be 500 "provision identity: ... duplicate key
	// value violates unique constraint \"users_tenant_id_provider_sub_key\"".
	if serr.Status == 500 {
		t.Fatalf("re-push returned an opaque 500 instead of the ADR-025 §4.1 conflict: %s", serr.Detail)
	}
	if serr.Status != 409 || serr.ScimType != "identity_conflict" {
		t.Fatalf("expected 409 identity_conflict, got %d %q: %s", serr.Status, serr.ScimType, serr.Detail)
	}
	if res == nil || !res.conflict {
		t.Fatalf("expected a conflict result, got %+v", res)
	}

	// A pending conflict must be queued for the admin, scoped to the NEW
	// connection, naming the surviving user — that is what makes it resolvable
	// from the Provisioning Conflicts queue.
	var status, gotUser string
	if err := pool.QueryRow(ctx,
		`SELECT status, user_id FROM scim_identity_conflicts
		  WHERE workspace_id = $1 AND connection_id = $2 AND canonical_identity_key = $3`,
		ws, newConn, subject).Scan(&status, &gotUser); err != nil {
		t.Fatalf("expected a pending conflict row for the new connection: %v", err)
	}
	if status != "pending" {
		t.Fatalf("expected conflict status 'pending', got %q", status)
	}
	if gotUser != scimUser {
		t.Fatalf("conflict names the wrong user: got %s want %s", gotUser, scimUser)
	}

	// The surviving user must be untouched: still unmanaged, never silently
	// reclaimed by the new connection (ADR-025 §4: unmanaged → scim needs an
	// explicit authorized admin action).
	var owner, provisionedBy, userStatus string
	if err := pool.QueryRow(ctx,
		`SELECT provisioning_owner, provisioned_by, status FROM users WHERE id = $1`,
		scimUser).Scan(&owner, &provisionedBy, &userStatus); err != nil {
		t.Fatalf("reload user: %v", err)
	}
	if owner != "unmanaged" {
		t.Fatalf("user was silently reclaimed: provisioning_owner = %q, want 'unmanaged'", owner)
	}
	if provisionedBy != "scim" || userStatus != "active" {
		t.Fatalf("user was mutated by a refused push: provisioned_by=%q status=%q", provisionedBy, userStatus)
	}

	// No duplicate user may have been created for the key.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM users WHERE tenant_id = $1 AND provider_sub = $2`, ws, subject).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 user holding the canonical key, got %d", n)
	}

	// Retrying (Okta retries hard) must be idempotent — one pending row, not two.
	if _, serr2 := ds.Provision(ctx, sc, resource, ""); serr2 == nil || serr2.Status != 409 {
		t.Fatalf("expected the retry to be refused with 409, got %+v", serr2)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM scim_identity_conflicts
		  WHERE workspace_id = $1 AND connection_id = $2 AND canonical_identity_key = $3`,
		ws, newConn, subject).Scan(&n); err != nil {
		t.Fatalf("count conflicts: %v", err)
	}
	if n != 1 {
		t.Fatalf("retry spawned duplicate conflict rows: got %d, want 1", n)
	}

	// --- The admin approves the re-link in the conflicts queue ---
	admin := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, provider, provider_sub, role, status, provisioned_by, provisioning_owner)
		 VALUES ($1,$2,$3,'okta','admin-sub','admin','active','manual','manual')`,
		admin, ws, "admin@reconnect.example.com"); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if err := permStore.Grant(ctx, ws, admin, permission.BreakGlassMapping, admin); err != nil {
		t.Fatalf("grant break_glass: %v", err)
	}
	if aerr := ds.AcceptLink(ctx, ws, newConn, admin, "admin@reconnect.example.com", subject, "reconnected after delete"); aerr != nil {
		t.Fatalf("accept link: %+v", aerr)
	}

	// After the explicit admin action the SAME user is now owned by the new
	// connection, and Okta's next push succeeds idempotently — no new user.
	res3, serr3 := ds.Provision(ctx, sc, resource, "")
	if serr3 != nil {
		t.Fatalf("expected the post-approval push to succeed, got %d %q: %s", serr3.Status, serr3.ScimType, serr3.Detail)
	}
	if res3.created {
		t.Fatal("post-approval push created a NEW user; it must reuse the preserved one")
	}
	if res3.user.ID != scimUser {
		t.Fatalf("post-approval push resolved to %s, want the preserved user %s", res3.user.ID, scimUser)
	}
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM users WHERE tenant_id = $1 AND provider_sub = $2`, ws, subject).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 user after re-link, got %d", n)
	}
}
