package shield

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/pki"
	"github.com/yourorg/ztna/controller/internal/spiffe"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Certificate audit bug 7: shield certificate renewal (proxied by the shield's
// connector) checked only that the shield row was not revoked and belonged to
// the calling connector. It never checked whether the connector itself was
// revoked or whether the shield had a revocation tombstone, and its final
// write was unconditional, so a revocation racing the renewal still produced
// an accepted certificate. A renewed certificate may now be returned only if
// the connector is unrevoked, the shield is eligible and untombstoned, and the
// final write re-confirms all of that.

// fakeShieldPKI issues a placeholder certificate; during runs mid-issuance,
// after the up-front checks and before the final write.
type fakeShieldPKI struct {
	pki.Service // nil: the renewal handler only calls RenewShieldCert
	calls       int
	during      func()
	err         error
}

func (f *fakeShieldPKI) RenewShieldCert(_ context.Context, _, _, _ string, _ []byte, ttl time.Duration) (*pki.ShieldCertResult, error) {
	f.calls++
	if f.during != nil {
		f.during()
	}
	if f.err != nil {
		return nil, f.err
	}
	return &pki.ShieldCertResult{
		CertificatePEM:    "RENEWED-CERT",
		WorkspaceCAPEM:    "WORKSPACE-CA",
		IntermediateCAPEM: "INTERMEDIATE-CA",
		Serial:            "renewed-serial",
		NotAfter:          time.Now().Add(ttl),
	}, nil
}

type renewalFixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	ctx    context.Context
	wsID   string
	td     string
	rnID   string
	connID string
	shield string
	pki    *fakeShieldPKI
	svc    *service
	key    *ecdsa.PrivateKey // the shields' key: renewal CSRs are signed with it
}

func newRenewalFixture(t *testing.T) *renewalFixture {
	t.Helper()
	pool, ctx := setupHeartbeatTestDB(t)
	f := &renewalFixture{t: t, pool: pool, ctx: ctx, pki: &fakeShieldPKI{}, key: newShieldKey(t)}
	slug := "renew-" + uuid.NewString()[:8]
	f.td = slug + ".zecurity.test"
	f.wsID = f.scalar(`INSERT INTO workspaces (slug, name, trust_domain, status) VALUES ($1, $1, $2, 'active') RETURNING id::text`, slug, f.td)
	f.rnID = f.scalar(`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'rn', 'office') RETURNING id::text`, f.wsID)
	f.connID = f.connector(f.td)
	f.shield = f.newShield(f.connID, &f.td)
	f.svc = &service{db: pool, pki: f.pki, cfg: Config{CertTTL: 7 * 24 * time.Hour}}
	return f
}

func (f *renewalFixture) scalar(sql string, args ...any) string {
	f.t.Helper()
	var v string
	if err := f.pool.QueryRow(f.ctx, sql, args...).Scan(&v); err != nil {
		f.t.Fatalf("query %q: %v", sql, err)
	}
	return v
}

