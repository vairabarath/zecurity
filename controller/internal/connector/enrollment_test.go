package connector

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	tcvalkey "github.com/testcontainers/testcontainers-go/modules/valkey"
	"github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeycompat"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/pki"
	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testValkeyRDB is a shared Valkey client for the entire test suite.
// Initialised once in TestMain so Docker startup cost is paid once, not per test.
var testValkeyRDB valkeycompat.Cmdable

func TestMain(m *testing.M) {
	ctx := context.Background()

	ctr, err := tcvalkey.Run(ctx, "valkey/valkey:7.2-alpine")
	if err != nil {
		log.Fatalf("start valkey container: %v", err)
	}
	defer func() { _ = ctr.Terminate(ctx) }()

	connStr, err := ctr.ConnectionString(ctx)
	if err != nil {
		log.Fatalf("valkey connection string: %v", err)
	}

	// ConnectionString returns "redis://host:port" — strip the scheme for valkey-go.
	addr := strings.TrimPrefix(connStr, "redis://")

	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress: []string{addr},
	})
	if err != nil {
		log.Fatalf("create valkey client: %v", err)
	}
	defer client.Close()

	testValkeyRDB = valkeycompat.NewAdapter(client)

	os.Exit(m.Run())
}

// ── Unit tests (no database required) ───────────────────────────────────────

func TestEnroll_InvalidJWT(t *testing.T) {
	handler := &EnrollmentHandler{
		Cfg: Config{JWTSecret: "test-secret", CertTTL: 7 * 24 * time.Hour},
	}

	_, err := handler.Enroll(context.Background(), &pb.EnrollRequest{
		EnrollmentToken: "not-a-valid-jwt",
	})

	if err == nil {
		t.Fatal("expected error for invalid JWT")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected codes.Unauthenticated, got %v", status.Code(err))
	}
}

func TestEnroll_ExpiredJWT(t *testing.T) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, EnrollmentClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			Issuer:    appmeta.ControllerIssuer,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
		},
		ConnectorID:   uuid.NewString(),
		WorkspaceID:   uuid.NewString(),
		TrustDomain:   "ws-acme.zecurity.in",
		CAFingerprint: "sha256:abc123",
	})
	tokenString, _ := token.SignedString([]byte("test-secret"))

	handler := &EnrollmentHandler{
		Cfg: Config{JWTSecret: "test-secret", CertTTL: 7 * 24 * time.Hour},
	}

	_, err := handler.Enroll(context.Background(), &pb.EnrollRequest{
		EnrollmentToken: tokenString,
	})

	if err == nil {
		t.Fatal("expected error for expired JWT")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected codes.Unauthenticated, got %v", status.Code(err))
	}
}

func TestEnroll_WrongIssuer(t *testing.T) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, EnrollmentClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			Issuer:    "evil-issuer",
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		ConnectorID:   uuid.NewString(),
		WorkspaceID:   uuid.NewString(),
		TrustDomain:   "ws-acme.zecurity.in",
		CAFingerprint: "sha256:abc123",
	})
	tokenString, _ := token.SignedString([]byte("test-secret"))

	handler := &EnrollmentHandler{
		Cfg: Config{JWTSecret: "test-secret", CertTTL: 7 * 24 * time.Hour},
	}

	_, err := handler.Enroll(context.Background(), &pb.EnrollRequest{
		EnrollmentToken: tokenString,
	})

	if err == nil {
		t.Fatal("expected error for wrong issuer")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected codes.Unauthenticated, got %v", status.Code(err))
	}
}

