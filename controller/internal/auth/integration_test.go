package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeycompat"
	"github.com/yourorg/ztna/controller/internal/auth/providers"
	"github.com/yourorg/ztna/controller/internal/bootstrap"
	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/pki"
	"github.com/yourorg/ztna/controller/internal/valkeyurl"
)

func TestAuthIntegration_LoginBootstrapAndJWTIssue(t *testing.T) {
	adminDSN := os.Getenv("AUTH_TEST_DATABASE_URL")
	if adminDSN == "" {
		adminDSN = os.Getenv("PKI_TEST_DATABASE_URL")
	}
	if adminDSN == "" {
		t.Skip("AUTH_TEST_DATABASE_URL or PKI_TEST_DATABASE_URL not set")
	}

	valkeyURL := os.Getenv("AUTH_TEST_VALKEY_URL")
	if valkeyURL == "" {
		valkeyURL = "redis://localhost:6379/15"
	}

	ctx := context.Background()
	// The database env var is set, so this test is meant to run: an
	// unparseable URL or unreachable Valkey FAILS it rather than skipping
	// (CI once reported "ok" here for months without running it).
	valkeyOpts, valkeyClient := mustConnectAuthTestValkey(t, ctx, valkeyURL)

	// Never FlushDB: this may be a developer's shared Valkey. The service only
	// writes pkce:<random state> (consumed at callback) and refresh:{<user id>}:<sid>
	// (a fresh UUID in this run's throwaway database); the test deletes exactly
	// its own keys, and a sentinel under a unique prefix proves nothing else
	// in the database was touched.
	keyPrefix := fmt.Sprintf("authtest:%d:%d:", os.Getpid(), time.Now().UnixNano())
	sentinel := keyPrefix + "sentinel"
	if err := valkeyClient.Set(ctx, sentinel, "keep", 10*time.Minute).Err(); err != nil {
		t.Fatalf("set sentinel: %v", err)
	}
	t.Cleanup(func() { _ = valkeyClient.Del(context.Background(), sentinel).Err() })

	dbName := uniqueAuthTestDatabaseName(t)
	adminPool := mustConnectAuthTestPool(t, ctx, adminDSN)
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

	testDBDSN, err := withAuthTestDatabaseName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test database dsn: %v", err)
	}

	pool := mustConnectAuthTestPool(t, ctx, testDBDSN)
	defer pool.Close()

	if err := applyAuthMigration(ctx, pool); err != nil {
		t.Fatalf("apply migration: %v", err)
	}

	t.Setenv("PKI_MASTER_SECRET", "phase-7-auth-integration-master-secret")

	pkiSvc, err := pki.Init(ctx, pool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}

	bootstrapSvc := &bootstrap.Service{
		Pool:       pool,
		PKIService: pkiSvc,
	}

	// Real identity pipeline over the real connection store: the seeded Google
	// platform connection (migration 031) is a genuine identity_connections row,
	// so the external_identities.connection_id FK the pipeline writes resolves.
	// NopPublisher keeps this test focused on login/JWT (audit is covered separately).
	identitySvc := identity.NewService(pool, identity.NewLinker(bootstrapSvc), identity.NopPublisher{})
	idpStore := idp.NewStore(pool, pkiSvc)

	authSvcIface, err := NewService(Config{
		Pool:               pool,
		IdentityService:    identitySvc,
		IdpStore:           idpStore,
		JWTSecret:          "phase-7-auth-jwt-secret-32-bytes!!",
		JWTIssuer:          "zecurity-controller",
		GoogleClientID:     "test-google-client-id",
		GoogleClientSecret: "test-google-client-secret",
		RedirectURI:        "http://localhost:8080/auth/callback",
		ValkeyURL:          valkeyURL,
		JWTAccessTTL:       "15m",
		JWTRefreshTTL:      "168h",
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	svc := authSvcIface.(*serviceImpl)

	// Override the adapter-selection seam with a fake provider that returns a
	// canned AuthenticationContext — no real Google network calls. This mirrors
	// what the (Chunk A) OIDC/Google adapters produce, so the callback → Bootstrap
	// → JWT flow is exercised end-to-end against the real DB.
	origProviderFor := providerForFn
	t.Cleanup(func() { providerForFn = origProviderFor })
	providerForFn = func(_ *idp.Connection, _ GoogleCreds) (providers.IdentityProvider, error) {
		return &fakeProvider{result: &providers.AuthenticationContext{
			Provider:      "google",
			Issuer:        "https://accounts.google.com",
			Subject:       "google-sub-123",
			Email:         "alice@example.com",
			Name:          "Alice Example",
			EmailVerified: true,
		}}, nil
	}

	firstToken, firstRefreshCookie := runAuthRoundTrip(t, svc, "code-1")

	claims, err := svc.verifyAccessToken(firstToken)
	if err != nil {
		t.Fatalf("verifyAccessToken: %v", err)
	}
	if claims.Subject == "" || claims.TenantID == "" {
		t.Fatalf("expected JWT to contain subject and tenant claims")
	}
	if claims.Role != "admin" {
		t.Fatalf("expected admin role in JWT, got %s", claims.Role)
	}

	var workspaceCount int
	var userCount int
	var keyCount int
	var workspaceStatus string
	var tenantID string
	var userID string
	var lastLoginAt *time.Time
	if err := pool.QueryRow(
		ctx,
		`SELECT
			(SELECT COUNT(*) FROM workspaces),
			(SELECT COUNT(*) FROM users),
			(SELECT COUNT(*) FROM workspace_ca_keys),
			(SELECT status FROM workspaces LIMIT 1),
			(SELECT id::text FROM workspaces LIMIT 1),
			(SELECT id::text FROM users LIMIT 1),
			(SELECT last_login_at FROM users LIMIT 1)`,
	).Scan(&workspaceCount, &userCount, &keyCount, &workspaceStatus, &tenantID, &userID, &lastLoginAt); err != nil {
		t.Fatalf("query bootstrap state after first callback: %v", err)
	}

	if workspaceCount != 1 || userCount != 1 || keyCount != 1 {
		t.Fatalf("expected one workspace, user, and workspace CA; got workspaces=%d users=%d keys=%d",
			workspaceCount, userCount, keyCount)
	}
	if workspaceStatus != "active" {
		t.Fatalf("expected workspace status active, got %s", workspaceStatus)
	}
	if claims.TenantID != tenantID || claims.Subject != userID {
		t.Fatalf("expected JWT claims to match DB identity")
	}
	if lastLoginAt != nil {
		t.Fatalf("expected first callback to create user without last_login_at")
	}

	refreshSID := sidOf(t, firstRefreshCookie.Value)
	t.Cleanup(func() {
		_ = svc.redisClient.DeleteAllRefreshSessions(context.Background(), userID)
	})

	storedRefresh, found, err := svc.redisClient.GetRefreshSession(ctx, userID, refreshSID)
	if err != nil {
		t.Fatalf("GetRefreshSession: %v", err)
	}
	if !found || storedRefresh.Token == "" {
		t.Fatalf("expected refresh session to be stored in Redis")
	}
	if storedRefresh.Token != firstRefreshCookie.Value {
		t.Fatalf("expected refresh cookie to match Redis refresh token")
	}

	// The URL's database index is honoured: the service wrote the refresh key
	// into that database (read here through the test's own client, which
	// selected the same index), and not into database 0.
	if n, err := valkeyClient.Exists(ctx, refreshKey(userID, refreshSID)).Result(); err != nil || n != 1 {
		t.Fatalf("refresh key not found in db %d (exists=%d, err=%v)", valkeyOpts.DB, n, err)
	}
	if valkeyOpts.DB != 0 {
		db0 := mustConnectAuthTestValkeyDB(t, ctx, valkeyOpts.Addr, 0)
		if n, err := db0.Exists(ctx, refreshKey(userID, refreshSID)).Result(); err != nil || n != 0 {
			t.Fatalf("refresh key leaked into db 0 although the URL selects db %d (exists=%d, err=%v)", valkeyOpts.DB, n, err)
		}
	}

	secondToken, _ := runAuthRoundTrip(t, svc, "code-2")

	secondClaims, err := svc.verifyAccessToken(secondToken)
	if err != nil {
		t.Fatalf("verifyAccessToken second token: %v", err)
	}
	if secondClaims.Subject != userID || secondClaims.TenantID != tenantID || secondClaims.Role != "admin" {
		t.Fatalf("expected returning login to reuse existing identity")
	}

	// identity.Service.touchLastLogin writes last_login_at in a background
	// goroutine (so login latency is unaffected); wait for it instead of
	// reading once, which raced the write under load.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := pool.QueryRow(
			ctx,
			`SELECT last_login_at FROM users WHERE id = $1`,
			userID,
		).Scan(&lastLoginAt); err != nil {
			t.Fatalf("query last_login_at after second callback: %v", err)
		}
		if lastLoginAt != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if lastLoginAt == nil {
		t.Fatalf("expected returning login to update last_login_at within 5s")
	}

	var workspaceCountAfter int
	var userCountAfter int
	var keyCountAfter int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM workspaces").Scan(&workspaceCountAfter); err != nil {
		t.Fatalf("count workspaces after second callback: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM users").Scan(&userCountAfter); err != nil {
		t.Fatalf("count users after second callback: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM workspace_ca_keys").Scan(&keyCountAfter); err != nil {
		t.Fatalf("count workspace_ca_keys after second callback: %v", err)
	}
	if workspaceCountAfter != 1 || userCountAfter != 1 || keyCountAfter != 1 {
		t.Fatalf("expected returning login to avoid duplicate rows; got workspaces=%d users=%d keys=%d",
			workspaceCountAfter, userCountAfter, keyCountAfter)
	}

	// Nothing outside this run's keys was disturbed.
	if v, err := valkeyClient.Get(ctx, sentinel).Result(); err != nil || v != "keep" {
		t.Fatalf("sentinel key %s was removed or changed (value=%q, err=%v)", sentinel, v, err)
	}
}

func runAuthRoundTrip(t *testing.T, svc *serviceImpl, code string) (string, *http.Cookie) {
	t.Helper()

	ctx := context.Background()

	initPayload, err := svc.InitiateAuth(ctx, "google", nil, nil)
	if err != nil {
		t.Fatalf("InitiateAuth: %v", err)
	}
	if initPayload.RedirectURL == "" || initPayload.State == "" {
		t.Fatalf("expected initiateAuth payload to include redirect URL and state")
	}

	redirectURL, err := url.Parse(initPayload.RedirectURL)
	if err != nil {
		t.Fatalf("parse redirect URL: %v", err)
	}
	if redirectURL.Query().Get("state") != initPayload.State {
		t.Fatalf("expected redirect URL state to match payload state")
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code="+code+"&state="+url.QueryEscape(initPayload.State), nil)
	rec := httptest.NewRecorder()
	svc.CallbackHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302 callback response, got %d", rec.Code)
	}

	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "/auth/callback#token=") {
		t.Fatalf("expected redirect to frontend callback with token, got %s", location)
	}

	token := strings.TrimPrefix(location, "/auth/callback#token=")
	if token == "" {
		t.Fatalf("expected JWT in redirect fragment")
	}

	res := rec.Result()
	defer res.Body.Close()

	var refreshCookie *http.Cookie
	for _, cookie := range res.Cookies() {
		if cookie.Name == "refresh_token" {
			refreshCookie = cookie
			break
		}
	}
	if refreshCookie == nil {
		t.Fatalf("expected refresh_token cookie to be set")
	}

	return token, refreshCookie
}

