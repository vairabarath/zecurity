package idp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/pki"
)

// base64Enc is a stand-in for pki.Service that implements only the two secret
// methods the store uses (reversibly, so round-trips work). It embeds pki.Service
// so it satisfies the full interface; the real crypto is covered in internal/pki.
type base64Enc struct{ pki.Service }

func (base64Enc) EncryptSecret(plaintext []byte, _ string) (string, string, error) {
	return base64.StdEncoding.EncodeToString(plaintext), "test-nonce", nil
}
func (base64Enc) DecryptSecret(ciphertextB64, _, _ string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(ciphertextB64)
}

func TestIdpStore_Integration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("idp_test_%d", os.Getpid())

	adminPool := mustConnectPool(t, ctx, adminDSN)
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
		t.Fatalf("drop stale test db: %v", err)
	}
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

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

	t.Run("unconfigured workspace resolves to the platform Google IdP", func(t *testing.T) {
		ws := mustInsertWorkspace(t, ctx, pool, "ws-idp-1")
		conns, err := store.ListForWorkspace(ctx, ws)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(conns) != 1 {
			t.Fatalf("want exactly the platform IdP, got %d: %+v", len(conns), conns)
		}
		g := conns[0]
		if g.TenantID != nil || !g.Managed || g.Provider != "google" || g.Issuer != "https://accounts.google.com" {
			t.Fatalf("unexpected platform seed: %+v", g)
		}
		if g.ClientSecret != "" {
			t.Fatal("managed connection must not carry a stored secret")
		}
	})

	t.Run("workspace BYO connection round-trips the encrypted secret", func(t *testing.T) {
		ws := mustInsertWorkspace(t, ctx, pool, "ws-idp-2")
		created, err := store.CreateWorkspaceConnection(ctx, ws, CreateInput{
			Provider:      "okta",
			DisplayName:   "Acme Okta",
			Issuer:        "https://acme.okta.com",
			ClientID:      "client-123",
			ClientSecret:  "s3cr3t-value",
			ClaimMappings: map[string]any{"groups": "roles"},
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if created.Managed || created.TenantID == nil || *created.TenantID != ws {
			t.Fatalf("workspace connection should be unmanaged + tenant-scoped: %+v", created)
		}
		if created.ClientSecret != "s3cr3t-value" {
			t.Fatalf("secret round-trip on create: got %q", created.ClientSecret)
		}

		got, err := store.GetByID(ctx, created.ID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.ClientSecret != "s3cr3t-value" {
			t.Fatalf("secret round-trip on read: got %q", got.ClientSecret)
		}
		if got.ClaimMappings["groups"] != "roles" {
			t.Fatalf("claim_mappings round-trip: got %+v", got.ClaimMappings)
		}

		// Resolution now returns platform (first) + this workspace connection.
		conns, err := store.ListForWorkspace(ctx, ws)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(conns) != 2 || conns[0].Provider != "google" || conns[1].Provider != "okta" {
			t.Fatalf("want [google, okta], got %+v", conns)
		}
	})

	t.Run("duplicate issuer within a tenant is rejected", func(t *testing.T) {
		ws := mustInsertWorkspace(t, ctx, pool, "ws-idp-3")
		in := CreateInput{Provider: "oidc", DisplayName: "A", Issuer: "https://dup.example.com"}
		if _, err := store.CreateWorkspaceConnection(ctx, ws, in); err != nil {
			t.Fatalf("first create: %v", err)
		}
		if _, err := store.CreateWorkspaceConnection(ctx, ws, in); err == nil {
			t.Fatal("expected unique-issuer violation on duplicate")
		}
	})

	t.Run("workspace cannot delete the platform connection", func(t *testing.T) {
		ws := mustInsertWorkspace(t, ctx, pool, "ws-idp-4")
		platform, err := store.GetByIssuer(ctx, ws, "https://accounts.google.com")
		if err != nil {
			t.Fatalf("get platform: %v", err)
		}
		// Deleting via a tenant guard must not touch the tenant_id-NULL platform row.
		if err := store.DeleteWorkspaceConnection(ctx, ws, platform.ID); !errors.Is(err, ErrConnectionNotFound) {
			t.Fatalf("want ErrConnectionNotFound deleting platform row, got %v", err)
		}
	})
}

// ── minimal DB harness (mirrors transport/policy integration helpers) ──────────

func mustConnectPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func withDBName(dsn, dbName string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			return fmt.Errorf("execute %s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

func mustInsertWorkspace(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $1, 'active', $1 || '.test') RETURNING id::text`, slug,
	).Scan(&id); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	return id
}