func TestEnroll_MissingClaims(t *testing.T) {
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, EnrollmentClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			Issuer:    appmeta.ControllerIssuer,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		// ConnectorID, WorkspaceID, TrustDomain are empty
	})
	tokenString, _ := token.SignedString([]byte("test-secret"))

	handler := &EnrollmentHandler{
		Cfg:   Config{JWTSecret: "test-secret", CertTTL: 7 * 24 * time.Hour},
		Redis: testValkeyRDB, // validation fails before Redis is reached
	}

	_, err := handler.Enroll(context.Background(), &pb.EnrollRequest{
		EnrollmentToken: tokenString,
	})

	if err == nil {
		t.Fatal("expected error for missing claims")
	}
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected codes.InvalidArgument, got %v", status.Code(err))
	}
}

func TestEnroll_JTINotFound(t *testing.T) {
	connectorID := uuid.NewString()
	workspaceID := uuid.NewString()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, EnrollmentClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(), // JTI not stored in Valkey
			Issuer:    appmeta.ControllerIssuer,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		ConnectorID: connectorID,
		WorkspaceID: workspaceID,
		TrustDomain: "ws-acme.zecurity.in",
	})
	tokenString, _ := token.SignedString([]byte("test-secret"))

	handler := &EnrollmentHandler{
		Cfg:   Config{JWTSecret: "test-secret", CertTTL: 7 * 24 * time.Hour},
		Redis: testValkeyRDB,
	}

	_, err := handler.Enroll(context.Background(), &pb.EnrollRequest{
		EnrollmentToken: tokenString,
	})

	if err == nil {
		t.Fatal("expected error when JTI not found")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected codes.PermissionDenied, got %v", status.Code(err))
	}
	if !strings.Contains(err.Error(), "token expired or already used") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestEnroll_CSRSignatureInvalid(t *testing.T) {
	t.Skip("requires database to reach CSR signature validation step")
}

func TestCSRHasSPIFFEURI(t *testing.T) {
	connectorID := "abc-123"
	trustDomain := "ws-acme.zecurity.in"
	expectedURI := appmeta.ConnectorSPIFFEID(trustDomain, connectorID)

	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	spiffeURI, err := url.Parse(expectedURI)
	if err != nil {
		t.Fatalf("parse SPIFFE URI: %v", err)
	}

	csrTemplate := &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName: appmeta.PKIConnectorCNPrefix + connectorID,
		},
		URIs: []*url.URL{spiffeURI},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}

	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}

	if !csrHasSPIFFEURI(csr, expectedURI) {
		t.Fatal("expected csrHasSPIFFEURI to return true")
	}

	if csrHasSPIFFEURI(csr, "spiffe://ws-bad.zecurity.in/connector/wrong-id") {
		t.Fatal("expected csrHasSPIFFEURI to return false for wrong URI")
	}
}

func TestCSRHasSPIFFEURI_MultipleURIs(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	uri1, _ := url.Parse("spiffe://ws-acme.zecurity.in/connector/abc-123")
	uri2, _ := url.Parse("https://example.com/other")

	csrTemplate := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "test"},
		URIs:    []*url.URL{uri1, uri2},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}

	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}

	if !csrHasSPIFFEURI(csr, "spiffe://ws-acme.zecurity.in/connector/abc-123") {
		t.Fatal("expected csrHasSPIFFEURI to find matching URI among multiple")
	}
}

func TestCSRHasSPIFFEURI_NoURIs(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	csrTemplate := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "test"},
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}

	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}

	if csrHasSPIFFEURI(csr, "spiffe://ws-acme.zecurity.in/connector/abc-123") {
		t.Fatal("expected csrHasSPIFFEURI to return false for CSR with no URIs")
	}
}

// ── Integration tests (require PostgreSQL) ──────────────────────────────────

