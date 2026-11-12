package auth

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// recObserver records SessionObserver calls (PENDING-09 C-1).
type recObserver struct {
	mu        sync.Mutex
	refreshed []string // "user/sid"
	idleUntil []time.Time
	ended     []string // "user/sid/reason"
	userEnded []string // "user/reason"
}

func (r *recObserver) SessionRefreshed(_ context.Context, userID, sid string, idleUntil time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshed = append(r.refreshed, userID+"/"+sid)
	r.idleUntil = append(r.idleUntil, idleUntil)
}

func (r *recObserver) SessionEnded(_ context.Context, userID, sid, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, userID+"/"+sid+"/"+reason)
}

func (r *recObserver) UserSessionsEnded(_ context.Context, userID, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.userEnded = append(r.userEnded, userID+"/"+reason)
}

func observedService(t *testing.T, mut func(*Config)) (*serviceImpl, *recObserver) {
	t.Helper()
	rc, _ := newTestValkey(t)
	cfg := testConfig()
	obs := &recObserver{}
	cfg.Sessions = obs
	if mut != nil {
		mut(&cfg)
	}
	return &serviceImpl{cfg: cfg, redisClient: rc}, obs
}

func TestSessionObserver_RefreshMovesIdleDeadline(t *testing.T) {
	svc, obs := observedService(t, nil)
	ctx := context.Background()
	access, _, refresh, err := svc.IssueSession(ctx, "user-1", "tenant-1", "member", "u@example.com", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.RefreshSession(ctx, access, refresh); err != nil {
		t.Fatal(err)
	}
	want := "user-1/" + sidOf(t, refresh)
	if len(obs.refreshed) != 1 || obs.refreshed[0] != want {
		t.Fatalf("refreshed %v, want [%s]", obs.refreshed, want)
	}
	if d := time.Until(obs.idleUntil[0]); d < 7*24*time.Hour-time.Minute || d > 7*24*time.Hour+time.Minute {
		t.Fatalf("idle deadline in %v, want ~168h", d)
	}
}

// A sign-out reports its session as ended even with an expired access token
// and even when the session itself is already gone (D-3): that is the logout
// gap of a client whose daemon was down for more than 15 minutes.
func TestSessionObserver_SignOutWithExpiredTokenStillEndsDevices(t *testing.T) {
	svc, obs := observedService(t, func(c *Config) { c.JWTAccessTTL = "-1m" })
	ctx := context.Background()
	access, _, refresh, err := svc.IssueSession(ctx, "user-1", "tenant-1", "member", "u@example.com", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerifyAccessToken(access); err == nil {
		t.Fatal("test token is not expired")
	}
	sid := sidOf(t, refresh)

	svc.EndSession(ctx, access)
	svc.EndSession(ctx, access) // session already gone: still reported

	want := "user-1/" + sid + "/signed_out"
	if len(obs.ended) != 2 || obs.ended[0] != want || obs.ended[1] != want {
		t.Fatalf("ended %v, want 2× %s", obs.ended, want)
	}
	// A forged token reports nothing.
	svc.EndSession(ctx, access+"x")
	if len(obs.ended) != 2 {
		t.Fatalf("forged token ended a session: %v", obs.ended)
	}
}

func TestSessionObserver_InvalidateUserSessionsEndsAllDevices(t *testing.T) {
	svc, obs := observedService(t, nil)
	ctx := context.Background()
	if _, _, _, err := svc.IssueSession(ctx, "user-1", "tenant-1", "member", "u@example.com", 1); err != nil {
		t.Fatal(err)
	}
	if err := svc.InvalidateUserSessions(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	if len(obs.userEnded) != 1 || obs.userEnded[0] != "user-1/sessions_revoked" {
		t.Fatalf("userEnded %v", obs.userEnded)
	}
}

func TestSessionObserver_MaxLifetimeRefusedAtRefresh(t *testing.T) {
	svc, obs := observedService(t, func(c *Config) { c.JWTRefreshMaxLifetime = "-1m" })
	ctx := context.Background()
	access, _, refresh, err := svc.IssueSession(ctx, "user-1", "tenant-1", "member", "u@example.com", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.RefreshSession(ctx, access, refresh); err == nil {
		t.Fatal("refresh past max lifetime succeeded")
	}
	want := "user-1/" + sidOf(t, refresh) + "/session_expired"
	if len(obs.ended) != 1 || obs.ended[0] != want || len(obs.refreshed) != 0 {
		t.Fatalf("ended %v refreshed %v, want [%s]", obs.ended, obs.refreshed, want)
	}
}

func TestSessionDeadlines(t *testing.T) {
	svc, _ := observedService(t, nil)
	ctx := context.Background()
	access, _, refresh, err := svc.IssueSession(ctx, "user-1", "tenant-1", "member", "u@example.com", 1)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := svc.VerifyAccessToken(access)
	if err != nil || claims.SessionID != sidOf(t, refresh) {
		t.Fatalf("claims sid %q, want %q (%v)", claims.SessionID, sidOf(t, refresh), err)
	}
	d, err := svc.SessionDeadlines(ctx, "user-1", claims.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if m := time.Until(d.MaxUntil); m < 30*24*time.Hour-time.Minute || m > 30*24*time.Hour+time.Minute {
		t.Fatalf("max deadline in %v, want ~720h", m)
	}
	svc.EndSession(ctx, access)
	if _, err := svc.SessionDeadlines(ctx, "user-1", claims.SessionID); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("ended session: %v", err)
	}
	if _, err := svc.SessionDeadlines(ctx, "user-1", ""); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("empty sid: %v", err)
	}
}