func (f *renewalFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
		f.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (f *renewalFixture) connector(td string) string {
	f.t.Helper()
	return f.scalar(`INSERT INTO connectors (tenant_id, remote_network_id, name, trust_domain, status)
		VALUES ($1, $2, 'conn-' || gen_random_uuid()::text, $3, 'active') RETURNING id::text`, f.wsID, f.rnID, td)
}

func (f *renewalFixture) newShield(connID string, td *string) string {
	f.t.Helper()
	return f.scalar(`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status, trust_domain, cert_serial)
		VALUES ($1, $2, $3, 'sh-' || gen_random_uuid()::text, 'active', $4, 'original-serial') RETURNING id::text`,
		f.wsID, f.rnID, connID, td)
}

// as returns a context carrying the identity the SPIFFE interceptor injects
// for a connector.
func (f *renewalFixture) as(connID, td string) context.Context {
	return spiffe.WithIdentity(f.ctx, fmt.Sprintf("spiffe://%s/connector/%s", td, connID),
		appmeta.SPIFFERoleConnector, connID, td)
}

func (f *renewalFixture) renew(ctx context.Context, shieldID string) (*shieldpb.RenewCertResponse, error) {
	return f.svc.RenewCert(ctx, &shieldpb.RenewCertRequest{ShieldId: shieldID, PublicKeyDer: shieldCSR(f.t, f.td, shieldID, f.key)})
}

func (f *renewalFixture) serial(shieldID string) string {
	f.t.Helper()
	return f.scalar(`SELECT COALESCE(cert_serial, '') FROM shields WHERE id = $1`, shieldID)
}

func (f *renewalFixture) revokeConnector(connID string) {
	f.t.Helper()
	f.exec(`UPDATE connectors SET status = 'revoked', revoked_at = NOW(), updated_at = NOW() WHERE id = $1`, connID)
}

// refused asserts the renewal was denied with code, returned no certificate
// and left the shield's recorded certificate untouched.
func (f *renewalFixture) refused(resp *shieldpb.RenewCertResponse, err error, code codes.Code, shieldID string) {
	f.t.Helper()
	if got := status.Code(err); got != code {
		f.t.Fatalf("code = %v (err %v), want %v", got, err, code)
	}
	if resp != nil {
		f.t.Fatalf("a certificate was returned: %q", resp.CertificatePem)
	}
	if shieldID != "" {
		if got := f.serial(shieldID); got != "original-serial" {
			f.t.Fatalf("cert_serial = %q: the refused renewal was recorded", got)
		}
	}
}

func TestShieldRenewCert_ValidRenewalStillWorks(t *testing.T) {
	f := newRenewalFixture(t)
	resp, err := f.renew(f.as(f.connID, f.td), f.shield)
	if err != nil {
		t.Fatalf("valid renewal refused: %v", err)
	}
	if string(resp.CertificatePem) != "RENEWED-CERT" || string(resp.WorkspaceCaPem) != "WORKSPACE-CA" || string(resp.IntermediateCaPem) != "INTERMEDIATE-CA" {
		t.Fatalf("response = %+v", resp)
	}
	if got := f.serial(f.shield); got != "renewed-serial" {
		t.Fatalf("cert_serial = %q, want renewed-serial", got)
	}

	// A disconnected connector (and shield) is still trusted: only revocation
	// is terminal. A shield row without a trust domain is still renewable by
	// its connector.
	f.exec(`UPDATE connectors SET status = 'disconnected' WHERE id = $1`, f.connID)
	f.exec(`UPDATE shields SET status = 'disconnected', trust_domain = NULL, cert_serial = 'original-serial' WHERE id = $1`, f.shield)
	if _, err := f.renew(f.as(f.connID, f.td), f.shield); err != nil {
		t.Fatalf("renewal through a disconnected connector refused: %v", err)
	}
	if got := f.serial(f.shield); got != "renewed-serial" {
		t.Fatalf("cert_serial = %q, want renewed-serial", got)
	}
}

func TestShieldRenewCert_RevokedConnectorRefused(t *testing.T) {
	f := newRenewalFixture(t)
	f.revokeConnector(f.connID)
	resp, err := f.renew(f.as(f.connID, f.td), f.shield)
	f.refused(resp, err, codes.PermissionDenied, f.shield)
	if f.pki.calls != 0 {
		t.Fatal("a certificate was signed for a revoked connector")
	}
}

// Either revocation marker is terminal on its own.
func TestShieldRenewCert_ConnectorRevokedAtWithInconsistentStatusRefused(t *testing.T) {
	for _, st := range []string{"active", "disconnected"} {
		t.Run(st, func(t *testing.T) {
			f := newRenewalFixture(t)
			f.exec(`UPDATE connectors SET status = $2, revoked_at = NOW() WHERE id = $1`, f.connID, st)
			resp, err := f.renew(f.as(f.connID, f.td), f.shield)
			f.refused(resp, err, codes.PermissionDenied, f.shield)
			if f.pki.calls != 0 {
				t.Fatal("a certificate was signed for a revoked connector")
			}
		})
	}
	t.Run("status revoked without revoked_at", func(t *testing.T) {
		f := newRenewalFixture(t)
		f.exec(`UPDATE connectors SET status = 'revoked', revoked_at = NULL WHERE id = $1`, f.connID)
		resp, err := f.renew(f.as(f.connID, f.td), f.shield)
		f.refused(resp, err, codes.PermissionDenied, f.shield)
		if f.pki.calls != 0 {
			t.Fatal("a certificate was signed for a revoked connector")
		}
	})
}

// The tombstone is authoritative even if the shields row still looks active.
func TestShieldRenewCert_TombstonedShieldRefused(t *testing.T) {
	f := newRenewalFixture(t)
	f.exec(`INSERT INTO shield_revocations (shield_id, tenant_id) VALUES ($1, $2)`, f.shield, f.wsID)
	resp, err := f.renew(f.as(f.connID, f.td), f.shield)
	f.refused(resp, err, codes.PermissionDenied, f.shield)
	if f.pki.calls != 0 {
		t.Fatal("a certificate was signed for a revoked shield")
	}
}

// A revoked shield whose row has since been deleted is still revoked.
func TestShieldRenewCert_RevokedShieldWithoutRowRefused(t *testing.T) {
	f := newRenewalFixture(t)
	if err := Revoke(f.ctx, poolTx{f.pool}, f.wsID, f.shield); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	f.exec(`DELETE FROM shields WHERE id = $1`, f.shield)
	resp, err := f.renew(f.as(f.connID, f.td), f.shield)
	f.refused(resp, err, codes.PermissionDenied, "")
	if f.pki.calls != 0 {
		t.Fatal("a certificate was signed for a revoked shield")
	}

	// A shield that never existed is still simply not found.
	resp, err = f.renew(f.as(f.connID, f.td), uuid.NewString())
	f.refused(resp, err, codes.NotFound, "")
}

func TestShieldRenewCert_RevokedShieldRowRefused(t *testing.T) {
	f := newRenewalFixture(t)
	if err := Revoke(f.ctx, poolTx{f.pool}, f.wsID, f.shield); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	resp, err := f.renew(f.as(f.connID, f.td), f.shield)
	f.refused(resp, err, codes.PermissionDenied, f.shield)
	if f.pki.calls != 0 {
		t.Fatal("a certificate was signed for a revoked shield")
	}
}

// A shield row marked revoked without a tombstone (older data) is refused
// before anything is signed.
func TestShieldRenewCert_RevokedStatusWithoutTombstoneRefused(t *testing.T) {
	f := newRenewalFixture(t)
	f.exec(`UPDATE shields SET status = 'revoked' WHERE id = $1`, f.shield)
	resp, err := f.renew(f.as(f.connID, f.td), f.shield)
	f.refused(resp, err, codes.PermissionDenied, f.shield)
	if f.pki.calls != 0 {
		t.Fatal("a certificate was signed for a revoked shield")
	}
}

func TestShieldRenewCert_OtherConnectorOrTrustDomainRefused(t *testing.T) {
	// The shield row names another trust domain than its (matching) connector.
	t.Run("shield row in another trust domain", func(t *testing.T) {
		f := newRenewalFixture(t)
		f.exec(`UPDATE shields SET trust_domain = 'other.zecurity.test' WHERE id = $1`, f.shield)
		resp, err := f.renew(f.as(f.connID, f.td), f.shield)
		f.refused(resp, err, codes.PermissionDenied, f.shield)
		if f.pki.calls != 0 {
			t.Fatal("a certificate was signed for a shield in another trust domain")
		}
	})
	t.Run("shield managed by another connector", func(t *testing.T) {
		f := newRenewalFixture(t)
		other := f.connector(f.td)
		resp, err := f.renew(f.as(other, f.td), f.shield)
		f.refused(resp, err, codes.PermissionDenied, f.shield)
		if f.pki.calls != 0 {
			t.Fatal("a certificate was signed for another connector's shield")
		}
	})
	t.Run("caller trust domain differs from the shield's", func(t *testing.T) {
		f := newRenewalFixture(t)
		resp, err := f.renew(f.as(f.connID, "other.zecurity.test"), f.shield)
		f.refused(resp, err, codes.PermissionDenied, f.shield)
		if f.pki.calls != 0 {
			t.Fatal("a certificate was signed for a shield in another trust domain")
		}
	})
	// The shield row carries no trust domain, so only the connector's own
	// trust domain can prove the caller is the connector it claims to be.
	t.Run("caller trust domain differs from the connector's", func(t *testing.T) {
		f := newRenewalFixture(t)
		f.exec(`UPDATE shields SET trust_domain = NULL WHERE id = $1`, f.shield)
		resp, err := f.renew(f.as(f.connID, "other.zecurity.test"), f.shield)
		f.refused(resp, err, codes.PermissionDenied, f.shield)
		if f.pki.calls != 0 {
			t.Fatal("a certificate was signed for a connector in another trust domain")
		}
	})
	t.Run("no connector identity", func(t *testing.T) {
		f := newRenewalFixture(t)
		resp, err := f.renew(f.ctx, f.shield)
		f.refused(resp, err, codes.PermissionDenied, f.shield)
	})
}

// Fail closed: if authorization state cannot be read, nothing is signed.
func TestShieldRenewCert_UnreadableStateFailsClosed(t *testing.T) {
	f := newRenewalFixture(t)
	// Make the tombstone table unreadable for this throwaway database.
	f.exec(`ALTER TABLE shield_revocations RENAME TO shield_revocations_hidden`)
	resp, err := f.renew(f.as(f.connID, f.td), f.shield)
	f.refused(resp, err, codes.Internal, f.shield)
	if f.pki.calls != 0 {
		t.Fatal("a certificate was signed although authorization state was unreadable")
	}
}

func TestShieldRenewCert_ConnectorRevokedDuringRenewalReturnsNoCertificate(t *testing.T) {
	for name, sql := range map[string]string{
		"revoked":                       `UPDATE connectors SET status = 'revoked', revoked_at = NOW() WHERE id = $1`,
		"revoked_at only":               `UPDATE connectors SET revoked_at = NOW() WHERE id = $1`,
		"status revoked only":           `UPDATE connectors SET status = 'revoked' WHERE id = $1`,
		"deleted":                       `DELETE FROM connectors WHERE id = $1`,
		"moved to another trust domain": `UPDATE connectors SET trust_domain = 'other.zecurity.test' WHERE id = $1`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newRenewalFixture(t)
			f.pki.during = func() { f.exec(sql, f.connID) }
			resp, err := f.renew(f.as(f.connID, f.td), f.shield)
			shieldID := f.shield
			if name == "deleted" {
				shieldID = "" // the shield row cascades away with its connector
			}
			f.refused(resp, err, codes.PermissionDenied, shieldID)
			if f.pki.calls != 1 {
				t.Fatalf("pki calls = %d, want 1 (the race lands mid-issuance)", f.pki.calls)
			}
		})
	}
}

