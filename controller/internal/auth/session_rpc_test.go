package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// RefreshSession / EndSession are the cores behind /auth/refresh,
// /auth/logout and the ClientService RefreshSession / Logout RPCs the Rust
// client uses over its pinned TLS channel (certificate audit follow-up).

func requireDead(t *testing.T, err error, msg string) {
	t.Helper()
	var se *SessionError
	if !errors.As(err, &se) || !se.Dead() || se.Msg != msg {
		t.Fatalf("expected dead session %q, got %v", msg, err)
	}
}

func TestRefreshSession_RotatesAndRejectsReuse(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	ctx := context.Background()
	access, _ := svc.issueAccessToken("user-1", "tenant-1", "member", "user-1@example.com", 1)
	refresh, _ := svc.issueRefreshToken(ctx, "user-1")

	newAccess, newRefresh, err := svc.RefreshSession(ctx, access, refresh)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if newRefresh == "" || newRefresh == refresh {
		t.Fatal("refresh token not rotated")
	}
	claims, err := svc.verifyAccessToken(newAccess)
	if err != nil || claims.Subject != "user-1" || claims.TenantID != "tenant-1" {
		t.Fatalf("new access token: claims=%+v err=%v", claims, err)
	}

	// The rotated-away token is single-use: a replay is refused.
	_, _, err = svc.RefreshSession(ctx, access, refresh)
	requireDead(t, err, "refresh token mismatch")
	// The rotated token works.
	if _, _, err := svc.RefreshSession(ctx, newAccess, newRefresh); err != nil {
		t.Fatalf("refresh with the rotated token: %v", err)
	}
}

func TestRefreshSession_DeadSessions(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	ctx := context.Background()
	access, _ := svc.issueAccessToken("user-1", "tenant-1", "member", "user-1@example.com", 1)
	refresh, _ := svc.issueRefreshToken(ctx, "user-1")

	other := &serviceImpl{cfg: testConfig(), redisClient: rc}
	other.cfg.JWTSecret = "a-different-secret-32-bytes-long!"
	forged, _ := other.issueAccessToken("user-1", "tenant-1", "member", "user-1@example.com", 1)
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"sub": "user-1", "tenant_id": "tenant-1"}).
		SignedString(jwt.UnsafeAllowNoneSignatureType)
	stranger, _ := svc.issueAccessToken("user-2", "tenant-1", "member", "user-2@example.com", 1)

	for _, tc := range []struct {
		name, access, refresh, msg string
	}{
		{"no refresh token", access, "", "missing refresh token"},
		{"no access token", "", refresh, "missing authorization header"},
		{"wrong signing key", forged, refresh, "invalid access token"},
		{"alg none", none, refresh, "invalid access token"},
		{"no session for user", stranger, refresh, "refresh token expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := svc.RefreshSession(ctx, tc.access, tc.refresh)
			requireDead(t, err, tc.msg)
		})
	}
	// None of the refused attempts consumed the genuine session.
	if _, _, err := svc.RefreshSession(ctx, access, refresh); err != nil {
		t.Fatalf("genuine refresh after refused attempts: %v", err)
	}
}

func TestEndSession(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	ctx := context.Background()
	access, _, refresh, _ := svc.IssueSession(ctx, "user-1", "tenant-1", "member", "user-1@example.com", 1)
	sid := sidOf(t, refresh)

	other := &serviceImpl{cfg: testConfig(), redisClient: rc}
	other.cfg.JWTSecret = "a-different-secret-32-bytes-long!"
	forged, _ := other.issueSessionAccessToken("user-1", "tenant-1", "member", "user-1@example.com", 1, sid)

	svc.EndSession(ctx, "")
	svc.EndSession(ctx, "not-a-jwt")
	svc.EndSession(ctx, forged)
	if _, found, _ := rc.GetRefreshSession(ctx, "user-1", sid); !found {
		t.Fatal("a token that does not verify ended the session")
	}

	svc.EndSession(ctx, access)
	if _, found, _ := rc.GetRefreshSession(ctx, "user-1", sid); found {
		t.Fatal("session survived EndSession")
	}
	_, _, err := svc.RefreshSession(ctx, access, refresh)
	requireDead(t, err, "refresh token expired")
	svc.EndSession(ctx, access) // idempotent
}

// The browser path still rotates via the cookie and never puts the refresh
// token in the JSON body.
func TestRefreshHandler_CookiePathKeepsTheRefreshTokenOutOfTheBody(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	access, _ := svc.issueAccessToken("user-1", "tenant-1", "admin", "user-1@example.com", 1)
	refresh, _ := svc.issueRefreshToken(context.Background(), "user-1")

	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: "refresh_token", Value: refresh})
	req.Header.Set("Authorization", "Bearer "+access)
	w := httptest.NewRecorder()
	svc.RefreshHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if _, leaked := body["refresh_token"]; leaked || len(body) != 1 || body["access_token"] == "" {
		t.Fatalf("unexpected body: %v", body)
	}
	var rotated string
	for _, c := range w.Result().Cookies() {
		if c.Name == "refresh_token" {
			rotated = c.Value
		}
	}
	if rotated == "" || rotated == refresh {
		t.Fatal("refresh cookie not rotated")
	}
}

// Certificate audit bug 4: device-certificate issuance compares the token's
// session generation with the current one, so VerifyAccessToken must expose it.
func TestVerifyAccessToken_ExposesTheSessionGeneration(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	token, _ := svc.issueAccessToken("user-1", "tenant-1", "member", "user-1@example.com", 7)
	claims, err := svc.VerifyAccessToken(token)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.Generation != 7 {
		t.Fatalf("Generation = %d, want 7", claims.Generation)
	}
}
