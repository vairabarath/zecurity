package client

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/outbox"
	"github.com/yourorg/ztna/controller/internal/pki"
)

// Certificate audit bug 9 (client device paths): the workspace CRL read the
// device's current cert_serial only, and every renewal overwrote it, so
// revoking a renewed device left its earlier certificates off the CRL. These
// tests drive the real EnrollDevice, RenewCert, RevokeDevice and SCIM
// offboarding handlers with real signing.

type deviceHistoryFixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	ctx    context.Context
	pki    pki.Service
	wsID   string
	userID string
	svc    *Service
}

func newDeviceHistoryFixture(t *testing.T) *deviceHistoryFixture {
	t.Helper()
	pool, ctx, wsID, userID, pkiSvc := deviceGateTestHarness(t)
	f := &deviceHistoryFixture{t: t, pool: pool, ctx: ctx, pki: pkiSvc, wsID: wsID, userID: userID}
	f.svc = f.service(pkiSvc)
	return f
}

func (f *deviceHistoryFixture) service(pkiSvc pki.Service) *Service {
	claims := &auth.AccessTokenClaims{UserID: f.userID, TenantID: f.wsID, Email: "history@example.test", Generation: 1}
	return issuanceService(f.pool, &Service{pool: f.pool, pkiSvc: pkiSvc}, claims)
}

// enrolled enrolls a device; returns its ID, its CSR (reused to renew, as the
// pinned key must match) and the issued serial.
func (f *deviceHistoryFixture) enrolled(name string) (deviceID, csrPEM, serial string) {
	f.t.Helper()
	csrPEM, _ = makeClientCSR(f.t)
	resp, err := f.svc.EnrollDevice(f.ctx, &clientv1.EnrollDeviceRequest{
		AccessToken: "still-valid-token", CsrPem: csrPEM, DeviceName: name, Os: "linux",
	})
	if err != nil {
		f.t.Fatalf("EnrollDevice: %v", err)
	}
	return resp.GetDeviceId(), csrPEM, certPEMSerial(f.t, resp.GetCertificatePem())
}

func (f *deviceHistoryFixture) renew(svc *Service, deviceID, csrPEM string) (*clientv1.RenewCertResponse, error) {
	return svc.RenewCert(f.ctx, &clientv1.RenewCertRequest{AccessToken: "still-valid-token", DeviceId: deviceID, CsrPem: csrPEM})
}

func (f *deviceHistoryFixture) mustRenew(deviceID, csrPEM string) string {
	f.t.Helper()
	resp, err := f.renew(f.svc, deviceID, csrPEM)
	if err != nil {
		f.t.Fatalf("RenewCert: %v", err)
	}
	return certPEMSerial(f.t, resp.GetCertificatePem())
}

func certPEMSerial(t *testing.T, certPEM string) string {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatal("certificate PEM does not decode")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert.SerialNumber.Text(16)
}

func (f *deviceHistoryFixture) crlSerials() map[string]bool {
	f.t.Helper()
	der, err := f.pki.GenerateClientCRL(f.ctx, f.wsID)
	if err != nil {
		f.t.Fatalf("GenerateClientCRL: %v", err)
	}
	list, err := x509.ParseRevocationList(der)
	if err != nil {
		f.t.Fatalf("parse CRL: %v", err)
	}
	got := map[string]bool{}
	for _, e := range list.RevokedCertificateEntries {
		got[e.SerialNumber.Text(16)] = true
	}
	return got
}

func (f *deviceHistoryFixture) exactlyListed(serials ...string) {
	f.t.Helper()
	crl := f.crlSerials()
	for _, s := range serials {
		if !crl[s] {
			f.t.Errorf("issued serial %s is not on the CRL (CRL: %v)", s, crl)
		}
	}
	if len(crl) != len(serials) {
		f.t.Errorf("CRL lists %d serials, want exactly %d: %v", len(crl), len(serials), crl)
	}
}