func TestShieldRenewCert_ShieldRevokedDuringRenewalReturnsNoCertificate(t *testing.T) {
	t.Run("revoked", func(t *testing.T) {
		f := newRenewalFixture(t)
		f.pki.during = func() {
			if err := Revoke(f.ctx, poolTx{f.pool}, f.wsID, f.shield); err != nil {
				t.Errorf("revoke: %v", err)
			}
		}
		resp, err := f.renew(f.as(f.connID, f.td), f.shield)
		f.refused(resp, err, codes.PermissionDenied, f.shield)
	})
	// The tombstone alone is enough, whatever the shields row says.
	t.Run("tombstoned", func(t *testing.T) {
		f := newRenewalFixture(t)
		f.pki.during = func() {
			f.exec(`INSERT INTO shield_revocations (shield_id, tenant_id) VALUES ($1, $2)`, f.shield, f.wsID)
		}
		resp, err := f.renew(f.as(f.connID, f.td), f.shield)
		f.refused(resp, err, codes.PermissionDenied, f.shield)
	})
	t.Run("moved to another connector", func(t *testing.T) {
		f := newRenewalFixture(t)
		other := f.connector(f.td)
		f.pki.during = func() { f.exec(`UPDATE shields SET connector_id = $2 WHERE id = $1`, f.shield, other) }
		resp, err := f.renew(f.as(f.connID, f.td), f.shield)
		f.refused(resp, err, codes.PermissionDenied, f.shield)
	})
	t.Run("moved to another trust domain", func(t *testing.T) {
		f := newRenewalFixture(t)
		f.pki.during = func() { f.exec(`UPDATE shields SET trust_domain = 'other.zecurity.test' WHERE id = $1`, f.shield) }
		resp, err := f.renew(f.as(f.connID, f.td), f.shield)
		f.refused(resp, err, codes.PermissionDenied, f.shield)
	})
	t.Run("status revoked only", func(t *testing.T) {
		f := newRenewalFixture(t)
		f.pki.during = func() { f.exec(`UPDATE shields SET status = 'revoked' WHERE id = $1`, f.shield) }
		resp, err := f.renew(f.as(f.connID, f.td), f.shield)
		f.refused(resp, err, codes.PermissionDenied, f.shield)
	})
	t.Run("deleted", func(t *testing.T) {
		f := newRenewalFixture(t)
		f.pki.during = func() { f.exec(`DELETE FROM shields WHERE id = $1`, f.shield) }
		resp, err := f.renew(f.as(f.connID, f.td), f.shield)
		f.refused(resp, err, codes.PermissionDenied, "")
	})
}

