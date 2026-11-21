package usage

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Sprint 25 U1a (D-57, D-58, D-59): the usage ledger's constraints, over a
// throwaway database with every migration (PKI_TEST_DATABASE_URL).

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
	name := fmt.Sprintf("usage_test_%d_%d", os.Getpid(), time.Now().UnixNano())
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

const (
	tenantA = "aaaaaaaa-0000-0000-0000-0000000000a1"
	userA   = "aaaaaaaa-0000-0000-0000-0000000000b1"
	relayA  = "aaaaaaaa-0000-0000-0000-0000000000c1"
)

func TestUsageSchema_Constraints(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	// Every refusal runs in a session time zone off UTC by 5:30, so the
	// on-the-hour and on-the-minute checks are shown to be zone independent.
	if _, err := db.Exec(ctx, "ALTER DATABASE "+currentDB(t, db)+" SET timezone TO 'Asia/Kolkata'"); err != nil {
		t.Fatal(err)
	}
	db.Reset()

	refused := []struct {
		name, sql string
		args      []any
	}{
		{"unknown meter", `INSERT INTO usage_hourly VALUES ($1, 'bytes', '2026-11-21T10:00:00Z', 1)`, []any{tenantA}},
		{"negative quantity", `INSERT INTO usage_hourly VALUES ($1, 'session_minutes', '2026-11-21T10:00:00Z', -1)`, []any{tenantA}},
		{"hour not on the hour", `INSERT INTO usage_hourly VALUES ($1, 'session_minutes', '2026-11-21T10:30:00Z', 1)`, []any{tenantA}},
		{"hour with seconds", `INSERT INTO usage_hourly VALUES ($1, 'session_minutes', '2026-11-21T10:00:00.5Z', 1)`, []any{tenantA}},
		{"minute not on the minute", `INSERT INTO usage_collector_minutes VALUES ('2026-11-21T10:00:30Z')`, nil},
		{"active-user month not day 1", `INSERT INTO usage_active_users VALUES ($1, '2026-11-02', $2, now())`, []any{tenantA, userA}},
		{"count month not day 1", `INSERT INTO usage_active_user_counts VALUES ($1, '2026-11-15', 3)`, []any{tenantA}},
		{"negative user count", `INSERT INTO usage_active_user_counts VALUES ($1, '2026-11-01', -1)`, []any{tenantA}},
		{"negative baseline", `INSERT INTO usage_relay_baselines VALUES ($1, $2, now(), -1, 0, now())`, []any{relayA, tenantA}},
	}
	for _, c := range refused {
		if _, err := db.Exec(ctx, c.sql, c.args...); err == nil || !strings.Contains(err.Error(), "violates check constraint") {
			t.Errorf("%s: want a check-constraint refusal, got %v", c.name, err)
		}
	}

	// In UTC 10:00 is 15:30 in Kolkata; the row is accepted (on the UTC hour).
	if _, err := db.Exec(ctx, `INSERT INTO usage_hourly VALUES ($1, 'relay_bytes_to_clients', '2026-11-21T10:00:00Z', 100)`, tenantA); err != nil {
		t.Fatalf("on-the-hour row refused: %v", err)
	}
	// The collector's upsert adds to the hour.
	if _, err := db.Exec(ctx, `INSERT INTO usage_hourly VALUES ($1, 'relay_bytes_to_clients', '2026-11-21T10:00:00Z', 50)
		ON CONFLICT (tenant_id, meter, hour) DO UPDATE SET quantity = usage_hourly.quantity + EXCLUDED.quantity`, tenantA); err != nil {
		t.Fatal(err)
	}
	var q int64
	if err := db.QueryRow(ctx, `SELECT quantity FROM usage_hourly WHERE tenant_id = $1 AND meter = 'relay_bytes_to_clients'`, tenantA).Scan(&q); err != nil {
		t.Fatal(err)
	}
	if q != 150 {
		t.Fatalf("upsert: quantity %d, want 150", q)
	}

	// Each minute and each (tenant, month, user) once.
	if _, err := db.Exec(ctx, `INSERT INTO usage_collector_minutes VALUES ('2026-11-21T10:01:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO usage_collector_minutes VALUES ('2026-11-21T10:01:00Z')`); err == nil {
		t.Error("a minute recorded twice")
	}
	if _, err := db.Exec(ctx, `INSERT INTO usage_active_users VALUES ($1, '2026-11-01', $2, now())`, tenantA, userA); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO usage_active_users VALUES ($1, '2026-11-01', $2, now())`, tenantA, userA); err == nil {
		t.Error("a user counted twice in a month")
	}
	// A tenant with usage needs no workspace row (usage outlives a purge, D-58).
	if _, err := db.Exec(ctx, `INSERT INTO usage_tenants (tenant_id, name, slug, plan_name, purged_at) VALUES ($1, 'acme', 'acme', 'Free', now())`, tenantA); err != nil {
		t.Fatalf("usage_tenants for a purged tenant: %v", err)
	}
}

// The app role gets the access the collector and readers need, and no more:
// usage_collector_minutes is never updated.
func TestUsageSchema_Grants(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	want := map[string][]string{
		"usage_hourly":             {"SELECT", "INSERT", "UPDATE", "DELETE"},
		"usage_active_users":       {"SELECT", "INSERT", "UPDATE", "DELETE"},
		"usage_active_user_counts": {"SELECT", "INSERT", "UPDATE", "DELETE"},
		"usage_tenants":            {"SELECT", "INSERT", "UPDATE", "DELETE"},
		"usage_relay_baselines":    {"SELECT", "INSERT", "UPDATE", "DELETE"},
		"usage_collector_minutes":  {"SELECT", "INSERT", "DELETE"},
	}
	for table, privs := range want {
		var got []string
		for _, p := range []string{"DELETE", "INSERT", "SELECT", "UPDATE"} {
			var ok bool
			if err := db.QueryRow(ctx, `SELECT has_table_privilege('ztna_app', $1, $2)`, table, p).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if ok {
				got = append(got, p)
			}
		}
		sort.Strings(privs)
		if strings.Join(got, ",") != strings.Join(privs, ",") {
			t.Errorf("%s: ztna_app has %v, want %v", table, got, privs)
		}
	}
}

func currentDB(t *testing.T, db *pgxpool.Pool) string {
	t.Helper()
	var n string
	if err := db.QueryRow(context.Background(), "SELECT current_database()").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