func TestEnroll_FullFlow(t *testing.T) {
	adminDSN := os.Getenv("ENROLLMENT_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("ENROLLMENT_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	dbName := uniqueEnrollmentTestDBName(t)

	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("create admin pool: %v", err)
	}
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

	testDSN, err := withDSNDatabaseName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test DSN: %v", err)
	}

	testPool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("create test pool: %v", err)
	}
	defer testPool.Close()

	if err := applyEnrollmentMigration(ctx, testPool, "001_schema.sql"); err != nil {
		t.Fatalf("apply 001 migration: %v", err)
	}
	if err := applyEnrollmentMigration(ctx, testPool, "002_connector_schema.sql"); err != nil {
		t.Fatalf("apply 002 migration: %v", err)
	}

	t.Setenv("PKI_MASTER_SECRET", "enrollment-test-secret")

	pkiSvc, err := pki.Init(ctx, testPool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}

	workspaceID := uuid.NewString()
	workspaceSlug := "acme-corp"
	connectorID := uuid.NewString()

	_, err = testPool.Exec(ctx,
		`INSERT INTO workspaces (id, slug, name, status, trust_domain)
		 VALUES ($1, $2, 'ACME Corp', 'active', $3)`,
		workspaceID, workspaceSlug, "ws-"+workspaceSlug+".zecurity.in",
	)
	if err != nil {
		t.Fatalf("insert workspace: %v", err)
	}

	caResult, err := pkiSvc.GenerateWorkspaceCA(ctx, workspaceID)
	if err != nil {
		t.Fatalf("GenerateWorkspaceCA: %v", err)
	}

	_, err = testPool.Exec(ctx,
		`INSERT INTO workspace_ca_keys
		 (tenant_id, encrypted_private_key, nonce, certificate_pem, key_algorithm, not_before, not_after)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		workspaceID,
		caResult.EncryptedPrivateKey,
		caResult.Nonce,
		caResult.CertificatePEM,
		caResult.KeyAlgorithm,
		caResult.NotBefore,
		caResult.NotAfter,
	)
	if err != nil {
		t.Fatalf("store workspace CA keys: %v", err)
	}

	_, err = testPool.Exec(ctx,
		`INSERT INTO connectors (id, tenant_id, remote_network_id, name, status)
		 VALUES ($1, $2, $3, $4, 'pending')`,
		connectorID, workspaceID, uuid.NewString(), "test-connector",
	)
	if err != nil {
		t.Fatalf("insert connector: %v", err)
	}

	jwtSecret := "test-jwt-secret"
	cfg := Config{
		JWTSecret:          jwtSecret,
		CertTTL:            7 * 24 * time.Hour,
		EnrollmentTokenTTL: 5 * time.Minute,
	}

	tokenString, jti, err := GenerateEnrollmentToken(cfg, connectorID, workspaceID, workspaceSlug, "sha256:ca-fingerprint")
	if err != nil {
		t.Fatalf("GenerateEnrollmentToken: %v", err)
	}

	if err := StoreEnrollmentJTI(ctx, testValkeyRDB, jti, connectorID, 5*time.Minute); err != nil {
		t.Fatalf("StoreEnrollmentJTI: %v", err)
	}

	trustDomain := appmeta.WorkspaceTrustDomain(workspaceSlug)
	csrDER := generateConnectorCSR(t, connectorID, trustDomain)

	handler := &EnrollmentHandler{
		Cfg:        cfg,
		Pool:       testPool,
		Redis:      testValkeyRDB,
		PKIService: pkiSvc,
	}

	resp, err := handler.Enroll(ctx, &pb.EnrollRequest{
		EnrollmentToken: tokenString,
		CsrDer:          csrDER,
		Version:         "1.0.0",
		Hostname:        "test-host",
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	if resp.ConnectorId != connectorID {
		t.Fatalf("expected connector ID %s, got %s", connectorID, resp.ConnectorId)
	}
	if len(resp.CertificatePem) == 0 {
		t.Fatal("expected non-empty certificate PEM")
	}
	if len(resp.WorkspaceCaPem) == 0 {
		t.Fatal("expected non-empty workspace CA PEM")
	}
	if len(resp.IntermediateCaPem) == 0 {
		t.Fatal("expected non-empty intermediate CA PEM")
	}

	var connStatus, connTrustDomain, certSerial, version, hostname string
	err = testPool.QueryRow(ctx,
		`SELECT status, trust_domain, cert_serial, version, hostname
		   FROM connectors WHERE id = $1`,
		connectorID,
	).Scan(&connStatus, &connTrustDomain, &certSerial, &version, &hostname)
	if err != nil {
		t.Fatalf("query connector after enrollment: %v", err)
	}

	if connStatus != "active" {
		t.Fatalf("expected status 'active', got %q", connStatus)
	}
	if connTrustDomain != trustDomain {
		t.Fatalf("expected trust domain %q, got %q", trustDomain, connTrustDomain)
	}
	if certSerial == "" {
		t.Fatal("expected cert serial to be set")
	}
	if version != "1.0.0" {
		t.Fatalf("expected version '1.0.0', got %q", version)
	}
	if hostname != "test-host" {
		t.Fatalf("expected hostname 'test-host', got %q", hostname)
	}

	// Verify JTI was burned (single-use token)
	_, found, err := BurnEnrollmentJTI(ctx, testValkeyRDB, jti)
	if err != nil {
		t.Fatalf("BurnEnrollmentJTI after enrollment: %v", err)
	}
	if found {
		t.Fatal("expected JTI to be burned after enrollment")
	}

	certPEM := string(resp.CertificatePem)
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatal("failed to decode certificate PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse signed certificate: %v", err)
	}

	expectedSPIFFE := appmeta.ConnectorSPIFFEID(trustDomain, connectorID)
	if len(cert.URIs) != 1 {
		t.Fatalf("expected 1 URI SAN, got %d", len(cert.URIs))
	}
	if cert.URIs[0].String() != expectedSPIFFE {
		t.Fatalf("expected SPIFFE URI %s, got %s", expectedSPIFFE, cert.URIs[0].String())
	}

	if cert.IsCA {
		t.Fatal("connector certificate should not be a CA")
	}

	if cert.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Fatalf("expected KeyUsageDigitalSignature, got %v", cert.KeyUsage)
	}
}

func TestEnroll_ReplayAttack(t *testing.T) {
	adminDSN := os.Getenv("ENROLLMENT_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("ENROLLMENT_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	dbName := uniqueEnrollmentTestDBName(t)

	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("create admin pool: %v", err)
	}
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

	testDSN, err := withDSNDatabaseName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test DSN: %v", err)
	}

	testPool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("create test pool: %v", err)
	}
	defer testPool.Close()

	if err := applyEnrollmentMigration(ctx, testPool, "001_schema.sql"); err != nil {
		t.Fatalf("apply 001 migration: %v", err)
	}
	if err := applyEnrollmentMigration(ctx, testPool, "002_connector_schema.sql"); err != nil {
		t.Fatalf("apply 002 migration: %v", err)
	}

	t.Setenv("PKI_MASTER_SECRET", "enrollment-test-secret")

	pkiSvc, err := pki.Init(ctx, testPool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}

	workspaceID := uuid.NewString()
	workspaceSlug := "replay-test"
	connectorID := uuid.NewString()

	_, err = testPool.Exec(ctx,
		`INSERT INTO workspaces (id, slug, name, status, trust_domain)
		 VALUES ($1, $2, 'Replay Test', 'active', $3)`,
		workspaceID, workspaceSlug, "ws-"+workspaceSlug+".zecurity.in",
	)
	if err != nil {
		t.Fatalf("insert workspace: %v", err)
	}

	caResult, err := pkiSvc.GenerateWorkspaceCA(ctx, workspaceID)
	if err != nil {
		t.Fatalf("GenerateWorkspaceCA: %v", err)
	}

	_, err = testPool.Exec(ctx,
		`INSERT INTO workspace_ca_keys
		 (tenant_id, encrypted_private_key, nonce, certificate_pem, key_algorithm, not_before, not_after)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		workspaceID,
		caResult.EncryptedPrivateKey,
		caResult.Nonce,
		caResult.CertificatePEM,
		caResult.KeyAlgorithm,
		caResult.NotBefore,
		caResult.NotAfter,
	)
	if err != nil {
		t.Fatalf("store workspace CA keys: %v", err)
	}

	_, err = testPool.Exec(ctx,
		`INSERT INTO connectors (id, tenant_id, remote_network_id, name, status)
		 VALUES ($1, $2, $3, $4, 'pending')`,
		connectorID, workspaceID, uuid.NewString(), "replay-connector",
	)
	if err != nil {
		t.Fatalf("insert connector: %v", err)
	}

	cfg := Config{
		JWTSecret:          "replay-secret",
		CertTTL:            7 * 24 * time.Hour,
		EnrollmentTokenTTL: 5 * time.Minute,
	}

	tokenString, jti, err := GenerateEnrollmentToken(cfg, connectorID, workspaceID, workspaceSlug, "sha256:fp")
	if err != nil {
		t.Fatalf("GenerateEnrollmentToken: %v", err)
	}

	if err := StoreEnrollmentJTI(ctx, testValkeyRDB, jti, connectorID, 5*time.Minute); err != nil {
		t.Fatalf("StoreEnrollmentJTI: %v", err)
	}

	csrDER := generateConnectorCSR(t, connectorID, appmeta.WorkspaceTrustDomain(workspaceSlug))

	handler := &EnrollmentHandler{
		Cfg:        cfg,
		Pool:       testPool,
		Redis:      testValkeyRDB,
		PKIService: pkiSvc,
	}

	_, err = handler.Enroll(ctx, &pb.EnrollRequest{
		EnrollmentToken: tokenString,
		CsrDer:          csrDER,
		Version:         "1.0.0",
		Hostname:        "replay-host",
	})
	if err != nil {
		t.Fatalf("first Enroll: %v", err)
	}

	// Reset connector to pending so we can attempt replay
	_, err = testPool.Exec(ctx,
		`UPDATE connectors SET status = 'pending', trust_domain = NULL,
		        cert_serial = NULL, cert_not_after = NULL,
		        enrollment_token_jti = NULL
		 WHERE id = $1`,
		connectorID,
	)
	if err != nil {
		t.Fatalf("reset connector status: %v", err)
	}

	_, err = handler.Enroll(ctx, &pb.EnrollRequest{
		EnrollmentToken: tokenString,
		CsrDer:          csrDER,
		Version:         "1.0.0",
		Hostname:        "replay-host",
	})
	if err == nil {
		t.Fatal("expected error on replay enrollment")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected codes.PermissionDenied for replay, got %v", status.Code(err))
	}
}

