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
