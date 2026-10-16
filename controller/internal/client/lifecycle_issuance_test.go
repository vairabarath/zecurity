package client

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/policy"
)

// Certificate audit bug 4: EnrollDevice and RenewCert trusted the access
// token alone. A user suspended (or locked, deleted, or with a non-active
// membership) still held a valid token for up to its 15-minute lifetime, and
// could turn it into a new or renewed 7-day device certificate. Both now read
// the user's current status, session generation and membership from the
// database first, as /auth/refresh does, and issue nothing on failure.

func issuanceService(pool *pgxpool.Pool, svc *Service, claims *auth.AccessTokenClaims) *Service {
	svc.authSvc = &postureTestAuth{claims: claims}
	svc.policyNotifier = policy.NewNotifier(policy.NewSnapshotCache())
	return svc
}

func execOrFail(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func deviceCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM client_devices WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count devices: %v", err)
	}
	return n
}

// lifecycleCase puts the (already active) user into some state.
type lifecycleCase struct {
	name   string
	setup  func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wsID, userID string)
	claims func(c *auth.AccessTokenClaims)
}

func addMembership(status string) func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wsID, userID string) {
	return func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wsID, userID string) {
		execOrFail(t, ctx, pool,
			`INSERT INTO workspace_members (workspace_id, user_id, email, status)
			 SELECT $1, id, email, $3 FROM users WHERE id = $2`, wsID, userID, status)
	}
}

func setUserStatus(status string) func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wsID, userID string) {
	return func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wsID, userID string) {
		execOrFail(t, ctx, pool, `UPDATE users SET status = $2 WHERE id = $1`, userID, status)
	}
}

func deniedCases() []lifecycleCase {
	none := func(*auth.AccessTokenClaims) {}
	return []lifecycleCase{
		{"suspended user", setUserStatus("suspended"), none},
		{"locked user", setUserStatus("locked"), none},
		{"deleted user", setUserStatus("deleted"), none},
		{"invited membership", addMembership("invited"), none},
		{"suspended membership", addMembership("suspended"), none},
		{"session generation bumped after the token was minted", func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, _, userID string) {
			execOrFail(t, ctx, pool, `UPDATE users SET identity_generation = identity_generation + 1 WHERE id = $1`, userID)
		}, none},
		{"token for another workspace", nil, func(c *auth.AccessTokenClaims) { c.TenantID = uuid.NewString() }},
		{"token for a user that does not exist", nil, func(c *auth.AccessTokenClaims) { c.UserID = uuid.NewString() }},
	}
}

func enrollRequest(t *testing.T) *clientv1.EnrollDeviceRequest {
	csrPEM, _ := makeClientCSR(t)
	return &clientv1.EnrollDeviceRequest{AccessToken: "still-valid-token", CsrPem: csrPEM, DeviceName: "laptop", Os: "linux"}
}

func TestEnrollDevice_ActiveUsersStillEnroll(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wsID, userID string)
	}{
		{"SCIM / first-login user without a membership row", func(*testing.T, context.Context, *pgxpool.Pool, string, string) {}},
		{"user with an active membership", addMembership("active")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, ctx, wsID, userID, pkiSvc := deviceGateTestHarness(t)
			tc.setup(t, ctx, pool, wsID, userID)
			claims := &auth.AccessTokenClaims{UserID: userID, TenantID: wsID, Email: "enroll@example.test", Generation: 1}
			svc := issuanceService(pool, &Service{pool: pool, pkiSvc: pkiSvc}, claims)
			resp, err := svc.EnrollDevice(ctx, enrollRequest(t))
			if err != nil {
				t.Fatalf("EnrollDevice: %v", err)
			}
			if resp.GetCertificatePem() == "" || deviceCount(t, ctx, pool, userID) != 1 {
				t.Fatal("an active user must get a device certificate")
			}
		})
	}
}

func TestEnrollDevice_RejectedForInactiveUserDespiteValidToken(t *testing.T) {
	for _, tc := range deniedCases() {
		t.Run(tc.name, func(t *testing.T) {
			pool, ctx, wsID, userID, pkiSvc := deviceGateTestHarness(t)
			if tc.setup != nil {
				tc.setup(t, ctx, pool, wsID, userID)
			}
			claims := &auth.AccessTokenClaims{UserID: userID, TenantID: wsID, Email: "enroll@example.test", Generation: 1}
			tc.claims(claims)
			svc := issuanceService(pool, &Service{pool: pool, pkiSvc: pkiSvc}, claims)

			resp, err := svc.EnrollDevice(ctx, enrollRequest(t))
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("EnrollDevice: want PermissionDenied, got resp=%v err=%v", resp != nil, err)
			}
			if resp != nil {
				t.Fatal("a certificate was returned")
			}
			if n := deviceCount(t, ctx, pool, userID); n != 0 {
				t.Fatalf("a refused enrollment left %d device row(s)", n)
			}
		})
	}
}

// pinnedDevice inserts a device with a pinned key; returns its id, a CSR for
// that key, and the current serial.
func pinnedDevice(t *testing.T, ctx context.Context, pool *pgxpool.Pool, wsID, userID string) (deviceID, csrPEM, serial string) {
	t.Helper()
	serial = "AABBCCDDEEFF0022"
	deviceID = insertDevTrustDevice(t, ctx, pool, wsID, userID, "renew-lifecycle", serial)
	csrPEM, pub := makeClientCSR(t)
	fp, err := publicKeyFingerprint(pub)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	execOrFail(t, ctx, pool, `UPDATE client_devices SET public_key_fingerprint = $1 WHERE id = $2`, fp, deviceID)
	return deviceID, csrPEM, serial
}

