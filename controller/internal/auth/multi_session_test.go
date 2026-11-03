package auth

import (
	"context"
	"testing"
	"time"
)

// testSID is a well-formed session id for store-level tests.
const testSID = "AAAAAAAAAAAAAAAAAAAAAA"

// sidOf returns the session id of a "<sid>.<secret>" refresh token.
func sidOf(t *testing.T, refreshToken string) string {
	t.Helper()
	sid, ok := splitRefreshToken(refreshToken)
	if !ok {
		t.Fatalf("refresh token %q carries no session id", refreshToken)
	}
	return sid
}

// Regression (2026-10-22 live finding): the admin UI and the client daemon of
// the same user shared one refresh:<user> slot, so each login or rotation
// killed the other with "refresh token mismatch". Sessions are now per login.
func TestRefreshSessions_IndependentPerLogin(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	ctx := context.Background()

	accessA, _, refreshA, err := svc.IssueSession(ctx, "user-1", "tenant-1", "admin", "u@example.com", 1)
	if err != nil {
		t.Fatalf("IssueSession A: %v", err)
	}
	accessB, _, refreshB, err := svc.IssueSession(ctx, "user-1", "tenant-1", "admin", "u@example.com", 1)
	if err != nil {
		t.Fatalf("IssueSession B: %v", err)
	}
	if sidOf(t, refreshA) == sidOf(t, refreshB) {
		t.Fatal("two logins share a session id")
	}

	// A login after A, then rotations interleaved in both orders.
	for i := range 3 {
		if accessA, refreshA, err = svc.RefreshSession(ctx, accessA, refreshA); err != nil {
			t.Fatalf("round %d: refresh A: %v", i, err)
		}
		if accessB, refreshB, err = svc.RefreshSession(ctx, accessB, refreshB); err != nil {
			t.Fatalf("round %d: refresh B: %v", i, err)
		}
	}
}

func TestRefreshSession_AccessTokenOfAnotherSessionRefused(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	ctx := context.Background()

	accessA, _, _, _ := svc.IssueSession(ctx, "user-1", "tenant-1", "admin", "u@example.com", 1)
	_, _, refreshB, _ := svc.IssueSession(ctx, "user-1", "tenant-1", "admin", "u@example.com", 1)

	_, _, err := svc.RefreshSession(ctx, accessA, refreshB)
	requireDead(t, err, "refresh token mismatch")
}

func TestRefreshSession_RotationKeepsSessionID(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	ctx := context.Background()

	access, _, refresh, _ := svc.IssueSession(ctx, "user-1", "tenant-1", "admin", "u@example.com", 1)
	sid := sidOf(t, refresh)
	newAccess, newRefresh, err := svc.RefreshSession(ctx, access, refresh)
	if err != nil {
		t.Fatalf("RefreshSession: %v", err)
	}
	if got := sidOf(t, newRefresh); got != sid {
		t.Fatalf("rotation changed sid %q → %q", sid, got)
	}
	claims, err := svc.verifyAccessToken(newAccess)
	if err != nil || claims.SessionID != sid {
		t.Fatalf("rotated access token sid = %q (err %v), want %q", claims.SessionID, err, sid)
	}
	// The old secret is spent.
	_, _, err = svc.RefreshSession(ctx, access, refresh)
	requireDead(t, err, "refresh token mismatch")
}

func TestEndSession_EndsOnlyItsOwnSession(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	ctx := context.Background()

	accessA, _, refreshA, _ := svc.IssueSession(ctx, "user-1", "tenant-1", "admin", "u@example.com", 1)
	accessB, _, refreshB, _ := svc.IssueSession(ctx, "user-1", "tenant-1", "admin", "u@example.com", 1)

	svc.EndSession(ctx, accessA)
	_, _, err := svc.RefreshSession(ctx, accessA, refreshA)
	requireDead(t, err, "refresh token expired")
	if _, _, err := svc.RefreshSession(ctx, accessB, refreshB); err != nil {
		t.Fatalf("logout of A ended B: %v", err)
	}
}

func TestInvalidateUserSessions_EndsEverySession(t *testing.T) {
	rc, mr := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	ctx := context.Background()

	accessA, _, refreshA, _ := svc.IssueSession(ctx, "user-1", "tenant-1", "admin", "u@example.com", 1)
	accessB, _, refreshB, _ := svc.IssueSession(ctx, "user-1", "tenant-1", "admin", "u@example.com", 1)
	if err := rc.SetRefreshSession(ctx, "user-1", "", RefreshSession{Token: "legacy"}, time.Hour); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	_, _, refreshOther, _ := svc.IssueSession(ctx, "user-2", "tenant-1", "admin", "v@example.com", 1)

	if err := svc.InvalidateUserSessions(ctx, "user-1"); err != nil {
		t.Fatalf("InvalidateUserSessions: %v", err)
	}
	_, _, err := svc.RefreshSession(ctx, accessA, refreshA)
	requireDead(t, err, "refresh token expired")
	_, _, err = svc.RefreshSession(ctx, accessB, refreshB)
	requireDead(t, err, "refresh token expired")
	for _, k := range []string{refreshKey("user-1", ""), refreshIndexKey("user-1")} {
		if mr.Exists(k) {
			t.Fatalf("%s survived InvalidateUserSessions", k)
		}
	}
	if _, found, _ := rc.GetRefreshSession(ctx, "user-2", sidOf(t, refreshOther)); !found {
		t.Fatal("another user's session was ended")
	}
}

// A session stored under the legacy refresh:<user> key (minted before
// per-login sessions) keeps working once and moves to a per-login key.
func TestRefreshSession_LegacySessionMigrates(t *testing.T) {
	rc, mr := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	ctx := context.Background()

	legacyAccess, _ := svc.issueAccessToken("user-1", "tenant-1", "admin", "u@example.com", 1)
	legacyRefresh := "bGVnYWN5LXJlZnJlc2gtdG9rZW4tdmFsdWUtMzJieXQ"
	if err := rc.SetRefreshSession(ctx, "user-1", "", RefreshSession{Token: legacyRefresh}, time.Hour); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	access, refresh, err := svc.RefreshSession(ctx, legacyAccess, legacyRefresh)
	if err != nil {
		t.Fatalf("legacy refresh: %v", err)
	}
	sid := sidOf(t, refresh)
	if mr.Exists(refreshKey("user-1", "")) {
		t.Fatal("legacy key survived migration")
	}
	if _, _, err := svc.RefreshSession(ctx, access, refresh); err != nil {
		t.Fatalf("refresh after migration (sid %s): %v", sid, err)
	}
}

func TestSplitRefreshToken(t *testing.T) {
	for _, tc := range []struct {
		token string
		ok    bool
	}{
		{testSID + ".secret", true},
		{"no-separator-legacy-token", false},
		{"short.secret", false},
		{"AAAAAAAAAAAAAAAAAAAA:A.secret", false}, // key-injection characters
		{"AAAAAAAAAAAAAAAAAAAAA*.secret", false},
		{".secret", false},
	} {
		if _, ok := splitRefreshToken(tc.token); ok != tc.ok {
			t.Errorf("splitRefreshToken(%q) ok=%v, want %v", tc.token, ok, tc.ok)
		}
	}
}
