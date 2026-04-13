package pki

import (
	"context"
	"crypto/x509"
	"os"
	"testing"

	"github.com/yourorg/ztna/controller/internal/appmeta"
)

func TestWorkspaceCAIntegration(t *testing.T) {
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

	t.Setenv("PKI_MASTER_SECRET", "phase-5-workspace-ca-integration-secret")

	svcIntf, err := Init(ctx, testPool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}

	svc, ok := svcIntf.(*serviceImpl)
	if !ok {
		t.Fatalf("expected *serviceImpl from Init, got %T", svcIntf)
	}

	tenantA := "11111111-1111-1111-1111-111111111111"
	tenantB := "22222222-2222-2222-2222-222222222222"

	resultA, err := svc.GenerateWorkspaceCA(ctx, tenantA)
	if err != nil {
		t.Fatalf("GenerateWorkspaceCA tenant A: %v", err)
	}

	resultB, err := svc.GenerateWorkspaceCA(ctx, tenantB)
	if err != nil {
		t.Fatalf("GenerateWorkspaceCA tenant B: %v", err)
	}

	assertWorkspaceCAResult(t, resultA)
	assertWorkspaceCAResult(t, resultB)

	if resultA.CertificatePEM == resultB.CertificatePEM {
		t.Fatalf("expected different workspace certificates for different tenants")
	}

	if resultA.EncryptedPrivateKey == resultB.EncryptedPrivateKey {
		t.Fatalf("expected different encrypted private keys for different tenants")
	}

	rootCert, rootKey, err := svc.loadRootCA(ctx)
	if err != nil {
		t.Fatalf("loadRootCA: %v", err)
	}
	defer rootKey.D.SetInt64(0)

	workspaceCertA, err := parseCertFromPEM(resultA.CertificatePEM)
	if err != nil {
		t.Fatalf("parse workspace cert A: %v", err)
	}

	workspaceCertB, err := parseCertFromPEM(resultB.CertificatePEM)
	if err != nil {
		t.Fatalf("parse workspace cert B: %v", err)
	}

	assertWorkspaceCert(t, workspaceCertA, tenantA)
	assertWorkspaceCert(t, workspaceCertB, tenantB)

	intermediates := x509.NewCertPool()
	intermediates.AddCert(svc.intermediateKey.cert)

	roots := x509.NewCertPool()
	roots.AddCert(rootCert)

	if _, err := workspaceCertA.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
	}); err != nil {
		t.Fatalf("verify workspace cert A chain: %v", err)
	}

	if _, err := workspaceCertB.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intermediates,
	}); err != nil {
		t.Fatalf("verify workspace cert B chain: %v", err)
	}

	nilSvc := &serviceImpl{}
	if _, err := nilSvc.GenerateWorkspaceCA(ctx, tenantA); err == nil {
		t.Fatalf("expected error when intermediateKey is nil")
	}
}

func assertWorkspaceCAResult(t *testing.T, result *WorkspaceCAResult) {
	t.Helper()

	if result == nil {
		t.Fatalf("expected non-nil WorkspaceCAResult")
	}

	if result.EncryptedPrivateKey == "" || result.Nonce == "" || result.CertificatePEM == "" {
		t.Fatalf("expected workspace CA result fields to be populated")
	}

	if result.KeyAlgorithm != "EC-P384" {
		t.Fatalf("unexpected key algorithm: %s", result.KeyAlgorithm)
	}

	if result.NotBefore.IsZero() || result.NotAfter.IsZero() {
		t.Fatalf("expected certificate validity timestamps to be populated")
	}
}

func assertWorkspaceCert(t *testing.T, cert *x509.Certificate, tenantID string) {
	t.Helper()

	if !cert.IsCA {
		t.Fatalf("expected workspace certificate to be a CA")
	}

	if cert.Subject.CommonName != "workspace-"+tenantID {
		t.Fatalf("unexpected workspace common name: %s", cert.Subject.CommonName)
	}

	if len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] != appmeta.PKIWorkspaceOrganization {
		t.Fatalf("unexpected workspace organization: %v", cert.Subject.Organization)
	}

	if cert.MaxPathLen != 0 || !cert.MaxPathLenZero {
		t.Fatalf("expected workspace MaxPathLen=0 with MaxPathLenZero=true")
	}

	if len(cert.URIs) != 1 {
		t.Fatalf("expected one tenant URI SAN, got %d", len(cert.URIs))
	}

	if cert.URIs[0].Scheme != "tenant" || cert.URIs[0].Opaque != tenantID {
		t.Fatalf("unexpected tenant URI SAN: %s", cert.URIs[0].String())
	}
}
