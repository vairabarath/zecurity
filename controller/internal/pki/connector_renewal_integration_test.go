package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"os"
	"testing"
	"time"

	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// RenewConnectorCert had never executed once. Its only caller is the RenewCert RPC,
// whose only trigger is ReEnrollSignal — which the controller never sent, because
// nothing read Cfg.RenewalWindow. Wiring that trigger up (control_stream.go
// maybeRequestRenewal) activates this path for the first time, so it needs a test
// that actually runs it rather than one that reasons about it.
//
// The assertion that matters most is the public key: renewal REUSES the connector's
// existing private key and returns only a new certificate. If the issued cert did
// not carry the CSR's public key, every connector would renew "successfully" and
// then fail mTLS on reconnect — converting a 7-day expiry into an immediate outage.
func TestRenewConnectorCertIntegration(t *testing.T) {
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
	t.Setenv("PKI_MASTER_SECRET", "connector-renewal-integration-secret")

	svcIntf, err := Init(ctx, testPool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}
	svc := svcIntf.(*serviceImpl)

	const (
		tenantID    = "33333333-3333-3333-3333-333333333333"
		connectorID = "44444444-4444-4444-4444-444444444444"
		trustDomain = "renewal-test.zecurity"
	)

	// GenerateWorkspaceCA only returns the material; persisting it is the caller's
	// job (bootstrap does this in production).
	caResult, err := svc.GenerateWorkspaceCA(ctx, tenantID)
	if err != nil {
		t.Fatalf("GenerateWorkspaceCA: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO workspaces (id, slug, name) VALUES ($1, $2, $2)`,
		tenantID, "renewal-test",
	); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO workspace_ca_keys
		   (tenant_id, encrypted_private_key, nonce, certificate_pem, not_before, not_after)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		tenantID, caResult.EncryptedPrivateKey, caResult.Nonce, caResult.CertificatePEM,
		caResult.NotBefore, caResult.NotAfter,
	); err != nil {
		t.Fatalf("insert workspace_ca_keys: %v", err)
	}

	// The connector sends a self-signed PKCS#10 CSR built over its EXISTING key.
	// The proto field is called public_key_der, but both connector/src/crypto.rs
	// and shield/src/crypto.rs put CSR bytes in it — CheckSignature is what proves
	// possession of the private key.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate connector key: %v", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "renewal"},
	}, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}

	const ttl = 7 * 24 * time.Hour
	result, err := svc.RenewConnectorCert(ctx, tenantID, connectorID, trustDomain, csrDER, ttl)
	if err != nil {
		t.Fatalf("RenewConnectorCert: %v", err)
	}

	cert, err := parseCertFromPEM(result.CertificatePEM)
	if err != nil {
		t.Fatalf("parse renewed cert: %v", err)
	}

	// THE assertion: the renewed cert must be usable with the key the connector
	// already holds, because renewal never ships a new private key.
	renewedPub, ok := cert.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		t.Fatalf("renewed cert public key is %T, want *ecdsa.PublicKey", cert.PublicKey)
	}
	if !renewedPub.Equal(&key.PublicKey) {
		t.Fatal("renewed certificate does not carry the CSR's public key — the connector " +
			"would renew successfully and then fail mTLS with its existing private key")
	}

	// SPIFFE identity must survive renewal, or the connector reconnects and is
	// rejected by StreamSPIFFEInterceptor.
	if len(cert.URIs) != 1 {
		t.Fatalf("renewed cert has %d URI SANs, want exactly 1", len(cert.URIs))
	}
	if want := appmeta.ConnectorSPIFFEID(trustDomain, connectorID); cert.URIs[0].String() != want {
		t.Fatalf("renewed SPIFFE ID = %q, want %q", cert.URIs[0], want)
	}
	if want := appmeta.PKIConnectorCNPrefix + connectorID; cert.Subject.CommonName != want {
		t.Fatalf("renewed CN = %q, want %q", cert.Subject.CommonName, want)
	}

	// ClientAuth is what mTLS to the controller needs; without it the renewed cert
	// is rejected at handshake rather than at SPIFFE parsing.
	var hasClientAuth bool
	for _, eku := range cert.ExtKeyUsage {
		if eku == x509.ExtKeyUsageClientAuth {
			hasClientAuth = true
		}
	}
	if !hasClientAuth {
		t.Fatal("renewed cert lacks ExtKeyUsageClientAuth")
	}
	if cert.IsCA {
		t.Fatal("renewed connector cert must be a leaf, not a CA")
	}

	// A renewal that does not actually extend the lifetime leaves the connector in
	// the same expiring state and prompts again on the next heartbeat, forever.
	if remaining := time.Until(cert.NotAfter); remaining < ttl-time.Hour {
		t.Fatalf("renewed cert expires in %v, want ~%v", remaining, ttl)
	}
	// Truncated to the second: x509 encodes NotAfter with second precision, so the
	// result's sub-second remainder is an encoding artefact, not a disagreement.
	if !result.NotAfter.Truncate(time.Second).Equal(cert.NotAfter.Truncate(time.Second)) {
		t.Fatalf("result.NotAfter %v disagrees with the certificate's %v — the DB would "+
			"record an expiry the cert does not have", result.NotAfter, cert.NotAfter)
	}

	// The renewed cert must chain to the workspace CA the connector already trusts.
	var workspaceCAPEM string
	if err := testPool.QueryRow(ctx,
		`SELECT certificate_pem FROM workspace_ca_keys WHERE tenant_id = $1`,
		tenantID,
	).Scan(&workspaceCAPEM); err != nil {
		t.Fatalf("load workspace CA: %v", err)
	}
	workspaceCA, err := parseCertFromPEM(workspaceCAPEM)
	if err != nil {
		t.Fatalf("parse workspace CA: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(workspaceCA)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("renewed cert does not chain to the workspace CA: %v", err)
	}
}
