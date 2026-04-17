package auth

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
)

// newTestRedis spins up an in-memory Redis (no Docker required).
// Called by: every test that needs a redisClient.
func newTestRedis(t *testing.T) (*redisClient, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	rc, err := newRedisClient("redis://" + mr.Addr())
	if err != nil {
		t.Fatalf("connect to miniredis: %v", err)
	}
	return rc, mr
}

// testConfig returns a Config with sensible test defaults.
// Called by: tests that need a serviceImpl.
func testConfig() Config {
	return Config{
		JWTSecret:          "test-jwt-secret-32-bytes-long!!!",
		JWTIssuer:          "zecurity-controller",
		JWTAccessTTL:       "15m",
		JWTRefreshTTL:      "168h",
		GoogleClientID:     "test-client-id.apps.googleusercontent.com",
		GoogleClientSecret: "test-client-secret",
		RedirectURI:        "https://localhost/auth/callback",
		RedisURL:           "redis://localhost:6379",
		AllowedOrigin:      "https://localhost",
	}
}

// assertJSONError checks that the response body contains a JSON error with the expected message.
// Called by: refresh and callback handler tests.
func assertJSONError(t *testing.T, w *httptest.ResponseRecorder, expectedMsg string) {
	t.Helper()
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if body["error"] != expectedMsg {
		t.Fatalf("expected error=%q, got %q", expectedMsg, body["error"])
	}
}
