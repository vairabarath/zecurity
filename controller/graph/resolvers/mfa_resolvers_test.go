package resolvers

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/vektah/gqlparser/v2/gqlerror"

	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/mfa"
	"github.com/yourorg/ztna/controller/internal/posture"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

type dummySealer struct{}

func (dummySealer) EncryptSecret([]byte, string) (string, string, error) { return "ct", "nonce", nil }
func (dummySealer) DecryptSecret(string, string, string) ([]byte, error) { return []byte("secret"), nil }

type dummyLimiter struct{}

func (dummyLimiter) Blocked(context.Context, string) (bool, time.Duration, error) {
	return false, 0, nil
}
func (dummyLimiter) RecordFailure(context.Context, string) (bool, error) { return false, nil }
func (dummyLimiter) Reset(context.Context, string) error                { return nil }

type fakeAuthServiceForResolver struct {
	auth.Service
	clearedUser string
}

func (f *fakeAuthServiceForResolver) ClearUserSessionMFA(_ context.Context, userID string) error {
	f.clearedUser = userID
	return nil
}

func setupMFAResolverFixture(t *testing.T) (*policyResolverFixture, *fakeAuthServiceForResolver, string, string) {
	t.Helper()
	f := newPolicyResolverFixture(t)
	f.mr.PostureStore = posture.NewStore(f.pool)
	f.mr.ConsoleMFA = mfa.NewConsolePolicy(f.pool)
	mfaStore := mfa.NewStore(f.pool)
	f.mr.MFA = mfa.NewService(mfaStore, dummySealer{}, dummyLimiter{})
	fakeAuth := &fakeAuthServiceForResolver{}
	f.mr.AuthService = fakeAuth

	adminID := createTestUser(t, f.ctx, f.pool, f.tenantID, "admin@mfa.test")
	otherID := createTestUser(t, f.ctx, f.pool, f.tenantID, "other@mfa.test")

	return f, fakeAuth, adminID, otherID
}

func TestResolvers_SetConsoleRequireMfa_LockoutRule(t *testing.T) {
	f, _, adminID, _ := setupMFAResolverFixture(t)

	// 1. Without MFA on caller session -> refused with MFA_REQUIRED
	ctxWithoutMFA := tenant.Set(f.ctx, tenant.TenantContext{
		TenantID: f.tenantID,
		UserID:   adminID,
		Email:    "admin@mfa.test",
		Role:     "ADMIN",
	})
	_, err := f.mr.SetConsoleRequireMfa(ctxWithoutMFA, true)
	if err == nil {
		t.Fatal("expected setConsoleRequireMfa(true) without caller MFA to fail")
	}
	var gerr *gqlerror.Error
	if !gqlerrorAs(err, &gerr) || gerr.Extensions["code"] != CodeMFARequired {
		t.Fatalf("expected MFA_REQUIRED error code, got: %v", err)
	}

	// 2. Disabling MFA is allowed even without caller MFA
	res, err := f.mr.SetConsoleRequireMfa(ctxWithoutMFA, false)
	if err != nil {
		t.Fatalf("expected setConsoleRequireMfa(false) to succeed: %v", err)
	}
	if res.RequireMfa {
		t.Errorf("expected requireMfa to be false")
	}

	// 3. With caller MFA verified -> enabling succeeds and audits
	ctxWithMFA := tenant.Set(f.ctx, tenant.TenantContext{
		TenantID: f.tenantID,
		UserID:   adminID,
		Email:    "admin@mfa.test",
		Role:     "ADMIN",
		MFAAt:    time.Now(),
	})
	res, err = f.mr.SetConsoleRequireMfa(ctxWithMFA, true)
	if err != nil {
		t.Fatalf("expected setConsoleRequireMfa(true) with MFA to succeed: %v", err)
	}
	if !res.RequireMfa {
		t.Errorf("expected requireMfa to be true")
	}

	// Verify audit row
	var action, targetID string
	err = f.pool.QueryRow(f.ctx,
		`SELECT action, target_id FROM audit_logs WHERE action = 'console_security.require_mfa' ORDER BY created_at DESC LIMIT 1`,
	).Scan(&action, &targetID)
	if err != nil {
		t.Fatalf("query audit row: %v", err)
	}
	if targetID != f.tenantID {
		t.Errorf("target_id = %s, want %s", targetID, f.tenantID)
	}
}

