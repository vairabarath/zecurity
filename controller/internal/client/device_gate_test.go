package client

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/pki"
)

// deviceGateTestHarness spins up a fresh test database (mirrors
// TestDeviceTrustReEnrollRequired's harness in device_trust_handler_test.go)
// and returns a pool with migrations applied plus one workspace/user ready
// to own devices.
func deviceGateTestHarness(t *testing.T) (pool *pgxpool.Pool, ctx context.Context, wsID, userID string, pkiSvc pki.Service) {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx = context.Background()
	dbName := devTrustTestDBName(t)
	adminPool := devTrustTestPool(t, ctx, adminDSN)
	t.Cleanup(adminPool.Close)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	})
	testDSN, err := devTrustTestDSN(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test database DSN: %v", err)
	}
	pool = devTrustTestPool(t, ctx, testDSN)
	t.Cleanup(pool.Close)
	if err := devTrustApplyMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	t.Setenv("PKI_MASTER_SECRET", "device-gate-test-secret")
	pkiSvc, err = pki.Init(ctx, pool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}
	wsID = insertDevTrustWorkspace(t, ctx, pool, pkiSvc, "gate")
	userID = insertDevTrustUser(t, ctx, pool, wsID, "gate")

	return pool, ctx, wsID, userID, pkiSvc
}

// TestDeviceGateDirectiveDerivation exercises every {status, revoked_at,
// cert_not_after} combination from Track2-Device-Trust-Directive.md D-C and
// Track3-Renew-Reenroll.md D-B: re_enroll_required takes priority over
// revoked (derived from revoked_at, never duplicated into status) takes
// priority over renew_soon (derived from cert_not_after vs renewalWindow,
// never a stored status value either), else none.
func TestDeviceGateDirectiveDerivation(t *testing.T) {
	pool, ctx, wsID, userID, _ := deviceGateTestHarness(t)
	claims := &auth.AccessTokenClaims{UserID: userID, TenantID: wsID}

	durPtr := func(d time.Duration) *time.Duration { return &d }

	cases := []struct {
		name          string
		status        string
		revoked       bool
		certNotAfter  *time.Duration // relative to now; nil = leave NULL
		wantDirective clientv1.DeviceDirective
		wantReason    bool
	}{
		{"active, no cert yet", "active", false, nil, clientv1.DeviceDirective_DIRECTIVE_NONE, false},
		{"active, cert far from expiry", "active", false, durPtr(6 * 24 * time.Hour), clientv1.DeviceDirective_DIRECTIVE_NONE, false},
		{"active, cert inside renewal window", "active", false, durPtr(2 * 24 * time.Hour), clientv1.DeviceDirective_DIRECTIVE_RENEW_SOON, true},
		{"active, cert already past due", "active", false, durPtr(-1 * time.Hour), clientv1.DeviceDirective_DIRECTIVE_RENEW_SOON, true},
		{"revoked, no cert timing pressure", "active", true, nil, clientv1.DeviceDirective_DIRECTIVE_REVOKED, true},
		{"revoked beats renew_soon", "active", true, durPtr(1 * time.Hour), clientv1.DeviceDirective_DIRECTIVE_REVOKED, true},
		{"re_enroll_required beats revoked", "re_enroll_required", true, nil, clientv1.DeviceDirective_DIRECTIVE_RE_ENROLL_REQUIRED, true},
		{"re_enroll_required beats renew_soon", "re_enroll_required", false, durPtr(1 * time.Hour), clientv1.DeviceDirective_DIRECTIVE_RE_ENROLL_REQUIRED, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deviceID := insertDevTrustDevice(t, ctx, pool, wsID, userID, "dev-"+tc.name, "")
			if _, err := pool.Exec(ctx,
				`UPDATE client_devices SET status = $1 WHERE id = $2`,
				tc.status, deviceID,
			); err != nil {
				t.Fatalf("set status: %v", err)
			}
			if tc.revoked {
				if _, err := pool.Exec(ctx,
					`UPDATE client_devices SET revoked_at = NOW() WHERE id = $1`,
					deviceID,
				); err != nil {
					t.Fatalf("set revoked_at: %v", err)
				}
			}
			if tc.certNotAfter != nil {
				if _, err := pool.Exec(ctx,
					`UPDATE client_devices SET cert_not_after = $1 WHERE id = $2`,
					time.Now().Add(*tc.certNotAfter), deviceID,
				); err != nil {
					t.Fatalf("set cert_not_after: %v", err)
				}
			}

			directive, reason, err := deviceGate(ctx, pool, deviceID, claims)
			if err != nil {
				t.Fatalf("deviceGate: %v", err)
			}
			if directive != tc.wantDirective {
				t.Fatalf("directive = %v, want %v", directive, tc.wantDirective)
			}
			if tc.wantReason && reason == "" {
				t.Fatalf("expected a non-empty reason for directive %v", directive)
			}
			if !tc.wantReason && reason != "" {
				t.Fatalf("expected an empty reason for DIRECTIVE_NONE, got %q", reason)
			}
		})
	}
}

