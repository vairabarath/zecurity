package usage

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/relay"
)

// Sprint 25 U1b (D-57, D-58, D-59): the collector over a throwaway database.

const (
	relay1  = "11111111-0000-0000-0000-000000000001"
	relay2  = "11111111-0000-0000-0000-000000000002"
	tenantB = "bbbbbbbb-0000-0000-0000-0000000000b1"
)

var t0 = time.Date(2026, 11, 21, 10, 0, 30, 0, time.UTC)

func scalar[T any](t *testing.T, db *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := db.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

// hourly returns a tenant's quantity for a meter and hour (0 when no row).
func hourly(t *testing.T, db *pgxpool.Pool, tenant, meter string, hour time.Time) int64 {
	t.Helper()
	return scalar[int64](t, db, `SELECT COALESCE(sum(quantity), 0)::bigint FROM usage_hourly WHERE tenant_id = $1 AND meter = $2 AND hour = $3`, tenant, meter, hour)
}

func total(t *testing.T, db *pgxpool.Pool, tenant, meter string) int64 {
	t.Helper()
	return scalar[int64](t, db, `SELECT COALESCE(sum(quantity), 0)::bigint FROM usage_hourly WHERE tenant_id = $1 AND meter = $2`, tenant, meter)
}

func sample(toClient, toConnector uint64) relay.WorkspaceTrafficSample {
	return relay.WorkspaceTrafficSample{ToClient: toClient, ToConnector: toConnector}
}

// hb sends one heartbeat; uptime is the relay's seconds since its start.
func hb(t *testing.T, c *Collector, relayID string, at time.Time, uptime uint64, counters map[string]relay.WorkspaceTrafficSample) {
	t.Helper()
	if err := c.recordRelayCounters(context.Background(), relayID, at, uptime, counters); err != nil {
		t.Fatal(err)
	}
}

func TestRelayBytes_ExactDeltasAcrossRelaysAndHours(t *testing.T) {
	db := testDB(t)
	c := &Collector{DB: db}
	// First sight of a relay: its counters become the baseline (they may
	// hold bytes from before metering), nothing is counted.
	hb(t, c, relay1, t0, 3600, map[string]relay.WorkspaceTrafficSample{tenantA: sample(5_000, 500)})
	if n := total(t, db, tenantA, MeterRelayBytesToClients); n != 0 {
		t.Fatalf("first sight counted %d bytes", n)
	}
	// Same run: exact differences, per direction.
	hb(t, c, relay1, t0.Add(30*time.Second), 3630, map[string]relay.WorkspaceTrafficSample{tenantA: sample(7_000, 800)})
	// Another relay carries tenant A and B (watched from its first heartbeat).
	hb(t, c, relay2, t0, 100, map[string]relay.WorkspaceTrafficSample{tenantA: sample(10, 1)})
	hb(t, c, relay2, t0.Add(30*time.Second), 130, map[string]relay.WorkspaceTrafficSample{tenantA: sample(110, 11), tenantB: sample(40, 4)})
	if n := total(t, db, tenantA, MeterRelayBytesToClients); n != 2_000+100 {
		t.Fatalf("tenant A to clients = %d, want 2100 (two relays add up)", n)
	}
	if n := total(t, db, tenantA, MeterRelayBytesToConnectors); n != 300+10 {
		t.Fatalf("tenant A to connectors = %d, want 310", n)
	}
	// Tenant B appeared during relay2's watched run: counted from 0.
	if n := total(t, db, tenantB, MeterRelayBytesToClients); n != 40 {
		t.Fatalf("tenant B to clients = %d, want 40 (new in a watched run)", n)
	}
	// A heartbeat after the hour boundary: the delta lands in the later hour.
	next := t0.Truncate(time.Hour).Add(time.Hour).Add(5 * time.Second)
	hb(t, c, relay1, next, 3630+uint64(next.Sub(t0.Add(30*time.Second)).Seconds()), map[string]relay.WorkspaceTrafficSample{tenantA: sample(7_500, 800)})
	if n := hourly(t, db, tenantA, MeterRelayBytesToClients, next.Truncate(time.Hour)); n != 500 {
		t.Fatalf("next hour = %d, want 500", n)
	}
	// A late or repeated heartbeat (not newer than the last counted) counts nothing.
	hb(t, c, relay1, next, 99999, map[string]relay.WorkspaceTrafficSample{tenantA: sample(9_999_999, 9_999_999)})
	hb(t, c, relay1, next.Add(-time.Second), 99999, map[string]relay.WorkspaceTrafficSample{tenantA: sample(9_999_999, 9_999_999)})
	if n := total(t, db, tenantA, MeterRelayBytesToClients); n != 2_600 {
		t.Fatalf("after late heartbeats = %d, want 2600", n)
	}
}

func TestRelayBytes_RelayRestartAndCounterReset(t *testing.T) {
	db := testDB(t)
	c := &Collector{DB: db}
	hb(t, c, relay1, t0, 1000, map[string]relay.WorkspaceTrafficSample{tenantA: sample(1_000_000, 1_000)})
	hb(t, c, relay1, t0.Add(30*time.Second), 1030, map[string]relay.WorkspaceTrafficSample{tenantA: sample(1_000_100, 1_010)})
	// The relay restarted (uptime 10 s < the 30 s gap): its counters restart
	// at 0, so everything they hold is new — never negative, never the old
	// total again.
	hb(t, c, relay1, t0.Add(60*time.Second), 10, map[string]relay.WorkspaceTrafficSample{tenantA: sample(70, 7)})
	if n := total(t, db, tenantA, MeterRelayBytesToClients); n != 100+70 {
		t.Fatalf("after restart = %d, want 170", n)
	}
	// Same run, but the tenant's counters went down (the relay dropped and
	// re-created its entry): what they hold now is new.
	hb(t, c, relay1, t0.Add(90*time.Second), 40, map[string]relay.WorkspaceTrafficSample{tenantA: sample(5, 1)})
	if n := total(t, db, tenantA, MeterRelayBytesToClients); n != 175 {
		t.Fatalf("after counter reset = %d, want 175", n)
	}
	if n := total(t, db, tenantA, MeterRelayBytesToConnectors); n != 10+7+1 {
		t.Fatalf("to connectors = %d, want 18", n)
	}
}

// A controller restart empties R1's in-memory tracker; the collector's
// baselines are in the database, so the span across the restart is counted
// once: no loss, no double count. A failed write counts nothing and the
// next heartbeat counts the whole span.
func TestRelayBytes_ControllerRestartAndFailedWrite(t *testing.T) {
	db := testDB(t)
	c := &Collector{DB: db}
	hb(t, c, relay1, t0, 500, map[string]relay.WorkspaceTrafficSample{tenantA: sample(1_000, 0)})
	hb(t, c, relay1, t0.Add(30*time.Second), 530, map[string]relay.WorkspaceTrafficSample{tenantA: sample(1_200, 0)})
	restarted := &Collector{DB: db} // a new process
	hb(t, restarted, relay1, t0.Add(10*time.Minute), 1100, map[string]relay.WorkspaceTrafficSample{tenantA: sample(9_200, 0)})
	if n := total(t, db, tenantA, MeterRelayBytesToClients); n != 200+8_000 {
		t.Fatalf("across a controller restart = %d, want 8200", n)
	}

	// A failed write (simulated with a cancelled context): nothing is
	// written, so the next heartbeat counts the whole span once.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := restarted.recordRelayCounters(ctx, relay1, t0.Add(11*time.Minute), 1160, map[string]relay.WorkspaceTrafficSample{tenantA: sample(9_700, 0)}); err == nil {
		t.Fatal("cancelled write reported success")
	}
	hb(t, restarted, relay1, t0.Add(12*time.Minute), 1220, map[string]relay.WorkspaceTrafficSample{tenantA: sample(10_200, 0)})
	if n := total(t, db, tenantA, MeterRelayBytesToClients); n != 8_200+1_000 {
		t.Fatalf("after a failed write = %d, want 9200 (the span counted once)", n)
	}
}

