package devicesession

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/audit"
)

// testDB creates a throwaway database from PKI_TEST_DATABASE_URL with every
// schema file. The returned pool runs as ztna_app, the controller's role.
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("devsession_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}
	parsed, _ := url.Parse(adminDSN)
	parsed.Path = "/" + dbName
	owner, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	var app *pgxpool.Pool
	t.Cleanup(func() {
		if app != nil {
			app.Close()
		}
		owner.Close()
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
		if _, err := owner.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	cfg, _ := pgxpool.ParseConfig(parsed.String())
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE ztna_app")
		return err
	}
	app, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

type fixture struct {
	ctx     context.Context
	pool    *pgxpool.Pool
	m       *Manager
	ws      string
	mu      sync.Mutex
	audits  []audit.Entry
	notifed map[string]int
}

type recNotifier struct{ f *fixture }

func (n recNotifier) NotifyPolicyChange(_ context.Context, ws string) error {
	n.f.mu.Lock()
	defer n.f.mu.Unlock()
	n.f.notifed[ws]++
	return nil
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{ctx: context.Background(), pool: testDB(t), notifed: map[string]int{}}
	f.m = New(f.pool, func(_ context.Context, e audit.Entry) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.audits = append(f.audits, e)
	})
	f.m.SetNotifier(recNotifier{f})
	f.ws = f.workspace(t)
	return f
}

func (f *fixture) workspace(t *testing.T) string {
	t.Helper()
	slug := fmt.Sprintf("ds-%d", time.Now().UnixNano())
	var ws string
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ($1, $1, 'active', $2) RETURNING id`,
		slug, slug+".example").Scan(&ws); err != nil {
		t.Fatal(err)
	}
	return ws
}

func (f *fixture) user(t *testing.T, ws string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		 VALUES ($1, $2, 'test', $3, 'member', 'active') RETURNING id`,
		ws, uuid.NewString()+"@example.test", uuid.NewString()).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// device inserts an active device bound to sid with the given deadlines
// (sid "" leaves the session columns NULL). It has a cert serial so its
// revocation lands on the CRL.
func (f *fixture) device(t *testing.T, ws, user, sid string, idle, max time.Time) string {
	t.Helper()
	var sidArg, idleArg, maxArg any
	if sid != "" {
		sidArg, idleArg, maxArg = sid, idle, max
	}
	var id string
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO client_devices (user_id, workspace_id, name, os, cert_serial, cert_not_after,
		        session_id, session_idle_until, session_max_until)
		 VALUES ($1, $2, $3, 'linux', $4, NOW() + INTERVAL '6 days', $5, $6, $7) RETURNING id`,
		user, ws, "dev-"+uuid.NewString()[:8], uuid.NewString(), sidArg, idleArg, maxArg).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *fixture) revoked(t *testing.T, id string) bool {
	t.Helper()
	var r bool
	if err := f.pool.QueryRow(f.ctx, `SELECT revoked_at IS NOT NULL FROM client_devices WHERE id = $1`, id).Scan(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

var (
	soon = func() time.Time { return time.Now().Add(7 * 24 * time.Hour) }
	far  = func() time.Time { return time.Now().Add(30 * 24 * time.Hour) }
	past = func() time.Time { return time.Now().Add(-time.Minute) }
)

// Signing out of one device's session revokes only that device: the same
// user's other device (another session) keeps working (D-7, cf. FU-15).
func TestSessionEnded_RevokesOnlyThatSessionsDevice(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, f.ws)
	other := f.user(t, f.ws)
	a := f.device(t, f.ws, u, "sid-a", soon(), far())
	b := f.device(t, f.ws, u, "sid-b", soon(), far())
	// Another user's device whose sid happens to be the same string.
	c := f.device(t, f.ws, other, "sid-a", soon(), far())

	f.m.SessionEnded(f.ctx, u, "sid-a", ReasonSignedOut)

	if !f.revoked(t, a) || f.revoked(t, b) || f.revoked(t, c) {
		t.Fatalf("revoked a=%v b=%v c=%v, want true false false", f.revoked(t, a), f.revoked(t, b), f.revoked(t, c))
	}
	if len(f.audits) != 1 || f.audits[0].TargetID != a || f.audits[0].Action != "device.revoked" ||
		f.audits[0].Details["reason"] != ReasonSignedOut || f.audits[0].ActorUserID != u {
		t.Fatalf("audits %+v", f.audits)
	}
	if f.notifed[f.ws] != 1 {
		t.Fatalf("notified %v", f.notifed)
	}
	// The revoked device's certificate is on the CRL (039 trigger).
	var onCRL bool
	if err := f.pool.QueryRow(f.ctx,
		`SELECT bool_and(revoked_at IS NOT NULL) FROM client_device_certificates WHERE device_id = $1`, a).Scan(&onCRL); err != nil || !onCRL {
		t.Fatalf("cert of revoked device not revoked: %v %v", onCRL, err)
	}

	// Idempotent: ending it again writes nothing more.
	f.m.SessionEnded(f.ctx, u, "sid-a", ReasonSignedOut)
	if len(f.audits) != 1 || f.notifed[f.ws] != 1 {
		t.Fatalf("second end: audits %d notified %v", len(f.audits), f.notifed)
	}
}

// Console sign-out ends the browser's session, whose sid no device carries:
// nothing is revoked, audited or notified.
func TestSessionEnded_BrowserSessionTouchesNoDevice(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, f.ws)
	a := f.device(t, f.ws, u, "sid-device", soon(), far())

	f.m.SessionEnded(f.ctx, u, "sid-browser", ReasonSignedOut)
	f.m.SessionEnded(f.ctx, u, "", ReasonSignedOut)

	if f.revoked(t, a) || len(f.audits) != 0 || len(f.notifed) != 0 {
		t.Fatalf("revoked=%v audits=%d notified=%v", f.revoked(t, a), len(f.audits), f.notifed)
	}
}

// A refresh moves the idle deadline of that session's devices only.
func TestSessionRefreshed_MovesIdleDeadline(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, f.ws)
	old := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	a := f.device(t, f.ws, u, "sid-a", old, far())
	b := f.device(t, f.ws, u, "sid-b", old, far())
	next := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)

	f.m.SessionRefreshed(f.ctx, u, "sid-a", next)

	idle := func(id string) time.Time {
		var v time.Time
		if err := f.pool.QueryRow(f.ctx, `SELECT session_idle_until FROM client_devices WHERE id = $1`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v.UTC()
	}
	if !idle(a).Equal(next) || !idle(b).Equal(old) {
		t.Fatalf("idle a=%v b=%v, want %v %v", idle(a), idle(b), next, old)
	}
}

// The sweep revokes devices whose idle or max deadline has passed, with the
// right reason, and leaves live and unbound devices alone.
func TestSweep_RevokesExpiredSessionsOnly(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, f.ws)
	ws2 := f.workspace(t)
	u2 := f.user(t, ws2)
	idled := f.device(t, f.ws, u, "sid-idle", past(), far())
	aged := f.device(t, ws2, u2, "sid-max", soon(), past())
	live := f.device(t, f.ws, u, "sid-live", soon(), far())
	unbound := f.device(t, f.ws, u, "", time.Time{}, time.Time{})

	n, err := f.m.Sweep(f.ctx)
	if err != nil || n != 2 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}
	if !f.revoked(t, idled) || !f.revoked(t, aged) || f.revoked(t, live) || f.revoked(t, unbound) {
		t.Fatal("wrong devices revoked")
	}
	reasons := map[string]any{}
	for _, a := range f.audits {
		reasons[a.TargetID] = a.Details["reason"]
		if a.ActorUserID != "" {
			t.Fatalf("sweep audit has an actor: %+v", a)
		}
	}
	if reasons[idled] != ReasonSessionIdle || reasons[aged] != ReasonSessionExpired || len(reasons) != 2 {
		t.Fatalf("reasons %v", reasons)
	}
	if f.notifed[f.ws] != 1 || f.notifed[ws2] != 1 {
		t.Fatalf("notified %v", f.notifed)
	}
	if n, err := f.m.Sweep(f.ctx); err != nil || n != 0 {
		t.Fatalf("second sweep: n=%d err=%v", n, err)
	}
}

// Ending every session of a user (generation bump, lifecycle check) revokes
// all of that user's devices and no one else's.
func TestUserSessionsEnded_RevokesAllOfTheUsersDevices(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, f.ws)
	other := f.user(t, f.ws)
	a := f.device(t, f.ws, u, "sid-a", soon(), far())
	b := f.device(t, f.ws, u, "sid-b", soon(), far())
	c := f.device(t, f.ws, other, "sid-c", soon(), far())

	f.m.UserSessionsEnded(f.ctx, u, ReasonSessionsRevoked)

	if !f.revoked(t, a) || !f.revoked(t, b) || f.revoked(t, c) {
		t.Fatal("wrong devices revoked")
	}
	if len(f.audits) != 2 || f.notifed[f.ws] != 1 {
		t.Fatalf("audits %d notified %v", len(f.audits), f.notifed)
	}
}

// Deadlines come from Postgres alone: with every session alive by its
// deadlines, a sweep revokes nothing, whatever state Valkey is in (a flush
// or restart must not revoke every device, D-2). The Manager has no Valkey
// dependency at all; this pins that a sweep over live deadlines is a no-op.
func TestSweep_LiveDeadlinesRevokeNothing(t *testing.T) {
	f := newFixture(t)
	u := f.user(t, f.ws)
	for i := 0; i < 5; i++ {
		f.device(t, f.ws, u, fmt.Sprintf("sid-%d", i), soon(), far())
	}
	if n, err := f.m.Sweep(f.ctx); err != nil || n != 0 {
		t.Fatalf("sweep: n=%d err=%v", n, err)
	}
	if len(f.audits) != 0 || len(f.notifed) != 0 {
		t.Fatalf("audits %d notified %v", len(f.audits), f.notifed)
	}
}
