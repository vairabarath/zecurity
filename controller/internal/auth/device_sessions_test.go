package auth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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

// schemaPool creates a throwaway database from PKI_TEST_DATABASE_URL with
// every schema file and returns an owner pool.
func schemaPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("auth_devsession_%d_%d", os.Getpid(), time.Now().UnixNano())
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	parsed, _ := url.Parse(adminDSN)
	parsed.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		admin.Close()
	})
	files, _ := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	return pool
}

// Regression (review of PENDING-09 C-1): the refresh-time generation check
// runs before the refresh token is compared and accepts an expired access
// token. A stale token from an older generation (a console tab left open
// across an IdP disable/re-enable) must end only its own session's devices,
// never those of the user's newer sessions.
func TestSessionObserver_StaleGenerationRefreshEndsOnlyItsOwnSession(t *testing.T) {
	pool := schemaPool(t)
	ctx := context.Background()
	var ws, user string
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ('gen', 'gen', 'active', 'gen.example') RETURNING id`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (tenant_id, email, provider, provider_sub, role, status, identity_generation)
		 VALUES ($1, 'u@example.test', 'test', 'sub-1', 'member', 'active', 1) RETURNING id`, ws).Scan(&user); err != nil {
		t.Fatal(err)
	}
	svc, obs := observedService(t, func(c *Config) { c.Pool = pool })

	oldAccess, _, oldRefresh, err := svc.IssueSession(ctx, user, ws, "member", "u@example.test", 1)
	if err != nil {
		t.Fatal(err)
	}
	// IdP disable/re-enable: generation 2, and the user signs in again.
	if _, err := pool.Exec(ctx, `UPDATE users SET identity_generation = 2 WHERE id = $1`, user); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := svc.IssueSession(ctx, user, ws, "member", "u@example.test", 2); err != nil {
		t.Fatal(err)
	}

	if _, _, err := svc.RefreshSession(ctx, oldAccess, oldRefresh); err == nil {
		t.Fatal("old-generation refresh succeeded")
	}
	want := user + "/" + sidOf(t, oldRefresh) + "/sessions_revoked"
	if len(obs.userEnded) != 0 || len(obs.ended) != 1 || obs.ended[0] != want {
		t.Fatalf("userEnded %v ended %v, want only [%s]", obs.userEnded, obs.ended, want)
	}
}
