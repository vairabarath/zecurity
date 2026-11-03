package scim

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ── minimal DB harness (mirrors idp/policy integration helpers) ──────────────

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

// mustInsertConnection inserts an active identity_connections row for a workspace.
// Mint locks WHERE id=$1 AND tenant_id=$2 AND status='active', so the connection
// must be owned by (tenant_id) the same workspace and be active.
func mustInsertConnection(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, issuer string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO identity_connections
		   (tenant_id, protocol, provider, managed, display_name, issuer, status)
		 VALUES ($1, 'oidc', 'okta', FALSE, $2, $3, 'active')
		 RETURNING id::text`,
		workspaceID, "conn-"+issuer, issuer,
	).Scan(&id); err != nil {
		t.Fatalf("insert connection: %v", err)
	}
	return id
}

func newTestStore(pool *pgxpool.Pool, grace time.Duration) *Store {
	s, err := NewStore(pool, []byte("test-scim-hash-key"), grace)
	if err != nil {
		panic(err)
	}
	return s
}

// ── integration entrypoint ───────────────────────────────────────────────────

func TestScimTokenStore_Integration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	dbName := "scim_token_test_" + uuid.NewString()[0:8]

	adminPool := mustConnectPool(t, ctx, adminDSN)
	defer adminPool.Close()

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

	t.Run("mint returns plaintext and persists only the hash", func(t *testing.T) {
		ws := mustInsertWorkspace(t, ctx, pool, "ws-mint")
		conn := mustInsertConnection(t, ctx, pool, ws, "https://mint.example.com")

		store := newTestStore(pool, 24*time.Hour)
		res, err := store.Mint(ctx, ws, conn, nil, nil, nil)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if res.Plaintext == "" {
			t.Fatal("mint must return a non-empty plaintext token")
		}
		if res.Token.ID == "" {
			t.Fatal("mint must return token metadata with an id")
		}

		// The stored row must NOT carry plaintext: re-look it up and confirm
		// it round-trips only via the hash.
		got, err := store.Lookup(ctx, res.Plaintext)
		if err != nil {
			t.Fatalf("lookup by plaintext: %v", err)
		}
		if got.ID != res.Token.ID {
			t.Fatalf("lookup returned wrong token: got %s want %s", got.ID, res.Token.ID)
		}
		if got.WorkspaceID != ws || got.ConnectionID != conn {
			t.Fatalf("lookup scope wrong: %+v", got)
		}
		if got.LastUsedAt == nil {
			t.Fatal("successful lookup must set last_used_at")
		}
	})

	t.Run("HMAC lookup never matches plaintext or tampered input", func(t *testing.T) {
		ws := mustInsertWorkspace(t, ctx, pool, "ws-hmac")
		conn := mustInsertConnection(t, ctx, pool, ws, "https://hmac.example.com")

		store := newTestStore(pool, 24*time.Hour)
		res, err := store.Mint(ctx, ws, conn, nil, nil, nil)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}

		// Empty bearer → not found (fail-closed).
		if _, err := store.Lookup(ctx, ""); !errors.Is(err, ErrTokenNotFound) {
			t.Fatalf("empty plaintext: want ErrTokenNotFound, got %v", err)
		}
		// Tampered plaintext → hash mismatch → not found.
		if _, err := store.Lookup(ctx, res.Plaintext+"x"); !errors.Is(err, ErrTokenNotFound) {
			t.Fatalf("tampered plaintext: want ErrTokenNotFound, got %v", err)
		}
		// A different token's plaintext must not resolve to this one.
		other, err := store.Mint(ctx, ws, conn, nil, nil, nil)
		if err != nil {
			t.Fatalf("mint other: %v", err)
		}
		if other.Plaintext == res.Plaintext {
			t.Fatal("two mints produced identical plaintext (entropy failure)")
		}
		if _, err := store.Lookup(ctx, other.Plaintext); errors.Is(err, ErrTokenNotFound) {
			t.Fatal("other plaintext should resolve")
		}
		// The other plaintext must NOT resolve the first token.
		got, err := store.Lookup(ctx, other.Plaintext)
		if err != nil {
			t.Fatalf("lookup other: %v", err)
		}
		if got.ID == res.Token.ID {
			t.Fatal("lookup cross-matched two distinct plaintexts")
		}
	})

	t.Run("expired and revoked tokens fail lookup", func(t *testing.T) {
		ws := mustInsertWorkspace(t, ctx, pool, "ws-exp")
		conn := mustInsertConnection(t, ctx, pool, ws, "https://exp.example.com")

		store := newTestStore(pool, 24*time.Hour)

		now := time.Now()
		expired, err := store.Mint(ctx, ws, conn, nil, nil, ptrTime(now.Add(-time.Hour)))
		if err != nil {
			t.Fatalf("mint expired: %v", err)
		}
		if _, err := store.Lookup(ctx, expired.Plaintext); !errors.Is(err, ErrTokenNotFound) {
			t.Fatalf("expired token: want ErrTokenNotFound, got %v", err)
		}

		good, err := store.Mint(ctx, ws, conn, nil, nil, nil)
		if err != nil {
			t.Fatalf("mint good: %v", err)
		}
		if err := store.Revoke(ctx, ws, conn, good.Token.ID); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		if _, err := store.Lookup(ctx, good.Plaintext); !errors.Is(err, ErrTokenNotFound) {
			t.Fatalf("revoked token: want ErrTokenNotFound, got %v", err)
		}
	})

	t.Run("scope isolation across workspace and connection", func(t *testing.T) {
		wsA := mustInsertWorkspace(t, ctx, pool, "ws-A")
		wsB := mustInsertWorkspace(t, ctx, pool, "ws-B")
		connX := mustInsertConnection(t, ctx, pool, wsA, "https://ax.example.com")
		connY := mustInsertConnection(t, ctx, pool, wsA, "https://ay.example.com")

		store := newTestStore(pool, 24*time.Hour)
		res, err := store.Mint(ctx, wsA, connX, nil, nil, nil)
		if err != nil {
			t.Fatalf("mint A/X: %v", err)
		}

		// List is scoped to the exact (workspace, connection) pair.
		listAX, err := store.List(ctx, wsA, connX)
		if err != nil {
			t.Fatalf("list A/X: %v", err)
		}
		if len(listAX) != 1 {
			t.Fatalf("list A/X want 1, got %d", len(listAX))
		}
		for _, scope := range [][2]string{
			{wsA, connY}, // same workspace, different connection
			{wsB, connX}, // different workspace, same connection id space
		} {
			l, err := store.List(ctx, scope[0], scope[1])
			if err != nil {
				t.Fatalf("list %v: %v", scope, err)
			}
			if len(l) != 0 {
				t.Fatalf("list %v must be empty, got %d", scope, len(l))
			}
		}

		// Revoke is guarded by (workspace, connection): a mismatched scope
		// affects zero rows → ErrTokenNotFound.
		if err := store.Revoke(ctx, wsA, connY, res.Token.ID); !errors.Is(err, ErrTokenNotFound) {
			t.Fatalf("revoke A/Y: want ErrTokenNotFound, got %v", err)
		}
		if err := store.Revoke(ctx, wsB, connX, res.Token.ID); !errors.Is(err, ErrTokenNotFound) {
			t.Fatalf("revoke B/X: want ErrTokenNotFound, got %v", err)
		}
		// Correct scope revokes successfully.
		if err := store.Revoke(ctx, wsA, connX, res.Token.ID); err != nil {
			t.Fatalf("revoke A/X: %v", err)
		}
	})

	t.Run("dual-token rotation revokes oldest and trims middle to grace", func(t *testing.T) {
		ws := mustInsertWorkspace(t, ctx, pool, "ws-rot")
		conn := mustInsertConnection(t, ctx, pool, ws, "https://rot.example.com")

		// Freeze time so grace math is deterministic.
		t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
		grace := 24 * time.Hour
		store := newTestStore(pool, grace)
		store.now = func() time.Time { return t0 }

		t1, err := store.Mint(ctx, ws, conn, nil, nil, nil)
		if err != nil {
			t.Fatalf("mint 1: %v", err)
		}
		// Middle token: short expiry (2h), well inside the 24h grace window.
		shortExpiry := t0.Add(2 * time.Hour)
		t2, err := store.Mint(ctx, ws, conn, nil, nil, &shortExpiry)
		if err != nil {
			t.Fatalf("mint 2: %v", err)
		}
		// Third mint triggers the rotation rule.
		t3, err := store.Mint(ctx, ws, conn, nil, nil, nil)
		if err != nil {
			t.Fatalf("mint 3: %v", err)
		}

		list, err := store.List(ctx, ws, conn)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		byID := map[string]Token{}
		for _, tok := range list {
			byID[tok.ID] = tok
		}

		if byID[t1.Token.ID].RevokedAt == nil {
			t.Fatal("oldest token must be revoked after third mint")
		}
		mid := byID[t2.Token.ID]
		if mid.RevokedAt != nil {
			t.Fatal("middle token must remain active")
		}
		// Expiry must be the SHORTER of (existing 2h, now+grace 24h) — never extended.
		if mid.ExpiresAt == nil {
			t.Fatal("middle token must have an expiry after grace trim")
		}
		if !mid.ExpiresAt.Before(t0.Add(3 * time.Hour)) {
			t.Fatalf("middle expiry not trimmed to short window: %v", *mid.ExpiresAt)
		}
		if !mid.ExpiresAt.Before(t0.Add(grace)) {
			t.Fatalf("middle expiry was extended to the grace window: %v", *mid.ExpiresAt)
		}
		if byID[t3.Token.ID].RevokedAt != nil {
			t.Fatal("newest token must be active")
		}

		// Exactly two active tokens remain.
		active := 0
		for _, tok := range list {
			if tok.RevokedAt == nil && (tok.ExpiresAt == nil || tok.ExpiresAt.After(t0)) {
				active++
			}
		}
		if active != 2 {
			t.Fatalf("want exactly 2 active tokens, got %d", active)
		}
	})

	t.Run("sequential mints never exceed two active tokens", func(t *testing.T) {
		ws := mustInsertWorkspace(t, ctx, pool, "ws-seq")
		conn := mustInsertConnection(t, ctx, pool, ws, "https://seq.example.com")

		store := newTestStore(pool, 24*time.Hour)
		for i := 0; i < 5; i++ {
			if _, err := store.Mint(ctx, ws, conn, nil, nil, nil); err != nil {
				t.Fatalf("mint %d: %v", i, err)
			}
		}
		list, err := store.List(ctx, ws, conn)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		active := 0
		now := time.Now()
		for _, tok := range list {
			if tok.RevokedAt == nil && (tok.ExpiresAt == nil || tok.ExpiresAt.After(now)) {
				active++
			}
		}
		if active != maxActiveTokens {
			t.Fatalf("want %d active tokens, got %d", maxActiveTokens, active)
		}
	})

	t.Run("invalid scope is rejected", func(t *testing.T) {
		ws := mustInsertWorkspace(t, ctx, pool, "ws-bad")
		conn := mustInsertConnection(t, ctx, pool, ws, "https://bad.example.com")

		store := newTestStore(pool, 24*time.Hour)

		if _, err := store.Mint(ctx, "", conn, nil, nil, nil); !errors.Is(err, ErrInvalidScope) {
			t.Fatalf("mint empty workspace: want ErrInvalidScope, got %v", err)
		}
		if _, err := store.Mint(ctx, ws, "", nil, nil, nil); !errors.Is(err, ErrInvalidScope) {
			t.Fatalf("mint empty connection: want ErrInvalidScope, got %v", err)
		}
		// Minting against a non-existent/disabled connection must fail closed.
		if _, err := store.Mint(ctx, ws, uuid.NewString(), nil, nil, nil); !errors.Is(err, ErrInvalidScope) {
			t.Fatalf("mint unknown connection: want ErrInvalidScope, got %v", err)
		}
		if err := store.Revoke(ctx, ws, conn, ""); !errors.Is(err, ErrInvalidScope) {
			t.Fatalf("revoke empty token id: want ErrInvalidScope, got %v", err)
		}
	})
}

// ── pure unit tests (no DB) ──────────────────────────────────────────────────

func TestParseTokenID(t *testing.T) {
	if _, err := ParseTokenID("not-a-uuid"); err == nil {
		t.Fatal("ParseTokenID should reject non-UUID input")
	}
	id := uuid.New().String()
	got, err := ParseTokenID(id)
	if err != nil {
		t.Fatalf("ParseTokenID: %v", err)
	}
	if got != id {
		t.Fatalf("ParseTokenID returned %s, want %s", got, id)
	}
}

func TestNewStoreValidation(t *testing.T) {
	// nil pool (typed) must be rejected.
	if _, err := NewStore(nil, []byte("k"), 0); err == nil {
		t.Fatal("NewStore must reject a nil pool")
	}
	// empty hash key must be rejected.
	if _, err := NewStore(testNilPool(), []byte(""), 0); err == nil {
		t.Fatal("NewStore must reject an empty hash key")
	}
}

// testNilPool returns a typed nil *pgxpool.Pool without allocating a connection.
func testNilPool() *pgxpool.Pool { return nil }

func ptrTime(t time.Time) *time.Time { return &t }
