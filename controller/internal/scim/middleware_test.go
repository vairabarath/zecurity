package scim

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestScimAuthMiddleware(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	dbName := "scim_mw_test_" + uuid.NewString()[0:8]

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

	ws := mustInsertWorkspace(t, ctx, pool, "ws-mw")
	conn := mustInsertConnection(t, ctx, pool, ws, "https://mw.example.com")

	store := newTestStore(pool, 24*time.Hour)

	// A probe handler that reports whether the authenticated token reached it.
	probe := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := TokenFromContext(r.Context())
		if tok == nil {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("X-Workspace", tok.WorkspaceID)
		w.Header().Set("X-Connection", tok.ConnectionID)
		w.WriteHeader(http.StatusOK)
	})

	handler := store.AuthMiddleware(probe)

	good, err := store.Mint(ctx, ws, conn, nil, nil, nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	do := func(t *testing.T, method, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/scim/v2/Users", nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	t.Run("valid bearer token reaches handler with scope", func(t *testing.T) {
		rec := do(t, "GET", "Bearer "+good.Plaintext)
		if rec.Code != http.StatusOK {
			t.Fatalf("want 200, got %d (%s)", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("X-Workspace") != ws {
			t.Fatalf("workspace not bound: %s", rec.Header().Get("X-Workspace"))
		}
		if rec.Header().Get("X-Connection") != conn {
			t.Fatalf("connection not bound: %s", rec.Header().Get("X-Connection"))
		}
	})

	t.Run("missing Authorization header → 401", func(t *testing.T) {
		rec := do(t, "GET", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "urn:ietf:params:scim:api:messages:error") {
			t.Fatalf("response is not a SCIM error envelope: %s", rec.Body.String())
		}
	})

	t.Run("malformed Authorization header → 401", func(t *testing.T) {
		rec := do(t, "GET", "Token abcdef")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", rec.Code)
		}
		// Missing scheme entirely.
		rec2 := do(t, "GET", good.Plaintext)
		if rec2.Code != http.StatusUnauthorized {
			t.Fatalf("want 401 for missing scheme, got %d", rec2.Code)
		}
	})

	t.Run("unknown/tampered token → 401 (no enumeration)", func(t *testing.T) {
		rec := do(t, "GET", "Bearer "+good.Plaintext+"garbage")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", rec.Code)
		}
		rec2 := do(t, "GET", "Bearer not-a-real-token")
		if rec2.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", rec2.Code)
		}
	})

	t.Run("expired token → 401", func(t *testing.T) {
		expired, err := store.Mint(ctx, ws, conn, nil, nil, ptrTime(time.Now().Add(-time.Hour)))
		if err != nil {
			t.Fatalf("mint expired: %v", err)
		}
		rec := do(t, "GET", "Bearer "+expired.Plaintext)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("want 401 for expired token, got %d", rec.Code)
		}
	})

	t.Run("revoked token → 401", func(t *testing.T) {
		rev, err := store.Mint(ctx, ws, conn, nil, nil, nil)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if err := store.Revoke(ctx, ws, conn, rev.Token.ID); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		rec := do(t, "GET", "Bearer "+rev.Plaintext)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("want 401 for revoked token, got %d", rec.Code)
		}
	})
}
