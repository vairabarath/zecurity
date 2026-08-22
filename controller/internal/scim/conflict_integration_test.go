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

func TestConflict_Integration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("scim_phase8_test_%d", os.Getpid())
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

	wsA := seedWorkspace(ctx, t, pool, "conf-a")
	wsB := seedWorkspace(ctx, t, pool, "conf-b")
	connA := seedSCIMConnection(ctx, t, pool, wsA, "okta", "sub", "externalId")
	connB := seedSCIMConnection(ctx, t, pool, wsB, "okta", "sub", "externalId")

	idpStore := idp.NewStore(pool, nil)
	permStore := permission.NewStore(pool)
	ds := NewDirectoryService(pool, idpStore, identity.NewAuditSink(pool), policy.NewNotifier(policy.NewSnapshotCache()),
		nil, nil).WithPermissionStore(permStore)

	// Admin actor who will be granted identity.mapping.break_glass for accept tests.
	adminActor := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, provider, provider_sub, role, status, provisioned_by, provisioning_owner)
		 VALUES ($1,$2,$3,'okta','actor','admin','active','manual','manual')`,
		adminActor, wsA, "actor@conf-a.example.com"); err != nil {
		t.Fatalf("seed admin actor: %v", err)
	}
	if err := permStore.Grant(ctx, wsA, adminActor, permission.BreakGlassMapping, adminActor); err != nil {
		t.Fatalf("grant break_glass: %v", err)
	}
	// An actor WITHOUT the permission (to prove ADMIN role alone is insufficient).
	plainAdmin := uuid.NewString()
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, provider, provider_sub, role, status, provisioned_by, provisioning_owner)
		 VALUES ($1,$2,$3,'okta','plain','admin','active','manual','manual')`,
		plainAdmin, wsA, "plain@conf-a.example.com"); err != nil {
		t.Fatalf("seed plain admin: %v", err)
	}

	// Seed a JIT/manual identity occupying a canonical key in workspace A / conn A.
	jitUser := uuid.NewString()
	jitKey := "okta-jit-conflict-1"
	if _, err := pool.Exec(ctx,
		`INSERT INTO users (id, tenant_id, email, provider, provider_sub, role, status, provisioned_by, provisioning_owner)
		 VALUES ($1,$2,$3,$4,$5,'member','active','jit','jit')`,
		jitUser, wsA, "jit@conf-a.example.com", "okta", jitKey); err != nil {
		t.Fatalf("seed jit user: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO external_identities (tenant_id, user_id, connection_id, issuer, subject)
		 VALUES ($1,$2,$3,$4,$5)`,
		wsA, jitUser, connA, "https://okta.example.com", jitKey); err != nil {
		t.Fatalf("seed jit link: %v", err)
	}
	// Local grant + device that must survive Accept-Link (preserved, not touched).
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspace_permissions (workspace_id, user_id, permission)
		 VALUES ($1,$2,'policy.editor')`, wsA, jitUser); err != nil {
		t.Fatalf("seed local permission: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO client_devices (user_id, workspace_id, name, os)
		 VALUES ($1,$2,'laptop','darwin')`, jitUser, wsA); err != nil {
		t.Fatalf("seed device: %v", err)
	}

	scA := scopeFor(t, ctx, ds, wsA, connA)
	_ = scopeFor(t, ctx, ds, wsB, connB) // wsB/connB used directly in cross-scope tests

	// helper: count approval audit rows for a user.
	countAudit := func(action string) int {
		var n int
		_ = pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM audit_logs WHERE action = $1 AND target_id = $2`, action, jitUser,
		).Scan(&n)
		return n
	}
	ownerOf := func() string {
		var o string
		_ = pool.QueryRow(ctx, `SELECT provisioning_owner FROM users WHERE id = $1`, jitUser).Scan(&o)
		return o
	}
	statusOf := func() string {
		var s string
		_ = pool.QueryRow(ctx,
			`SELECT status FROM scim_identity_conflicts WHERE workspace_id = $1 AND connection_id = $2 AND canonical_identity_key = $3
			  ORDER BY created_at DESC LIMIT 1`,
			wsA, connA, jitKey).Scan(&s)
		return s
	}

	t.Run("collision returns 409 identity_conflict and creates a pending conflict", func(t *testing.T) {
		res, serr := ds.Provision(ctx, scA, map[string]any{
			"userName":   "jit@conf-a.example.com",
			"externalId": jitKey,
		}, "")
		if res == nil || !res.conflict {
			t.Fatal("expected a conflict result")
		}
		if serr == nil || serr.Status != 409 || serr.ScimType != "identity_conflict" {
			t.Fatalf("expected 409 identity_conflict, got %+v", serr)
		}
		if statusOf() != conflictPending {
			t.Fatalf("expected pending conflict, got %q", statusOf())
		}
		// JIT owner must be untouched by the collision.
		if ownerOf() != "jit" {
			t.Fatalf("JIT owner must be untouched, got %q", ownerOf())
		}
	})

	t.Run("repeated collision reuses the same pending conflict (no duplicate)", func(t *testing.T) {
		// First collision already created one. Trigger again.
		_, _ = ds.Provision(ctx, scA, map[string]any{
			"userName":   "jit@conf-a.example.com",
			"externalId": jitKey,
		}, "")
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM scim_identity_conflicts
			  WHERE workspace_id = $1 AND connection_id = $2 AND canonical_identity_key = $3 AND status = 'pending'`,
			wsA, connA, jitKey).Scan(&n); err != nil {
			t.Fatalf("count pending: %v", err)
		}
		if n != 1 {
			t.Fatalf("expected exactly 1 pending conflict, got %d", n)
		}
	})

	t.Run("pending conflict blocks SCIM mutation (update/deprovision)", func(t *testing.T) {
		active := false
		if _, serr := ds.Update(ctx, scA, jitUser, &userPatch{Active: &active}, ""); serr == nil || serr.Status != 409 {
			t.Fatalf("expected 409 update on pending-conflict identity, got %+v", serr)
		}
		if serr := ds.Deprovision(ctx, scA, jitUser, false, ""); serr == nil || serr.Status != 409 {
			t.Fatalf("expected 409 deprovision on pending-conflict identity, got %+v", serr)
		}
		if ownerOf() != "jit" {
			t.Fatalf("owner must remain jit, got %q", ownerOf())
		}
	})

	t.Run("Accept-Link requires identity.mapping.break_glass (ADMIN alone denied, nothing commits)", func(t *testing.T) {
		// plainAdmin holds ADMIN but NOT the permission.
		serr := ds.AcceptLink(ctx, wsA, connA, plainAdmin, "plain@conf-a.example.com", jitKey, "i am admin")
		if serr == nil || serr.Status != 403 {
			t.Fatalf("expected 403 without break_glass, got %+v", serr)
		}
		// Nothing committed: ownership unchanged, conflict still pending, no approval audit.
		if ownerOf() != "jit" {
			t.Fatalf("ownership must be unchanged on denied accept, got %q", ownerOf())
		}
		if statusOf() != conflictPending {
			t.Fatalf("conflict must stay pending, got %q", statusOf())
		}
		if countAudit(actionConflictApproved) != 0 {
			t.Fatalf("no approval audit may be written on denied accept")
		}
	})

	t.Run("Accept-Link creates link, sets provisioning_owner=scim, preserves provisioned_by/roles/policies/devices, audits", func(t *testing.T) {
		serr := ds.AcceptLink(ctx, wsA, connA, adminActor, "actor@conf-a.example.com", jitKey, "approved by admin")
		if serr != nil {
			t.Fatalf("accept-link: %v", serr)
		}
		// provisioning_owner -> scim
		if ownerOf() != "scim" {
			t.Fatalf("expected provisioning_owner=scim, got %q", ownerOf())
		}
		// provisioned_by preserved (immutable)
		var provBy string
		_ = pool.QueryRow(ctx, `SELECT provisioned_by FROM users WHERE id = $1`, jitUser).Scan(&provBy)
		if provBy != "jit" {
			t.Fatalf("provisioned_by must be preserved, got %q", provBy)
		}
		// external_identities link confirmed for the connection+key
		var links int
		_ = pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM external_identities WHERE user_id = $1 AND connection_id = $2 AND subject = $3`,
			jitUser, connA, jitKey).Scan(&links)
		if links != 1 {
			t.Fatalf("expected exactly 1 external_identity link, got %d", links)
		}
		// local permission preserved
		var perms int
		_ = pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM workspace_permissions WHERE workspace_id = $1 AND user_id = $2`,
			wsA, jitUser).Scan(&perms)
		if perms != 1 {
			t.Fatalf("local permission must be preserved, got %d rows", perms)
		}
		// device preserved
		var devs int
		_ = pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM client_devices WHERE workspace_id = $1 AND user_id = $2`,
			wsA, jitUser).Scan(&devs)
		if devs != 1 {
			t.Fatalf("device must be preserved, got %d rows", devs)
		}
		// conflict now linked
		if statusOf() != conflictApproved {
			t.Fatalf("expected linked conflict, got %q", statusOf())
		}
		// approval audited exactly once
		if n := countAudit(actionConflictApproved); n != 1 {
			t.Fatalf("expected exactly 1 approval audit, got %d", n)
		}
	})

	t.Run("rejected conflict never auto-approves on a later SCIM retry", func(t *testing.T) {
		// Fresh JIT user + collision -> reject it.
		jit2 := uuid.NewString()
		key2 := "okta-jit-conflict-2"
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, tenant_id, email, provider, provider_sub, role, status, provisioned_by, provisioning_owner)
			 VALUES ($1,$2,$3,$4,$5,'member','active','manual','manual')`,
			jit2, wsA, "jit2@conf-a.example.com", "okta", key2); err != nil {
			t.Fatalf("seed jit2: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO external_identities (tenant_id, user_id, connection_id, issuer, subject)
			 VALUES ($1,$2,$3,$4,$5)`,
			wsA, jit2, connA, "https://okta.example.com", key2); err != nil {
			t.Fatalf("seed jit2 link: %v", err)
		}
		// collision
		_, _ = ds.Provision(ctx, scA, map[string]any{"userName": "jit2@conf-a.example.com", "externalId": key2}, "")
		// reject
		if serr := ds.Reject(ctx, wsA, connA, adminActor, "actor@conf-a.example.com", key2, "not this one"); serr != nil {
			t.Fatalf("reject: %v", serr)
		}
		// a later SCIM retry must STILL 409 and NOT auto-approve.
		_, serr := ds.Provision(ctx, scA, map[string]any{"userName": "jit2@conf-a.example.com", "externalId": key2}, "")
		if serr == nil || serr.Status != 409 {
			t.Fatalf("expected 409 on rejected-conflict retry, got %+v", serr)
		}
		var o string
		_ = pool.QueryRow(ctx, `SELECT provisioning_owner FROM users WHERE id = $1`, jit2).Scan(&o)
		if o != "manual" {
			t.Fatalf("rejected conflict must not auto-approve; owner=%q", o)
		}
	})

	t.Run("rejected conflict continues blocking SCIM until reopen", func(t *testing.T) {
		jit2 := jitUserByKey(ctx, t, pool, wsA, connA, "okta-jit-conflict-2")
		_ = jit2
		// Update on a rejected-conflict identity (key2) must still be refused.
		active := false
		_, serr := ds.Update(ctx, scA, jitUserByKey(ctx, t, pool, wsA, connA, "okta-jit-conflict-2"), &userPatch{Active: &active}, "")
		if serr == nil || serr.Status != 409 {
			t.Fatalf("expected 409 update while rejected, got %+v", serr)
		}
	})

	t.Run("reopen moves rejected -> pending and emits reopened audit", func(t *testing.T) {
		key2 := "okta-jit-conflict-2"
		if serr := ds.Reopen(ctx, wsA, connA, adminActor, "actor@conf-a.example.com", key2, "reconsider"); serr != nil {
			t.Fatalf("reopen: %v", serr)
		}
		var s string
		_ = pool.QueryRow(ctx,
			`SELECT status FROM scim_identity_conflicts WHERE workspace_id = $1 AND connection_id = $2 AND canonical_identity_key = $3
			  ORDER BY created_at DESC LIMIT 1`,
			wsA, connA, key2).Scan(&s)
		if s != conflictPending {
			t.Fatalf("expected pending after reopen, got %q", s)
		}
		// reopen audit emitted
		var n int
		_ = pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM audit_logs WHERE action = $1 AND target_id = $2`,
			actionConflictReopened, jitUserByKey(ctx, t, pool, wsA, connA, key2)).Scan(&n)
		if n != 1 {
			t.Fatalf("expected exactly 1 reopened audit, got %d", n)
		}
	})

	t.Run("invalid transition fails safely (reject a linked/rejected again)", func(t *testing.T) {
		// jitKey is linked; rejecting a linked conflict is invalid.
		serr := ds.Reject(ctx, wsA, connA, adminActor, "actor@conf-a.example.com", jitKey, "nope")
		if serr == nil || serr.Status != 409 {
			t.Fatalf("expected 409 invalid transition, got %+v", serr)
		}
		if ownerOf() != "scim" {
			t.Fatalf("linked identity must be unchanged by invalid reject, owner=%q", ownerOf())
		}
	})

	t.Run("wrong workspace/connection cannot resolve the conflict", func(t *testing.T) {
		// Workspace B scope looking up A's key -> not found (404).
		c, err := ds.GetConflict(ctx, wsB, connB, jitKey)
		if err != nil {
			t.Fatalf("get conflict (B): %v", err)
		}
		if c != nil {
			t.Fatalf("workspace B must NOT see workspace A's conflict, got %+v", c)
		}
		// Accepting from B scope must 404 (no cross-workspace takeover).
		serr := ds.AcceptLink(ctx, wsB, connB, adminActor, "actor@conf-a.example.com", jitKey, "cross")
		if serr == nil || serr.Status != 404 {
			t.Fatalf("expected 404 for cross-scope accept, got %+v", serr)
		}
	})

	t.Run("no email-based resolution", func(t *testing.T) {
		// The canonical key is the subject, never the email. Looking up by the
		// JIT user's email must not resolve the conflict.
		c, err := ds.GetConflict(ctx, wsA, connA, "jit@conf-a.example.com")
		if err != nil {
			t.Fatalf("get conflict by email: %v", err)
		}
		if c != nil {
			t.Fatalf("conflict must NOT resolve by email, got %+v", c)
		}
	})

	t.Run("POST/PUT/PATCH/DELETE collision paths behave consistently", func(t *testing.T) {
		// New JIT identity to collide on every verb.
		jit3 := uuid.NewString()
		key3 := "okta-jit-conflict-3"
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, tenant_id, email, provider, provider_sub, role, status, provisioned_by, provisioning_owner)
			 VALUES ($1,$2,$3,$4,$5,'member','active','jit','jit')`,
			jit3, wsA, "jit3@conf-a.example.com", "okta", key3); err != nil {
			t.Fatalf("seed jit3: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO external_identities (tenant_id, user_id, connection_id, issuer, subject)
			 VALUES ($1,$2,$3,$4,$5)`,
			wsA, jit3, connA, "https://okta.example.com", key3); err != nil {
			t.Fatalf("seed jit3 link: %v", err)
		}
		active := false
		cases := []struct {
			name string
			call func() *SCIMError
		}{
			{"POST/provision", func() *SCIMError {
				_, serr := ds.Provision(ctx, scA, map[string]any{"userName": "jit3@conf-a.example.com", "externalId": key3}, "")
				return serr
			}},
			{"PUT/update", func() *SCIMError {
				_, serr := ds.Update(ctx, scA, jit3, &userPatch{Active: &active}, "")
				return serr
			}},
			{"PATCH/update", func() *SCIMError {
				_, serr := ds.Update(ctx, scA, jit3, &userPatch{Attributes: []attrChange{{Name: "active", Value: "false"}}}, "")
				return serr
			}},
			{"DELETE/deprovision", func() *SCIMError {
				return ds.Deprovision(ctx, scA, jit3, false, "")
			}},
		}
		for _, tc := range cases {
			serr := tc.call()
			if serr == nil || serr.Status != 409 || serr.ScimType != "identity_conflict" {
				t.Fatalf("%s: expected 409 identity_conflict, got %+v", tc.name, serr)
			}
			// Each verb must have created/reused the pending conflict.
			var s string
			_ = pool.QueryRow(ctx,
				`SELECT status FROM scim_identity_conflicts WHERE workspace_id = $1 AND connection_id = $2 AND canonical_identity_key = $3
				  ORDER BY created_at DESC LIMIT 1`,
				wsA, connA, key3).Scan(&s)
			if s != conflictPending {
				t.Fatalf("%s: expected pending conflict after verb, got %q", tc.name, s)
			}
			if ownerOf() != "jit" {
				// note: ownerOf reads jitUser (key1); assert jit3 instead
				_ = 0
			}
			var o3 string
			_ = pool.QueryRow(ctx, `SELECT provisioning_owner FROM users WHERE id = $1`, jit3).Scan(&o3)
			if o3 != "jit" {
				t.Fatalf("%s: jit3 owner must remain jit, got %q", tc.name, o3)
			}
		}
	})
}

// jitUserByKey returns the user id occupying a canonical key (test helper).
func jitUserByKey(ctx context.Context, t *testing.T, pool *pgxpool.Pool, ws, conn, key string) string {
	t.Helper()
	var uid string
	if err := pool.QueryRow(ctx,
		`SELECT ei.user_id FROM external_identities ei
		  WHERE ei.connection_id = $1 AND ei.subject = $2 AND ei.tenant_id = $3
		  LIMIT 1`,
		conn, key, ws).Scan(&uid); err != nil {
		t.Fatalf("lookup jit user by key %s: %v", key, err)
	}
	return uid
}
