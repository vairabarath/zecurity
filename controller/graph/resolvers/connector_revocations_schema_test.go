package resolvers

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Connector revocation Phase F (O14, migration 041): a durable tombstone for
// revoked connector identities, and no reuse of a revoked identity.

func requireUniqueViolation(t *testing.T, err error, what string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" || !strings.Contains(pgErr.Message, "was revoked and cannot be reused") {
		t.Fatalf("%s: got %v, want the revoked-identity unique_violation", what, err)
	}
}

// AT-1: revoking writes the tombstone in the same statement; it outlives the
// deleted row; a revoked identity can't be inserted again or taken by an
// update of id. Active and pending connectors get no tombstone.
func TestConnectorRevocations_TombstoneAndNoReuse(t *testing.T) {
	f := newCertHistoryFixture(t)
	id := f.connector(t, "to-revoke", []string{"f1f1f1f1"}, valid)
	active := f.connector(t, "stays-active", []string{"f2f2f2f2"}, valid)
	if n := f.count(t, `connector_revocations WHERE connector_id = $1`, id); n != "0" {
		t.Fatal("tombstone before revocation")
	}

	if _, err := f.mr.RevokeConnector(f.ctx, id); err != nil {
		t.Fatalf("RevokeConnector: %v", err)
	}
	if n := f.count(t, `connector_revocations WHERE connector_id = $1 AND tenant_id = $2`, id, f.ws); n != "1" {
		t.Fatal("revocation must write the tombstone")
	}
	if ok, err := f.mr.DeleteConnector(f.ctx, id); err != nil || !ok {
		t.Fatalf("DeleteConnector: %v", err)
	}
	if n := f.count(t, `connector_revocations WHERE connector_id = $1`, id); n != "1" {
		t.Fatal("the tombstone must outlive the deleted row")
	}

	_, err := f.pool.Exec(f.ctx, `INSERT INTO connectors (id, tenant_id, remote_network_id, name, status, trust_domain)
		VALUES ($1, $2, $3, 'reused', 'pending', 'certhist.test')`, id, f.ws, f.rn)
	requireUniqueViolation(t, err, "insert with a revoked id")
	_, err = f.pool.Exec(f.ctx, `UPDATE connectors SET id = $1 WHERE id = $2`, id, active)
	requireUniqueViolation(t, err, "update id to a revoked id")

	pending := f.scalar(t, `INSERT INTO connectors (tenant_id, remote_network_id, name, status)
		VALUES ($1, $2, 'pending', 'pending') RETURNING id::text`, f.ws, f.rn)
	if ok, err := f.mr.DeleteConnector(f.ctx, pending); err != nil || !ok {
		t.Fatalf("DeleteConnector(pending): %v", err)
	}
	if n := f.count(t, `connector_revocations WHERE connector_id IN ($1, $2)`, active, pending); n != "0" {
		t.Fatal("active and pending connectors must not be tombstoned")
	}
}

// AT-2: a connector marked revoked by revoked_at alone (either marker counts,
// as in 039) is tombstoned too, once.
func TestConnectorRevocations_RevokedAtAloneCounts(t *testing.T) {
	f := newCertHistoryFixture(t)
	id := f.connector(t, "revoked-at-only", []string{"f3f3f3f3"}, valid)
	f.exec(t, `UPDATE connectors SET revoked_at = NOW() WHERE id = $1`, id)
	f.exec(t, `UPDATE connectors SET status = 'revoked' WHERE id = $1`, id)
	if n := f.count(t, `connector_revocations WHERE connector_id = $1`, id); n != "1" {
		t.Fatalf("tombstones = %s, want 1", n)
	}
}

// AT-3: the backfill tombstones connectors revoked before 041 — rows still
// present and revoked-then-deleted ones known only from the certificate
// history — and nothing else.
func TestConnectorRevocations_Backfill(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := "resolvers_connrev_" + strconv.Itoa(os.Getpid()) + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(adminPool.Close)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() { _, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName) })
	parsed, _ := url.Parse(adminDSN)
	parsed.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("test pool: %v", err)
	}
	t.Cleanup(pool.Close)

	files, _ := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	sort.Strings(files)
	apply := func(f string) {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	var target string
	for _, f := range files {
		if filepath.Base(f) >= "041_" {
			if strings.HasPrefix(filepath.Base(f), "041_") {
				target = f
			}
			continue
		}
		apply(f)
	}
	if target == "" {
		t.Fatal("041 migration not found")
	}

	q := func(sql string, args ...any) string {
		t.Helper()
		var v string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return v
	}
	ws := q(`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ('bf', 'bf', 'active', 'bf.test') RETURNING id::text`)
	rn := q(`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'rn', 'office') RETURNING id::text`, ws)
	mk := func(name, status string) string {
		id := q(`INSERT INTO connectors (tenant_id, remote_network_id, name, status, trust_domain)
			VALUES ($1, $2, $3, 'active', 'bf.test') RETURNING id::text`, ws, rn, name)
		if _, err := pool.Exec(ctx, `UPDATE connectors SET cert_serial = $1, cert_not_after = NOW() + interval '7 days' WHERE id = $2`, "s-"+name, id); err != nil {
			t.Fatal(err)
		}
		if status == "revoked" {
			if _, err := pool.Exec(ctx, `UPDATE connectors SET status = 'revoked', revoked_at = NOW() WHERE id = $1`, id); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	revokedRow := mk("revoked-row", "revoked")
	revokedDeleted := mk("revoked-deleted", "revoked")
	active := mk("active", "active")
	if _, err := pool.Exec(ctx, `DELETE FROM connectors WHERE id = $1`, revokedDeleted); err != nil {
		t.Fatal(err)
	}

	apply(target)

	got := q(`SELECT COALESCE(string_agg(connector_id::text, ',' ORDER BY connector_id::text), '') FROM connector_revocations`)
	want := []string{revokedRow, revokedDeleted}
	sort.Strings(want)
	if got != strings.Join(want, ",") {
		t.Fatalf("backfilled tombstones = %q, want %q (active %s must be absent)", got, strings.Join(want, ","), active)
	}
	if n := q(`SELECT count(*)::text FROM connector_revocations WHERE tenant_id = $1`, ws); n != "2" {
		t.Fatalf("tombstones in the workspace = %s, want 2", n)
	}
}
