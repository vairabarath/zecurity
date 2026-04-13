package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/yourorg/ztna/controller/internal/bootstrap"
	"github.com/yourorg/ztna/controller/internal/pki"
)

func TestAuthIntegration_LoginBootstrapAndJWTIssue(t *testing.T) {
	adminDSN := os.Getenv("AUTH_TEST_DATABASE_URL")
	if adminDSN == "" {
		adminDSN = os.Getenv("PKI_TEST_DATABASE_URL")
	}
	if adminDSN == "" {
		t.Skip("AUTH_TEST_DATABASE_URL or PKI_TEST_DATABASE_URL not set")
	}

	redisURL := os.Getenv("AUTH_TEST_REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://localhost:6379/15"
	}

	ctx := context.Background()
	redisClient := mustConnectAuthTestRedis(t, ctx, redisURL)
	defer redisClient.Close()
	redisClient.FlushDB(ctx)

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

	authSvcIface, err := NewService(Config{
		Pool:               pool,
		BootstrapService:   bootstrapSvc,
		JWTSecret:          "phase-7-auth-jwt-secret",
		JWTIssuer:          "zecurity-controller",
		GoogleClientID:     "test-google-client-id",
		GoogleClientSecret: "test-google-client-secret",
		RedirectURI:        "http://localhost:8080/auth/callback",
		RedisURL:           redisURL,
		JWTAccessTTL:       "15m",
		JWTRefreshTTL:      "168h",
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	svc := authSvcIface.(*serviceImpl)

	origExchangeHook := exchangeCodeForTokensHook
	origVerifyHook := verifyGoogleIDTokenHook
	t.Cleanup(func() {
		exchangeCodeForTokensHook = origExchangeHook
		verifyGoogleIDTokenHook = origVerifyHook
	})

	exchangeCodeForTokensHook = func(_ *serviceImpl, _ context.Context, code, codeVerifier string) (*GoogleTokenResponse, error) {
		if code != "code-1" && code != "code-2" {
			return nil, fmt.Errorf("unexpected code: %s", code)
		}
		if codeVerifier == "" {
			return nil, fmt.Errorf("missing code verifier")
		}

		return &GoogleTokenResponse{
			IDToken:     "fake-google-id-token",
			AccessToken: "fake-google-access-token",
			ExpiresIn:   3600,
			TokenType:   "Bearer",
		}, nil
	}
	verifyGoogleIDTokenHook = func(_ context.Context, idToken, clientID string) (*GoogleClaims, error) {
		if idToken != "fake-google-id-token" {
			return nil, fmt.Errorf("unexpected id token: %s", idToken)
		}
		if clientID != "test-google-client-id" {
			return nil, fmt.Errorf("unexpected client id: %s", clientID)
		}

		return &GoogleClaims{
			Email:         "alice@example.com",
			EmailVerified: true,
			Name:          "Alice Example",
			Sub:           "google-sub-123",
		}, nil
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

	storedRefresh, found, err := svc.redisClient.GetRefreshToken(ctx, userID)
	if err != nil {
		t.Fatalf("GetRefreshToken: %v", err)
	}
	if !found || storedRefresh == "" {
		t.Fatalf("expected refresh token to be stored in Redis")
	}
	if storedRefresh != firstRefreshCookie.Value {
		t.Fatalf("expected refresh cookie to match Redis refresh token")
	}

	secondToken, _ := runAuthRoundTrip(t, svc, "code-2")

	secondClaims, err := svc.verifyAccessToken(secondToken)
	if err != nil {
		t.Fatalf("verifyAccessToken second token: %v", err)
	}
	if secondClaims.Subject != userID || secondClaims.TenantID != tenantID || secondClaims.Role != "admin" {
		t.Fatalf("expected returning login to reuse existing identity")
	}

	if err := pool.QueryRow(
		ctx,
		`SELECT last_login_at FROM users WHERE id = $1`,
		userID,
	).Scan(&lastLoginAt); err != nil {
		t.Fatalf("query last_login_at after second callback: %v", err)
	}
	if lastLoginAt == nil {
		t.Fatalf("expected returning login to update last_login_at")
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
}

func runAuthRoundTrip(t *testing.T, svc *serviceImpl, code string) (string, *http.Cookie) {
	t.Helper()

	ctx := context.Background()

	initPayload, err := svc.InitiateAuth(ctx, "google", nil)
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

func mustConnectAuthTestRedis(t *testing.T, ctx context.Context, redisURL string) *redis.Client {
	t.Helper()

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("parse redis URL: %v", err)
	}

	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		client.Close()
		t.Skipf("redis not available at %s: %v", redisURL, err)
	}

	return client
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

func applyAuthMigration(ctx context.Context, pool *pgxpool.Pool) error {
	migrationPath, err := authMigrationPath()
	if err != nil {
		return err
	}

	sqlBytes, err := os.ReadFile(migrationPath)
	if err != nil {
		return fmt.Errorf("read migration file: %w", err)
	}

	if _, err := pool.Exec(ctx, string(sqlBytes)); err != nil {
		return fmt.Errorf("execute migration SQL: %w", err)
	}

	return nil
}

func authMigrationPath() (string, error) {
	return filepath.Abs(filepath.Join("..", "..", "migrations", "001_schema.sql"))
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
