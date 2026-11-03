package idp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
)

// TestIdpStore_AdminMethods_Integration covers the Phase 6 management methods
// (update / list-own / users-for-connection / active-count) against real
// Postgres. Reuses the harness helpers from store_integration_test.go.
func TestIdpStore_AdminMethods_Integration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("idp_admin_test_%d", os.Getpid())

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
	if err := applyMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	store := NewStore(pool, base64Enc{})
	ws := mustInsertWorkspace(t, ctx, pool, "ws-idp-admin")

	created, err := store.CreateWorkspaceConnection(ctx, ws, CreateInput{
		Provider:     "okta",
		DisplayName:  "Acme Okta",
		Issuer:       "https://acme.okta.com",
		ClientID:     "client-1",
		ClientSecret: "secret-1",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	t.Run("update changes fields and rotates the secret", func(t *testing.T) {
		newName := "Acme Okta (prod)"
		newSecret := "secret-2"
		updated, err := store.UpdateWorkspaceConnection(ctx, ws, created.ID, UpdateInput{
			DisplayName:  &newName,
			ClientSecret: &newSecret,
		})
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if updated.DisplayName != newName {
			t.Fatalf("display name not updated: %q", updated.DisplayName)
		}
		if updated.ClientSecret != newSecret {
			t.Fatalf("rotated secret not round-tripped: %q", updated.ClientSecret)
		}
		// Issuer is immutable and must be untouched.
		if updated.Issuer != "https://acme.okta.com" {
			t.Fatalf("issuer changed unexpectedly: %q", updated.Issuer)
		}
	})

	t.Run("update on the managed platform connection is refused", func(t *testing.T) {
		platform, err := store.GetPlatformByProvider(ctx, "google")
		if err != nil {
			t.Fatalf("get platform: %v", err)
		}
		name := "hijacked"
		_, err = store.UpdateWorkspaceConnection(ctx, ws, platform.ID, UpdateInput{DisplayName: &name})
		if !errors.Is(err, ErrConnectionNotFound) {
			t.Fatalf("expected ErrConnectionNotFound updating platform, got %v", err)
		}
	})

	t.Run("list-own returns only the workspace connection, not the platform IdP", func(t *testing.T) {
		conns, err := store.ListWorkspaceConnections(ctx, ws)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(conns) != 1 || conns[0].Provider != "okta" {
			t.Fatalf("want exactly the okta connection, got %+v", conns)
		}
	})

	t.Run("active count reflects status changes", func(t *testing.T) {
		n, err := store.CountActiveWorkspaceConnections(ctx, ws)
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Fatalf("expected 1 active, got %d", n)
		}
		if err := store.SetStatus(ctx, ws, created.ID, "disabled"); err != nil {
			t.Fatalf("disable: %v", err)
		}
		if n, _ = store.CountActiveWorkspaceConnections(ctx, ws); n != 0 {
			t.Fatalf("expected 0 active after disable, got %d", n)
		}
		if err := store.SetStatus(ctx, ws, created.ID, "active"); err != nil {
			t.Fatalf("re-enable: %v", err)
		}
	})

	t.Run("platform login toggle defaults true and round-trips", func(t *testing.T) {
		enabled, err := store.PlatformLoginEnabled(ctx, ws)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if !enabled {
			t.Fatal("platform login should default to true")
		}
		if err := store.SetPlatformLoginEnabled(ctx, ws, false); err != nil {
			t.Fatalf("disable: %v", err)
		}
		if enabled, _ = store.PlatformLoginEnabled(ctx, ws); enabled {
			t.Fatal("expected platform login disabled after set")
		}
		if err := store.SetPlatformLoginEnabled(ctx, ws, true); err != nil {
			t.Fatalf("re-enable: %v", err)
		}
	})

	// Regression for Sprint 17 FE Phase 7 Finding F7-7 (migration
	// 036_idp_connection_deleted_issuer_reuse.sql): soft-deleting a connection
	// used to permanently occupy its (tenant_id, issuer) slot — the unique
	// index did not exclude status='deleted' — so a fresh connection for the
	// same issuer could never be created again. Reproduced live against a
	// real Okta org 2026-08-28.
	t.Run("issuer can be reused after the connection holding it is soft-deleted", func(t *testing.T) {
		dyingWS := mustInsertWorkspace(t, ctx, pool, "ws-idp-reuse")
		dying, err := store.CreateWorkspaceConnection(ctx, dyingWS, CreateInput{
			Provider:     "okta",
			DisplayName:  "Soon Deleted",
			Issuer:       "https://reuse-me.okta.com",
			ClientID:     "client-old",
			ClientSecret: "secret-old",
		})
		if err != nil {
			t.Fatalf("create original: %v", err)
		}

		if err := store.SoftDeleteConnection(ctx, dyingWS, dying.ID); err != nil {
			t.Fatalf("soft-delete: %v", err)
		}

		// Before the fix, this next create would fail with a duplicate-key
		// error on idx_idp_conn_ws_issuer even though the original connection
		// is gone.
		fresh, err := store.CreateWorkspaceConnection(ctx, dyingWS, CreateInput{
			Provider:     "okta",
			DisplayName:  "Fresh Connection",
			Issuer:       "https://reuse-me.okta.com",
			ClientID:     "client-new",
			ClientSecret: "secret-new",
		})
		if err != nil {
			t.Fatalf("expected the issuer to be reusable after soft-delete, got: %v", err)
		}
		if fresh.ID == dying.ID {
			t.Fatalf("expected a new connection row, got the same id back")
		}

		// The deleted connection must not resurface in the workspace's own
		// connection list (list-only fix, same finding).
		conns, err := store.ListWorkspaceConnections(ctx, dyingWS)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(conns) != 1 || conns[0].ID != fresh.ID {
			t.Fatalf("expected only the fresh connection listed, got %+v", conns)
		}

		// And the login-discovery view (ListForWorkspace) must not offer the
		// deleted connection as a sign-in option either.
		discoverable, err := store.ListForWorkspace(ctx, dyingWS)
		if err != nil {
			t.Fatalf("list for workspace: %v", err)
		}
		for _, c := range discoverable {
			if c.ID == dying.ID {
				t.Fatalf("deleted connection %s must not appear in login discovery", dying.ID)
			}
		}
	})

	t.Run("users-for-connection returns linked users", func(t *testing.T) {
		var userID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
			 VALUES ($1, 'user@acme.com', 'okta', 'okta-sub-1', 'member', 'active')
			 RETURNING id::text`, ws,
		).Scan(&userID); err != nil {
			t.Fatalf("insert user: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO external_identities (tenant_id, user_id, connection_id, issuer, subject)
			 VALUES ($1, $2, $3, 'https://acme.okta.com', 'okta-sub-1')`, ws, userID, created.ID,
		); err != nil {
			t.Fatalf("insert external_identity: %v", err)
		}

		ids, err := store.UserIDsForConnection(ctx, ws, created.ID)
		if err != nil {
			t.Fatalf("users-for-connection: %v", err)
		}
		if len(ids) != 1 || ids[0] != userID {
			t.Fatalf("expected [%s], got %+v", userID, ids)
		}
	})
}