// TestDeviceGateNotFoundOrWrongWorkspace ensures deviceGate errors — so the
// RPC handlers keep returning PermissionDenied exactly as before — when the
// device doesn't exist or belongs to a different user/workspace.
func TestDeviceGateNotFoundOrWrongWorkspace(t *testing.T) {
	pool, ctx, wsID, userID, _ := deviceGateTestHarness(t)
	deviceID := insertDevTrustDevice(t, ctx, pool, wsID, userID, "owned", "")

	otherUserID := insertDevTrustUser(t, ctx, pool, wsID, "other")
	if _, _, err := deviceGate(ctx, pool, deviceID, &auth.AccessTokenClaims{
		UserID: otherUserID, TenantID: wsID,
	}); err == nil {
		t.Fatal("expected error for a device belonging to a different user")
	}

	const nonexistent = "00000000-0000-0000-0000-000000000000"
	if _, _, err := deviceGate(ctx, pool, nonexistent, &auth.AccessTokenClaims{
		UserID: userID, TenantID: wsID,
	}); err == nil {
		t.Fatal("expected error for a nonexistent device")
	}
}

// TestDeviceGateLastSeenThrottle covers D-E: two rapid calls must not
// double-stamp last_seen_at within the 5-minute throttle window.
func TestDeviceGateLastSeenThrottle(t *testing.T) {
	pool, ctx, wsID, userID, _ := deviceGateTestHarness(t)
	claims := &auth.AccessTokenClaims{UserID: userID, TenantID: wsID}
	deviceID := insertDevTrustDevice(t, ctx, pool, wsID, userID, "throttle", "")

	if _, _, err := deviceGate(ctx, pool, deviceID, claims); err != nil {
		t.Fatalf("first deviceGate: %v", err)
	}
	firstSeen := queryLastSeenAt(t, ctx, pool, deviceID)
	if firstSeen.IsZero() {
		t.Fatal("last_seen_at not stamped on first call")
	}

	if _, _, err := deviceGate(ctx, pool, deviceID, claims); err != nil {
		t.Fatalf("second deviceGate: %v", err)
	}
	secondSeen := queryLastSeenAt(t, ctx, pool, deviceID)
	if !secondSeen.Equal(firstSeen) {
		t.Fatalf("last_seen_at advanced on rapid second call: first=%v second=%v", firstSeen, secondSeen)
	}
}

func queryLastSeenAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, deviceID string) time.Time {
	t.Helper()
	var seen time.Time
	if err := pool.QueryRow(ctx,
		`SELECT last_seen_at FROM client_devices WHERE id = $1`, deviceID,
	).Scan(&seen); err != nil {
		t.Fatalf("query last_seen_at: %v", err)
	}
	return seen
}
