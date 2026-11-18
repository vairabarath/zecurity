package client

import (
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/tenantstate"
)

// Sprint 23 S1 (D-45): no client device certificate (enrollment or renewal)
// while the tenant is suspended by the provider, both at the authorization
// check and inside the final conditional write (a suspension that lands while
// a certificate is being issued means none is recorded).
func TestDeviceIssuance_RefusedWhileTenantSuspended(t *testing.T) {
	pool, ctx, wsID, userID, _ := deviceGateTestHarness(t)
	claims := &auth.AccessTokenClaims{UserID: userID, TenantID: wsID}

	if err := authorizeDeviceIssuance(ctx, pool, claims); err != nil {
		t.Fatalf("active tenant: %v", err)
	}
	deviceID, err := insertClientDevice(ctx, pool, userID, wsID, "laptop", "linux",
		deviceSession{id: "sid-1", deadlines: auth.SessionDeadlines{IdleUntil: time.Now().Add(time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `UPDATE workspaces SET suspended_at = NOW(), suspended_by = 'op@x', suspension_reason = 'test' WHERE id = $1`, wsID); err != nil {
		t.Fatal(err)
	}
	if err := authorizeDeviceIssuance(ctx, pool, claims); !errors.Is(err, errIssuanceNotAuthorized) {
		t.Fatalf("suspended tenant: %v, want errIssuanceNotAuthorized", err)
	}
	if n, err := updateClientDeviceCert(ctx, pool, deviceID, "serial-1", time.Now().Add(time.Hour), "spiffe://x/client/1", "fp"); err != nil || n != 0 {
		t.Fatalf("enroll write while suspended: n=%d err=%v, want 0 rows", n, err)
	}
	if n, err := updateClientDeviceCertOnRenewal(ctx, pool, deviceID, "serial-2", time.Now().Add(time.Hour)); err != nil || n != 0 {
		t.Fatalf("renewal write while suspended: n=%d err=%v, want 0 rows", n, err)
	}

	if _, err := pool.Exec(ctx, `UPDATE workspaces SET suspended_at = NULL, suspended_by = NULL, suspension_reason = NULL WHERE id = $1`, wsID); err != nil {
		t.Fatal(err)
	}
	if err := authorizeDeviceIssuance(ctx, pool, claims); err != nil {
		t.Fatalf("after resume: %v", err)
	}
	if n, err := updateClientDeviceCert(ctx, pool, deviceID, "serial-3", time.Now().Add(time.Hour), "spiffe://x/client/1", "fp"); err != nil || n != 1 {
		t.Fatalf("enroll write after resume: n=%d err=%v, want 1 row", n, err)
	}
}

// Sprint 23 S1 (D-45): no new client session while the tenant is suspended.
// TokenExchange refuses after the PKCE check and before anything is written
// (no user, no invitation acceptance, no session).
func TestTokenExchange_RefusedWhileTenantSuspended(t *testing.T) {
	pool, ctx, wsID, _, _ := deviceGateTestHarness(t)
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET suspended_at = NOW(), suspended_by = 'op@x', suspension_reason = 'test' WHERE id = $1`, wsID); err != nil {
		t.Fatal(err)
	}
	var usersBefore int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&usersBefore); err != nil {
		t.Fatal(err)
	}
	verifier := "suspension-test-verifier-0123456789abcdef"
	sid, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}
	putSession(sid, &authSession{
		WorkspaceID:       wsID,
		Email:             "new@example.test",
		Provider:          "google",
		Subject:           "sub-new",
		CliCodeChallenge:  sha256b64url([]byte(verifier)),
		CtrlCode:          "ctrl-1",
		CtrlCodeExpiresAt: time.Now().Add(time.Minute),
		ExpiresAt:         time.Now().Add(10 * time.Minute),
	})
	s := &Service{pool: pool}
	_, err = s.TokenExchange(ctx, &clientv1.TokenExchangeRequest{SessionId: sid, CtrlCode: "ctrl-1", CodeVerifier: verifier})
	if status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), tenantstate.Message) {
		t.Fatalf("TokenExchange while suspended: %v, want PermissionDenied %q", err, tenantstate.Message)
	}
	var usersAfter int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&usersAfter); err != nil {
		t.Fatal(err)
	}
	if usersAfter != usersBefore {
		t.Fatalf("users %d → %d: a refused sign-in wrote a user", usersBefore, usersAfter)
	}
}
