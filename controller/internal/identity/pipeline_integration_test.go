package identity_test

// External test package (identity_test) rather than white-box: this test imports
// internal/bootstrap, which imports internal/identity, so it must not be compiled
// into package identity itself (that would be an import cycle). It exercises the
// pipeline through its exported API against a real Postgres.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/auth/providers"
	"github.com/yourorg/ztna/controller/internal/bootstrap"
	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/pki"
)

func TestIdentityPipeline_Integration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	t.Setenv("PKI_MASTER_SECRET", "identity-pipeline-integration-master-secret")

	ctx := context.Background()
	dbName := fmt.Sprintf("identity_test_%d", os.Getpid())

	adminPool := mustConnectIdentityPool(t, ctx, adminDSN)
	defer adminPool.Close()
	_, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() { _, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName) }()

	testDSN, err := withIdentityDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	pool := mustConnectIdentityPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyAllMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	pkiSvc, err := pki.Init(ctx, pool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}

	bootstrapSvc := &bootstrap.Service{Pool: pool, PKIService: pkiSvc}
	svc := identity.NewService(pool, identity.NewLinker(bootstrapSvc), identity.NewAuditSink(pool))

	// The seeded platform Google connection (migration 031) — a real
	// identity_connections row so the external_identities FK resolves.
	var googleConnID string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM identity_connections WHERE provider = 'google' AND tenant_id IS NULL`,
	).Scan(&googleConnID); err != nil {
		t.Fatalf("read seeded google connection: %v", err)
	}

	// Same email throughout — the pipeline must never key on it.
	authFor := func(subject string) *providers.AuthenticationContext {
		return &providers.AuthenticationContext{
			Provider: "google",
			Issuer:   "https://accounts.google.com",
			Subject:  subject,
			Email:    "alice@example.com",
			Name:     "Alice",
		}
	}

	t.Run("first login JIT-creates a user and its identity link", func(t *testing.T) {
		p, err := svc.Authenticate(ctx, authFor("google-sub-A"), googleConnID, "Acme A")
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if p.Core.UserID == "" || p.Core.TenantID == "" || p.Core.Generation != 1 {
			t.Fatalf("unexpected principal: %+v", p.Core)
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM external_identities WHERE connection_id = $1 AND subject = $2`,
			googleConnID, "google-sub-A",
		).Scan(&n); err != nil {
			t.Fatalf("count external_identities: %v", err)
		}
		if n != 1 {
			t.Fatalf("expected exactly one external_identity link, got %d", n)
		}
	})

	t.Run("returning login resolves the same canonical user", func(t *testing.T) {
		p1, err := svc.Authenticate(ctx, authFor("google-sub-A"), googleConnID, "")
		if err != nil {
			t.Fatalf("first: %v", err)
		}
		p2, err := svc.Authenticate(ctx, authFor("google-sub-A"), googleConnID, "")
		if err != nil {
			t.Fatalf("second: %v", err)
		}
		if p1.Core.UserID != p2.Core.UserID {
			t.Fatalf("returning login must resolve the same user: %s vs %s", p1.Core.UserID, p2.Core.UserID)
		}
	})

	t.Run("same email different subject yields distinct users (no email-merge)", func(t *testing.T) {
		pA, err := svc.Authenticate(ctx, authFor("google-sub-A"), googleConnID, "")
		if err != nil {
			t.Fatalf("A: %v", err)
		}
		pB, err := svc.Authenticate(ctx, authFor("google-sub-B"), googleConnID, "Acme B")
		if err != nil {
			t.Fatalf("B: %v", err)
		}
		if pA.Core.UserID == pB.Core.UserID {
			t.Fatal("same email, different subject must NOT merge into one canonical user")
		}
	})

	t.Run("suspended user is rejected at the lifecycle gate", func(t *testing.T) {
		p, err := svc.Authenticate(ctx, authFor("google-sub-C"), googleConnID, "Acme C")
		if err != nil {
			t.Fatalf("provision C: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE users SET status = 'suspended' WHERE id = $1`, p.Core.UserID); err != nil {
			t.Fatalf("suspend: %v", err)
		}
		if _, err := svc.Authenticate(ctx, authFor("google-sub-C"), googleConnID, ""); !errors.Is(err, identity.ErrUserNotActive) {
			t.Fatalf("expected ErrUserNotActive for suspended user, got %v", err)
		}
	})

	t.Run("generation bump increments and revokes older tokens", func(t *testing.T) {
		p, err := svc.Authenticate(ctx, authFor("google-sub-D"), googleConnID, "Acme D")
		if err != nil {
			t.Fatalf("provision D: %v", err)
		}
		oldGen := p.Core.Generation
		rev := identity.NewRevoker(pool, nil, identity.NopPublisher{})
		newGen, err := rev.BumpGeneration(ctx, p.Core.TenantID, p.Core.UserID, "admin@example.com")
		if err != nil {
			t.Fatalf("bump: %v", err)
		}
		if newGen != oldGen+1 {
			t.Fatalf("expected generation %d, got %d", oldGen+1, newGen)
		}
		if err := identity.CheckGeneration(oldGen, newGen); !errors.Is(err, identity.ErrSessionRevoked) {
			t.Fatalf("a token minted at the old generation must be revoked, got %v", err)
		}
	})
}

// ── minimal DB harness (mirrors internal/idp integration helpers) ───────────

func mustConnectIdentityPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
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

func withIdentityDBName(dsn, dbName string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func applyAllMigrations(ctx context.Context, pool *pgxpool.Pool) error {
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
