package auth

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeycompat"
)

// newTestValkey spins up an in-memory Valkey (no Docker required).
// The *miniredis.Miniredis return allows callers to use FastForward for TTL tests.
func newTestValkey(t *testing.T) (*valkeyClient, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("start miniredis: %v", err)
	}
	t.Cleanup(mr.Close)

	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:  []string{mr.Addr()},
		DisableCache: true,
	})
	if err != nil {
		t.Fatalf("create valkey test client: %v", err)
	}
	t.Cleanup(client.Close)

	return &valkeyClient{rdb: valkeycompat.NewAdapter(client)}, mr
}

// testConfig returns a Config with sensible test defaults.
func testConfig() Config {
	return Config{
		JWTSecret:          "test-jwt-secret-32-bytes-long!!!",
		JWTIssuer:          "zecurity-controller",
		JWTAccessTTL:       "15m",
		JWTRefreshTTL:      "168h",
		GoogleClientID:     "test-client-id.apps.googleusercontent.com",
		GoogleClientSecret: "test-client-secret",
		RedirectURI:        "https://localhost/auth/callback",
		ValkeyURL:          "redis://localhost:6379",
		AllowedOrigin:      "https://localhost",
	}
}

// assertJSONError checks that the response body contains a JSON error with the expected message.
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
