package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"os"
	"testing"
	"time"
)

func TestChainAuditIntegration(t *testing.T) {
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

	t.Setenv("PKI_MASTER_SECRET", "phase-a-chain-audit-secret")

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
	tdA := "ws-a.zecurity.in"
	tdB := "ws-b.zecurity.in"

	insertWorkspaceCA := func(tenantID, slug string, r *WorkspaceCAResult) {
		t.Helper()
		if _, err := testPool.Exec(ctx,
			`INSERT INTO workspaces (id, slug, name) VALUES ($1, $2, $2)`,
			tenantID, slug,
		); err != nil {
			t.Fatalf("insert workspace %s: %v", slug, err)
		}
		if _, err := testPool.Exec(ctx,
			`INSERT INTO workspace_ca_keys
			   (tenant_id, encrypted_private_key, nonce, certificate_pem, not_before, not_after)
			 VALUES ($1, $2, $3, $4, $5, $6)`,
			tenantID, r.EncryptedPrivateKey, r.Nonce, r.CertificatePEM, r.NotBefore, r.NotAfter,
		); err != nil {
			t.Fatalf("insert workspace_ca_keys %s: %v", slug, err)
		}
	}

	resultA, err := svc.GenerateWorkspaceCA(ctx, tenantA)
	if err != nil {
		t.Fatalf("GenerateWorkspaceCA tenant A: %v", err)
	}
	insertWorkspaceCA(tenantA, "ws-a", resultA)

	resultB, err := svc.GenerateWorkspaceCA(ctx, tenantB)
	if err != nil {
		t.Fatalf("GenerateWorkspaceCA tenant B: %v", err)
	}
	insertWorkspaceCA(tenantB, "ws-b", resultB)

	connA, err := svc.SignConnectorCert(ctx, tenantA, "conn-a-id", tdA, makeCSR(t, "connector-a"), time.Hour)
	if err != nil {
		t.Fatalf("SignConnectorCert A: %v", err)
	}
	cliA, err := svc.SignClientCert(ctx, tenantA, "dev-a-id", tdA, makeCSR(t, "client-a"), time.Hour)
	if err != nil {
		t.Fatalf("SignClientCert A: %v", err)
	}
	connB, err := svc.SignConnectorCert(ctx, tenantB, "conn-b-id", tdB, makeCSR(t, "connector-b"), time.Hour)
	if err != nil {
		t.Fatalf("SignConnectorCert B: %v", err)
	}
	cliB, err := svc.SignClientCert(ctx, tenantB, "dev-b-id", tdB, makeCSR(t, "client-b"), time.Hour)
	if err != nil {
		t.Fatalf("SignClientCert B: %v", err)
	}

	roots := x509.NewCertPool()
	roots.AddCert(svc.intermediateKey.cert)

	wsA, err := parseCertFromPEM(resultA.CertificatePEM)
	if err != nil {
		t.Fatalf("parse workspace CA A: %v", err)
	}
	wsB, err := parseCertFromPEM(resultB.CertificatePEM)
	if err != nil {
		t.Fatalf("parse workspace CA B: %v", err)
	}

	intsA := x509.NewCertPool()
	intsA.AddCert(wsA)
	intsB := x509.NewCertPool()
	intsB.AddCert(wsB)

	verifyOK := func(label, leafPEM string, ints *x509.CertPool) {
		t.Helper()
		leaf, err := parseCertFromPEM(leafPEM)
		if err != nil {
			t.Fatalf("%s parse: %v", label, err)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots:         roots,
			Intermediates: ints,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		}); err != nil {
			t.Fatalf("%s must verify: %v", label, err)
		}
	}
	verifyOK("connector A", connA.CertificatePEM, intsA)
	verifyOK("client A", cliA.CertificatePEM, intsA)
	verifyOK("connector B", connB.CertificatePEM, intsB)
	verifyOK("client B", cliB.CertificatePEM, intsB)

	leafA, err := parseCertFromPEM(connA.CertificatePEM)
	if err != nil {
		t.Fatalf("parse connector A leaf: %v", err)
	}
	if _, err := leafA.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: intsB,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err == nil {
		t.Fatalf("leaf A must NOT verify via workspace B CA")
	}

	fakeCAKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("fake CA key: %v", err)
	}
	fakeCATpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake-ws-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	fakeCADER, err := x509.CreateCertificate(rand.Reader, fakeCATpl, fakeCATpl, &fakeCAKey.PublicKey, fakeCAKey)
	if err != nil {
		t.Fatalf("create fake CA: %v", err)
	}
	fakeCA, err := x509.ParseCertificate(fakeCADER)
	if err != nil {
		t.Fatalf("parse fake CA: %v", err)
	}

	fakeLeafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("fake leaf key: %v", err)
	}
	fakeLeafTpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "fake-leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	fakeLeafDER, err := x509.CreateCertificate(rand.Reader, fakeLeafTpl, fakeCA, &fakeLeafKey.PublicKey, fakeCAKey)
	if err != nil {
		t.Fatalf("create fake leaf: %v", err)
	}
	fakeLeaf, err := x509.ParseCertificate(fakeLeafDER)
	if err != nil {
		t.Fatalf("parse fake leaf: %v", err)
	}

	fakeInts := x509.NewCertPool()
	fakeInts.AddCert(fakeCA)
	if _, err := fakeLeaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: fakeInts,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err == nil {
		t.Fatalf("leaf signed by an unknown CA must NOT verify")
	}
}

func makeCSR(t *testing.T, cn string) *x509.CertificateRequest {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}}, key)
	if err != nil {
		t.Fatalf("create csr: %v", err)
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatalf("parse csr: %v", err)
	}
	return csr
}
