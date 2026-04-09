package bootstrap

import (
	"context"
	"crypto/x509"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/pki"
)

func TestBootstrapIntegration_NewAndReturningUser(t *testing.T) {
	ctx, pool := setupBootstrapTestDB(t)
	defer pool.Close()

	t.Setenv("PKI_MASTER_SECRET", "phase-6-bootstrap-integration-secret")

	pkiSvc, err := pki.Init(ctx, pool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}

	svc := &Service{
		Pool:       pool,
		PKIService: pkiSvc,
	}

	result, err := svc.Bootstrap(ctx, "alice@example.com", "google", "sub-123", "Acme Corp")
	if err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}

	if result.TenantID == "" || result.UserID == "" {
		t.Fatalf("expected tenant and user IDs to be populated")
	}
	if result.Role != "admin" {
		t.Fatalf("expected first user role admin, got %s", result.Role)
	}

	var workspaceCount int
	var workspaceSlug string
	var workspaceStatus string
	var workspaceCACert string
	err = pool.QueryRow(
		ctx,
		`SELECT COUNT(*), MIN(slug), MIN(status), MIN(COALESCE(ca_cert_pem, ''))
		 FROM workspaces`,
	).Scan(&workspaceCount, &workspaceSlug, &workspaceStatus, &workspaceCACert)
	if err != nil {
		t.Fatalf("query workspaces: %v", err)
	}

	if workspaceCount != 1 {
		t.Fatalf("expected one workspace row, got %d", workspaceCount)
	}
	if workspaceSlug != "acme-corp" {
		t.Fatalf("expected slug acme-corp, got %s", workspaceSlug)
	}
	if workspaceStatus != "active" {
		t.Fatalf("expected workspace status active, got %s", workspaceStatus)
	}
	if workspaceCACert == "" {
		t.Fatalf("expected workspace ca_cert_pem to be populated")
	}

	var userCount int
	var storedTenantID string
	var storedUserID string
	var storedRole string
	var lastLoginAt *time.Time
	err = pool.QueryRow(
		ctx,
		`SELECT COUNT(*), MIN(tenant_id::text), MIN(id::text), MIN(role), MAX(last_login_at)
		 FROM users`,
	).Scan(&userCount, &storedTenantID, &storedUserID, &storedRole, &lastLoginAt)
	if err != nil {
		t.Fatalf("query users: %v", err)
	}

	if userCount != 1 {
		t.Fatalf("expected one user row, got %d", userCount)
	}
	if storedTenantID != result.TenantID || storedUserID != result.UserID {
		t.Fatalf("stored user/workspace IDs do not match bootstrap result")
	}
	if storedRole != "admin" {
		t.Fatalf("expected stored role admin, got %s", storedRole)
	}
	if lastLoginAt != nil {
		t.Fatalf("expected first signup last_login_at to be nil")
	}

	var caKeyCount int
	err = pool.QueryRow(ctx, "SELECT COUNT(*) FROM workspace_ca_keys").Scan(&caKeyCount)
	if err != nil {
		t.Fatalf("count workspace_ca_keys: %v", err)
	}
	if caKeyCount != 1 {
		t.Fatalf("expected one workspace_ca_keys row, got %d", caKeyCount)
	}

	returningResult, err := svc.Bootstrap(ctx, "alice@example.com", "google", "sub-123", "Acme Corp")
	if err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}

	if returningResult.TenantID != result.TenantID || returningResult.UserID != result.UserID {
		t.Fatalf("expected returning user to reuse existing tenant/user IDs")
	}
	if returningResult.Role != "admin" {
		t.Fatalf("expected returning role admin, got %s", returningResult.Role)
	}

	err = pool.QueryRow(
		ctx,
		`SELECT last_login_at
		 FROM users
		 WHERE id = $1`,
		result.UserID,
	).Scan(&lastLoginAt)
	if err != nil {
		t.Fatalf("query last_login_at after returning bootstrap: %v", err)
	}
	if lastLoginAt == nil {
		t.Fatalf("expected last_login_at to be updated for returning user")
	}

	var workspaceCountAfter int
	var userCountAfter int
	var caKeyCountAfter int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM workspaces").Scan(&workspaceCountAfter); err != nil {
		t.Fatalf("count workspaces after returning bootstrap: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM users").Scan(&userCountAfter); err != nil {
		t.Fatalf("count users after returning bootstrap: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM workspace_ca_keys").Scan(&caKeyCountAfter); err != nil {
		t.Fatalf("count workspace_ca_keys after returning bootstrap: %v", err)
	}

	if workspaceCountAfter != 1 || userCountAfter != 1 || caKeyCountAfter != 1 {
		t.Fatalf("expected no new rows for returning user; got workspaces=%d users=%d keys=%d",
			workspaceCountAfter, userCountAfter, caKeyCountAfter)
	}

	_, err = pool.Exec(
		ctx,
		`INSERT INTO users
		 (tenant_id, email, provider, provider_sub, role, status)
		 VALUES ($1, $2, $3, $4, 'member', 'active')`,
		result.TenantID,
		"alice-duplicate@example.com",
		"google",
		"sub-123",
	)
	if err == nil {
		t.Fatalf("expected UNIQUE (tenant_id, provider_sub) to reject duplicate user")
	}
}

