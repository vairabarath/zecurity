package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRefreshHandler_MissingCookie(t *testing.T) {
	rc, _ := newTestRedis(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	handler := svc.RefreshHandler()

	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	assertJSONError(t, w, "missing refresh token")
}

func TestRefreshHandler_MissingAuthHeader(t *testing.T) {
	rc, _ := newTestRedis(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	handler := svc.RefreshHandler()

	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: "refresh_token", Value: "some-token"})
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	assertJSONError(t, w, "missing authorization header")
}

func TestRefreshHandler_InvalidAccessToken(t *testing.T) {
	rc, _ := newTestRedis(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	handler := svc.RefreshHandler()

	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: "refresh_token", Value: "some-token"})
	req.Header.Set("Authorization", "Bearer garbage-token")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	assertJSONError(t, w, "invalid access token")
}

func TestRefreshHandler_RefreshTokenExpired(t *testing.T) {
	rc, _ := newTestRedis(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	handler := svc.RefreshHandler()

	// Issue a valid access token but do NOT store a refresh token in Redis.
	accessToken, _ := svc.issueAccessToken("user-1", "tenant-1", "admin")

	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: "refresh_token", Value: "old-token"})
	req.Header.Set("Authorization", "Bearer "+accessToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	assertJSONError(t, w, "refresh token expired")
}

func TestRefreshHandler_RefreshTokenMismatch(t *testing.T) {
	rc, _ := newTestRedis(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	handler := svc.RefreshHandler()

	// Issue access token and store a refresh token in Redis.
	accessToken, _ := svc.issueAccessToken("user-1", "tenant-1", "admin")
	svc.issueRefreshToken(context.Background(), "user-1")

	// Send a DIFFERENT refresh token in the cookie.
	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: "refresh_token", Value: "wrong-token"})
	req.Header.Set("Authorization", "Bearer "+accessToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
	assertJSONError(t, w, "refresh token mismatch")
}

func TestRefreshHandler_Success(t *testing.T) {
	rc, _ := newTestRedis(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	handler := svc.RefreshHandler()

	// Issue access token and refresh token.
	accessToken, _ := svc.issueAccessToken("user-1", "tenant-1", "admin")
	refreshToken, _ := svc.issueRefreshToken(context.Background(), "user-1")

	req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
	req.AddCookie(&http.Cookie{Name: "refresh_token", Value: refreshToken})
	req.Header.Set("Authorization", "Bearer "+accessToken)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Parse response body — should contain a new access_token.
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	newToken, ok := body["access_token"]
	if !ok || newToken == "" {
		t.Fatal("expected access_token in response body")
	}

	// The new token should be valid and contain the same claims.
	claims, err := svc.verifyAccessToken(newToken)
	if err != nil {
		t.Fatalf("new access token invalid: %v", err)
	}
	if claims.Subject != "user-1" {
		t.Fatalf("expected sub=user-1, got %s", claims.Subject)
	}
	if claims.TenantID != "tenant-1" {
		t.Fatalf("expected tenant_id=tenant-1, got %s", claims.TenantID)
	}
	if claims.Role != "admin" {
		t.Fatalf("expected role=admin, got %s", claims.Role)
	}
}
