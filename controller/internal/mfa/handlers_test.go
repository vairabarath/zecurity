package mfa

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yourorg/ztna/controller/internal/audit"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

type fakeSessionStamper struct {
	validToken string
	claims     *auth.AccessTokenClaims
	stampedTok string
	stampErr   error
}

func (f *fakeSessionStamper) VerifyAccessToken(token string) (*auth.AccessTokenClaims, error) {
	if token == "" || token != f.validToken {
		return nil, auth.ErrSessionNotFound
	}
	return f.claims, nil
}

func (f *fakeSessionStamper) StampSessionMFA(_ context.Context, _ *auth.AccessTokenClaims, _ time.Time) (string, int64, error) {
	if f.stampErr != nil {
		return "", 0, f.stampErr
	}
	return f.stampedTok, 900, nil
}

func newTestHandlers(t *testing.T, repo Repo) (*Handlers, *fakeSessionStamper, *[]audit.Entry, *time.Time, *ConsolePolicy) {
	t.Helper()
	var aud []audit.Entry
	auditFn := func(_ context.Context, e audit.Entry) {
		aud = append(aud, e)
	}
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	svc := NewService(repo, fakeSealer{}, &fakeLimiter{})
	svc.now = func() time.Time { return now }
	stamper := &fakeSessionStamper{
		validToken: "good-token",
		claims: &auth.AccessTokenClaims{
			UserID:   "u1",
			TenantID: "w1",
			Email:    "u1@example.com",
		},
		stampedTok: "stamped-token",
	}
	cp := NewConsolePolicy(nil)
	cp.cache["w1"] = consoleEntry{required: false, until: now.Add(time.Hour)}
	handlers := NewHandlers(svc, stamper, cp, auditFn)
	return handlers, stamper, &aud, &now, cp
}