func TestBootstrapIntegration_RollbackWhenGenerateWorkspaceCAFails(t *testing.T) {
	ctx, pool := setupBootstrapTestDB(t)
	defer pool.Close()

	svc := &Service{
		Pool:       pool,
		PKIService: failingPKIService{err: fmt.Errorf("workspace ca boom")},
	}

	_, err := svc.Bootstrap(ctx, "bob@example.com", "google", "sub-fail", "Broken Org")
	if err == nil {
		t.Fatalf("expected bootstrap to fail when GenerateWorkspaceCA fails")
	}

	assertNoBootstrapRows(t, ctx, pool)

	t.Setenv("PKI_MASTER_SECRET", "phase-6-bootstrap-retry-secret")

	pkiSvc, err := pki.Init(ctx, pool)
	if err != nil {
		t.Fatalf("pki.Init after rollback: %v", err)
	}

	svc.PKIService = pkiSvc
	result, err := svc.Bootstrap(ctx, "bob@example.com", "google", "sub-fail", "Broken Org")
	if err != nil {
		t.Fatalf("bootstrap retry after rollback: %v", err)
	}
	if result.TenantID == "" || result.UserID == "" {
		t.Fatalf("expected successful bootstrap after rollback retry")
	}
}

func TestBootstrapIntegration_RollbackWhenWorkspaceCAInsertFails(t *testing.T) {
	ctx, pool := setupBootstrapTestDB(t)
	defer pool.Close()

	_, err := pool.Exec(ctx,
		`ALTER TABLE workspace_ca_keys
		 ADD CONSTRAINT workspace_ca_keys_nonempty_cert CHECK (certificate_pem <> '')`,
	)
	if err != nil {
		t.Fatalf("add workspace_ca_keys failure constraint: %v", err)
	}

	svc := &Service{
		Pool: pool,
		PKIService: stubPKIService{
			result: &pki.WorkspaceCAResult{
				EncryptedPrivateKey: "ciphertext",
				Nonce:               "nonce",
				CertificatePEM:      "",
				KeyAlgorithm:        "EC-P384",
				NotBefore:           time.Now().UTC(),
				NotAfter:            time.Now().UTC().Add(2 * time.Hour),
			},
		},
	}

	_, err = svc.Bootstrap(ctx, "carol@example.com", "google", "sub-insert-fail", "Insert Fail Org")
	if err == nil {
		t.Fatalf("expected bootstrap to fail when workspace_ca_keys insert fails")
	}

	assertNoBootstrapRows(t, ctx, pool)
}

func TestBootstrapIntegration_RollbackWhenWorkspaceActivationFails(t *testing.T) {
	ctx, pool := setupBootstrapTestDB(t)
	defer pool.Close()

	_, err := pool.Exec(ctx,
		`ALTER TABLE workspaces
		 ADD CONSTRAINT workspaces_nonempty_ca_cert CHECK (ca_cert_pem <> '')`,
	)
	if err != nil {
		t.Fatalf("add workspace activation failure constraint: %v", err)
	}

	svc := &Service{
		Pool: pool,
		PKIService: stubPKIService{
			result: &pki.WorkspaceCAResult{
				EncryptedPrivateKey: "ciphertext",
				Nonce:               "nonce",
				CertificatePEM:      "",
				KeyAlgorithm:        "EC-P384",
				NotBefore:           time.Now().UTC(),
				NotAfter:            time.Now().UTC().Add(2 * time.Hour),
			},
		},
	}

	_, err = svc.Bootstrap(ctx, "dana@example.com", "google", "sub-update-fail", "Update Fail Org")
	if err == nil {
		t.Fatalf("expected bootstrap to fail when workspace activation update fails")
	}

	assertNoBootstrapRows(t, ctx, pool)
}