func TestEnroll_ConnectorNotPending(t *testing.T) {
	adminDSN := os.Getenv("ENROLLMENT_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("ENROLLMENT_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	dbName := uniqueEnrollmentTestDBName(t)

	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("create admin pool: %v", err)
	}
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

	testDSN, err := withDSNDatabaseName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test DSN: %v", err)
	}

	testPool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("create test pool: %v", err)
	}
	defer testPool.Close()

	if err := applyEnrollmentMigration(ctx, testPool, "001_schema.sql"); err != nil {
		t.Fatalf("apply 001 migration: %v", err)
	}
	if err := applyEnrollmentMigration(ctx, testPool, "002_connector_schema.sql"); err != nil {
		t.Fatalf("apply 002 migration: %v", err)
	}

	t.Setenv("PKI_MASTER_SECRET", "enrollment-test-secret")

	pkiSvc, err := pki.Init(ctx, testPool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}

	workspaceID := uuid.NewString()
	workspaceSlug := "pending-test"
	connectorID := uuid.NewString()

	_, err = testPool.Exec(ctx,
		`INSERT INTO workspaces (id, slug, name, status, trust_domain)
		 VALUES ($1, $2, 'Pending Test', 'active', $3)`,
		workspaceID, workspaceSlug, "ws-"+workspaceSlug+".zecurity.in",
	)
	if err != nil {
		t.Fatalf("insert workspace: %v", err)
	}

	caResult, err := pkiSvc.GenerateWorkspaceCA(ctx, workspaceID)
	if err != nil {
		t.Fatalf("GenerateWorkspaceCA: %v", err)
	}

	_, err = testPool.Exec(ctx,
		`INSERT INTO workspace_ca_keys
		 (tenant_id, encrypted_private_key, nonce, certificate_pem, key_algorithm, not_before, not_after)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		workspaceID,
		caResult.EncryptedPrivateKey,
		caResult.Nonce,
		caResult.CertificatePEM,
		caResult.KeyAlgorithm,
		caResult.NotBefore,
		caResult.NotAfter,
	)
	if err != nil {
		t.Fatalf("store workspace CA keys: %v", err)
	}

	// Connector is already active — not pending
	_, err = testPool.Exec(ctx,
		`INSERT INTO connectors (id, tenant_id, remote_network_id, name, status)
		 VALUES ($1, $2, $3, $4, 'active')`,
		connectorID, workspaceID, uuid.NewString(), "active-connector",
	)
	if err != nil {
		t.Fatalf("insert connector: %v", err)
	}

	cfg := Config{
		JWTSecret:          "pending-secret",
		CertTTL:            7 * 24 * time.Hour,
		EnrollmentTokenTTL: 5 * time.Minute,
	}

	tokenString, jti, err := GenerateEnrollmentToken(cfg, connectorID, workspaceID, workspaceSlug, "sha256:fp")
	if err != nil {
		t.Fatalf("GenerateEnrollmentToken: %v", err)
	}

	if err := StoreEnrollmentJTI(ctx, testValkeyRDB, jti, connectorID, 5*time.Minute); err != nil {
		t.Fatalf("StoreEnrollmentJTI: %v", err)
	}

	csrDER := generateConnectorCSR(t, connectorID, appmeta.WorkspaceTrustDomain(workspaceSlug))

	handler := &EnrollmentHandler{
		Cfg:        cfg,
		Pool:       testPool,
		Redis:      testValkeyRDB,
		PKIService: pkiSvc,
	}

	_, err = handler.Enroll(ctx, &pb.EnrollRequest{
		EnrollmentToken: tokenString,
		CsrDer:          csrDER,
		Version:         "1.0.0",
		Hostname:        "pending-host",
	})
	if err == nil {
		t.Fatal("expected error for non-pending connector")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected codes.PermissionDenied, got %v", status.Code(err))
	}
	if !strings.Contains(err.Error(), "expected pending") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestEnroll_WorkspaceNotActive(t *testing.T) {
	adminDSN := os.Getenv("ENROLLMENT_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("ENROLLMENT_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	dbName := uniqueEnrollmentTestDBName(t)

	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("create admin pool: %v", err)
	}
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

	testDSN, err := withDSNDatabaseName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test DSN: %v", err)
	}

	testPool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("create test pool: %v", err)
	}
	defer testPool.Close()

	if err := applyEnrollmentMigration(ctx, testPool, "001_schema.sql"); err != nil {
		t.Fatalf("apply 001 migration: %v", err)
	}
	if err := applyEnrollmentMigration(ctx, testPool, "002_connector_schema.sql"); err != nil {
		t.Fatalf("apply 002 migration: %v", err)
	}

	t.Setenv("PKI_MASTER_SECRET", "enrollment-test-secret")

	pkiSvc, err := pki.Init(ctx, testPool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}

	workspaceID := uuid.NewString()
	workspaceSlug := "suspended-ws"
	connectorID := uuid.NewString()

	_, err = testPool.Exec(ctx,
		`INSERT INTO workspaces (id, slug, name, status, trust_domain)
		 VALUES ($1, $2, 'Suspended WS', 'suspended', $3)`,
		workspaceID, workspaceSlug, "ws-"+workspaceSlug+".zecurity.in",
	)
	if err != nil {
		t.Fatalf("insert workspace: %v", err)
	}

	caResult, err := pkiSvc.GenerateWorkspaceCA(ctx, workspaceID)
	if err != nil {
		t.Fatalf("GenerateWorkspaceCA: %v", err)
	}

	_, err = testPool.Exec(ctx,
		`INSERT INTO workspace_ca_keys
		 (tenant_id, encrypted_private_key, nonce, certificate_pem, key_algorithm, not_before, not_after)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		workspaceID,
		caResult.EncryptedPrivateKey,
		caResult.Nonce,
		caResult.CertificatePEM,
		caResult.KeyAlgorithm,
		caResult.NotBefore,
		caResult.NotAfter,
	)
	if err != nil {
		t.Fatalf("store workspace CA keys: %v", err)
	}

	_, err = testPool.Exec(ctx,
		`INSERT INTO connectors (id, tenant_id, remote_network_id, name, status)
		 VALUES ($1, $2, $3, $4, 'pending')`,
		connectorID, workspaceID, uuid.NewString(), "suspended-connector",
	)
	if err != nil {
		t.Fatalf("insert connector: %v", err)
	}

	cfg := Config{
		JWTSecret:          "suspended-secret",
		CertTTL:            7 * 24 * time.Hour,
		EnrollmentTokenTTL: 5 * time.Minute,
	}

	tokenString, jti, err := GenerateEnrollmentToken(cfg, connectorID, workspaceID, workspaceSlug, "sha256:fp")
	if err != nil {
		t.Fatalf("GenerateEnrollmentToken: %v", err)
	}

	if err := StoreEnrollmentJTI(ctx, testValkeyRDB, jti, connectorID, 5*time.Minute); err != nil {
		t.Fatalf("StoreEnrollmentJTI: %v", err)
	}

	csrDER := generateConnectorCSR(t, connectorID, appmeta.WorkspaceTrustDomain(workspaceSlug))

	handler := &EnrollmentHandler{
		Cfg:        cfg,
		Pool:       testPool,
		Redis:      testValkeyRDB,
		PKIService: pkiSvc,
	}

	_, err = handler.Enroll(ctx, &pb.EnrollRequest{
		EnrollmentToken: tokenString,
		CsrDer:          csrDER,
		Version:         "1.0.0",
		Hostname:        "suspended-host",
	})
	if err == nil {
		t.Fatal("expected error for suspended workspace")
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("expected codes.FailedPrecondition, got %v", status.Code(err))
	}
	if !strings.Contains(err.Error(), "expected active") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestEnroll_CSRSPIFFEMismatch(t *testing.T) {
	adminDSN := os.Getenv("ENROLLMENT_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("ENROLLMENT_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	dbName := uniqueEnrollmentTestDBName(t)

	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("create admin pool: %v", err)
	}
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

	testDSN, err := withDSNDatabaseName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test DSN: %v", err)
	}

	testPool, err := pgxpool.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("create test pool: %v", err)
	}
	defer testPool.Close()

	if err := applyEnrollmentMigration(ctx, testPool, "001_schema.sql"); err != nil {
		t.Fatalf("apply 001 migration: %v", err)
	}
	if err := applyEnrollmentMigration(ctx, testPool, "002_connector_schema.sql"); err != nil {
		t.Fatalf("apply 002 migration: %v", err)
	}

	t.Setenv("PKI_MASTER_SECRET", "enrollment-test-secret")

	pkiSvc, err := pki.Init(ctx, testPool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}

	workspaceID := uuid.NewString()
	workspaceSlug := "mismatch-test"
	connectorID := uuid.NewString()

	_, err = testPool.Exec(ctx,
		`INSERT INTO workspaces (id, slug, name, status, trust_domain)
		 VALUES ($1, $2, 'Mismatch Test', 'active', $3)`,
		workspaceID, workspaceSlug, "ws-"+workspaceSlug+".zecurity.in",
	)
	if err != nil {
		t.Fatalf("insert workspace: %v", err)
	}

	caResult, err := pkiSvc.GenerateWorkspaceCA(ctx, workspaceID)
	if err != nil {
		t.Fatalf("GenerateWorkspaceCA: %v", err)
	}

	_, err = testPool.Exec(ctx,
		`INSERT INTO workspace_ca_keys
		 (tenant_id, encrypted_private_key, nonce, certificate_pem, key_algorithm, not_before, not_after)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		workspaceID,
		caResult.EncryptedPrivateKey,
		caResult.Nonce,
		caResult.CertificatePEM,
		caResult.KeyAlgorithm,
		caResult.NotBefore,
		caResult.NotAfter,
	)
	if err != nil {
		t.Fatalf("store workspace CA keys: %v", err)
	}

	_, err = testPool.Exec(ctx,
		`INSERT INTO connectors (id, tenant_id, remote_network_id, name, status)
		 VALUES ($1, $2, $3, $4, 'pending')`,
		connectorID, workspaceID, uuid.NewString(), "mismatch-connector",
	)
	if err != nil {
		t.Fatalf("insert connector: %v", err)
	}

	cfg := Config{
		JWTSecret:          "mismatch-secret",
		CertTTL:            7 * 24 * time.Hour,
		EnrollmentTokenTTL: 5 * time.Minute,
	}

	tokenString, jti, err := GenerateEnrollmentToken(cfg, connectorID, workspaceID, workspaceSlug, "sha256:fp")
	if err != nil {
		t.Fatalf("GenerateEnrollmentToken: %v", err)
	}

	if err := StoreEnrollmentJTI(ctx, testValkeyRDB, jti, connectorID, 5*time.Minute); err != nil {
		t.Fatalf("StoreEnrollmentJTI: %v", err)
	}

	// CSR uses a different connector ID — SPIFFE mismatch
	wrongConnectorID := uuid.NewString()
	wrongTrustDomain := appmeta.WorkspaceTrustDomain(workspaceSlug)
	csrDER := generateConnectorCSR(t, wrongConnectorID, wrongTrustDomain)

	handler := &EnrollmentHandler{
		Cfg:        cfg,
		Pool:       testPool,
		Redis:      testValkeyRDB,
		PKIService: pkiSvc,
	}

	_, err = handler.Enroll(ctx, &pb.EnrollRequest{
		EnrollmentToken: tokenString,
		CsrDer:          csrDER,
		Version:         "1.0.0",
		Hostname:        "mismatch-host",
	})
	if err == nil {
		t.Fatal("expected error for SPIFFE mismatch")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected codes.PermissionDenied, got %v", status.Code(err))
	}
	if !strings.Contains(err.Error(), "SPIFFE ID in CSR does not match token") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

// ── Test helpers ─────────────────────────────────────────────────────────────

func generateConnectorCSR(t *testing.T, connectorID, trustDomain string) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate EC key: %v", err)
	}

	spiffeURI, err := url.Parse(appmeta.ConnectorSPIFFEID(trustDomain, connectorID))
	if err != nil {
		t.Fatalf("parse SPIFFE URI: %v", err)
	}

	csrTemplate := &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   appmeta.PKIConnectorCNPrefix + connectorID,
			Organization: []string{appmeta.PKIWorkspaceOrganization},
		},
		URIs: []*url.URL{spiffeURI},
	}

	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTemplate, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}

	return csrDER
}

func applyEnrollmentMigration(ctx context.Context, pool *pgxpool.Pool, filename string) error {
	migrationPath, err := enrollmentMigrationPath(filename)
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

func enrollmentMigrationPath(filename string) (string, error) {
	return filepath.Abs(filepath.Join("..", "..", "migrations", filename))
}

func withDSNDatabaseName(dsn, dbName string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}

	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func uniqueEnrollmentTestDBName(t *testing.T) string {
	t.Helper()

	name := strings.ToLower(t.Name())
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, " ", "_")

	return fmt.Sprintf("%s_%d_%d", name, os.Getpid(), time.Now().UnixNano())
}