func currentSerial(t *testing.T, ctx context.Context, pool *pgxpool.Pool, deviceID string) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(ctx, `SELECT cert_serial FROM client_devices WHERE id = $1`, deviceID).Scan(&s); err != nil {
		t.Fatalf("serial: %v", err)
	}
	return s
}

func TestRenewCert_ActiveUsersStillRenew(t *testing.T) {
	pool, ctx, wsID, userID, pkiSvc := deviceGateTestHarness(t)
	addMembership("active")(t, ctx, pool, wsID, userID)
	deviceID, csrPEM, serial := pinnedDevice(t, ctx, pool, wsID, userID)
	claims := &auth.AccessTokenClaims{UserID: userID, TenantID: wsID, Email: "renew@example.test", Generation: 1}
	svc := issuanceService(pool, &Service{pool: pool, pkiSvc: pkiSvc}, claims)
	resp, err := svc.RenewCert(ctx, &clientv1.RenewCertRequest{AccessToken: "t", DeviceId: deviceID, CsrPem: csrPEM})
	if err != nil || resp.GetCertificatePem() == "" {
		t.Fatalf("RenewCert for an active member: %v", err)
	}
	if currentSerial(t, ctx, pool, deviceID) == serial {
		t.Fatal("certificate not renewed")
	}
}

func TestRenewCert_RejectedForInactiveUserDespiteValidToken(t *testing.T) {
	for _, tc := range deniedCases() {
		t.Run(tc.name, func(t *testing.T) {
			pool, ctx, wsID, userID, pkiSvc := deviceGateTestHarness(t)
			deviceID, csrPEM, serial := pinnedDevice(t, ctx, pool, wsID, userID)
			if tc.setup != nil {
				tc.setup(t, ctx, pool, wsID, userID)
			}
			claims := &auth.AccessTokenClaims{UserID: userID, TenantID: wsID, Email: "renew@example.test", Generation: 1}
			tc.claims(claims)
			svc := issuanceService(pool, &Service{pool: pool, pkiSvc: pkiSvc}, claims)

			resp, err := svc.RenewCert(ctx, &clientv1.RenewCertRequest{AccessToken: "t", DeviceId: deviceID, CsrPem: csrPEM})
			if status.Code(err) != codes.PermissionDenied || resp != nil {
				t.Fatalf("RenewCert: want PermissionDenied and no certificate, got resp=%v err=%v", resp != nil, err)
			}
			if got := currentSerial(t, ctx, pool, deviceID); got != serial {
				t.Fatalf("a refused renewal changed the device's certificate (%s -> %s)", serial, got)
			}
		})
	}
}

// SCIM reactivation commits a device-trust re-enrollment event in its own
// transaction; its consumer now invalidates the workspace policy, so the
// reactivation's propagation is durable instead of a best-effort call after
// commit.
func TestDeviceTrustReEnrollHandler_InvalidatesThePolicy(t *testing.T) {
	pool, ctx, wsID, userID, _ := deviceGateTestHarness(t)
	notifier := policy.NewNotifier(policy.NewSnapshotCache())
	var pushed []string
	notifier.RegisterPushHook(func(ws string) { pushed = append(pushed, ws) })

	handler := NewDeviceTrustReEnrollHandler(pool, notifier)
	evt := newDevTrustEvent(identity.EventDeviceTrustReEnrollmentRequired, wsID, userID, uuid.NewString(), identity.DeviceTrustReason(""))
	for i := 0; i < 2; i++ { // duplicate delivery is harmless
		if err := handler.Handle(ctx, evt); err != nil {
			t.Fatalf("Handle: %v", err)
		}
	}
	if len(pushed) != 2 || pushed[0] != wsID {
		t.Fatalf("policy invalidated for %v, want %s on each delivery", pushed, wsID)
	}
}

// A lifecycle change that lands while an enrollment is in flight: the user
// passes the up-front check, then is suspended (or loses an active
// membership) before the certificate is recorded. The final conditional write
// refuses, no certificate is returned, and the half-made device row is
// removed.
func TestEnrollDevice_LifecycleChangeDuringEnrollmentReturnsNoCertificate(t *testing.T) {
	for name, body := range map[string]string{
		"user suspended":              `UPDATE users SET status = 'suspended' WHERE id = NEW.user_id`,
		"membership no longer active": `INSERT INTO workspace_members (workspace_id, user_id, email, status) SELECT NEW.workspace_id, id, email, 'suspended' FROM users WHERE id = NEW.user_id`,
	} {
		t.Run(name, func(t *testing.T) {
			pool, ctx, wsID, userID, pkiSvc := deviceGateTestHarness(t)
			execOrFail(t, ctx, pool, `CREATE FUNCTION change_on_device_insert() RETURNS trigger AS $$ BEGIN `+body+`; RETURN NEW; END; $$ LANGUAGE plpgsql`)
			execOrFail(t, ctx, pool, `CREATE TRIGGER change_on_device_insert AFTER INSERT ON client_devices FOR EACH ROW EXECUTE FUNCTION change_on_device_insert()`)
			claims := &auth.AccessTokenClaims{UserID: userID, TenantID: wsID, Email: "race@example.test", Generation: 1}
			svc := issuanceService(pool, &Service{pool: pool, pkiSvc: pkiSvc}, claims)

			resp, err := svc.EnrollDevice(ctx, enrollRequest(t))
			if status.Code(err) != codes.PermissionDenied || resp != nil {
				t.Fatalf("want PermissionDenied and no certificate, got resp=%v err=%v", resp != nil, err)
			}
			if n := deviceCount(t, ctx, pool, userID); n != 0 {
				t.Fatalf("the refused enrollment left %d device row(s)", n)
			}
		})
	}
}
