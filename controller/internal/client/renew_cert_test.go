package client

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/auth"
)

// makeClientCSR builds a self-signed CSR the way the real client does
// (login.rs: rcgen-generated key + CSR) — here with a plain ECDSA P-256 key,
// since RenewCert/SignClientCert don't care about curve choice, only that the
// CSR's self-signature verifies.
func makeClientCSR(t *testing.T) (csrPEM string, pub crypto.PublicKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CSR key: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "test-device"},
	}, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	block := &pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}
	return string(pem.EncodeToMemory(block)), key.Public()
}

// TestRenewCertHappyPath covers the success path per Track3-Renew-Reenroll.md
// D-C: a CSR whose public key matches the fingerprint pinned at "enrollment"
// (simulated here by pinning it directly, since EnrollDevice isn't under
// test) gets a fresh cert. cert_serial changes; public_key_fingerprint does
// NOT (D-A) — that column has exactly one writer, EnrollDevice, forever.
func TestRenewCertHappyPath(t *testing.T) {
	pool, ctx, wsID, userID, pkiSvc := deviceGateTestHarness(t)
	deviceID := insertDevTrustDevice(t, ctx, pool, wsID, userID, "renew-happy", "AABBCCDDEEFF0011")

	csrPEM, pub := makeClientCSR(t)
	fp, err := publicKeyFingerprint(pub)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE client_devices SET public_key_fingerprint = $1 WHERE id = $2`,
		fp, deviceID,
	); err != nil {
		t.Fatalf("pin fingerprint: %v", err)
	}

	claims := &auth.AccessTokenClaims{UserID: userID, TenantID: wsID, Email: "renew-happy@example.test"}
	svc := &Service{pool: pool, authSvc: &postureTestAuth{claims: claims}, pkiSvc: pkiSvc}

	resp, err := svc.RenewCert(ctx, &clientv1.RenewCertRequest{
		AccessToken: "irrelevant-fake-token",
		DeviceId:    deviceID,
		CsrPem:      csrPEM,
	})
	if err != nil {
		t.Fatalf("RenewCert: %v", err)
	}
	if resp.GetCertificatePem() == "" {
		t.Fatal("expected a certificate_pem in the response")
	}
	if resp.GetCertExpiresAt() == 0 {
		t.Fatal("expected a non-zero cert_expires_at")
	}

	var newSerial, storedFingerprint string
	if err := pool.QueryRow(ctx,
		`SELECT cert_serial, public_key_fingerprint FROM client_devices WHERE id = $1`, deviceID,
	).Scan(&newSerial, &storedFingerprint); err != nil {
		t.Fatalf("query device: %v", err)
	}
	if newSerial == "AABBCCDDEEFF0011" {
		t.Fatal("cert_serial did not change after renewal")
	}
	if storedFingerprint != fp {
		t.Fatalf("public_key_fingerprint must not change on renewal: got %s want %s", storedFingerprint, fp)
	}

	assertAuditActionCount(t, ctx, pool, wsID, "device.cert.renewed", deviceID, 1)
}

// TestRenewCertDenials covers every denial path from Track3-Renew-Reenroll.md
// D-C/D-D: the wire response is identical (uniform PermissionDenied) across
// every authorization-level reason, while the server-side audit row records
// the real cause. Malformed-CSR stays a distinguishable InvalidArgument (an
// ordinary request-validation error, not an authorization decision — same
// class EnrollDevice already returns without any obscurity requirement).
func TestRenewCertDenials(t *testing.T) {
	pool, ctx, wsID, userID, pkiSvc := deviceGateTestHarness(t)
	claims := &auth.AccessTokenClaims{UserID: userID, TenantID: wsID, Email: "renew-deny@example.test"}
	svc := &Service{pool: pool, authSvc: &postureTestAuth{claims: claims}, pkiSvc: pkiSvc}

	csrPEM, pub := makeClientCSR(t)
	fp, err := publicKeyFingerprint(pub)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	t.Run("fingerprint mismatch", func(t *testing.T) {
		deviceID := insertDevTrustDevice(t, ctx, pool, wsID, userID, "mismatch", "")
		if _, err := pool.Exec(ctx,
			`UPDATE client_devices SET public_key_fingerprint = 'not-the-real-fingerprint' WHERE id = $1`,
			deviceID,
		); err != nil {
			t.Fatalf("pin fingerprint: %v", err)
		}
		resp, err := svc.RenewCert(ctx, &clientv1.RenewCertRequest{AccessToken: "x", DeviceId: deviceID, CsrPem: csrPEM})
		assertUniformDenial(t, resp, err)
		assertAuditDenialReason(t, ctx, pool, wsID, deviceID, "fingerprint_mismatch")
	})

	t.Run("fingerprint missing (legacy device, no TOFU)", func(t *testing.T) {
		deviceID := insertDevTrustDevice(t, ctx, pool, wsID, userID, "legacy", "")
		// public_key_fingerprint left NULL — simulates a device enrolled
		// before this migration. D-A: must re-enroll, never trust-on-first-use.
		resp, err := svc.RenewCert(ctx, &clientv1.RenewCertRequest{AccessToken: "x", DeviceId: deviceID, CsrPem: csrPEM})
		assertUniformDenial(t, resp, err)
		assertAuditDenialReason(t, ctx, pool, wsID, deviceID, "fingerprint_missing")
	})

	t.Run("revoked", func(t *testing.T) {
		deviceID := insertDevTrustDevice(t, ctx, pool, wsID, userID, "revoked", "")
		if _, err := pool.Exec(ctx,
			`UPDATE client_devices SET public_key_fingerprint = $1, revoked_at = NOW() WHERE id = $2`,
			fp, deviceID,
		); err != nil {
			t.Fatalf("setup: %v", err)
		}
		resp, err := svc.RenewCert(ctx, &clientv1.RenewCertRequest{AccessToken: "x", DeviceId: deviceID, CsrPem: csrPEM})
		assertUniformDenial(t, resp, err)
		assertAuditDenialReason(t, ctx, pool, wsID, deviceID, "revoked")
	})

	t.Run("re_enroll_required", func(t *testing.T) {
		deviceID := insertDevTrustDevice(t, ctx, pool, wsID, userID, "reenroll", "")
		if _, err := pool.Exec(ctx,
			`UPDATE client_devices SET public_key_fingerprint = $1, status = 're_enroll_required' WHERE id = $2`,
			fp, deviceID,
		); err != nil {
			t.Fatalf("setup: %v", err)
		}
		resp, err := svc.RenewCert(ctx, &clientv1.RenewCertRequest{AccessToken: "x", DeviceId: deviceID, CsrPem: csrPEM})
		assertUniformDenial(t, resp, err)
		assertAuditDenialReason(t, ctx, pool, wsID, deviceID, "re_enroll_required")
	})

	t.Run("malformed csr", func(t *testing.T) {
		deviceID := insertDevTrustDevice(t, ctx, pool, wsID, userID, "badcsr", "")
		if _, err := pool.Exec(ctx,
			`UPDATE client_devices SET public_key_fingerprint = $1 WHERE id = $2`,
			fp, deviceID,
		); err != nil {
			t.Fatalf("setup: %v", err)
		}
		_, err := svc.RenewCert(ctx, &clientv1.RenewCertRequest{AccessToken: "x", DeviceId: deviceID, CsrPem: "not a csr"})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument for a malformed CSR, got %v", err)
		}
		assertAuditDenialReason(t, ctx, pool, wsID, deviceID, "invalid_csr")
	})

	t.Run("invalid csr signature", func(t *testing.T) {
		deviceID := insertDevTrustDevice(t, ctx, pool, wsID, userID, "badsig", "")
		if _, err := pool.Exec(ctx,
			`UPDATE client_devices SET public_key_fingerprint = $1 WHERE id = $2`,
			fp, deviceID,
		); err != nil {
			t.Fatalf("setup: %v", err)
		}
		// Flip the DER's last byte — inside the signature BIT STRING's raw
		// content, not any ASN.1 tag/length byte, so the CSR still PARSES
		// (reaching CheckSignature) but the signature no longer verifies.
		// Tampering the PEM text directly risks corrupting base64 in a way
		// that breaks parsing itself, which would misattribute this to
		// invalid_csr instead of invalid_csr_signature.
		block, _ := pem.Decode([]byte(csrPEM))
		tamperedDER := append([]byte(nil), block.Bytes...)
		tamperedDER[len(tamperedDER)-1] ^= 0xFF
		tamperedPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: tamperedDER}))

		_, err := svc.RenewCert(ctx, &clientv1.RenewCertRequest{AccessToken: "x", DeviceId: deviceID, CsrPem: tamperedPEM})
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument for a tampered CSR, got %v", err)
		}
		assertAuditDenialReason(t, ctx, pool, wsID, deviceID, "invalid_csr_signature")
	})
}

func assertUniformDenial(t *testing.T, resp *clientv1.RenewCertResponse, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	if resp != nil {
		t.Fatal("expected a nil response on denial")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", status.Code(err))
	}
	if got := status.Convert(err).Message(); got != renewalDeniedMsg {
		t.Fatalf("denial message = %q, want the uniform %q (D-D)", got, renewalDeniedMsg)
	}
}

func assertAuditActionCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wsID, action, targetID string, want int) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
		wsID, action, targetID,
	).Scan(&count); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if count != want {
		t.Fatalf("expected %d audit row(s) for action=%s target=%s, got %d", want, action, targetID, count)
	}
}

func assertAuditDenialReason(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wsID, targetID, reason string) {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs
		  WHERE tenant_id = $1 AND action = 'device.cert.renew_denied' AND target_id = $2
		    AND details::jsonb ->> 'reason' = $3`,
		wsID, targetID, reason,
	).Scan(&count); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 device.cert.renew_denied row with reason=%q for target=%s, got %d", reason, targetID, count)
	}
}