// seedTenant creates an active workspace with a user, devices (live, idle
// expired, revoked), two connectors (one active) and a shield.
func seedTenant(t *testing.T, db *pgxpool.Pool, slug string, now time.Time) (ws, user string) {
	t.Helper()
	ctx := context.Background()
	if err := db.QueryRow(ctx, `INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ($1, $1 || ' Inc', 'active', $1 || '.zecurity.test') RETURNING id::text`, slug).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO users (tenant_id, email, provider, provider_sub, role, status) VALUES ($1, $2 || '@u.test', 'test', $2, 'member', 'active') RETURNING id::text`, ws, slug+"-u1").Scan(&user); err != nil {
		t.Fatal(err)
	}
	for _, d := range []struct {
		name      string
		idle, max time.Duration
		revoked   bool
	}{{"live", time.Hour, 2 * time.Hour, false}, {"live2", time.Hour, 2 * time.Hour, false}, {"idle", -time.Minute, time.Hour, false}, {"revoked", time.Hour, time.Hour, true}} {
		if _, err := db.Exec(ctx, `INSERT INTO client_devices (user_id, workspace_id, name, os, session_idle_until, session_max_until, revoked_at)
		                            VALUES ($1, $2, $3, 'linux', $4, $5, CASE WHEN $6 THEN NOW() END)`,
			user, ws, d.name, now.Add(d.idle), now.Add(d.max), d.revoked); err != nil {
			t.Fatal(err)
		}
	}
	var network, conn string
	if err := db.QueryRow(ctx, `INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'n', 'other') RETURNING id::text`, ws).Scan(&network); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO connectors (tenant_id, remote_network_id, name, status) VALUES ($1, $2, 'c1', 'active') RETURNING id::text`, ws, network).Scan(&conn); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO connectors (tenant_id, remote_network_id, name, status) VALUES ($1, $2, 'c2', 'disconnected')`, ws, network); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status) VALUES ($1, $2, $3, 's1', 'active')`, ws, network, conn); err != nil {
		t.Fatal(err)
	}
	return ws, user
}

