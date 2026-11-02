package rbac

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func newCheckerDB(t *testing.T) (*pgxpool.Pool, string, string) {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := "rbac_checker_" + strconv.Itoa(os.Getpid()) + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName) })
	u, _ := url.Parse(adminDSN)
	u.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	files, _ := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	sort.Strings(files)
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	var ws, user string
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (slug, name, status, trust_domain)
		VALUES ('chk', 'chk', 'active', 'chk.test') RETURNING id::text`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		VALUES ($1, 'd@chk.test', 'google', 'd', 'devops', 'active') RETURNING id::text`, ws).Scan(&user); err != nil {
		t.Fatal(err)
	}
	return pool, ws, user
}

// Migration 047: every console role is accepted, anything else is not.
func TestMigration047RoleCheck(t *testing.T) {
	pool, ws, user := newCheckerDB(t)
	ctx := context.Background()
	for _, r := range Roles {
		if _, err := pool.Exec(ctx, `UPDATE users SET role = $2 WHERE id = $1`, user, r); err != nil {
			t.Fatalf("role %s rejected: %v", r, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_members (workspace_id, email, role, status)
			VALUES ($1, $2, $3, 'invited')`, ws, r+"@chk.test", r); err != nil {
			t.Fatalf("workspace_members role %s rejected: %v", r, err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'owner' WHERE id = $1`, user); err == nil {
		t.Fatal("role 'owner' accepted")
	}
}

func TestChecker_TTLBackstopAndInvalidate(t *testing.T) {
	pool, ws, user := newCheckerDB(t)
	ctx := context.Background()
	now := time.Now()
	c := NewChecker(pool)
	c.now = func() time.Time { return now }

	if r, err := c.Role(ctx, ws, user); err != nil || r != RoleDevOps {
		t.Fatalf("Role = %q, %v", r, err)
	}
	// A direct SQL change is not seen until the TTL backstop...
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'viewer' WHERE id = $1`, user); err != nil {
		t.Fatal(err)
	}
	if r, _ := c.Role(ctx, ws, user); r != RoleDevOps {
		t.Fatalf("cached role = %q, want devops", r)
	}
	now = now.Add(DefaultTTL + time.Second)
	if r, _ := c.Role(ctx, ws, user); r != RoleViewer {
		t.Fatalf("after TTL role = %q, want viewer", r)
	}
	// ...but Invalidate / InvalidateTenant apply at once.
	if _, err := pool.Exec(ctx, `UPDATE users SET role = 'member' WHERE id = $1`, user); err != nil {
		t.Fatal(err)
	}
	c.Invalidate(ws, user)
	if r, _ := c.Role(ctx, ws, user); r != RoleMember {
		t.Fatalf("after Invalidate role = %q, want member", r)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'suspended' WHERE id = $1`, user); err != nil {
		t.Fatal(err)
	}
	c.InvalidateTenant(ws)
	if _, err := c.Role(ctx, ws, user); err != ErrNoAccess {
		t.Fatalf("suspended user: err = %v, want ErrNoAccess", err)
	}
}

// An invalidation that lands between a lookup's DB read and its cache store
// must win: the stale read is not cached.
func TestChecker_InvalidateRacingLookupIsNotOverwritten(t *testing.T) {
	pool, ws, user := newCheckerDB(t)
	ctx := context.Background()
	for _, tenantWide := range []bool{false, true} {
		c := NewChecker(pool)
		if _, err := pool.Exec(ctx, `UPDATE users SET role = 'devops', status = 'active' WHERE id = $1`, user); err != nil {
			t.Fatal(err)
		}
		c.afterRead = func() {
			c.afterRead = nil
			if _, err := pool.Exec(ctx, `UPDATE users SET role = 'member' WHERE id = $1`, user); err != nil {
				t.Error(err)
			}
			if tenantWide {
				c.InvalidateTenant(ws)
			} else {
				c.Invalidate(ws, user)
			}
		}
		if r, _ := c.Role(ctx, ws, user); r != RoleDevOps {
			t.Fatalf("in-flight read = %q, want devops (read before the change)", r)
		}
		if r, _ := c.Role(ctx, ws, user); r != RoleMember {
			t.Fatalf("tenantWide=%v: next read = %q, want member (stale role was cached)", tenantWide, r)
		}
	}
}

func TestChecker_FailClosed(t *testing.T) {
	pool, ws, user := newCheckerDB(t)
	ctx := context.Background()
	c := NewChecker(pool)
	if _, err := c.Role(ctx, ws, "00000000-0000-0000-0000-000000000000"); err != ErrNoAccess {
		t.Fatalf("missing user: %v", err)
	}
	if _, err := c.Role(ctx, "", user); err != ErrNoAccess {
		t.Fatalf("empty tenant: %v", err)
	}
	pool.Close()
	if _, err := c.Role(ctx, ws, user); err == nil {
		t.Fatal("lookup on a closed pool must error")
	}
}

func TestLastAdminCheck(t *testing.T) {
	for _, tc := range []struct {
		admins  []string
		removed []string
		want    error
	}{
		{nil, []string{"a"}, nil},                              // no admins: non-admin target passes
		{[]string{"a"}, []string{"b"}, nil},                    // target is not an admin
		{[]string{"a"}, []string{"a"}, ErrLastAdmin},           // last admin
		{[]string{"a", "b"}, []string{"a"}, nil},               // another remains
		{[]string{"a", "b"}, []string{"a", "b"}, ErrLastAdmin}, // bulk removes all
		{[]string{"a", "b"}, []string{"a", "c"}, nil},          // bulk leaves b
	} {
		set := map[string]bool{}
		for _, id := range tc.removed {
			set[id] = true
		}
		if got := lastAdminCheck(tc.admins, set); got != tc.want {
			t.Errorf("admins=%v removed=%v: got %v, want %v", tc.admins, tc.removed, got, tc.want)
		}
	}
}