// The device's own RevokeDevice RPC (revokeClientDevice).
func TestDeviceCertificateHistory_RevokeDeviceListsEverySerial(t *testing.T) {
	f := newDeviceHistoryFixture(t)
	deviceID, csrPEM, first := f.enrolled("laptop")
	issued := []string{first, f.mustRenew(deviceID, csrPEM), f.mustRenew(deviceID, csrPEM)}
	if len(f.crlSerials()) != 0 {
		t.Fatal("nothing may be listed before revocation")
	}
	if _, err := f.svc.RevokeDevice(f.ctx, &clientv1.RevokeDeviceRequest{AccessToken: "still-valid-token", DeviceId: deviceID}); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}
	f.exactlyListed(issued...)
}

// SCIM offboarding (revokeUserDevices through the device-trust consumer)
// revokes every certificate of every device of the user.
func TestDeviceCertificateHistory_SCIMOffboardingListsEverySerial(t *testing.T) {
	f := newDeviceHistoryFixture(t)
	laptop, laptopCSR, l1 := f.enrolled("laptop")
	phone, phoneCSR, p1 := f.enrolled("phone")
	issued := []string{l1, f.mustRenew(laptop, laptopCSR), p1, f.mustRenew(phone, phoneCSR)}

	userID := uuid.MustParse(f.userID)
	evt := outbox.OutboxEvent{WorkspaceID: uuid.MustParse(f.wsID), UserID: &userID}
	if err := NewDeviceTrustRevokeHandler(f.pool, nil).Handle(f.ctx, evt); err != nil {
		t.Fatalf("revoke handler: %v", err)
	}
	f.exactlyListed(issued...)
}

// clientSignHookPKI runs during once inside the first signing call.
type clientSignHookPKI struct {
	pki.Service
	once   sync.Once
	during func()
}

func (p *clientSignHookPKI) SignClientCert(ctx context.Context, tenantID, deviceID, trustDomain string, csr *x509.CertificateRequest, ttl time.Duration) (*pki.ClientCertResult, error) {
	if p.during != nil {
		p.once.Do(p.during)
	}
	return p.Service.SignClientCert(ctx, tenantID, deviceID, trustDomain, csr, ttl)
}

// A revocation that lands while a renewal is being signed: the renewal is
// refused, and every certificate the device held is listed.
func TestDeviceCertificateHistory_RenewalRacingRevocation(t *testing.T) {
	f := newDeviceHistoryFixture(t)
	deviceID, csrPEM, first := f.enrolled("laptop")
	issued := []string{first, f.mustRenew(deviceID, csrPEM)}
	hook := &clientSignHookPKI{Service: f.pki, during: func() {
		if err := revokeClientDevice(f.ctx, f.pool, deviceID, f.userID, f.wsID); err != nil {
			t.Errorf("revoke: %v", err)
		}
	}}
	if resp, err := f.renew(f.service(hook), deviceID, csrPEM); err == nil {
		t.Fatalf("the racing renewal returned a certificate (%s)", certPEMSerial(t, resp.GetCertificatePem()))
	}
	f.exactlyListed(issued...)
}

// A renewal refused for a revoked device issues nothing, so nothing new is
// listed.
func TestDeviceCertificateHistory_RefusedRenewalRecordsNothing(t *testing.T) {
	f := newDeviceHistoryFixture(t)
	deviceID, csrPEM, first := f.enrolled("laptop")
	if err := revokeClientDevice(f.ctx, f.pool, deviceID, f.userID, f.wsID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := f.renew(f.svc, deviceID, csrPEM); err == nil {
		t.Fatal("a revoked device renewed")
	}
	f.exactlyListed(first)
}

// An enrollment refused by the lifecycle check (certificate audit bug 4)
// issues nothing, so it records no certificate.
func TestDeviceCertificateHistory_RefusedEnrollmentRecordsNothing(t *testing.T) {
	f := newDeviceHistoryFixture(t)
	execOrFail(t, f.ctx, f.pool, `UPDATE users SET status = 'suspended' WHERE id = $1`, f.userID)
	if _, err := f.svc.EnrollDevice(f.ctx, enrollRequest(t)); err == nil {
		t.Fatal("a suspended user enrolled a device")
	}
	var n int
	if err := f.pool.QueryRow(f.ctx, `SELECT count(*) FROM client_device_certificates WHERE tenant_id = $1`, f.wsID).Scan(&n); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if n != 0 {
		t.Fatalf("a refused enrollment recorded %d certificates", n)
	}
}