func TestMinutes_CountedOncePerMinute(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 21, 10, 5, 10, 0, time.UTC)
	ws, _ := seedTenant(t, db, "u1-a", now)
	// A deleted workspace with live sessions and agents is not metered.
	gone, _ := seedTenant(t, db, "u1-gone", now)
	if _, err := db.Exec(ctx, `UPDATE workspaces SET status = 'deleted' WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	c := &Collector{DB: db}
	for i := 0; i < 3; i++ { // minutes 10:05, 10:06, 10:07
		at := now.Add(time.Duration(i) * time.Minute)
		if ok, err := c.CollectMinute(ctx, at); err != nil || !ok {
			t.Fatalf("minute %d: counted=%v err=%v", i, ok, err)
		}
		// The same minute again (a re-run or a restart): nothing more.
		if ok, err := c.CollectMinute(ctx, at.Add(20*time.Second)); err != nil || ok {
			t.Fatalf("minute %d re-run: counted=%v err=%v", i, ok, err)
		}
	}
	hour := now.Truncate(time.Hour)
	// Two live devices × 3 minutes; the idle-expired and revoked ones don't count.
	for meter, want := range map[string]int64{MeterSessionMinutes: 6, MeterConnectorMinutes: 3, MeterShieldMinutes: 3} {
		if n := hourly(t, db, ws, meter, hour); n != want {
			t.Errorf("%s = %d, want %d", meter, n, want)
		}
	}
	if n := scalar[int64](t, db, `SELECT count(*) FROM usage_hourly WHERE tenant_id = $1`, gone); n != 0 {
		t.Errorf("a deleted workspace was metered (%d rows)", n)
	}
	if n := scalar[int64](t, db, `SELECT count(*) FROM usage_active_users WHERE tenant_id = $1`, gone); n != 0 {
		t.Errorf("a deleted workspace's users were counted (%d rows)", n)
	}
	if n := scalar[int64](t, db, `SELECT count(*) FROM usage_collector_minutes WHERE minute >= $1 AND minute < $2`, hour, hour.Add(time.Hour)); n != 3 {
		t.Fatalf("%d collected minutes, want 3 (the hour is incomplete)", n)
	}
	// The snapshot names the tenant and its plan.
	if name := scalar[string](t, db, `SELECT name || '/' || slug || '/' || COALESCE(plan_name, '-') FROM usage_tenants WHERE tenant_id = $1`, ws); name != "u1-a Inc/u1-a/Free" {
		t.Fatalf("snapshot %q", name)
	}
	// Sprint 26 P1b (D-65): the snapshot also keeps the plan id, kept current.
	const free, legacy = "00000000-0000-0000-0001-000000000001", "00000000-0000-0000-0001-000000000002"
	if id := scalar[string](t, db, `SELECT COALESCE(plan_id::text, '-') FROM usage_tenants WHERE tenant_id = $1`, ws); id != free {
		t.Fatalf("snapshot plan_id %q, want Free", id)
	}
	if _, err := db.Exec(ctx, `UPDATE workspaces SET plan_id = $2 WHERE id = $1`, ws, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CollectMinute(ctx, now.Add(3*time.Minute)); err != nil { // 10:08, not yet collected
		t.Fatal(err)
	}
	if id := scalar[string](t, db, `SELECT plan_id::text FROM usage_tenants WHERE tenant_id = $1`, ws); id != legacy {
		t.Fatalf("snapshot plan_id %q after a plan change, want Legacy", id)
	}
}

func TestActiveUsers_DistinctPerUTCMonth(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	// 23:59:30 on Oct 31 UTC: still October (it is already Nov 1 in Asia).
	oct := time.Date(2026, 10, 31, 23, 59, 30, 0, time.UTC)
	ws, user := seedTenant(t, db, "u1-au", oct)
	c := &Collector{DB: db}
	if _, err := c.CollectMinute(ctx, oct); err != nil {
		t.Fatal(err)
	}
	// Two devices of one user, two minutes: one active user.
	if _, err := c.CollectMinute(ctx, oct.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int64](t, db, `SELECT count(*) FROM usage_active_users WHERE tenant_id = $1 AND month = '2026-10-01'`, ws); n != 1 {
		t.Fatalf("October active users = %d, want 1 (one user on two devices)", n)
	}
	// November: last month's user is not this month's until active again.
	nov := oct.Add(time.Minute)
	if _, err := db.Exec(ctx, `UPDATE client_devices SET session_idle_until = $1 WHERE user_id = $2`, nov.Add(-time.Second), user); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CollectMinute(ctx, nov); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int64](t, db, `SELECT count(*) FROM usage_active_users WHERE tenant_id = $1 AND month = '2026-11-01'`, ws); n != 0 {
		t.Fatalf("November active users = %d, want 0", n)
	}
}

func TestFreezeTenant_KeepsNameDropsUserIDs(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 21, 10, 5, 0, 0, time.UTC)
	ws, _ := seedTenant(t, db, "u1-p", now)
	other, _ := seedTenant(t, db, "u1-q", now)
	c := &Collector{DB: db}
	if _, err := c.CollectMinute(ctx, now); err != nil {
		t.Fatal(err)
	}
	// A second user active in October too.
	if _, err := db.Exec(ctx, `INSERT INTO usage_active_users VALUES ($1, '2026-10-01', gen_random_uuid(), now()), ($1, '2026-10-01', gen_random_uuid(), now())`, ws); err != nil {
		t.Fatal(err)
	}
	freeze := func() {
		if err := pgx.BeginTxFunc(ctx, db, pgx.TxOptions{}, func(tx pgx.Tx) error { return FreezeTenant(ctx, tx, ws) }); err != nil {
			t.Fatal(err)
		}
	}
	freeze()
	if n := scalar[int64](t, db, `SELECT count(*) FROM usage_active_users WHERE tenant_id = $1`, ws); n != 0 {
		t.Fatalf("%d user ids kept after the freeze", n)
	}
	counts := scalar[string](t, db, `SELECT string_agg(month::text || '=' || users, ',' ORDER BY month) FROM usage_active_user_counts WHERE tenant_id = $1`, ws)
	if counts != "2026-10-01=2,2026-11-01=1" {
		t.Fatalf("counts %q", counts)
	}
	if v := scalar[string](t, db, `SELECT name || '/' || slug || '/' || (purged_at IS NOT NULL)::text FROM usage_tenants WHERE tenant_id = $1`, ws); v != "u1-p Inc/u1-p/true" {
		t.Fatalf("snapshot %q", v)
	}
	// A rename after the freeze (the row is gone in reality) never changes it.
	if _, err := db.Exec(ctx, `UPDATE workspaces SET name = 'renamed', plan_id = '00000000-0000-0000-0001-000000000002' WHERE id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CollectMinute(ctx, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if v := scalar[string](t, db, `SELECT name FROM usage_tenants WHERE tenant_id = $1`, ws); v != "u1-p Inc" {
		t.Fatalf("frozen name changed to %q", v)
	}
	if v := scalar[string](t, db, `SELECT plan_id::text FROM usage_tenants WHERE tenant_id = $1`, ws); v != "00000000-0000-0000-0001-000000000001" {
		t.Fatalf("frozen plan_id changed to %q", v)
	}
	// Usage itself stays; the other tenant is untouched.
	if n := total(t, db, ws, MeterSessionMinutes); n == 0 {
		t.Fatal("a purged tenant's usage was dropped")
	}
	if n := scalar[int64](t, db, `SELECT count(*) FROM usage_active_users WHERE tenant_id = $1`, other); n != 1 {
		t.Fatalf("other tenant's active users = %d, want 1", n)
	}
	if n := scalar[int64](t, db, `SELECT count(*) FROM usage_tenants WHERE tenant_id = $1 AND purged_at IS NULL`, other); n != 1 {
		t.Fatal("other tenant's snapshot was frozen")
	}
}

func TestPrune_RetentionOnly(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 21, 10, 5, 0, 0, time.UTC)
	old := now.AddDate(0, -26, 0).Truncate(time.Hour)
	keep := now.AddDate(0, -24, 0).Truncate(time.Hour)
	for _, h := range []time.Time{old, keep} {
		if _, err := db.Exec(ctx, `INSERT INTO usage_hourly VALUES ($1, 'session_minutes', $2, 5)`, tenantA, h); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO usage_collector_minutes VALUES ($1)`, h); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(ctx, `INSERT INTO usage_active_user_counts VALUES ($1, $2, 3), ($1, $3, 4)`, tenantA,
		time.Date(old.Year(), old.Month(), 1, 0, 0, 0, 0, time.UTC), time.Date(keep.Year(), keep.Month(), 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	// A purged tenant with only old usage: its snapshot goes with it.
	if _, err := db.Exec(ctx, `INSERT INTO usage_hourly VALUES ($1, 'session_minutes', $2, 1)`, tenantB, old); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO usage_tenants (tenant_id, name, slug, purged_at) VALUES ($1, 'b', 'b', now()), ($2, 'a', 'a', now())`, tenantB, tenantA); err != nil {
		t.Fatal(err)
	}
	if err := (&Collector{DB: db}).Prune(ctx, now); err != nil {
		t.Fatal(err)
	}
	if n := scalar[int64](t, db, `SELECT count(*) FROM usage_hourly`); n != 1 {
		t.Fatalf("%d hourly rows left, want 1 (the 24-month-old one)", n)
	}
	if n := scalar[int64](t, db, `SELECT count(*) FROM usage_collector_minutes`); n != 1 {
		t.Fatalf("%d collected minutes left, want 1", n)
	}
	if n := scalar[int64](t, db, `SELECT count(*) FROM usage_active_user_counts`); n != 1 {
		t.Fatalf("%d active-user counts left, want 1", n)
	}
	if s := scalar[string](t, db, `SELECT string_agg(slug, ',' ORDER BY slug) FROM usage_tenants`); s != "a" {
		t.Fatalf("snapshots left %q, want a (b has no usage left)", s)
	}
}