// mustConnectAuthTestValkey parses the URL with the production parser and
// connects to the database it names. Any failure is fatal: the caller only
// gets here when the database env var says this test should run.
func mustConnectAuthTestValkey(t *testing.T, ctx context.Context, valkeyURL string) (valkeyurl.Options, valkeycompat.Cmdable) {
	t.Helper()
	opts, err := valkeyurl.Parse(valkeyURL)
	if err != nil {
		t.Fatalf("AUTH_TEST_VALKEY_URL %q: %v", valkeyURL, err)
	}
	return opts, mustConnectAuthTestValkeyDB(t, ctx, opts.Addr, opts.DB)
}

func mustConnectAuthTestValkeyDB(t *testing.T, ctx context.Context, addr string, db int) valkeycompat.Cmdable {
	t.Helper()
	client, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{addr}, SelectDB: db})
	if err != nil {
		t.Fatalf("valkey not available at %s (db %d): %v", addr, db, err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := client.Do(pingCtx, client.B().Ping().Build()).Error(); err != nil {
		client.Close()
		t.Fatalf("valkey not available at %s (db %d): %v", addr, db, err)
	}
	t.Cleanup(client.Close)
	return valkeycompat.NewAdapter(client)
}

func mustConnectAuthTestPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect pool: %v", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping pool: %v", err)
	}

	return pool
}

// applyAuthMigration applies every schema file in lexical order, like the
// other DB-backed suites. A hand-picked list went stale (it missed the file
// adding subject_claim) and the test failed whenever it actually ran.
func applyAuthMigration(ctx context.Context, pool *pgxpool.Pool) error {
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return fmt.Errorf("no migrations found")
	}
	sort.Strings(files)
	for _, f := range files {
		sqlBytes, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", filepath.Base(f), err)
		}
		if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("execute migration %s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

func withAuthTestDatabaseName(dsn, dbName string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}

	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func uniqueAuthTestDatabaseName(t *testing.T) string {
	t.Helper()

	name := strings.ToLower(t.Name())
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, " ", "_")

	return fmt.Sprintf("%s_%d_%d", name, os.Getpid(), time.Now().UnixNano())
}
