package provider_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// Sprint 21 C-a: GET /provider/me includes last_login_at — the timestamp of the
// CURRENT successful sign-in (shown as "Signed in at" in the provider console).
// Reuses newE2EServer, login, changePassword and call from e2e_test.go.

type meBody struct {
	UserID      string     `json:"user_id"`
	Email       string     `json:"email"`
	Role        string     `json:"role"`
	LastLoginAt *time.Time `json:"last_login_at"`
}

func getMe(t *testing.T, code int, body string) meBody {
	t.Helper()
	if code != http.StatusOK {
		t.Fatalf("/provider/me: %d %s", code, body)
	}
	var m meBody
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode /provider/me: %v (%s)", err, body)
	}
	return m
}

func TestMe_IncludesCurrentSignInTime(t *testing.T) {
	srv := newE2EServer(t)
	before := time.Now().Add(-time.Second)
	tok := changePassword(t, srv, login(t, srv, e2eTempPass).Token, e2eTempPass, e2eFinalPass).Token

	code, body := call(t, srv, http.MethodGet, "/provider/me", tok, nil)
	m := getMe(t, code, body)
	if m.UserID == "" || m.Email != e2eEmail || m.Role != "super-admin" {
		t.Fatalf("identity fields: %+v", m)
	}
	if m.LastLoginAt == nil || m.LastLoginAt.Before(before) || m.LastLoginAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("last_login_at = %v, want the current sign-in time (after %v)", m.LastLoginAt, before)
	}

	// A new sign-in moves it forward: it is the CURRENT sign-in, not a previous one.
	first := *m.LastLoginAt
	time.Sleep(20 * time.Millisecond)
	again := login(t, srv, e2eFinalPass).Token
	code, body = call(t, srv, http.MethodGet, "/provider/me", again, nil)
	if m2 := getMe(t, code, body); m2.LastLoginAt == nil || !m2.LastLoginAt.After(first) {
		t.Fatalf("after a new sign-in last_login_at = %v, want later than %v", m2.LastLoginAt, first)
	}
}

// The response carries exactly the four documented fields — /me is not a
// broader read API and never exposes credential or session material.
func TestMe_ResponseShape(t *testing.T) {
	srv := newE2EServer(t)
	tok := changePassword(t, srv, login(t, srv, e2eTempPass).Token, e2eTempPass, e2eFinalPass).Token

	code, body := call(t, srv, http.MethodGet, "/provider/me", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("/provider/me: %d %s", code, body)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"user_id": true, "email": true, "role": true, "last_login_at": true}
	if len(raw) != len(want) {
		t.Fatalf("unexpected /provider/me fields: %v", raw)
	}
	for k := range raw {
		if !want[k] {
			t.Fatalf("unexpected /provider/me field %q in %s", k, body)
		}
	}
}