func TestSlugify(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "simple", in: "Acme Corp", want: "acme-corp"},
		{name: "punctuation", in: "My Company!", want: "my-company"},
		{name: "spaces collapse", in: "  Hello   World  ", want: "hello-world"},
		{name: "unicode letters", in: "Zecurity Labs 2026", want: "zecurity-labs-2026"},
		{name: "empty fallback", in: "!!!", want: "workspace"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := slugify(tc.in)
			if got != tc.want {
				t.Fatalf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

type failingPKIService struct {
	err error
}

func (f failingPKIService) GenerateWorkspaceCA(ctx context.Context, tenantID string) (*pki.WorkspaceCAResult, error) {
	return nil, f.err
}

func (f failingPKIService) SignConnectorCert(ctx context.Context, tenantID, connectorID, trustDomain string, csr *x509.CertificateRequest, certTTL time.Duration) (*pki.ConnectorCertResult, error) {
	return nil, fmt.Errorf("not implemented in test stub")
}

type stubPKIService struct {
	result *pki.WorkspaceCAResult
}

func (s stubPKIService) GenerateWorkspaceCA(ctx context.Context, tenantID string) (*pki.WorkspaceCAResult, error) {
	return s.result, nil
}

func (s stubPKIService) SignConnectorCert(ctx context.Context, tenantID, connectorID, trustDomain string, csr *x509.CertificateRequest, certTTL time.Duration) (*pki.ConnectorCertResult, error) {
	return nil, fmt.Errorf("not implemented in test stub")
}

func setupBootstrapTestDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()

	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	dbName := uniqueTestDatabaseName(t)

	adminPool := mustConnectBootstrapTestPool(t, ctx, adminDSN)

	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		adminPool.Close()
		t.Fatalf("create test database: %v", err)
	}

	testDBDSN, err := withDatabaseName(adminDSN, dbName)
	if err != nil {
		adminPool.Close()
		t.Fatalf("build test database dsn: %v", err)
	}

	testPool := mustConnectBootstrapTestPool(t, ctx, testDBDSN)

	if err := applyBootstrapMigration(ctx, testPool); err != nil {
		testPool.Close()
		adminPool.Close()
		t.Fatalf("apply migration: %v", err)
	}

	t.Cleanup(func() {
		testPool.Close()
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
		adminPool.Close()
	})

	return ctx, testPool
}

func mustConnectBootstrapTestPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
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

func applyBootstrapMigration(ctx context.Context, pool *pgxpool.Pool) error {
	migrationPath, err := bootstrapMigrationPath()
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

func bootstrapMigrationPath() (string, error) {
	return filepath.Abs(filepath.Join("..", "..", "migrations", "001_schema.sql"))
}

func withDatabaseName(dsn, dbName string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}

	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func uniqueTestDatabaseName(t *testing.T) string {
	t.Helper()

	name := strings.ToLower(t.Name())
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, " ", "_")

	return fmt.Sprintf("%s_%d_%d", name, os.Getpid(), time.Now().UnixNano())
}

func assertNoBootstrapRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	var workspaceCount int
	var userCount int
	var caKeyCount int

	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM workspaces").Scan(&workspaceCount); err != nil {
		t.Fatalf("count workspaces after rollback: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM users").Scan(&userCount); err != nil {
		t.Fatalf("count users after rollback: %v", err)
	}
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM workspace_ca_keys").Scan(&caKeyCount); err != nil {
		t.Fatalf("count workspace_ca_keys after rollback: %v", err)
	}

	if workspaceCount != 0 || userCount != 0 || caKeyCount != 0 {
		t.Fatalf("expected rollback to leave no bootstrap rows; got workspaces=%d users=%d keys=%d",
			workspaceCount, userCount, caKeyCount)
	}
}