func TestResolvers_ResetUserMfa(t *testing.T) {
	f, fakeAuth, adminID, otherID := setupMFAResolverFixture(t)

	ctx := tenant.Set(f.ctx, tenant.TenantContext{
		TenantID: f.tenantID,
		UserID:   adminID,
		Email:    "admin@mfa.test",
		Role:     "ADMIN",
		MFAAt:    time.Now(),
	})

	// 1. Refuse resetting own MFA
	if _, err := f.mr.ResetUserMfa(ctx, adminID); err == nil {
		t.Fatal("expected resetting own MFA to fail")
	}

	// 2. Refuse unknown user
	if _, err := f.mr.ResetUserMfa(ctx, uuid.NewString()); err == nil {
		t.Fatal("expected resetting unknown user to fail")
	}

	// 3. Set up an authenticator and device for otherID
	_, err := f.pool.Exec(f.ctx,
		`INSERT INTO user_totp (user_id, workspace_id, secret_ciphertext, secret_nonce, enabled_at)
		 VALUES ($1, $2, 'ct', 'nonce', NOW())`, otherID, f.tenantID)
	if err != nil {
		t.Fatalf("insert user_totp: %v", err)
	}

	deviceID := uuid.New()
	_, err = f.pool.Exec(f.ctx,
		`INSERT INTO client_devices (id, user_id, workspace_id, name, os, public_key_fingerprint, session_id, session_mfa_at)
		 VALUES ($1, $2, $3, 'test-dev', 'linux', 'fp', 'sid-1', NOW())`,
		deviceID, otherID, f.tenantID)
	if err != nil {
		t.Fatalf("insert device: %v", err)
	}

	// Reset other user's MFA
	existed, err := f.mr.ResetUserMfa(ctx, otherID)
	if err != nil {
		t.Fatalf("resetUserMfa: %v", err)
	}
	if !existed {
		t.Errorf("expected existed=true")
	}
	if fakeAuth.clearedUser != otherID {
		t.Errorf("expected ClearUserSessionMFA called for %s, got %s", otherID, fakeAuth.clearedUser)
	}

	// Verify device session_mfa_at cleared
	var devMFA *time.Time
	err = f.pool.QueryRow(f.ctx, `SELECT session_mfa_at FROM client_devices WHERE id = $1`, deviceID).Scan(&devMFA)
	if err != nil {
		t.Fatal(err)
	}
	if devMFA != nil {
		t.Errorf("device session_mfa_at = %v, want nil", devMFA)
	}

	// Verify audit log
	var targetID string
	err = f.pool.QueryRow(f.ctx,
		`SELECT target_id FROM audit_logs WHERE action = $1 AND target_id = $2`,
		mfa.ActionReset, otherID,
	).Scan(&targetID)
	if err != nil {
		t.Fatalf("query reset audit log: %v", err)
	}
}

func TestResolvers_SetResourcePolicyAuthentication(t *testing.T) {
	f, _, adminID, _ := setupMFAResolverFixture(t)

	ctx := tenant.Set(f.ctx, tenant.TenantContext{
		TenantID: f.tenantID,
		UserID:   adminID,
		Email:    "admin@mfa.test",
		Role:     "ADMIN",
		MFAAt:    time.Now(),
	})

	wsUUID := uuid.MustParse(f.tenantID)
	p, err := f.mr.PostureStore.CreateResourcePolicy(f.ctx, wsUUID, "Test Policy", nil)
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}

	minutes := 1440
	res, err := f.mr.SetResourcePolicyAuthentication(ctx, p.ID.String(), &minutes, true)
	if err != nil {
		t.Fatalf("setResourcePolicyAuthentication: %v", err)
	}
	if res.AuthFrequencyMinutes == nil || *res.AuthFrequencyMinutes != 1440 {
		t.Errorf("freq = %v, want 1440", res.AuthFrequencyMinutes)
	}
	if !res.RequireMfa {
		t.Errorf("requireMfa = false, want true")
	}

	// Audit logged
	var targetID string
	err = f.pool.QueryRow(f.ctx,
		`SELECT target_id FROM audit_logs WHERE action = 'resource_policy.authentication' AND target_id = $1`,
		p.ID.String(),
	).Scan(&targetID)
	if err != nil {
		t.Fatalf("query policy auth audit: %v", err)
	}
}

func gqlerrorAs(err error, target **gqlerror.Error) bool {
	if err == nil {
		return false
	}
	if g, ok := err.(*gqlerror.Error); ok {
		*target = g
		return true
	}
	return false
}
