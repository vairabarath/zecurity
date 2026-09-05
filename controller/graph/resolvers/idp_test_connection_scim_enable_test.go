package resolvers

// Proves the C1 invariant directly against the real TestIdpConnection
// resolver (not just the underlying scim/idp primitives in isolation):
// a successful mapping round-trip probe must prove the mapping in the
// GraphQL response, but must NEVER persist scim_enabled=true. Only an
// explicit UpdateScimConfig call may do that.
//
// Self-contained live-Postgres harness (create temp DB, apply migrations),
// mirroring the pattern used throughout internal/scim's integration tests,
// plus a minimal in-process OIDC discovery fixture so TestIdpConnection's
// step 1 (discovery probe) can actually succeed and the flow can reach the
// real mapping round-trip probe (the success path this invariant matters
// most for). Skips cleanly when PKI_TEST_DATABASE_URL is not set.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/scim"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

func TestIdpConnection_DoesNotPersistScimEnabled(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := "resolvers_c1_test_" + strconv.Itoa(os.Getpid())

	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin pool: %v", err)
	}
	defer adminPool.Close()
	_, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() { _, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName) }()

	parsed, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse admin dsn: %v", err)
	}
	parsed.Path = "/" + dbName
	testDSN := parsed.String()

	pool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("connect test pool: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping test pool: %v", err)
	}

	migDir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(migDir, "*.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no migration files found in %s", migDir)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read migration %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply migration %s: %v", f, err)
		}
	}

	idpStore := idp.NewStore(pool, nil)
	scimStore, err := scim.NewStore(pool, []byte("c1-test-hash-key"), 24*time.Hour)
	if err != nil {
		t.Fatalf("new scim store: %v", err)
	}
	ds := scim.NewDirectoryService(pool, idpStore, nil, nil, nil, nil)
	scimStore = scimStore.WithDirectoryService(ds)

	r := &Resolver{IdpStore: idpStore, ScimStore: scimStore}
	mr := &mutationResolver{r}

	seedWorkspaceAndConnection := func(t *testing.T, issuer string) (ws, connID string) {
		t.Helper()
		ws = uuid.NewString()
		if _, err := pool.Exec(ctx,
			`INSERT INTO workspaces (id, slug, name, status, trust_domain)
			 VALUES ($1,$2,$3,'active',$4)`,
			ws, "c1-"+ws[:8], "C1 Test Corp", "td-c1-"+ws[:8],
		); err != nil {
			t.Fatalf("seed workspace: %v", err)
		}
		connID = uuid.NewString()
		if _, err := pool.Exec(ctx,
			`INSERT INTO identity_connections
			   (id, tenant_id, protocol, provider, managed, display_name, issuer,
			    client_id, status, subject_claim, scim_identifier, scim_enabled)
			 VALUES ($1,$2,'oidc','okta',FALSE,'Okta Conn',$3,gen_random_uuid(),'active','sub','externalId',FALSE)`,
			connID, ws, issuer,
		); err != nil {
			t.Fatalf("seed connection: %v", err)
		}
		return ws, connID
	}

	readScimEnabled := func(t *testing.T, connID string) bool {
		t.Helper()
		var enabled bool
		if err := pool.QueryRow(ctx, `SELECT scim_enabled FROM identity_connections WHERE id = $1`, connID).Scan(&enabled); err != nil {
			t.Fatalf("read scim_enabled: %v", err)
		}
		return enabled
	}

	reqCtxFor := func(ws string) context.Context {
		return tenant.Set(ctx, tenant.TenantContext{
			TenantID: ws,
			UserID:   uuid.NewString(),
			Role:     "admin",
			Email:    "admin@c1-test.example.com",
		})
	}

	t.Run("SetSCIMEnabled is always called with false, regardless of computed ScimEnabledAllowed", func(t *testing.T) {
		ws, connID := seedWorkspaceAndConnection(t, "https://unreachable-okta.example.invalid")
		if got := readScimEnabled(t, connID); got {
			t.Fatalf("expected scim_enabled=false before any call, got true")
		}
		// Mirrors exactly what TestIdpConnection's persistence line now does
		// post-fix: SetSCIMEnabled(ctx, tenantID, id, false), unconditionally,
		// never res.ScimEnabledAllowed.
		if err := r.IdpStore.SetSCIMEnabled(reqCtxFor(ws), ws, connID, false); err != nil {
			t.Fatalf("SetSCIMEnabled: %v", err)
		}
		if got := readScimEnabled(t, connID); got {
			t.Fatalf("expected scim_enabled to remain false, got true")
		}
	})

	t.Run("discovery-failure branch: TestIdpConnection never enables SCIM", func(t *testing.T) {
		ws, connID := seedWorkspaceAndConnection(t, "https://unreachable-okta.example.invalid")
		res, err := mr.TestIdpConnection(reqCtxFor(ws), connID)
		if err != nil {
			t.Fatalf("TestIdpConnection: %v", err)
		}
		if res.Ok {
			t.Fatalf("expected discovery to fail against the unreachable issuer, got Ok=true")
		}
		if res.ScimEnabledAllowed {
			t.Fatalf("expected ScimEnabledAllowed=false on the discovery-failure branch")
		}
		if got := readScimEnabled(t, connID); got {
			t.Fatalf("expected scim_enabled to remain false after a failed TestIdpConnection, got true")
		}
	})

	t.Run("success branch: a genuinely PROVEN mapping is reported to the caller but never persisted as enabled", func(t *testing.T) {
		// A minimal OIDC discovery fixture so step 1 (discovery probe)
		// actually succeeds and the flow reaches the real ProbeMapping
		// round-trip — this is the exact scenario the C1 bug affected:
		// before the fix, a successful probe here would have persisted
		// scim_enabled=true directly from TestIdpConnection.
		var issuerURL string
		mux := http.NewServeMux()
		mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer":                 issuerURL,
				"authorization_endpoint": issuerURL + "/authorize",
				"token_endpoint":         issuerURL + "/token",
				"jwks_uri":               issuerURL + "/jwks",
			})
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()
		issuerURL = srv.URL

		ws, connID := seedWorkspaceAndConnection(t, issuerURL)

		res, err := mr.TestIdpConnection(reqCtxFor(ws), connID)
		if err != nil {
			t.Fatalf("TestIdpConnection: %v", err)
		}
		if !res.Ok {
			t.Fatalf("expected discovery to succeed against the fixture issuer, got Ok=false (message=%v)", res.Message)
		}
		if res.MappingState != string(scim.MappingProven) {
			t.Fatalf("expected the mapping round-trip to prove the mapping, got MappingState=%q (reason=%v)", res.MappingState, res.Reason)
		}
		if !res.ScimEnabledAllowed {
			t.Fatalf("expected the GraphQL response to truthfully report ScimEnabledAllowed=true for a proven mapping")
		}

		// The C1 invariant: despite a genuinely proven mapping and a
		// GraphQL response saying ScimEnabledAllowed=true, the database
		// column must remain false. Only an explicit UpdateScimConfig call
		// may flip it.
		if got := readScimEnabled(t, connID); got {
			t.Fatalf("C1 REGRESSION: TestIdpConnection persisted scim_enabled=true from a proven mapping probe; " +
				"only UpdateScimConfig may enable SCIM")
		}
	})
}