func TestShieldRenewCert_FinalWriteFailureReturnsNoCertificate(t *testing.T) {
	f := newRenewalFixture(t)
	f.exec(`CREATE FUNCTION fail_shield_update() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'write failed'; END; $$ LANGUAGE plpgsql`)
	f.exec(`CREATE TRIGGER fail_shield_update BEFORE UPDATE ON shields FOR EACH ROW EXECUTE FUNCTION fail_shield_update()`)
	resp, err := f.renew(f.as(f.connID, f.td), f.shield)
	f.refused(resp, err, codes.Internal, f.shield)
}

func TestShieldRenewCert_SigningFailureReturnsNoCertificate(t *testing.T) {
	f := newRenewalFixture(t)
	f.pki.err = errors.New("bad csr")
	resp, err := f.renew(f.as(f.connID, f.td), f.shield)
	f.refused(resp, err, codes.Internal, f.shield)
}

// poolTx adapts a pool to TxBeginner for Revoke.
type poolTx struct{ pool *pgxpool.Pool }

func (p poolTx) BeginTx(ctx context.Context) (pgx.Tx, error) { return p.pool.Begin(ctx) }

// inFlight starts a transaction running sql, leaves it uncommitted while the
// renewal's final write begins, and commits it shortly after. The final write
// must wait for it and then honour it.
func (f *renewalFixture) inFlight(done chan<- error, sqls ...string) {
	f.t.Helper()
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		f.t.Fatalf("begin: %v", err)
	}
	for _, sql := range sqls {
		if _, err := tx.Exec(f.ctx, sql); err != nil {
			_ = tx.Rollback(f.ctx)
			f.t.Fatalf("in-flight %q: %v", sql, err)
		}
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		done <- tx.Commit(f.ctx)
	}()
}

