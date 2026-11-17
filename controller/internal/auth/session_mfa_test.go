package auth

import (
	"context"
	"errors"
	"testing"
	"time"
)

// PENDING-06 M-3: only a verified amr method that proves more than a
// password counts as MFA.
func TestMFAFromAMR(t *testing.T) {
	cases := []struct {
		amr  []string
		want bool
	}{
		{nil, false},
		{[]string{}, false},
		{[]string{"pwd"}, false},
		{[]string{"pwd", "kba"}, false},
		{[]string{"pwd", "mfa"}, true},
		{[]string{"pwd", "otp"}, true},
		{[]string{"hwk"}, true},
		{[]string{" MFA "}, true},
		{[]string{"mfa-ish", "notmfa"}, false},
	}
	for _, c := range cases {
		if got := MFAFromAMR(c.amr); got != c.want {
			t.Errorf("MFAFromAMR(%v) = %v, want %v", c.amr, got, c.want)
		}
	}
}

func mfaOf(t *testing.T, svc *serviceImpl, access string) time.Time {
	t.Helper()
	claims, err := svc.VerifyAccessToken(access)
	if err != nil {
		t.Fatalf("verify access token: %v", err)
	}
	return claims.MFAAt
}

// The session's MFA mark: absent by default, set by an IdP that proved MFA,
// set later by StampSessionMFA, kept across refresh rotation, per session.
func TestSessionMFA_MarkLifecycle(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	ctx := context.Background()

	// A plain sign-in carries no mark, before or after a refresh.
	access, _, refresh, err := svc.IssueSession(ctx, "user-1", "tenant-1", "admin", "u@example.com", 1)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}
	if !mfaOf(t, svc, access).IsZero() {
		t.Fatal("a plain sign-in carries an MFA mark")
	}
	if access, refresh, err = svc.RefreshSession(ctx, access, refresh); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !mfaOf(t, svc, access).IsZero() {
		t.Fatal("a refresh invented an MFA mark")
	}

	// Stamp it: the returned token carries the mark, and so does every
	// token minted by later rotations.
	claims, _ := svc.VerifyAccessToken(access)
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	stamped, expiresIn, err := svc.StampSessionMFA(ctx, claims, at)
	if err != nil || expiresIn <= 0 {
		t.Fatalf("StampSessionMFA: expiresIn=%d err=%v", expiresIn, err)
	}
	if got := mfaOf(t, svc, stamped); !got.Equal(at) {
		t.Fatalf("stamped token mfa_at = %v, want %v", got, at)
	}
	for i := range 3 {
		if stamped, refresh, err = svc.RefreshSession(ctx, stamped, refresh); err != nil {
			t.Fatalf("round %d: refresh: %v", i, err)
		}
		if got := mfaOf(t, svc, stamped); !got.Equal(at) {
			t.Fatalf("round %d: mfa_at = %v, want %v (the mark must survive rotation)", i, got, at)
		}
	}
	// SessionDeadlines reports it (a device enrolled now starts with it).
	sid := sidOf(t, refresh)
	d, err := svc.SessionDeadlines(ctx, "user-1", sid)
	if err != nil || !d.MFAAt.Equal(at) || d.AuthAt.IsZero() {
		t.Fatalf("SessionDeadlines: %+v err=%v", d, err)
	}

	// The user's other session is untouched.
	otherAccess, _, otherRefresh, _ := svc.IssueSession(ctx, "user-1", "tenant-1", "admin", "u@example.com", 1)
	if otherAccess, _, err = svc.RefreshSession(ctx, otherAccess, otherRefresh); err != nil {
		t.Fatalf("refresh other: %v", err)
	}
	if !mfaOf(t, svc, otherAccess).IsZero() {
		t.Fatal("stamping one session marked another")
	}

	// Reset (M-8) clears the mark on every session; the next refresh drops it.
	if err := svc.ClearUserSessionMFA(ctx, "user-1"); err != nil {
		t.Fatalf("ClearUserSessionMFA: %v", err)
	}
	if stamped, _, err = svc.RefreshSession(ctx, stamped, refresh); err != nil {
		t.Fatalf("refresh after clear: %v", err)
	}
	if !mfaOf(t, svc, stamped).IsZero() {
		t.Fatal("the mark survived ClearUserSessionMFA")
	}
}

