package pki

import (
	"context"
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// Sprint 21 B4: CRL thisUpdate is backdated so consumers whose clock is a
// little behind the controller still accept a fresh CRL.

func TestCRLWindow(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, validity := range []time.Duration{relayCRLValidity, 24 * time.Hour} {
		thisUpdate, nextUpdate := crlWindow(now, validity)
		if want := now.Add(-5 * time.Minute); !thisUpdate.Equal(want) {
			t.Errorf("validity %s: thisUpdate = %s, want %s (5 min before signing)", validity, thisUpdate, want)
		}
		if want := now.Add(validity); !nextUpdate.Equal(want) {
			t.Errorf("validity %s: nextUpdate = %s, want %s (unchanged: signing time + validity)", validity, nextUpdate, want)
		}
	}
}

// assertBackdatedWindow checks a generated CRL's validity window against the
// signing interval [before, after], at the one-second precision CRLs encode.
func assertBackdatedWindow(t *testing.T, crl *x509.RevocationList, before, after time.Time, validity time.Duration) {
	t.Helper()
	lo, hi := before.Truncate(time.Second), after
	if tu := crl.ThisUpdate; tu.Before(lo.Add(-crlThisUpdateBackdate)) || tu.After(hi.Add(-crlThisUpdateBackdate)) {
		t.Errorf("ThisUpdate %s not 5 min before signing [%s, %s]", tu, before, after)
	}
	if nu := crl.NextUpdate; nu.Before(lo.Add(validity)) || nu.After(hi.Add(validity)) {
		t.Errorf("NextUpdate %s not signing time + %s [%s, %s]", nu, validity, before, after)
	}
	// What a consumer checks (thisUpdate <= its now < nextUpdate): a consumer
	// whose clock is 4m59s behind the controller accepts the CRL.
	consumerNow := before.Add(-(crlThisUpdateBackdate - time.Second))
	if crl.ThisUpdate.After(consumerNow) {
		t.Errorf("a consumer 4m59s behind would reject: thisUpdate %s > its now %s", crl.ThisUpdate, consumerNow)
	}
	if !consumerNow.Before(crl.NextUpdate) {
		t.Errorf("a consumer 4m59s behind would see the CRL as expired")
	}
}

func TestGenerateRelayCRL_BackdatesThisUpdate(t *testing.T) {
	svc, caCert := newTestIntermediateService(t)
	before := time.Now()
	der, err := svc.GenerateRelayCRL(context.Background(), nil)
	after := time.Now()
	if err != nil {
		t.Fatalf("GenerateRelayCRL: %v", err)
	}
	crl, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatalf("ParseRevocationList: %v", err)
	}
	if err := crl.CheckSignatureFrom(caCert); err != nil {
		t.Fatalf("CRL not signed by Intermediate CA: %v", err)
	}
	assertBackdatedWindow(t, crl, before, after, relayCRLValidity)
}

// The workspace CRL (/ca.crl) loads its CA from the database.
func TestGenerateClientCRL_BackdatesThisUpdate(t *testing.T) {
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

	// GenerateClientCRL reads client_devices and connectors too, so apply
	// every schema file, not just 001.
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("find migrations: %v (%d files)", err, len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		sqlBytes, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := testPool.Exec(ctx, string(sqlBytes)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}

	t.Setenv("PKI_MASTER_SECRET", "sprint-21-b4-crl-window-secret")
	svcIntf, err := Init(ctx, testPool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}
	svc := svcIntf.(*serviceImpl)

	tenant := "33333333-3333-3333-3333-333333333333"
	ca, err := svc.GenerateWorkspaceCA(ctx, tenant)
	if err != nil {
		t.Fatalf("GenerateWorkspaceCA: %v", err)
	}
	if _, err := testPool.Exec(ctx, `INSERT INTO workspaces (id, slug, name, trust_domain) VALUES ($1, $2, $2, $2 || '.zecurity.test')`, tenant, "ws-b4"); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`INSERT INTO workspace_ca_keys (tenant_id, encrypted_private_key, nonce, certificate_pem, not_before, not_after)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		tenant, ca.EncryptedPrivateKey, ca.Nonce, ca.CertificatePEM, ca.NotBefore, ca.NotAfter,
	); err != nil {
		t.Fatalf("insert workspace_ca_keys: %v", err)
	}
	caCert, err := parseCertFromPEM(ca.CertificatePEM)
	if err != nil {
		t.Fatalf("parse workspace CA: %v", err)
	}

	before := time.Now()
	der, err := svc.GenerateClientCRL(ctx, tenant)
	after := time.Now()
	if err != nil {
		t.Fatalf("GenerateClientCRL: %v", err)
	}
	crl, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatalf("ParseRevocationList: %v", err)
	}
	if err := crl.CheckSignatureFrom(caCert); err != nil {
		t.Fatal(fmt.Errorf("CRL not signed by the workspace CA: %w", err))
	}
	assertBackdatedWindow(t, crl, before, after, 24*time.Hour)
}
