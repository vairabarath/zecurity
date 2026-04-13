package pki

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/appmeta"
)

func TestRootCAIntegration(t *testing.T) {
	t.Parallel()

	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	dbName := uniqueTestDatabaseName(t)

	adminPool := mustConnectTestPool(t, ctx, adminDSN)
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

	testDBDSN, err := withDatabaseName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test database dsn: %v", err)
	}

	testPool := mustConnectTestPool(t, ctx, testDBDSN)
	defer testPool.Close()

	if err := applyMigration(ctx, testPool); err != nil {
		t.Fatalf("apply migration: %v", err)
	}

	svc := &serviceImpl{
		masterSecret: "phase-3-root-ca-integration-secret",
		pool:         testPool,
	}

	if err := svc.initRootCA(ctx); err != nil {
		t.Fatalf("first initRootCA: %v", err)
	}

	row := testPool.QueryRow(ctx,
		`SELECT COUNT(*), encrypted_key, nonce, certificate_pem
		 FROM ca_root
		 GROUP BY encrypted_key, nonce, certificate_pem`,
	)

	var count int
	var encryptedKey string
	var nonce string
	var certPEM string
	if err := row.Scan(&count, &encryptedKey, &nonce, &certPEM); err != nil {
		t.Fatalf("query ca_root after first init: %v", err)
	}

	if count != 1 {
		t.Fatalf("expected exactly one root CA row, got %d", count)
	}

	if encryptedKey == "" || nonce == "" || certPEM == "" {
		t.Fatalf("expected encrypted key material and certificate to be stored")
	}

	if strings.Contains(certPEM, encryptedKey) || strings.Contains(encryptedKey, "BEGIN EC PRIVATE KEY") {
		t.Fatalf("encrypted key appears to contain plaintext private key material")
	}

	cert, privKey, err := svc.loadRootCA(ctx)
	if err != nil {
		t.Fatalf("loadRootCA: %v", err)
	}
	defer privKey.D.SetInt64(0)

	if !cert.IsCA {
		t.Fatalf("expected root certificate to be a CA")
	}

	if cert.Subject.CommonName != appmeta.PKIRootCACommonName {
		t.Fatalf("unexpected common name: %s", cert.Subject.CommonName)
	}

	if cert.MaxPathLen != 1 {
		t.Fatalf("expected MaxPathLen=1, got %d", cert.MaxPathLen)
	}

	if cert.Issuer.String() != cert.Subject.String() {
		t.Fatalf("expected root certificate to be self-signed")
	}

	if err := svc.initRootCA(ctx); err != nil {
		t.Fatalf("second initRootCA: %v", err)
	}

	var finalCount int
	if err := testPool.QueryRow(ctx, "SELECT COUNT(*) FROM ca_root").Scan(&finalCount); err != nil {
		t.Fatalf("count ca_root after second init: %v", err)
	}

	if finalCount != 1 {
		t.Fatalf("expected one root CA row after second init, got %d", finalCount)
	}
}

func mustConnectTestPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
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

func applyMigration(ctx context.Context, pool *pgxpool.Pool) error {
	migrationPath, err := rootMigrationPath()
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

func rootMigrationPath() (string, error) {
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
