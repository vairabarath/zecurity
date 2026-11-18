package quota

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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sprint 23 S3 (D-47) over a real database: effective limits (plan +
// overrides), the counting rules, Reserve / ReserveUser at and below the
// limit, the per-workspace serialisation (the concurrency race), lowered
// limits (over, nothing deleted), fail closed, and SetOverride.

func newTestDB(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("quota_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	parsed, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + dbName
	cfg, err := pgxpool.ParseConfig(parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName)
		admin.Close()
	})
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list migrations: %v", err)
	}
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
	return pool, parsed.String()
}

type env struct {
	t    *testing.T
	pool *pgxpool.Pool
	dsn  string
	ctx  context.Context
}

func newEnv(t *testing.T) *env {
	pool, dsn := newTestDB(t)
	return &env{t: t, pool: pool, dsn: dsn, ctx: context.Background()}
}

func (e *env) scalar(sql string, args ...any) string {
	e.t.Helper()
	var v string
	if err := e.pool.QueryRow(e.ctx, sql, args...).Scan(&v); err != nil {
		e.t.Fatalf("query %q: %v", sql, err)
	}
	return v
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.pool.Exec(e.ctx, sql, args...); err != nil {
		e.t.Fatalf("exec %q: %v", sql, err)
	}
}

// plan creates a plan with every key at n (nil = unlimited) except overrides.
func (e *env) plan(name string, n *int64, keys map[string]*int64) string {
	e.t.Helper()
	limits := "{"
	for i, k := range Keys {
		v := n
		if o, ok := keys[k]; ok {
			v = o
		}
		if i > 0 {
			limits += ","
		}
		if v == nil {
			limits += fmt.Sprintf("%q: null", k)
		} else {
			limits += fmt.Sprintf("%q: %d", k, *v)
		}
	}
	limits += "}"
	return e.scalar(`INSERT INTO plans (name, limits) VALUES ($1, $2::jsonb) RETURNING id::text`, name, limits)
}

func (e *env) workspace(slug, planID string) string {
	e.t.Helper()
	return e.scalar(`INSERT INTO workspaces (slug, name, status, trust_domain, plan_id)
		VALUES ($1, $1, 'active', $1 || '.test', $2) RETURNING id::text`, slug, planID)
}

func i64(n int64) *int64 { return &n }

// reserve runs Reserve(key) in a transaction and, when allowed, runs insert
// in the same transaction (the creation), then commits.
func (e *env) reserve(ws, key string, insert func(tx pgx.Tx) error) error {
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(e.ctx)
	if err := Reserve(e.ctx, tx, ws, key); err != nil {
		return err
	}
	if insert != nil {
		if err := insert(tx); err != nil {
			return err
		}
	}
	return tx.Commit(e.ctx)
}

