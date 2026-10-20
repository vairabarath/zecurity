package resolvers

import (
	"context"
	"crypto/x509"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/db"
	"github.com/yourorg/ztna/controller/internal/pki"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// Certificate audit bug 9: the workspace CRL read the revoked connectors' and
// devices' current cert_serial only. Every renewal overwrote it, so revoking
// a connector or device left its earlier, still-valid certificates off the
// CRL, and deleting a revoked connector dropped it from the CRL altogether.

type certHistoryFixture struct {
	pool *pgxpool.Pool
	ctx  context.Context
	pki  pki.Service
	mr   *mutationResolver
	ws   string
	rn   string
	user string
}

func newCertHistoryFixture(t *testing.T) *certHistoryFixture {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := "resolvers_certhist_" + strconv.Itoa(os.Getpid()) + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(adminPool.Close)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() { _, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName) })
	parsed, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	parsed.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	files, _ := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	t.Setenv("PKI_MASTER_SECRET", "cert-history-test-secret")
	pkiSvc, err := pki.Init(ctx, pool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}

	f := &certHistoryFixture{pool: pool, ctx: ctx, pki: pkiSvc}
	f.ws = f.scalar(t, `INSERT INTO workspaces (slug, name, status, trust_domain)
		VALUES ('certhist', 'certhist', 'active', 'certhist.test') RETURNING id::text`)
	ca, err := pkiSvc.GenerateWorkspaceCA(ctx, f.ws)
	if err != nil {
		t.Fatalf("GenerateWorkspaceCA: %v", err)
	}
	f.exec(t, `INSERT INTO workspace_ca_keys
	             (tenant_id, encrypted_private_key, nonce, certificate_pem, key_algorithm, not_before, not_after)
	           VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		f.ws, ca.EncryptedPrivateKey, ca.Nonce, ca.CertificatePEM, ca.KeyAlgorithm, ca.NotBefore, ca.NotAfter)
	f.rn = f.scalar(t, `INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'rn', 'office') RETURNING id::text`, f.ws)
	f.user = f.scalar(t, `INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		VALUES ($1, 'owner@certhist.test', 'google', 'sub-owner', 'member', 'active') RETURNING id::text`, f.ws)
	f.mr = &mutationResolver{&Resolver{
		TenantDB:       db.NewTenantDB(pool),
		Pool:           pool,
		PolicyNotifier: policy.NewNotifier(policy.NewSnapshotCache()),
	}}
	f.ctx = tenant.Set(ctx, tenant.TenantContext{TenantID: f.ws, UserID: f.user, Role: "admin", Email: "admin@certhist.test"})
	return f
}

func (f *certHistoryFixture) scalar(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var v string
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return v
}

func (f *certHistoryFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// connector creates an active connector whose certificate is issued, then
// renewed, once per entry in serials, with each issuance written as the
// enrollment and renewal handlers write it.
func (f *certHistoryFixture) connector(t *testing.T, name string, serials []string, notAfter time.Time) string {
	t.Helper()
	id := f.scalar(t, `INSERT INTO connectors (tenant_id, remote_network_id, name, status, trust_domain)
		VALUES ($1, $2, $3, 'active', 'certhist.test') RETURNING id::text`, f.ws, f.rn, name)
	for _, s := range serials {
		f.exec(t, `UPDATE connectors SET cert_serial = $1, cert_not_after = $2, updated_at = NOW() WHERE id = $3`, s, notAfter, id)
	}
	return id
}

// device creates a client device whose certificate is issued, then renewed,
// once per entry in serials.
func (f *certHistoryFixture) device(t *testing.T, name string, serials []string, notAfter time.Time) string {
	t.Helper()
	id := f.scalar(t, `INSERT INTO client_devices (user_id, workspace_id, name, os)
		VALUES ($1, $2, $3, 'linux') RETURNING id::text`, f.user, f.ws, name)
	for _, s := range serials {
		f.exec(t, `UPDATE client_devices SET cert_serial = $1, cert_not_after = $2 WHERE id = $3`, s, notAfter, id)
	}
	return id
}

// crl returns the serials on the workspace CRL.
func (f *certHistoryFixture) crl(t *testing.T) map[string]bool {
	t.Helper()
	der, err := f.pki.GenerateClientCRL(f.ctx, f.ws)
	if err != nil {
		t.Fatalf("GenerateClientCRL: %v", err)
	}
	list, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatalf("parse CRL: %v", err)
	}
	got := map[string]bool{}
	for _, e := range list.RevokedCertificateEntries {
		got[e.SerialNumber.Text(16)] = true
	}
	return got
}

func requireListed(t *testing.T, crl map[string]bool, serials ...string) {
	t.Helper()
	for _, s := range serials {
		if !crl[s] {
			t.Errorf("serial %s is not on the CRL (CRL: %v)", s, crl)
		}
	}
}

func requireNotListed(t *testing.T, crl map[string]bool, serials ...string) {
	t.Helper()
	for _, s := range serials {
		if crl[s] {
			t.Errorf("serial %s is on the CRL but must not be", s)
		}
	}
}

var valid = time.Now().Add(7 * 24 * time.Hour)

func TestRevokeConnector_CRLListsEverySerialItHeld(t *testing.T) {
	f := newCertHistoryFixture(t)
	serials := []string{"a1a1a1a1", "a2a2a2a2", "a3a3a3a3"}
	id := f.connector(t, "renewed-twice", serials, valid)
	if ok, err := f.mr.RevokeConnector(f.ctx, id); err != nil || !ok {
		t.Fatalf("RevokeConnector: %v", err)
	}
	requireListed(t, f.crl(t), serials...)
}

func TestDeleteRevokedConnector_SerialsStayOnCRL(t *testing.T) {
	f := newCertHistoryFixture(t)
	serials := []string{"b1b1b1b1", "b2b2b2b2"}
	id := f.connector(t, "deleted-after-revoke", serials, valid)
	if _, err := f.mr.RevokeConnector(f.ctx, id); err != nil {
		t.Fatalf("RevokeConnector: %v", err)
	}
	if ok, err := f.mr.DeleteConnector(f.ctx, id); err != nil || !ok {
		t.Fatalf("DeleteConnector: %v", err)
	}
	if n := f.scalar(t, `SELECT count(*)::text FROM connectors WHERE id = $1`, id); n != "0" {
		t.Fatal("test setup: the connector row should be gone")
	}
	requireListed(t, f.crl(t), serials...)
}

func TestRevokeDevice_CRLListsEverySerialItHeld(t *testing.T) {
	f := newCertHistoryFixture(t)
	serials := []string{"c1c1c1c1", "c2c2c2c2", "c3c3c3c3"}
	id := f.device(t, "laptop", serials, valid)
	if ok, err := f.mr.RevokeDevice(f.ctx, id); err != nil || !ok {
		t.Fatalf("RevokeDevice: %v", err)
	}
	requireListed(t, f.crl(t), serials...)
}

// Revoked certificates drop off the CRL once they expire; unexpired ones stay.
func TestWorkspaceCRL_ExpiredRevokedCertificatesDropOff(t *testing.T) {
	f := newCertHistoryFixture(t)
	expired := time.Now().Add(-time.Hour)
	gone := f.connector(t, "expired-only", []string{"d1d1d1d1"}, expired)
	if _, err := f.mr.RevokeConnector(f.ctx, gone); err != nil {
		t.Fatalf("RevokeConnector: %v", err)
	}
	goneDevice := f.device(t, "expired-device", []string{"d2d2d2d2"}, expired)
	if _, err := f.mr.RevokeDevice(f.ctx, goneDevice); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	live := f.connector(t, "still-valid", []string{"d3d3d3d3"}, valid)
	if _, err := f.mr.RevokeConnector(f.ctx, live); err != nil {
		t.Fatalf("RevokeConnector: %v", err)
	}
	crl := f.crl(t)
	requireNotListed(t, crl, "d1d1d1d1", "d2d2d2d2")
	requireListed(t, crl, "d3d3d3d3")
}

// Only revoked identities are listed: a renewal supersedes a certificate but
// does not revoke it.
func TestWorkspaceCRL_UnrevokedCertificatesAreNotListed(t *testing.T) {
	f := newCertHistoryFixture(t)
	f.connector(t, "healthy", []string{"e1e1e1e1", "e2e2e2e2"}, valid)
	f.device(t, "healthy-laptop", []string{"e3e3e3e3", "e4e4e4e4"}, valid)
	revoked := f.connector(t, "revoked", []string{"e5e5e5e5"}, valid)
	if _, err := f.mr.RevokeConnector(f.ctx, revoked); err != nil {
		t.Fatalf("RevokeConnector: %v", err)
	}
	crl := f.crl(t)
	requireNotListed(t, crl, "e1e1e1e1", "e2e2e2e2", "e3e3e3e3", "e4e4e4e4")
	requireListed(t, crl, "e5e5e5e5")
}

// Each workspace's CRL lists only its own revoked certificates.
func TestWorkspaceCRL_OtherWorkspacesAreNotListed(t *testing.T) {
	f := newCertHistoryFixture(t)
	other := f.scalar(t, `INSERT INTO workspaces (slug, name, status, trust_domain)
		VALUES ('certhist-other', 'other', 'active', 'other.test') RETURNING id::text`)
	otherRN := f.scalar(t, `INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'rn', 'office') RETURNING id::text`, other)
	otherConn := f.scalar(t, `INSERT INTO connectors (tenant_id, remote_network_id, name, status, cert_serial, cert_not_after)
		VALUES ($1, $2, 'theirs', 'active', 'f1f1f1f1', $3) RETURNING id::text`, other, otherRN, valid)
	f.exec(t, `UPDATE connectors SET status = 'revoked', revoked_at = NOW() WHERE id = $1`, otherConn)
	otherUser := f.scalar(t, `INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		VALUES ($1, 'them@other.test', 'google', 'sub-them', 'member', 'active') RETURNING id::text`, other)
	f.exec(t, `INSERT INTO client_devices (user_id, workspace_id, name, os, cert_serial, cert_not_after, revoked_at)
		VALUES ($1, $2, 'their-laptop', 'linux', 'f3f3f3f3', $3, NOW())`, otherUser, other, valid)
	mine := f.connector(t, "mine", []string{"f2f2f2f2"}, valid)
	if _, err := f.mr.RevokeConnector(f.ctx, mine); err != nil {
		t.Fatalf("RevokeConnector: %v", err)
	}
	crl := f.crl(t)
	requireNotListed(t, crl, "f1f1f1f1", "f3f3f3f3")
	requireListed(t, crl, "f2f2f2f2")
}

// Either revocation marker revokes a connector's certificates on its own
// (older rows can carry one without the other).
func TestWorkspaceCRL_EitherConnectorRevocationMarkerCounts(t *testing.T) {
	f := newCertHistoryFixture(t)
	statusOnly := f.connector(t, "status-only", []string{"a7a7a7a1", "a7a7a7a2"}, valid)
	f.exec(t, `UPDATE connectors SET status = 'revoked' WHERE id = $1`, statusOnly)
	stampOnly := f.connector(t, "revoked-at-only", []string{"a8a8a8a1", "a8a8a8a2"}, valid)
	f.exec(t, `UPDATE connectors SET revoked_at = NOW() WHERE id = $1`, stampOnly)
	requireListed(t, f.crl(t), "a7a7a7a1", "a7a7a7a2", "a8a8a8a1", "a8a8a8a2")
}

// A certificate recorded on an owner that is already revoked starts out
// revoked: revocation is terminal and covers it too.
func TestWorkspaceCRL_CertificateRecordedOnARevokedOwnerIsRevoked(t *testing.T) {
	f := newCertHistoryFixture(t)
	f.scalar(t, `INSERT INTO connectors (tenant_id, remote_network_id, name, status, revoked_at, cert_serial, cert_not_after)
		VALUES ($1, $2, 'born-revoked', 'revoked', NOW(), 'a9a9a9a1', $3) RETURNING id::text`, f.ws, f.rn, valid)
	conn := f.connector(t, "revoked-then-written", []string{"a9a9a9a2"}, valid)
	f.exec(t, `UPDATE connectors SET status = 'revoked', revoked_at = NOW() WHERE id = $1`, conn)
	f.exec(t, `UPDATE connectors SET cert_serial = 'a9a9a9a3', cert_not_after = $2 WHERE id = $1`, conn, valid)
	f.exec(t, `INSERT INTO client_devices (user_id, workspace_id, name, os, cert_serial, cert_not_after, revoked_at)
		VALUES ($1, $2, 'born-revoked', 'linux', 'a9a9a9a4', $3, NOW())`, f.user, f.ws, valid)
	dev := f.device(t, "revoked-then-written", []string{"a9a9a9a5"}, valid)
	f.exec(t, `UPDATE client_devices SET revoked_at = NOW() WHERE id = $1`, dev)
	f.exec(t, `UPDATE client_devices SET cert_serial = 'a9a9a9a6', cert_not_after = $2 WHERE id = $1`, dev, valid)
	requireListed(t, f.crl(t), "a9a9a9a1", "a9a9a9a2", "a9a9a9a3", "a9a9a9a4", "a9a9a9a5", "a9a9a9a6")
}

// A stored serial the CRL cannot encode fails CRL generation rather than
// being listed as a different serial.
func TestWorkspaceCRL_UnparsableSerialFailsClosed(t *testing.T) {
	f := newCertHistoryFixture(t)
	dev := f.device(t, "bad-serial", []string{"not-hex"}, valid)
	f.exec(t, `UPDATE client_devices SET revoked_at = NOW() WHERE id = $1`, dev)
	if _, err := f.pki.GenerateClientCRL(f.ctx, f.ws); err == nil {
		t.Fatal("a CRL was generated although a revoked serial could not be encoded")
	}
}