// M-3: MFA proven by the IdP at sign-in marks the session from the start.
func TestSessionMFA_IdPProof(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)

	access, _, refresh, err := svc.IssueSessionWithAuth(ctx, "user-1", "tenant-1", "admin", "u@example.com", 1,
		SessionAuthFromAMR([]string{"pwd", "mfa"}, now.Unix(), now))
	if err != nil {
		t.Fatalf("IssueSessionWithAuth: %v", err)
	}
	if got := mfaOf(t, svc, access); !got.Equal(now) {
		t.Fatalf("mfa_at = %v, want %v", got, now)
	}
	if access, _, err = svc.RefreshSession(ctx, access, refresh); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := mfaOf(t, svc, access); !got.Equal(now) {
		t.Fatalf("after refresh mfa_at = %v, want %v", got, now)
	}
	if SignInMFALabel(SessionAuthFromAMR([]string{"pwd"}, now.Unix(), now)) != "none" || SignInMFALabel(SessionAuthFromAMR([]string{"otp"}, now.Unix(), now)) != "idp" {
		t.Fatal("sign-in audit label wrong")
	}
}

// A session that has ended can't be stamped, and ending a session removes
// its mark (a new session with the same user starts clean).
func TestSessionMFA_EndedSession(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	ctx := context.Background()

	access, _, refresh, _ := svc.IssueSession(ctx, "user-1", "tenant-1", "admin", "u@example.com", 1)
	claims, _ := svc.VerifyAccessToken(access)
	if _, _, err := svc.StampSessionMFA(ctx, claims, time.Now()); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	sid := sidOf(t, refresh)
	if err := rc.DeleteRefreshSession(ctx, "user-1", sid); err != nil {
		t.Fatalf("end session: %v", err)
	}
	if at, err := rc.GetSessionMFA(ctx, "user-1", sid); err != nil || at != 0 {
		t.Fatalf("mark after the session ended: %d err=%v, want 0", at, err)
	}
	if _, _, err := svc.StampSessionMFA(ctx, claims, time.Now()); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("stamp an ended session: %v, want ErrSessionNotFound", err)
	}
	// A token with no session can't be stamped either.
	if _, _, err := svc.StampSessionMFA(ctx, &AccessTokenClaims{UserID: "user-1"}, time.Now()); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("stamp a session-less token: %v, want ErrSessionNotFound", err)
	}
}

// M-3: the IdP's MFA is dated by its auth_time, never by the sign-in. A
// silent sign-in on a live IdP session must not look like a fresh MFA.
func TestIdPMFAAt(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	old := now.Add(-6 * time.Hour)
	cases := []struct {
		name     string
		amr      []string
		authTime int64
		want     time.Time
	}{
		{"mfa with auth_time", []string{"mfa"}, now.Unix(), now},
		{"old mfa keeps its time", []string{"pwd", "otp"}, old.Unix(), old},
		{"missing auth_time is no mfa", []string{"mfa"}, 0, time.Time{}},
		{"negative auth_time is no mfa", []string{"mfa"}, -5, time.Time{}},
		{"future auth_time capped at now", []string{"mfa"}, now.Add(time.Hour).Unix(), now},
		{"no mfa in amr", []string{"pwd"}, now.Unix(), time.Time{}},
	}
	for _, c := range cases {
		if got := IdPMFAAt(c.amr, c.authTime, now); !got.Equal(c.want) {
			t.Errorf("%s: IdPMFAAt = %v, want %v", c.name, got, c.want)
		}
	}
	if got := SessionAuthFromAMR([]string{"mfa"}, old.Unix(), now).MFAAt; !got.Equal(old) {
		t.Errorf("SessionAuthFromAMR stamped %v, want the IdP auth_time %v", got, old)
	}
}

// Review fix: /auth/mfa/verify mints a new access token, so it must refuse a
// suspended user or a revoked generation, the same as RefreshSession.
func TestStampSessionMFA_RefusesSuspendedOrRevoked(t *testing.T) {
	pool := schemaPool(t)
	ctx := context.Background()
	var ws, user string
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ('mfa', 'mfa', 'active', 'mfa.example') RETURNING id`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (tenant_id, email, provider, provider_sub, role, status, identity_generation)
		 VALUES ($1, 'u@example.test', 'test', 'sub-1', 'member', 'active', 1) RETURNING id`, ws).Scan(&user); err != nil {
		t.Fatal(err)
	}
	svc, _ := observedService(t, func(c *Config) { c.Pool = pool })
	access, _, _, err := svc.IssueSession(ctx, user, ws, "member", "u@example.test", 1)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := svc.VerifyAccessToken(access)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.StampSessionMFA(ctx, claims, time.Now()); err != nil {
		t.Fatalf("active user: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'suspended' WHERE id = $1`, user); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.StampSessionMFA(ctx, claims, time.Now()); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("suspended user: err = %v, want ErrSessionNotFound", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'active', identity_generation = 2 WHERE id = $1`, user); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.StampSessionMFA(ctx, claims, time.Now()); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("revoked generation: err = %v, want ErrSessionNotFound", err)
	}
}