func TestShieldRenewCert_InFlightRevocationCommittingDuringFinalWrite(t *testing.T) {
	t.Run("connector", func(t *testing.T) {
		f := newRenewalFixture(t)
		done := make(chan error, 1)
		f.pki.during = func() {
			f.inFlight(done, fmt.Sprintf(
				`UPDATE connectors SET status = 'revoked', revoked_at = NOW() WHERE id = '%s'`, f.connID))
		}
		resp, err := f.renew(f.as(f.connID, f.td), f.shield)
		if cerr := <-done; cerr != nil {
			t.Fatalf("commit revocation: %v", cerr)
		}
		f.refused(resp, err, codes.PermissionDenied, f.shield)
	})
	t.Run("shield", func(t *testing.T) {
		f := newRenewalFixture(t)
		done := make(chan error, 1)
		f.pki.during = func() {
			f.inFlight(done,
				fmt.Sprintf(`UPDATE shields SET status = 'revoked', updated_at = NOW() WHERE id = '%s'`, f.shield),
				fmt.Sprintf(`INSERT INTO shield_revocations (shield_id, tenant_id) VALUES ('%s', '%s')`, f.shield, f.wsID))
		}
		resp, err := f.renew(f.as(f.connID, f.td), f.shield)
		if cerr := <-done; cerr != nil {
			t.Fatalf("commit revocation: %v", cerr)
		}
		f.refused(resp, err, codes.PermissionDenied, f.shield)
	})
}
