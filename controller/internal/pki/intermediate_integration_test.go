package pki

import (
	"context"
	"crypto/x509"
	"os"
	"testing"

	"github.com/yourorg/ztna/controller/internal/appmeta"
)

func TestIntermediateCAIntegration(t *testing.T) {
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

	t.Setenv("PKI_MASTER_SECRET", "phase-4-intermediate-ca-integration-secret")

	svcIntf, err := Init(ctx, testPool)
	if err != nil {
		t.Fatalf("first pki.Init: %v", err)
	}

	svc, ok := svcIntf.(*serviceImpl)
	if !ok {
		t.Fatalf("expected *serviceImpl from Init, got %T", svcIntf)
	}

	var intermediateCount int
	var encryptedKey string
	var nonce string
	var certPEM string
	err = testPool.QueryRow(
		ctx,
		`SELECT COUNT(*), encrypted_key, nonce, certificate_pem
		 FROM ca_intermediate
		 GROUP BY encrypted_key, nonce, certificate_pem`,
	).Scan(&intermediateCount, &encryptedKey, &nonce, &certPEM)
	if err != nil {
		t.Fatalf("query ca_intermediate after first init: %v", err)
	}

	if intermediateCount != 1 {
		t.Fatalf("expected exactly one intermediate CA row, got %d", intermediateCount)
	}

	if encryptedKey == "" || nonce == "" || certPEM == "" {
		t.Fatalf("expected encrypted intermediate key material and certificate to be stored")
	}

	if svc.intermediateKey == nil || svc.intermediateKey.cert == nil || svc.intermediateKey.privKey == nil {
		t.Fatalf("expected intermediate CA signing material to be loaded into memory")
	}

	rootCert, rootKey, err := svc.loadRootCA(ctx)
	if err != nil {
		t.Fatalf("loadRootCA: %v", err)
	}
	defer rootKey.D.SetInt64(0)

	intermediateCert, err := parseCertFromPEM(certPEM)
	if err != nil {
		t.Fatalf("parse intermediate cert: %v", err)
	}

	if !intermediateCert.IsCA {
		t.Fatalf("expected intermediate certificate to be a CA")
	}

	if intermediateCert.Subject.CommonName != appmeta.PKIIntermediateCommonName {
		t.Fatalf("unexpected intermediate common name: %s", intermediateCert.Subject.CommonName)
	}

	if intermediateCert.MaxPathLen != 0 || !intermediateCert.MaxPathLenZero {
		t.Fatalf("expected intermediate MaxPathLen=0 with MaxPathLenZero=true")
	}

	roots := x509.NewCertPool()
	roots.AddCert(rootCert)

	if _, err := intermediateCert.Verify(x509.VerifyOptions{Roots: roots}); err != nil {
		t.Fatalf("verify intermediate cert chain against root: %v", err)
	}

	svcIntf2, err := Init(ctx, testPool)
	if err != nil {
		t.Fatalf("second pki.Init: %v", err)
	}

	svc2, ok := svcIntf2.(*serviceImpl)
	if !ok {
		t.Fatalf("expected *serviceImpl from second Init, got %T", svcIntf2)
	}

	if svc2.intermediateKey == nil || svc2.intermediateKey.cert == nil || svc2.intermediateKey.privKey == nil {
		t.Fatalf("expected intermediate CA signing material to be loaded on second Init")
	}

	var finalIntermediateCount int
	if err := testPool.QueryRow(ctx, "SELECT COUNT(*) FROM ca_intermediate").Scan(&finalIntermediateCount); err != nil {
		t.Fatalf("count ca_intermediate after second init: %v", err)
	}

	if finalIntermediateCount != 1 {
		t.Fatalf("expected one intermediate CA row after second init, got %d", finalIntermediateCount)
	}

	// Best-effort sanity check that the in-memory intermediate material is the
	// same certificate we stored in the database.
	if got := string(svc2.intermediateKey.cert.RawSubject); got == "" {
		t.Fatalf("expected loaded intermediate certificate subject to be present")
	}

	if svc2.intermediateKey.cert.Subject.CommonName != appmeta.PKIIntermediateCommonName {
		t.Fatalf("unexpected loaded intermediate common name: %s", svc2.intermediateKey.cert.Subject.CommonName)
	}

	if svc2.intermediateKey.privKey == nil {
		t.Fatalf("expected loaded intermediate private key")
	}

	if svc2.intermediateKey.cert.Issuer.String() != rootCert.Subject.String() {
		t.Fatalf("expected intermediate issuer to match root subject")
	}
}