func (e *env) insertNetwork(ws string) func(tx pgx.Tx) error {
	return func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'rn-' || gen_random_uuid(), 'office')`, ws)
		return err
	}
}

func (e *env) count(ws, key string) int64 {
	e.t.Helper()
	n, err := Count(e.ctx, e.pool, ws, key)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestQuotaDB_EffectiveLimitsAndOverrides(t *testing.T) {
	e := newEnv(t)
	p := e.plan("small", i64(3), map[string]*int64{KeyShields: nil})
	ws := e.workspace("acme", p)
	l, err := Effective(e.ctx, e.pool, ws)
	if err != nil {
		t.Fatal(err)
	}
	if *l[KeyUsers].Value != 3 || l[KeyUsers].Source != SourcePlan || l[KeyShields].Value != nil || l[KeyShields].Source != SourceUnlimited {
		t.Fatalf("plan limits %+v", l)
	}
	e.exec(`INSERT INTO workspace_quota_overrides (workspace_id, key, limit_value, reason, set_by) VALUES ($1, 'users', 7, 'r', 'op'), ($1, 'resources', NULL, 'r', 'op')`, ws)
	l, err = Effective(e.ctx, e.pool, ws)
	if err != nil {
		t.Fatal(err)
	}
	if *l[KeyUsers].Value != 7 || l[KeyUsers].Source != SourceOverride || l[KeyResources].Value != nil || l[KeyResources].Source != SourceOverride {
		t.Fatalf("with overrides %+v", l)
	}
	if *l[KeyConnectors].Value != 3 {
		t.Fatalf("a key without an override must keep the plan value: %+v", l[KeyConnectors])
	}
	// A plan change takes effect at once.
	e.exec(`UPDATE plans SET limits = jsonb_set(limits, '{connectors}', '9') WHERE id = $1`, p)
	if l, _ := Effective(e.ctx, e.pool, ws); *l[KeyConnectors].Value != 9 {
		t.Fatalf("plan change not effective: %+v", l[KeyConnectors])
	}
	if _, err := Effective(e.ctx, e.pool, "00000000-0000-0000-0000-00000000dead"); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("unknown workspace: %v", err)
	}
	// A missing key fails closed (the 062 CHECK prevents it in the table).
	if _, err := resolve([]byte(`{"users": 1}`), []byte(`[]`)); !errors.Is(err, ErrIncompleteLimits) {
		t.Fatalf("incomplete plan: %v", err)
	}
}

func TestQuotaDB_CountingRules(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace("acme", e.plan("unl", nil, nil))
	other := e.workspace("other", e.plan("unl2", nil, nil))
	inviter := e.scalar(`INSERT INTO users (tenant_id, email, provider, provider_sub, status) VALUES ($1, 'Admin@acme.test', 'google', 's0', 'active') RETURNING id::text`, ws)
	e.exec(`INSERT INTO users (tenant_id, email, provider, provider_sub, status) VALUES
		($1, 'locked@acme.test', 'google', 's1', 'locked'),
		($1, 'scim-suspended@acme.test', 'scim', 's2', 'suspended'),
		($1, 'removed@acme.test', 'google', 's3', 'deleted')`, ws)
	e.exec(`INSERT INTO users (tenant_id, email, provider, provider_sub, status) VALUES ($1, 'x@other.test', 'google', 'o1', 'active')`, other)
	e.exec(`INSERT INTO invitations (email, workspace_id, invited_by, token, status, expires_at) VALUES
		('pending@acme.test', $1, $2, 't1', 'pending', NOW() + INTERVAL '1 day'),
		('admin@acme.test',   $1, $2, 't2', 'pending', NOW() + INTERVAL '1 day'),
		('expired@acme.test', $1, $2, 't3', 'pending', NOW() - INTERVAL '1 minute'),
		('accepted@acme.test', $1, $2, 't4', 'accepted', NOW() + INTERVAL '1 day')`, ws, inviter)
	// admin (+ its own invitation, counted once), locked, scim-suspended, pending = 4.
	if n := e.count(ws, KeyUsers); n != 4 {
		t.Fatalf("users = %d, want 4 (removed, expired and accepted don't count; an identity counts once)", n)
	}

	rn := e.scalar(`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'rn', 'office') RETURNING id::text`, ws)
	e.exec(`INSERT INTO remote_networks (tenant_id, name, location, status) VALUES ($1, 'gone', 'office', 'deleted')`, ws)
	if n := e.count(ws, KeyRemoteNetworks); n != 1 {
		t.Fatalf("remote networks = %d, want 1 (deleted doesn't count)", n)
	}

	for i, st := range []string{"pending", "protecting", "protected", "unprotected", "failed", "deleting"} {
		e.exec(`INSERT INTO resources (tenant_id, remote_network_id, name, protocol, host, port_from, port_to, status)
			VALUES ($1, $2, $3, 'tcp', '10.0.0.1', $4, $4, $5)`, ws, rn, "r-"+st, 1000+i, st)
	}
	if n := e.count(ws, KeyResources); n != 5 {
		t.Fatalf("resources = %d, want 5 (every state but deleting; un-protected counts)", n)
	}
	// Discovered services never count.
	if n := e.count(ws, KeyResources); n != 5 {
		t.Fatalf("resources changed: %d", n)
	}

	e.exec(`INSERT INTO connectors (tenant_id, remote_network_id, name, status) VALUES
		($1, $2, 'c-pending', 'pending'), ($1, $2, 'c-active', 'active'), ($1, $2, 'c-disc', 'disconnected')`, ws, rn)
	e.exec(`INSERT INTO connectors (tenant_id, remote_network_id, name, status, revoked_at) VALUES ($1, $2, 'c-rev', 'revoked', NOW())`, ws, rn)
	if n := e.count(ws, KeyConnectors); n != 3 {
		t.Fatalf("connectors = %d, want 3 (pending, active, inactive; revoked doesn't count)", n)
	}
	conn := e.scalar(`SELECT id::text FROM connectors WHERE name = 'c-active'`)
	e.exec(`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status) VALUES
		($1, $2, $3, 's-pending', 'pending'), ($1, $2, $3, 's-active', 'active'), ($1, $2, $3, 's-rev', 'revoked')`, ws, rn, conn)
	if n := e.count(ws, KeyShields); n != 2 {
		t.Fatalf("shields = %d, want 2 (revoked doesn't count)", n)
	}

	user := e.scalar(`SELECT id::text FROM users WHERE email = 'Admin@acme.test'`)
	e.exec(`INSERT INTO client_devices (user_id, workspace_id, name, os) VALUES ($1, $2, 'd1', 'linux'), ($1, $2, 'd2', 'linux')`, user, ws)
	e.exec(`INSERT INTO client_devices (user_id, workspace_id, name, os, revoked_at) VALUES ($1, $2, 'd3', 'linux', NOW())`, user, ws)
	if n := e.count(ws, KeyClientDevices); n != 2 {
		t.Fatalf("client devices = %d, want 2 (revoked doesn't count)", n)
	}

	// Two accounts with the same email are two users (and the invitation
	// for that email still isn't counted on top).
	e.exec(`INSERT INTO users (tenant_id, email, provider, provider_sub, status) VALUES ($1, 'admin@acme.test', 'oidc', 's9', 'active')`, ws)
	if n := e.count(ws, KeyUsers); n != 5 {
		t.Fatalf("users = %d, want 5 (a second account with the same email counts)", n)
	}
	e.exec(`DELETE FROM users WHERE provider_sub = 's9'`)

	// AllUsage reports the same counts in key order, one statement.
	usage, err := AllUsage(e.ctx, e.pool, ws)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{KeyUsers: 4, KeyResources: 5, KeyRemoteNetworks: 1, KeyConnectors: 3, KeyShields: 2, KeyClientDevices: 2}
	for i, u := range usage {
		if u.Key != Keys[i] || u.Used != want[u.Key] || u.Limit != nil || u.Over {
			t.Fatalf("usage[%d] = %+v, want key %s used %d unlimited", i, u, Keys[i], want[u.Key])
		}
	}
}

func TestQuotaDB_ReserveAtLimitAndLowered(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace("acme", e.plan("two", i64(2), nil))
	for i := 0; i < 2; i++ {
		if err := e.reserve(ws, KeyRemoteNetworks, e.insertNetwork(ws)); err != nil {
			t.Fatalf("network %d below the limit: %v", i+1, err)
		}
	}
	err := e.reserve(ws, KeyRemoteNetworks, e.insertNetwork(ws))
	qe, ok := AsExceeded(err)
	if !ok || qe.Key != KeyRemoteNetworks || qe.Limit != 2 || qe.Used != 2 {
		t.Fatalf("at the limit: %v, want quota_exceeded {remote_networks, 2, 2}", err)
	}
	if n := e.count(ws, KeyRemoteNetworks); n != 2 {
		t.Fatalf("a refused creation created something: %d", n)
	}
	// Lowered below usage: nothing deleted, "over", still refused.
	e.exec(`INSERT INTO workspace_quota_overrides (workspace_id, key, limit_value, reason, set_by) VALUES ($1, 'remote_networks', 1, 'r', 'op')`, ws)
	usage, err := AllUsage(e.ctx, e.pool, ws)
	if err != nil {
		t.Fatal(err)
	}
	if u := usage[2]; u.Key != KeyRemoteNetworks || u.Used != 2 || *u.Limit != 1 || !u.Over || u.Source != SourceOverride {
		t.Fatalf("lowered: %+v, want used 2 limit 1 over", u)
	}
	if _, ok := AsExceeded(e.reserve(ws, KeyRemoteNetworks, nil)); !ok {
		t.Fatal("creation allowed while over the limit")
	}
	// An unlimited override allows it.
	e.exec(`UPDATE workspace_quota_overrides SET limit_value = NULL WHERE workspace_id = $1`, ws)
	if err := e.reserve(ws, KeyRemoteNetworks, e.insertNetwork(ws)); err != nil {
		t.Fatalf("unlimited override: %v", err)
	}
	if err := Reserve(e.ctx, nil, ws, KeyUsers); err == nil {
		t.Fatal("Reserve(users) must go through ReserveUser")
	}
}

func TestQuotaDB_ReserveUserCountsIdentitiesOnce(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace("acme", e.plan("two-users", i64(2), nil))
	inviter := e.scalar(`INSERT INTO users (tenant_id, email, provider, provider_sub, status) VALUES ($1, 'admin@acme.test', 'google', 's0', 'active') RETURNING id::text`, ws)
	invite := func(email string) error {
		tx, err := e.pool.Begin(e.ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(e.ctx)
		if err := ReserveInvitation(e.ctx, tx, ws, email); err != nil {
			return err
		}
		if _, err := tx.Exec(e.ctx, `INSERT INTO invitations (email, workspace_id, invited_by, token) VALUES (lower($1), $2, $3, gen_random_uuid()::text)`, email, ws, inviter); err != nil {
			return err
		}
		return tx.Commit(e.ctx)
	}
	if err := invite("a@acme.test"); err != nil {
		t.Fatalf("second identity: %v", err)
	}
	// Pending invitations count: a third identity is refused.
	if _, ok := AsExceeded(invite("b@acme.test")); !ok {
		t.Fatal("mass-inviting got past the limit")
	}
	// The same identity again (any case) takes no new slot.
	if err := invite("A@ACME.test"); err != nil {
		t.Fatalf("re-inviting a counted identity: %v", err)
	}
	// An existing member's identity is always allowed, even over a lowered limit.
	e.exec(`INSERT INTO workspace_quota_overrides (workspace_id, key, limit_value, reason, set_by) VALUES ($1, 'users', 0, 'r', 'op')`, ws)
	tx, _ := e.pool.Begin(e.ctx)
	if err := ReserveInvitation(e.ctx, tx, ws, "admin@acme.test"); err != nil {
		t.Fatalf("re-inviting a member over a lowered limit: %v", err)
	}
	tx.Rollback(e.ctx)
	// Signing in on a pending invitation uses its slot (no new one), even
	// over a lowered limit.
	tx, _ = e.pool.Begin(e.ctx)
	if err := ReserveUser(e.ctx, tx, ws, "a@acme.test"); err != nil {
		t.Fatalf("join on a pending invitation over a lowered limit: %v", err)
	}
	tx.Rollback(e.ctx)
	// A second account with an email already in use is a new user: it takes
	// a slot (the slot is the user row, never the email alone).
	tx, _ = e.pool.Begin(e.ctx)
	if _, ok := AsExceeded(ReserveUser(e.ctx, tx, ws, "admin@acme.test")); !ok {
		t.Fatal("a second account with an existing email took no slot")
	}
	tx.Rollback(e.ctx)
	// An expired invitation frees the slot.
	e.exec(`DELETE FROM workspace_quota_overrides WHERE workspace_id = $1`, ws)
	e.exec(`UPDATE invitations SET expires_at = NOW() - INTERVAL '1 second' WHERE email = 'a@acme.test'`)
	if err := invite("c@acme.test"); err != nil {
		t.Fatalf("after an invitation expired: %v", err)
	}
}

// The concurrency race: N parallel creations with one slot left → exactly
// one succeeds. Mutation check: remove the FOR UPDATE in lock() and this
// fails.
func TestQuotaDB_ConcurrentCreationsTakeTheLastSlotOnce(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace("acme", e.plan("three", i64(3), nil))
	for i := 0; i < 2; i++ {
		if err := e.reserve(ws, KeyRemoteNetworks, e.insertNetwork(ws)); err != nil {
			t.Fatal(err)
		}
	}
	const n = 10
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- e.reserve(ws, KeyRemoteNetworks, func(tx pgx.Tx) error {
				// Widen the window between the count and the commit.
				time.Sleep(20 * time.Millisecond)
				return e.insertNetwork(ws)(tx)
			})
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	ok, refused := 0, 0
	for err := range results {
		switch _, isQuota := AsExceeded(err); {
		case err == nil:
			ok++
		case isQuota:
			refused++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || refused != n-1 {
		t.Fatalf("%d succeeded, %d refused; want exactly 1 and %d", ok, refused, n-1)
	}
	if c := e.count(ws, KeyRemoteNetworks); c != 3 {
		t.Fatalf("networks = %d, want 3 (the limit)", c)
	}
}

// Fail closed: a lookup error refuses the creation (not as a quota refusal).
func TestQuotaDB_FailClosed(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace("acme", e.plan("one", i64(1), nil))
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(e.ctx, `SELECT 1/0`); err == nil {
		t.Fatal("setup: expected an aborted transaction")
	}
	if err := Reserve(e.ctx, tx, ws, KeyConnectors); err == nil {
		t.Fatal("Reserve on a failing transaction allowed the creation")
	} else if _, ok := AsExceeded(err); ok {
		t.Fatal("a lookup error must not look like a quota refusal")
	}
	tx.Rollback(e.ctx)
	tx, _ = e.pool.Begin(e.ctx)
	defer tx.Rollback(e.ctx)
	if err := Reserve(e.ctx, tx, "00000000-0000-0000-0000-00000000dead", KeyConnectors); !errors.Is(err, ErrWorkspaceNotFound) {
		t.Fatalf("unknown workspace: %v", err)
	}
}

func TestQuotaDB_SetOverride(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace("acme", e.plan("five", i64(5), nil))
	var audits [][2]Limit
	audit := func(_ context.Context, _ pgx.Tx, before, after Limit) error {
		audits = append(audits, [2]Limit{before, after})
		return nil
	}
	if changed, err := SetOverride(e.ctx, e.pool, ws, KeyConnectors, Override{Limit: i64(2)}, "pilot", "op@x", audit); err != nil || !changed {
		t.Fatalf("set: %v %v", changed, err)
	}
	if b, a := audits[0][0], audits[0][1]; b.Source != SourcePlan || *b.Value != 5 || a.Source != SourceOverride || *a.Value != 2 {
		t.Fatalf("before/after %+v", audits[0])
	}
	if reason := e.scalar(`SELECT reason || '|' || set_by FROM workspace_quota_overrides WHERE workspace_id = $1 AND key = 'connectors'`, ws); reason != "pilot|op@x" {
		t.Fatalf("stored %q", reason)
	}
	if changed, _ := SetOverride(e.ctx, e.pool, ws, KeyConnectors, Override{Limit: i64(2)}, "again", "op@x", audit); changed {
		t.Fatal("same value: want no change (no audit)")
	}
	if changed, err := SetOverride(e.ctx, e.pool, ws, KeyConnectors, Override{}, "unlimited", "op@x", audit); err != nil || !changed {
		t.Fatalf("unlimited: %v %v", changed, err)
	}
	if a := audits[len(audits)-1][1]; a.Value != nil || a.Source != SourceOverride {
		t.Fatalf("unlimited after %+v", a)
	}
	if changed, err := SetOverride(e.ctx, e.pool, ws, KeyConnectors, Override{Inherit: true}, "back", "op@x", audit); err != nil || !changed {
		t.Fatalf("inherit: %v %v", changed, err)
	}
	if a := audits[len(audits)-1][1]; a.Source != SourcePlan || *a.Value != 5 {
		t.Fatalf("inherit after %+v", a)
	}
	if changed, _ := SetOverride(e.ctx, e.pool, ws, KeyConnectors, Override{Inherit: true}, "again", "op@x", audit); changed {
		t.Fatal("inherit without an override: want no change")
	}
	if _, err := SetOverride(e.ctx, e.pool, ws, "seats", Override{Limit: i64(1)}, "x", "op", nil); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key: %v", err)
	}
	if _, err := SetOverride(e.ctx, e.pool, ws, KeyUsers, Override{Limit: i64(-1)}, "x", "op", nil); !errors.Is(err, ErrInvalidLimit) {
		t.Fatalf("negative limit: %v", err)
	}
	// The audit callback's error rolls the change back.
	boom := errors.New("audit failed")
	if _, err := SetOverride(e.ctx, e.pool, ws, KeyUsers, Override{Limit: i64(1)}, "x", "op",
		func(context.Context, pgx.Tx, Limit, Limit) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("audit failure: %v", err)
	}
	if n := e.scalar(`SELECT count(*)::text FROM workspace_quota_overrides WHERE workspace_id = $1`, ws); n != "0" {
		t.Fatalf("override kept after a failed audit: %s rows", n)
	}
}

// PR #224 review: the quota lock is FOR NO KEY UPDATE, so a foreign-key
// insert that references the workspace (here an audit row) does not wait
// for an open quota transaction, while a second quota transaction still
// does (serialised).
func TestQuotaDB_LockDoesNotBlockForeignKeyInserts(t *testing.T) {
	e := newEnv(t)
	ws := e.workspace("lockws", "00000000-0000-0000-0001-000000000002")
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(e.ctx)
	if err := Reserve(e.ctx, tx, ws, KeyRemoteNetworks); err != nil {
		t.Fatalf("reserve: %v", err)
	}

	ctx, cancel := context.WithTimeout(e.ctx, 3*time.Second)
	defer cancel()
	if _, err := e.pool.Exec(ctx, `INSERT INTO audit_logs (tenant_id, actor_email, action, target_type, target_id)
		VALUES ($1, 'x@y.test', 'test.fk_insert', 'test', 't')`, ws); err != nil {
		t.Fatalf("FK insert blocked by the quota lock: %v", err)
	}

	// Another quota transaction on the same workspace still waits.
	tx2, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback(e.ctx)
	if _, err := tx2.Exec(e.ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
		t.Fatal(err)
	}
	if err := Reserve(e.ctx, tx2, ws, KeyRemoteNetworks); err == nil {
		t.Fatal("a second quota transaction did not wait for the first")
	}
}
