package metricstore

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/providerquery"
)

// Sprint 24 M1 (D-49) over a throwaway database with every migration
// (PKI_TEST_DATABASE_URL): writes, roll-ups, retention, and the values read
// from real relay, tenant, device and agent rows.

func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("metricstore_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
		admin.Close()
	})
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("migration %s: %v", filepath.Base(f), err)
		}
	}
	return pool
}

func countRows(t *testing.T, db *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

const tenantA = "aaaaaaaa-0000-0000-0000-0000000000a1"

func TestWrite_OnTheMinuteAndIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := &Sampler{DB: db}
	at := time.Date(2026, 11, 21, 10, 7, 42, 0, time.UTC)
	samples := []Sample{
		{providerquery.SeriesTenantActiveSessions, providerquery.MetricSubjectTenant, tenantA, 3},
		{providerquery.SeriesFleetActiveSessions, providerquery.MetricSubjectFleet, fleet, 3},
	}
	if err := s.Write(ctx, at, samples); err != nil {
		t.Fatal(err)
	}
	// Same minute again (a restart): kept, not replaced or duplicated.
	samples[0].Value = 99
	if err := s.Write(ctx, at.Add(10*time.Second), samples); err != nil {
		t.Fatal(err)
	}
	var ts time.Time
	var v float64
	if err := db.QueryRow(ctx, `SELECT ts, value FROM metric_samples WHERE series = $1`, providerquery.SeriesTenantActiveSessions).Scan(&ts, &v); err != nil {
		t.Fatal(err)
	}
	if !ts.Equal(time.Date(2026, 11, 21, 10, 7, 0, 0, time.UTC)) || v != 3 {
		t.Fatalf("stored %v = %v, want 10:07:00 = 3", ts, v)
	}
	if n := countRows(t, db, `SELECT count(*) FROM metric_samples`); n != 2 {
		t.Fatalf("%d rows, want 2", n)
	}
}