func TestHandlers_Status(t *testing.T) {
	repo := &fakeRepo{}
	h, _, _, now, cp := newTestHandlers(t, repo)

	// Unauthorized without context
	req := httptest.NewRequest(http.MethodGet, "/auth/mfa/status", nil)
	rec := httptest.NewRecorder()
	h.Status(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status without ctx: got %d, want 401", rec.Code)
	}

	// Authorized with context, not enrolled
	ctx := tenant.Set(context.Background(), tenant.TenantContext{
		TenantID: "w1",
		UserID:   "u1",
		Email:    "u1@example.com",
	})
	req = httptest.NewRequest(http.MethodGet, "/auth/mfa/status", nil).WithContext(ctx)
	rec = httptest.NewRecorder()
	h.Status(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status with ctx: got %d, want 200", rec.Code)
	}
	var res map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res["enrolled"] != false {
		t.Errorf("enrolled = %v, want false", res["enrolled"])
	}
	if res["consoleRequireMfa"] != false {
		t.Errorf("consoleRequireMfa = %v, want false", res["consoleRequireMfa"])
	}
	if res["sessionMfaAt"] != nil {
		t.Errorf("sessionMfaAt = %v, want nil", res["sessionMfaAt"])
	}

	// Enrolled with MFAAt and consoleRequireMfa = true
	cp.cache["w1"] = consoleEntry{required: true, until: now.Add(time.Hour)}
	mfaAt := now.Truncate(time.Second)
	repo.rec = &Record{UserID: "u1", WorkspaceID: "w1", EnabledAt: &mfaAt}
	ctx = tenant.Set(context.Background(), tenant.TenantContext{
		TenantID: "w1",
		UserID:   "u1",
		Email:    "u1@example.com",
		MFAAt:    mfaAt,
	})
	req = httptest.NewRequest(http.MethodGet, "/auth/mfa/status", nil).WithContext(ctx)
	rec = httptest.NewRecorder()
	h.Status(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	res = nil
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res["enrolled"] != true {
		t.Errorf("enrolled = %v, want true", res["enrolled"])
	}
	if res["consoleRequireMfa"] != true {
		t.Errorf("consoleRequireMfa = %v, want true", res["consoleRequireMfa"])
	}
	if res["sessionMfaAt"] == nil {
		t.Errorf("sessionMfaAt = nil, want timestamp")
	}
}

func TestHandlers_Enroll(t *testing.T) {
	repo := &fakeRepo{}
	h, _, _, _, _ := newTestHandlers(t, repo)

	ctx := tenant.Set(context.Background(), tenant.TenantContext{
		TenantID: "w1",
		UserID:   "u1",
		Email:    "u1@example.com",
	})

	// First enrollment succeeds
	req := httptest.NewRequest(http.MethodPost, "/auth/mfa/enroll", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.Enroll(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll: got %d, want 200", rec.Code)
	}
	var res map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if res["secret"] == "" || res["uri"] == "" {
		t.Fatalf("missing secret or uri: %+v", res)
	}

	// Already enabled user gets 409
	now := time.Now()
	repo.rec.EnabledAt = &now
	req = httptest.NewRequest(http.MethodPost, "/auth/mfa/enroll", nil).WithContext(ctx)
	rec = httptest.NewRecorder()
	h.Enroll(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("enroll when already enabled: got %d, want 409", rec.Code)
	}
}

func TestHandlers_Verify(t *testing.T) {
	repo := &fakeRepo{}
	h, stamper, aud, now, _ := newTestHandlers(t, repo)

	ctx := tenant.Set(context.Background(), tenant.TenantContext{
		TenantID: "w1",
		UserID:   "u1",
		Email:    "u1@example.com",
	})

	// 1. Not enrolled
	body, _ := json.Marshal(map[string]string{"code": "123456"})
	req := httptest.NewRequest(http.MethodPost, "/auth/mfa/verify", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	h.Verify(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("verify not enrolled: got %d, want 409", rec.Code)
	}

	// Start enrollment so we have a valid secret
	enr, err := h.svc.StartEnrollment(ctx, "u1", "w1", "u1@example.com")
	if err != nil {
		t.Fatal(err)
	}

	// 2. Missing/bad bearer token
	req = httptest.NewRequest(http.MethodPost, "/auth/mfa/verify", bytes.NewReader(body)).WithContext(ctx)
	rec = httptest.NewRecorder()
	h.Verify(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("verify without token: got %d, want 401", rec.Code)
	}

	// 3. Invalid TOTP code
	body, _ = json.Marshal(map[string]string{"code": "000000"})
	req = httptest.NewRequest(http.MethodPost, "/auth/mfa/verify", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer good-token")
	rec = httptest.NewRecorder()
	h.Verify(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("verify invalid code: got %d, want 401", rec.Code)
	}

	// 4. Valid TOTP code verifies and enrolls
	code := codeFor(t, enr, *now)
	body, _ = json.Marshal(map[string]string{"code": code})
	req = httptest.NewRequest(http.MethodPost, "/auth/mfa/verify", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer good-token")
	rec = httptest.NewRecorder()
	h.Verify(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify valid code: got %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["accessToken"] != "stamped-token" {
		t.Errorf("token = %v, want stamped-token", out["accessToken"])
	}
	if out["enrolled"] != true {
		t.Errorf("enrolled = %v, want true (first code confirms enrollment)", out["enrolled"])
	}
	if len(*aud) == 0 || (*aud)[len(*aud)-1].Action != ActionEnrolled {
		t.Errorf("audit not recorded; got %+v", *aud)
	}

	// 5. Subsequent valid verification (already enrolled)
	code2 := codeFor(t, enr, now.Add(30*time.Second))
	body, _ = json.Marshal(map[string]string{"code": code2})
	req = httptest.NewRequest(http.MethodPost, "/auth/mfa/verify", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer good-token")
	rec = httptest.NewRecorder()
	h.Verify(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("verify second code: got %d, want 200", rec.Code)
	}
	if (*aud)[len(*aud)-1].Action != ActionVerified {
		t.Errorf("audit action = %v, want %s", (*aud)[len(*aud)-1].Action, ActionVerified)
	}

	// 6. Session stamp failure surfaces
	stamper.stampErr = auth.ErrSessionNotFound
	code3 := codeFor(t, enr, now.Add(60*time.Second))
	body, _ = json.Marshal(map[string]string{"code": code3})
	req = httptest.NewRequest(http.MethodPost, "/auth/mfa/verify", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer good-token")
	rec = httptest.NewRecorder()
	h.Verify(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("verify with expired session: got %d, want 401", rec.Code)
	}
}
