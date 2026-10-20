package pki

import (
	"context"
	"crypto/x509"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Certificate audit bug 9: migration 039 adds per-certificate history for
// connectors and client devices and backfills the serials the database still
// holds (only the current one per row; earlier serials were overwritten and
// cannot be recovered). Revocation state comes from the owning row.

func applyMigrationFiles(t *testing.T, ctx context.Context, pool *pgxpool.Pool, keep func(name string) bool) int {
	t.Helper()
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	sort.Strings(files)
	applied := 0
	for _, f := range files {
		if !keep(filepath.Base(f)) {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
		applied++
	}
	return applied
}

func TestCertificateHistoryMigration_BackfillsCurrentSerials(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := uniqueTestDatabaseName(t)
	adminPool := mustConnectTestPool(t, ctx, adminDSN)
	t.Cleanup(adminPool.Close)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() { _, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName) })
	dsn, err := withDatabaseName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}
	pool := mustConnectTestPool(t, ctx, dsn)
	t.Cleanup(pool.Close)

	// The schema as it was before 039, with legacy data in it.
	applyMigrationFiles(t, ctx, pool, func(n string) bool { return n < "039_" })
	scalar := func(sql string, args ...any) string {
		t.Helper()
		var v string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return v
	}
	ws := scalar(`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ('hist', 'hist', 'active', 'hist.test') RETURNING id::text`)
	rn := scalar(`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'rn', 'office') RETURNING id::text`, ws)
	user := scalar(`INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		VALUES ($1, 'u@hist.test', 'google', 'sub-u', 'member', 'active') RETURNING id::text`, ws)
	notAfter := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Microsecond)
	revokedAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	conn := func(name, status, serial string, notAfter, revokedAt any) string {
		return scalar(`INSERT INTO connectors (tenant_id, remote_network_id, name, status, cert_serial, cert_not_after, revoked_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id::text`, ws, rn, name, status, serial, notAfter, revokedAt)
	}
	active := conn("active", "active", "1a", notAfter, nil)
	revoked := conn("revoked", "revoked", "1b", notAfter, revokedAt)
	legacyRevoked := conn("legacy-revoked", "revoked", "1c", notAfter, nil) // status only
	conn("pending", "pending", "", nil, nil)                                // never issued
	noExpiry := conn("no-expiry", "active", "1e", nil, nil)                 // expiry unknown
	dev := func(name, serial string, notAfter, revokedAt any) string {
		return scalar(`INSERT INTO client_devices (user_id, workspace_id, name, os, cert_serial, cert_not_after, revoked_at)
			VALUES ($1, $2, $3, 'linux', NULLIF($4, ''), $5, $6) RETURNING id::text`, user, ws, name, serial, notAfter, revokedAt)
	}
	liveDevice := dev("laptop", "2a", notAfter, nil)
	revokedDevice := dev("phone", "2b", notAfter, revokedAt)
	noExpiryDevice := dev("no-expiry", "2c", nil, nil)
	if _, err := pool.Exec(ctx, `UPDATE connectors SET cert_serial = NULL WHERE name = 'pending'`); err != nil {
		t.Fatalf("clear pending serial: %v", err)
	}

	if n := applyMigrationFiles(t, ctx, pool, func(n string) bool { return strings.HasPrefix(n, "039_") }); n != 1 {
		t.Fatalf("migration 039 not found (%d files)", n)
	}

	type row struct {
		owner, tenant string
		notAfter      *time.Time // nil: infinity (expiry unknown)
		revoked       *time.Time
	}
	read := func(table, ownerCol string) map[string]row {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT serial, `+ownerCol+`::text, tenant_id::text, NULLIF(not_after, 'infinity'), revoked_at FROM `+table)
		if err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		defer rows.Close()
		got := map[string]row{}
		for rows.Next() {
			var serial string
			var r row
			if err := rows.Scan(&serial, &r.owner, &r.tenant, &r.notAfter, &r.revoked); err != nil {
				t.Fatalf("scan %s: %v", table, err)
			}
			got[serial] = r
		}
		return got
	}

	connectors := read("connector_certificates", "connector_id")
	if len(connectors) != 4 {
		t.Fatalf("connector history %v, want exactly 1a, 1b, 1c, 1e", connectors)
	}
	for serial, want := range map[string]struct {
		owner   string
		revoked bool
	}{"1a": {active, false}, "1b": {revoked, true}, "1c": {legacyRevoked, true}} {
		got, ok := connectors[serial]
		if !ok || got.owner != want.owner || got.tenant != ws || got.notAfter == nil || !got.notAfter.Equal(notAfter) || (got.revoked != nil) != want.revoked {
			t.Errorf("connector history %s = %+v, want owner %s, tenant %s, not_after %v, revoked %v", serial, got, want.owner, ws, notAfter, want.revoked)
		}
	}
	// Expiry unknown: kept with not_after = infinity, so if it is ever
	// revoked it stays listed rather than dropping off.
	if got, ok := connectors["1e"]; !ok || got.owner != noExpiry || got.notAfter != nil || got.revoked != nil {
		t.Errorf("connector history 1e = %+v, want owner %s, infinite not_after, not revoked", got, noExpiry)
	}
	if got := connectors["1b"].revoked; got == nil || !got.Equal(revokedAt) {
		t.Errorf("1b revoked_at = %v, want the connector's %v", got, revokedAt)
	}

	devices := read("client_device_certificates", "device_id")
	if len(devices) != 3 {
		t.Fatalf("device history %v, want exactly 2a, 2b, 2c", devices)
	}
	if got := devices["2a"]; got.owner != liveDevice || got.tenant != ws || got.revoked != nil || got.notAfter == nil || !got.notAfter.Equal(notAfter) {
		t.Errorf("device history 2a = %+v", got)
	}
	if got := devices["2c"]; got.owner != noExpiryDevice || got.notAfter != nil || got.revoked != nil {
		t.Errorf("device history 2c = %+v, want infinite not_after, not revoked", got)
	}
	if got := devices["2b"]; got.owner != revokedDevice || got.revoked == nil || !got.revoked.Equal(revokedAt) {
		t.Errorf("device history 2b = %+v", got)
	}

	// The CRL built from the backfilled history lists the revoked ones.
	t.Setenv("PKI_MASTER_SECRET", "cert-history-migration-secret")
	svc, err := Init(ctx, pool)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	ca, err := svc.GenerateWorkspaceCA(ctx, ws)
	if err != nil {
		t.Fatalf("GenerateWorkspaceCA: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_ca_keys
	    (tenant_id, encrypted_private_key, nonce, certificate_pem, key_algorithm, not_before, not_after)
	  VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		ws, ca.EncryptedPrivateKey, ca.Nonce, ca.CertificatePEM, ca.KeyAlgorithm, ca.NotBefore, ca.NotAfter); err != nil {
		t.Fatalf("store workspace CA: %v", err)
	}
	der, err := svc.GenerateClientCRL(ctx, ws)
	if err != nil {
		t.Fatalf("GenerateClientCRL: %v", err)
	}
	list, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatalf("parse CRL: %v", err)
	}
	listed := map[string]bool{}
	for _, e := range list.RevokedCertificateEntries {
		listed[e.SerialNumber.Text(16)] = true
	}
	if len(listed) != 3 || !listed["1b"] || !listed["1c"] || !listed["2b"] {
		t.Fatalf("CRL after backfill = %v, want exactly 1b, 1c, 2b", listed)
	}
}