func TestSchema_RefusesBadRows(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	bad := []struct {
		name string
		sql  string
		args []any
	}{
		{"NaN", `INSERT INTO metric_samples VALUES ('tenant.active_sessions','tenant',$1,'2026-11-21T10:00:00Z','NaN')`, []any{tenantA}},
		{"infinity", `INSERT INTO metric_samples VALUES ('tenant.active_sessions','tenant',$1,'2026-11-21T10:00:00Z','Infinity')`, []any{tenantA}},
		{"off the minute", `INSERT INTO metric_samples VALUES ('tenant.active_sessions','tenant',$1,'2026-11-21T10:00:30Z',1)`, []any{tenantA}},
		{"prefix mismatch", `INSERT INTO metric_samples VALUES ('relay.connections','tenant',$1,'2026-11-21T10:00:00Z',1)`, []any{tenantA}},
		{"fleet with a subject", `INSERT INTO metric_samples VALUES ('fleet.connections','fleet',$1,'2026-11-21T10:00:00Z',1)`, []any{tenantA}},
		{"rollup off the hour", `INSERT INTO metric_rollups VALUES ('tenant.active_sessions','tenant',$1,'2026-11-21T10:30:00Z',1,1,1,1)`, []any{tenantA}},
	}
	for _, b := range bad {
		if _, err := db.Exec(ctx, b.sql, b.args...); err == nil {
			t.Errorf("%s: accepted", b.name)
		}
	}
	// On the hour in UTC even when the session time zone is offset by 30 minutes.
	if _, err := db.Exec(ctx, `SET TIME ZONE 'Asia/Kolkata'`); err != nil {
		t.Fatal(err)
	}
	s := &Sampler{DB: db}
	if err := s.Write(ctx, time.Date(2026, 11, 21, 10, 5, 0, 0, time.UTC), []Sample{{providerquery.SeriesTenantActiveSessions, providerquery.MetricSubjectTenant, tenantA, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Rollup(ctx, time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("roll-up under an offset time zone: %v", err)
	}
}

func TestRollup_MinMaxAvgCountAndRerun(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := &Sampler{DB: db}
	hour := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	write := func(minute int, v float64) {
		t.Helper()
		if err := s.Write(ctx, hour.Add(time.Duration(minute)*time.Minute), []Sample{{providerquery.SeriesTenantActiveSessions, providerquery.MetricSubjectTenant, tenantA, v}}); err != nil {
			t.Fatal(err)
		}
	}
	write(0, 2)
	write(1, 4)
	write(59, 9)
	write(60, 1000)                                                 // the next hour: not in this roll-up
	if err := s.Rollup(ctx, hour.Add(17*time.Minute)); err != nil { // any time in the hour
		t.Fatal(err)
	}
	var mn, mx, avg float64
	var n int
	read := func() {
		t.Helper()
		if err := db.QueryRow(ctx, `SELECT min, max, avg, count FROM metric_rollups WHERE series = $1 AND ts = $2`,
			providerquery.SeriesTenantActiveSessions, hour).Scan(&mn, &mx, &avg, &n); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if mn != 2 || mx != 9 || avg != 5 || n != 3 {
		t.Fatalf("roll-up min %v max %v avg %v count %v, want 2 9 5 3", mn, mx, avg, n)
	}
	// A late sample, then a re-run: the row is replaced, not duplicated.
	write(30, 5)
	if err := s.Rollup(ctx, hour); err != nil {
		t.Fatal(err)
	}
	read()
	if n != 4 || avg != 5 {
		t.Fatalf("after re-run count %v avg %v, want 4 5", n, avg)
	}
	if c := countRows(t, db, `SELECT count(*) FROM metric_rollups`); c != 1 {
		t.Fatalf("%d roll-up rows, want 1", c)
	}
}

func TestRollup_MeanClampedIntoMinMax(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	s := &Sampler{DB: db}
	hour := time.Date(2026, 11, 21, 11, 0, 0, 0, time.UTC)
	for m := 0; m < 3; m++ { // mean of three 0.1s rounds to 0.10000000000000002
		if err := s.Write(ctx, hour.Add(time.Duration(m)*time.Minute), []Sample{{providerquery.SeriesFleetBytesToClients, providerquery.MetricSubjectFleet, fleet, 0.1}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Rollup(ctx, hour); err != nil {
		t.Fatalf("roll-up refused by the ordering CHECK: %v", err)
	}
}

func TestPrune_ByRetention(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s := &Sampler{DB: db, SampleRetention: time.Hour, RollupRetention: 48 * time.Hour}
	for _, at := range []time.Time{now.Add(-61 * time.Minute), now.Add(-60 * time.Minute), now.Add(-time.Minute)} {
		if err := s.Write(ctx, at, []Sample{{providerquery.SeriesFleetConnections, providerquery.MetricSubjectFleet, fleet, 1}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []time.Time{now.Add(-49 * time.Hour), now.Add(-48 * time.Hour)} {
		if _, err := db.Exec(ctx, `INSERT INTO metric_rollups VALUES ('fleet.connections','fleet',$1,$2,1,1,1,1)`, fleet, h); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Prune(ctx, now); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, db, `SELECT count(*) FROM metric_samples`); n != 2 {
		t.Fatalf("%d samples left, want 2 (the one exactly at the boundary is kept)", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM metric_rollups`); n != 1 {
		t.Fatalf("%d roll-ups left, want 1", n)
	}
}

func TestCollect_FromRealRows(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	var ws, other string
	if err := db.QueryRow(ctx, `INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ('m1-a','m1-a','active','m1-a.zecurity.test') RETURNING id::text`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ('m1-gone','m1-gone','deleted','m1-gone.zecurity.test') RETURNING id::text`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	var user, network, conn string
	if err := db.QueryRow(ctx, `INSERT INTO users (tenant_id, email, provider, provider_sub, role, status) VALUES ($1,'u@m1.test','test','sub-m1','member','active') RETURNING id::text`, ws).Scan(&user); err != nil {
		t.Fatal(err)
	}
	// Devices: one live session, one idle-expired, one revoked.
	for _, d := range []struct {
		name      string
		idle, max time.Duration
		revoked   bool
	}{{"live", time.Hour, 2 * time.Hour, false}, {"idle", -time.Minute, time.Hour, false}, {"revoked", time.Hour, time.Hour, true}} {
		if _, err := db.Exec(ctx, `INSERT INTO client_devices (user_id, workspace_id, name, os, session_idle_until, session_max_until, revoked_at)
		                            VALUES ($1,$2,$3,'linux',$4,$5, CASE WHEN $6 THEN NOW() END)`,
			user, ws, d.name, now.Add(d.idle), now.Add(d.max), d.revoked); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.QueryRow(ctx, `INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1,'n','other') RETURNING id::text`, ws).Scan(&network); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO connectors (tenant_id, remote_network_id, name, status) VALUES ($1,$2,'c1','active') RETURNING id::text`, ws, network).Scan(&conn); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO connectors (tenant_id, remote_network_id, name, status) VALUES ($1,$2,'c2','disconnected')`, ws, network); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status) VALUES ($1,$2,$3,'s1','active')`, ws, network, conn); err != nil {
		t.Fatal(err)
	}
	// Relays: one active with rates, one inactive.
	var active, offline string
	if err := db.QueryRow(ctx, `INSERT INTO relays (name, dns_allowlist, ip_allowlist) VALUES ('m1-r1','{}','{}') RETURNING id::text`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE relays SET status='active', connection_count=5, connection_count_at=NOW(),
	        bytes_to_client_per_sec=2000, bytes_to_connector_per_sec=20, bytes_rate_window_seconds=300, bytes_sampled_at=NOW(),
	        bytes_to_client_total=1, bytes_to_connector_total=1, last_heartbeat_at=NOW() WHERE id=$1`, active); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO relays (name, dns_allowlist, ip_allowlist) VALUES ('m1-r2','{}','{}') RETURNING id::text`).Scan(&offline); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `UPDATE relays SET status='inactive', connection_count=7 WHERE id=$1`, offline); err != nil {
		t.Fatal(err)
	}

	s := &Sampler{DB: db}
	samples, err := s.Collect(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	m := index(samples)
	checks := []struct {
		series, subject string
		want            float64
	}{
		{providerquery.SeriesTenantActiveSessions, ws, 1},
		{providerquery.SeriesTenantConnectorsOnline, ws, 1},
		{providerquery.SeriesTenantShieldsOnline, ws, 1},
		{providerquery.SeriesRelayConnections, active, 5},
		{providerquery.SeriesRelayBytesToClients, active, 2000},
		{providerquery.SeriesRelayBytesToConnectors, active, 20},
		{providerquery.SeriesFleetConnections, fleet, 5},
		{providerquery.SeriesFleetActiveSessions, fleet, 1},
	}
	for _, c := range checks {
		got, ok := m[key{c.series, c.subject}]
		if !ok || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s/%s = %v (present %v), want %v", c.series, c.subject, got, ok, c.want)
		}
	}
	if has(m, providerquery.SeriesRelayConnections, offline) {
		t.Error("an offline relay was sampled (must be a gap)")
	}
	if has(m, providerquery.SeriesTenantActiveSessions, other) {
		t.Error("a deleted workspace was sampled")
	}
	if has(m, providerquery.SeriesTenantRelayStreams, ws) {
		t.Error("no R1 source → no tenant relay series")
	}
	// And they store.
	if err := s.Write(ctx, now, samples); err != nil {
		t.Fatal(err)
	}
}
