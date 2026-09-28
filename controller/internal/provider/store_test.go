package provider

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

	"github.com/jackc/pgx/v5/pgxpool"
)

// newTestStore creates a throwaway database (PKI_TEST_DATABASE_URL is the admin
// DSN), applies every schema file in controller/migrations in order, and drops
// the database when the test ends. It never touches the dev database.
func newTestStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("provider_test_%d_%d", os.Getpid(), time.Now().UnixNano())

	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}

	parsed, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
		admin.Close()
	})

	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list migrations: %v (%d files)", err, len(files))
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
	return NewStore(pool), pool
}

func countAudit(t *testing.T, pool *pgxpool.Pool, action string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM provider_audit_logs WHERE action = $1`, action).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStore_ReadsIncludeDisabledWhereNeeded(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	u, err := s.Create(ctx, "Ops@InkYank.com", RoleRelayOps)
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "ops@inkyank.com" || u.SessionGeneration != 1 || u.MustChangePassword || u.PasswordHash != "" {
		t.Fatalf("new row defaults: %+v", u)
	}
	if err := s.Disable(ctx, u.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := s.GetByEmail(ctx, "ops@inkyank.com"); err != ErrProviderUserNotFound {
		t.Fatalf("GetByEmail must hide disabled rows, got %v", err)
	}
	byID, err := s.GetByID(ctx, u.ID)
	if err != nil || byID.DisabledAt == nil {
		t.Fatalf("GetByID must return disabled rows: %+v %v", byID, err)
	}
	forLogin, err := s.GetByEmailForLogin(ctx, "OPS@inkyank.com")
	if err != nil || forLogin.DisabledAt == nil {
		t.Fatalf("GetByEmailForLogin must return disabled rows: %+v %v", forLogin, err)
	}
	if _, err := s.GetByID(ctx, "00000000-0000-0000-0000-000000000000"); err != ErrProviderUserNotFound {
		t.Fatalf("unknown id: %v", err)
	}
}

// SetPassword bumps session_generation in the same statement, so a password
// change or reset always invalidates every existing session (AT-CORE-2).
func TestStore_SetPassword_BumpsGeneration(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	u, _ := s.Create(ctx, "p@inkyank.com", RoleSuperAdmin)

	updated, err := s.SetPassword(ctx, u.ID, "$argon2id$fake", true)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SessionGeneration != u.SessionGeneration+1 || !updated.MustChangePassword || updated.PasswordHash != "$argon2id$fake" {
		t.Fatalf("after SetPassword: %+v", updated)
	}
	again, _ := s.SetPassword(ctx, u.ID, "$argon2id$fake2", false)
	if again.SessionGeneration != u.SessionGeneration+2 || again.MustChangePassword {
		t.Fatalf("second SetPassword: %+v", again)
	}
}

func TestStore_LogoutAndDisable_BumpGeneration(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	u, _ := s.Create(ctx, "g@inkyank.com", RoleRelayOps)

	if err := s.BumpSessionGeneration(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Disable(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetByID(ctx, u.ID)
	if got.SessionGeneration != u.SessionGeneration+2 {
		t.Fatalf("generation = %d, want %d", got.SessionGeneration, u.SessionGeneration+2)
	}
	if err := s.BumpSessionGeneration(ctx, "00000000-0000-0000-0000-000000000000"); err != ErrProviderUserNotFound {
		t.Fatalf("unknown id: %v", err)
	}
	if err := s.RecordLogin(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetByID(ctx, u.ID); got.LastLoginAt == nil {
		t.Fatal("RecordLogin did not set last_login_at")
	}
}

func TestBootstrap_CreateOnly(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	res, u, err := s.CreateBootstrapSuperAdminIfNone(ctx, "Barath@InkYank.com", "$argon2id$first")
	if err != nil || res != BootstrapCreated {
		t.Fatalf("first bootstrap: res=%v err=%v", res, err)
	}
	if u.Email != "barath@inkyank.com" || u.Role != RoleSuperAdmin || !u.MustChangePassword || u.PasswordHash != "$argon2id$first" {
		t.Fatalf("bootstrap row: %+v", u)
	}
	if n := countAudit(t, pool, AuditBootstrapCreate); n != 1 {
		t.Fatalf("bootstrap audit rows = %d, want 1", n)
	}

	// A second start with a different password changes nothing.
	res, _, err = s.CreateBootstrapSuperAdminIfNone(ctx, "barath@inkyank.com", "$argon2id$second")
	if err != nil || res != BootstrapSkippedAdminExists {
		t.Fatalf("second bootstrap: res=%v err=%v", res, err)
	}
	got, _ := s.GetByID(ctx, u.ID)
	if got.PasswordHash != "$argon2id$first" || got.SessionGeneration != u.SessionGeneration {
		t.Fatalf("existing account changed by bootstrap: %+v", got)
	}
	if n := countAudit(t, pool, AuditBootstrapCreate); n != 1 {
		t.Fatalf("bootstrap audit rows = %d after skip, want 1", n)
	}
}

// Once any super-admin exists — even a disabled one — bootstrap is ignored,
// whatever email it names.
func TestBootstrap_IgnoredOnceSuperAdminExists(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	admin, _ := s.Create(ctx, "admin@inkyank.com", RoleSuperAdmin)
	_ = s.Disable(ctx, admin.ID)
	before, _ := s.GetByID(ctx, admin.ID)

	for _, email := range []string{"new-person@inkyank.com", "admin@inkyank.com"} {
		res, u, err := s.CreateBootstrapSuperAdminIfNone(ctx, email, "$argon2id$x")
		if err != nil || res != BootstrapSkippedAdminExists || u != nil {
			t.Fatalf("%s: res=%v u=%v err=%v", email, res, u, err)
		}
	}
	after, _ := s.GetByID(ctx, admin.ID)
	if after.DisabledAt == nil || after.PasswordHash != before.PasswordHash || after.SessionGeneration != before.SessionGeneration {
		t.Fatalf("existing super-admin changed: before=%+v after=%+v", before, after)
	}
	if _, err := s.GetByEmail(ctx, "new-person@inkyank.com"); err != ErrProviderUserNotFound {
		t.Fatal("bootstrap created an account although a super-admin exists")
	}
}

// With no super-admin, an existing relay-ops row with the bootstrap email is
// never promoted, overwritten or re-enabled.
func TestBootstrap_NoPromotionOfExistingRow(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	ops, _ := s.Create(ctx, "ops@inkyank.com", RoleRelayOps)

	res, u, err := s.CreateBootstrapSuperAdminIfNone(ctx, "ops@inkyank.com", "$argon2id$x")
	if err != nil || res != BootstrapEmailTaken || u != nil {
		t.Fatalf("res=%v u=%v err=%v", res, u, err)
	}
	got, _ := s.GetByID(ctx, ops.ID)
	if got.Role != RoleRelayOps || got.PasswordHash != "" || got.MustChangePassword {
		t.Fatalf("existing row modified: %+v", got)
	}
}

// The bootstrap transaction takes the bootstrap advisory lock, so while another
// session holds it a bootstrap attempt blocks instead of racing. This proves
// the serialization structurally (a timing-based race test alone can pass
// without the lock).
func TestBootstrap_SerializedByAdvisoryLock(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, bootstrapLockKey); err != nil {
		t.Fatal(err)
	}

	done := make(chan BootstrapResult, 1)
	go func() {
		res, _, err := s.CreateBootstrapSuperAdminIfNone(ctx, "waiter@inkyank.com", "$argon2id$x")
		if err != nil {
			t.Errorf("bootstrap: %v", err)
		}
		done <- res
	}()

	select {
	case <-done:
		_ = holder.Rollback(ctx)
		t.Fatal("bootstrap did not wait for the advisory lock")
	case <-time.After(300 * time.Millisecond):
	}
	if err := holder.Rollback(ctx); err != nil { // releases the xact lock
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if res != BootstrapCreated {
			t.Fatalf("after the lock was released: %v, want BootstrapCreated", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap still blocked after the lock was released")
	}
}

// Concurrent bootstrap attempts (even with different emails) create exactly
// one super-admin.
func TestBootstrap_SingleStatement(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	results := make([]BootstrapResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _, errs[i] = s.CreateBootstrapSuperAdminIfNone(ctx, fmt.Sprintf("racer%d@inkyank.com", i), "$argon2id$x")
		}(i)
	}
	wg.Wait()

	created := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("attempt %d: %v", i, errs[i])
		}
		if results[i] == BootstrapCreated {
			created++
		}
	}
	var admins int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM provider_users WHERE role = 'super-admin'`).Scan(&admins); err != nil {
		t.Fatal(err)
	}
	if created != 1 || admins != 1 {
		t.Fatalf("created=%d super-admins=%d, want exactly 1", created, admins)
	}
}
