package provider

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ── fixtures ──────────────────────────────────────────────────────────────────

// mkOperator creates an operator with a real password hash.
func mkOperator(t *testing.T, s *Store, email, role string) *ProviderUser {
	t.Helper()
	ctx := context.Background()
	u, err := s.Create(ctx, email, role)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := HashPassword(ctx, "operator-password-1")
	if err != nil {
		t.Fatal(err)
	}
	if u, err = s.SetPassword(ctx, u.ID, hash, false); err != nil {
		t.Fatal(err)
	}
	return u
}

func actorFor(u *ProviderUser) AuditActor {
	return AuditActor{UserID: u.ID, Email: u.Email, IP: "203.0.113.9"}
}

func reload(t *testing.T, s *Store, id string) *ProviderUser {
	t.Helper()
	u, err := s.GetByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func auditCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM provider_audit_logs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func lastAudit(t *testing.T, pool *pgxpool.Pool, action string) (actorID, details string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT provider_user_id::text, COALESCE(details::text, '')
           FROM provider_audit_logs WHERE action = $1 ORDER BY created_at DESC LIMIT 1`, action).Scan(&actorID, &details); err != nil {
		t.Fatalf("no %s audit row: %v", action, err)
	}
	return actorID, details
}

// ── validation ────────────────────────────────────────────────────────────────

func TestNormalizeOperatorEmail(t *testing.T) {
	for in, want := range map[string]string{
		"ops@inkyank.com":       "ops@inkyank.com",
		"  Ops@InkYank.COM  ":   "ops@inkyank.com",
		"a@b":                   "a@b",
		"first.last+x@corp.dev": "first.last+x@corp.dev",
	} {
		if got, err := NormalizeOperatorEmail(in); err != nil || got != want {
			t.Errorf("%q: got %q err=%v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{
		"", "   ", "no-at-sign", "@inkyank.com", "ops@", "a@b@c",
		"op s@inkyank.com", "ops@ink yank.com", "ops\t@inkyank.com",
		strings.Repeat("a", 250) + "@b.cd", // 255 chars
	} {
		if _, err := NormalizeOperatorEmail(bad); !errors.Is(err, ErrInvalidEmail) {
			t.Errorf("%q: err=%v, want ErrInvalidEmail", bad, err)
		}
	}
	if !ValidRole(RoleSuperAdmin) || !ValidRole(RoleRelayOps) || ValidRole("admin") || ValidRole("") {
		t.Fatal("ValidRole table wrong")
	}
}

// ── create ────────────────────────────────────────────────────────────────────

func TestCreateOperator_StoresTemporaryCredentialAndAudits(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	admin := mkOperator(t, s, "admin@inkyank.com", RoleSuperAdmin)

	u, err := s.CreateOperator(ctx, actorFor(admin), "ops@inkyank.com", RoleRelayOps, "$argon2id$temp")
	if err != nil {
		t.Fatal(err)
	}
	if u.Role != RoleRelayOps || !u.MustChangePassword || u.PasswordHash != "$argon2id$temp" || u.DisabledAt != nil {
		t.Fatalf("created row: %+v", u)
	}
	actorID, details := lastAudit(t, pool, AuditOperatorCreate)
	if actorID != admin.ID || !strings.Contains(details, `"role": "relay-ops"`) || strings.Contains(details, "argon2id") {
		t.Fatalf("create audit actor=%s details=%s", actorID, details)
	}
}

func TestCreateOperator_Duplicates(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	admin := mkOperator(t, s, "admin@inkyank.com", RoleSuperAdmin)
	active := mkOperator(t, s, "active@inkyank.com", RoleRelayOps)
	gone := mkOperator(t, s, "gone@inkyank.com", RoleRelayOps)
	if _, err := s.SetDisabled(ctx, actorFor(admin), gone.ID, true); err != nil {
		t.Fatal(err)
	}
	_ = active
	before := auditCount(t, pool)

	email, _ := NormalizeOperatorEmail("ACTIVE@InkYank.com") // case-insensitive via normalization
	if _, err := s.CreateOperator(ctx, actorFor(admin), email, RoleRelayOps, "$h"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("active duplicate: %v", err)
	}
	if _, err := s.CreateOperator(ctx, actorFor(admin), "gone@inkyank.com", RoleRelayOps, "$h"); !errors.Is(err, ErrExistsDisabled) {
		t.Fatalf("disabled duplicate: %v", err)
	}
	if _, err := s.CreateOperator(ctx, actorFor(admin), "new@inkyank.com", "admin", "$h"); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("invalid role: %v", err)
	}
	if after := auditCount(t, pool); after != before {
		t.Fatalf("refused creates wrote audit rows: %d -> %d", before, after)
	}
}

// ── role change ───────────────────────────────────────────────────────────────

func TestChangeRole_BumpsGenerationAndAudits(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	admin := mkOperator(t, s, "admin@inkyank.com", RoleSuperAdmin)
	ops := mkOperator(t, s, "ops@inkyank.com", RoleRelayOps)

	u, changed, err := s.ChangeRole(ctx, actorFor(admin), ops.ID, RoleSuperAdmin)
	if err != nil || !changed || u.Role != RoleSuperAdmin || u.SessionGeneration != ops.SessionGeneration+1 {
		t.Fatalf("promote: u=%+v changed=%v err=%v", u, changed, err)
	}
	if _, details := lastAudit(t, pool, AuditOperatorRoleChange); !strings.Contains(details, `"from": "relay-ops"`) || !strings.Contains(details, `"to": "super-admin"`) {
		t.Fatalf("role-change audit details: %s", details)
	}

	// Same role: no update, no bump, no audit.
	before := auditCount(t, pool)
	_, changed, err = s.ChangeRole(ctx, actorFor(admin), ops.ID, RoleSuperAdmin)
	if err != nil || changed {
		t.Fatalf("same-role: changed=%v err=%v", changed, err)
	}
	if got := reload(t, s, ops.ID); got.SessionGeneration != u.SessionGeneration || auditCount(t, pool) != before {
		t.Fatalf("same-role no-op bumped generation (%d->%d) or audited", u.SessionGeneration, got.SessionGeneration)
	}
}

// ── disable / enable ──────────────────────────────────────────────────────────

func TestSetDisabled_DisableThenEnableClearsCredential(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	admin := mkOperator(t, s, "admin@inkyank.com", RoleSuperAdmin)
	ops := mkOperator(t, s, "ops@inkyank.com", RoleRelayOps)

	changed, err := s.SetDisabled(ctx, actorFor(admin), ops.ID, true)
	disabled := reload(t, s, ops.ID)
	if err != nil || !changed || disabled.DisabledAt == nil || disabled.SessionGeneration != ops.SessionGeneration+1 {
		t.Fatalf("disable: changed=%v err=%v row=%+v", changed, err, disabled)
	}
	if disabled.PasswordHash != ops.PasswordHash {
		t.Fatal("disable must not touch the credential")
	}

	// Disable again: no-op.
	before := auditCount(t, pool)
	if changed, err := s.SetDisabled(ctx, actorFor(admin), ops.ID, true); err != nil || changed {
		t.Fatalf("re-disable: changed=%v err=%v", changed, err)
	}
	if got := reload(t, s, ops.ID); got.SessionGeneration != disabled.SessionGeneration || auditCount(t, pool) != before {
		t.Fatal("re-disable bumped generation or audited")
	}

	// Enable: the old credential is cleared, sessions invalidated, no password issued.
	changed, err = s.SetDisabled(ctx, actorFor(admin), ops.ID, false)
	enabled := reload(t, s, ops.ID)
	if err != nil || !changed || enabled.DisabledAt != nil {
		t.Fatalf("enable: changed=%v err=%v row=%+v", changed, err, enabled)
	}
	if enabled.PasswordHash != "" || enabled.MustChangePassword || enabled.SessionGeneration != disabled.SessionGeneration+1 {
		t.Fatalf("enable must clear the hash and bump the generation: %+v", enabled)
	}
	if _, details := lastAudit(t, pool, AuditOperatorEnable); !strings.Contains(details, `"credential_cleared": true`) {
		t.Fatalf("enable audit details: %s", details)
	}
}

// Critical: enabling an already-enabled operator must never modify or clear
// its password (and is a full no-op).
func TestSetDisabled_EnableActiveIsNoOpAndKeepsPassword(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	admin := mkOperator(t, s, "admin@inkyank.com", RoleSuperAdmin)
	ops := mkOperator(t, s, "ops@inkyank.com", RoleRelayOps)
	before := auditCount(t, pool)

	changed, err := s.SetDisabled(ctx, actorFor(admin), ops.ID, false)
	got := reload(t, s, ops.ID)
	if err != nil || changed {
		t.Fatalf("enable active: changed=%v err=%v", changed, err)
	}
	if got.PasswordHash != ops.PasswordHash || got.SessionGeneration != ops.SessionGeneration || auditCount(t, pool) != before {
		t.Fatalf("enable of an active operator modified it: before=%+v after=%+v", ops, got)
	}
}

// ── reset password ────────────────────────────────────────────────────────────

func TestResetOperatorPassword(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	admin := mkOperator(t, s, "admin@inkyank.com", RoleSuperAdmin)
	ops := mkOperator(t, s, "ops@inkyank.com", RoleRelayOps)

	if err := s.ResetOperatorPassword(ctx, actorFor(admin), ops.ID, "$argon2id$new"); err != nil {
		t.Fatal(err)
	}
	got := reload(t, s, ops.ID)
	if got.PasswordHash != "$argon2id$new" || !got.MustChangePassword || got.SessionGeneration != ops.SessionGeneration+1 {
		t.Fatalf("after reset: %+v", got)
	}
	if _, details := lastAudit(t, pool, AuditOperatorPasswordReset); strings.Contains(details, "argon2id") {
		t.Fatalf("credential material in audit: %s", details)
	}
}

// A disabled account gets no password: enable first, then reset.
func TestResetOperatorPassword_DisabledRefused(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	admin := mkOperator(t, s, "admin@inkyank.com", RoleSuperAdmin)
	ops := mkOperator(t, s, "ops@inkyank.com", RoleRelayOps)
	if _, err := s.SetDisabled(ctx, actorFor(admin), ops.ID, true); err != nil {
		t.Fatal(err)
	}
	before, beforeAudit := reload(t, s, ops.ID), auditCount(t, pool)

	if err := s.ResetOperatorPassword(ctx, actorFor(admin), ops.ID, "$argon2id$new"); !errors.Is(err, ErrAccountDisabled) {
		t.Fatalf("reset of disabled: %v, want ErrAccountDisabled", err)
	}
	if got := reload(t, s, ops.ID); got.PasswordHash != before.PasswordHash || got.SessionGeneration != before.SessionGeneration || auditCount(t, pool) != beforeAudit {
		t.Fatal("refused reset changed the account or audited")
	}
}

// ── guards ────────────────────────────────────────────────────────────────────

func TestOperatorGuards_CannotModifySelf(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	admin := mkOperator(t, s, "admin@inkyank.com", RoleSuperAdmin)
	mkOperator(t, s, "other-admin@inkyank.com", RoleSuperAdmin) // so the last-admin guard is not what fires
	me, before := actorFor(admin), auditCount(t, pool)

	if _, _, err := s.ChangeRole(ctx, me, admin.ID, RoleRelayOps); !errors.Is(err, ErrCannotModifySelf) {
		t.Errorf("self-demote: %v", err)
	}
	if _, err := s.SetDisabled(ctx, me, admin.ID, true); !errors.Is(err, ErrCannotModifySelf) {
		t.Errorf("self-disable: %v", err)
	}
	if err := s.ResetOperatorPassword(ctx, me, admin.ID, "$h"); !errors.Is(err, ErrCannotModifySelf) {
		t.Errorf("self-reset: %v", err)
	}
	if got := reload(t, s, admin.ID); got.Role != RoleSuperAdmin || got.DisabledAt != nil || got.SessionGeneration != admin.SessionGeneration || auditCount(t, pool) != before {
		t.Fatal("refused self-modification changed the account or audited")
	}
}

func TestOperatorGuards_LastSuperAdmin(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	only := mkOperator(t, s, "only-admin@inkyank.com", RoleSuperAdmin)
	// The store guards the invariant itself, independent of who the actor is.
	actor := actorFor(mkOperator(t, s, "ops@inkyank.com", RoleRelayOps))
	before := auditCount(t, pool)

	if _, _, err := s.ChangeRole(ctx, actor, only.ID, RoleRelayOps); !errors.Is(err, ErrLastSuperAdmin) {
		t.Errorf("demote last super-admin: %v", err)
	}
	if _, err := s.SetDisabled(ctx, actor, only.ID, true); !errors.Is(err, ErrLastSuperAdmin) {
		t.Errorf("disable last super-admin: %v", err)
	}
	if got := reload(t, s, only.ID); got.Role != RoleSuperAdmin || got.DisabledAt != nil || auditCount(t, pool) != before {
		t.Fatal("refused last-admin change modified the account or audited")
	}

	// With a second active super-admin the same operations are allowed.
	mkOperator(t, s, "second-admin@inkyank.com", RoleSuperAdmin)
	if _, changed, err := s.ChangeRole(ctx, actor, only.ID, RoleRelayOps); err != nil || !changed {
		t.Fatalf("demote with another super-admin present: changed=%v err=%v", changed, err)
	}
}

// ── atomicity ─────────────────────────────────────────────────────────────────

// A mutation and its audit row commit together: when the audit insert fails
// (here: the actor id violates the audit FK), the change is rolled back too.
func TestOperatorMutations_AtomicWithAudit(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	mkOperator(t, s, "admin@inkyank.com", RoleSuperAdmin)
	ops := mkOperator(t, s, "ops@inkyank.com", RoleRelayOps)
	ghost := AuditActor{UserID: "00000000-0000-0000-0000-00000000dead", Email: "ghost@inkyank.com"}
	before := auditCount(t, pool)

	if _, err := s.CreateOperator(ctx, ghost, "new@inkyank.com", RoleRelayOps, "$h"); err == nil {
		t.Fatal("create with a failing audit succeeded")
	}
	if _, err := s.GetByEmailForLogin(ctx, "new@inkyank.com"); !errors.Is(err, ErrProviderUserNotFound) {
		t.Fatal("create was not rolled back with its audit row")
	}
	if _, _, err := s.ChangeRole(ctx, ghost, ops.ID, RoleSuperAdmin); err == nil {
		t.Fatal("role change with a failing audit succeeded")
	}
	if _, err := s.SetDisabled(ctx, ghost, ops.ID, true); err == nil {
		t.Fatal("disable with a failing audit succeeded")
	}
	if err := s.ResetOperatorPassword(ctx, ghost, ops.ID, "$h"); err == nil {
		t.Fatal("reset with a failing audit succeeded")
	}
	got := reload(t, s, ops.ID)
	if got.Role != RoleRelayOps || got.DisabledAt != nil || got.PasswordHash != ops.PasswordHash ||
		got.SessionGeneration != ops.SessionGeneration || auditCount(t, pool) != before {
		t.Fatalf("mutations not rolled back with their audit rows: %+v", got)
	}
}

// ── concurrency (the last-super-admin invariant under races) ──────────────────

// Role change and disable lock the WHOLE active super-admin set before
// counting. Holding the lock on a third super-admin — neither actor nor target
// — must therefore block a demotion: only the set lock can cause that wait (the
// target-row lock alone would not). Fails if the set lock is removed.
func TestGuard_LastSuperAdmin_SerializedByRowLock(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	a := mkOperator(t, s, "a@inkyank.com", RoleSuperAdmin)
	b := mkOperator(t, s, "b@inkyank.com", RoleSuperAdmin)
	c := mkOperator(t, s, "c@inkyank.com", RoleSuperAdmin)

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, `SELECT id FROM provider_users WHERE id = $1 FOR UPDATE`, c.ID); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		changed bool
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		_, changed, err := s.ChangeRole(ctx, actorFor(a), b.ID, RoleRelayOps)
		done <- outcome{changed, err}
	}()
	select {
	case <-done:
		_ = holder.Rollback(ctx)
		t.Fatal("ChangeRole did not wait for the super-admin set lock (a third admin's row was locked)")
	case <-time.After(300 * time.Millisecond):
	}
	if err := holder.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case o := <-done:
		if o.err != nil || !o.changed {
			t.Fatalf("after the lock was released: changed=%v err=%v", o.changed, o.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ChangeRole still blocked after the lock was released")
	}
}

// Two super-admins demote (or disable) each other. The first transaction is
// paused right after taking the super-admin set lock, and the second starts
// while it is still open — a guaranteed overlap. With the lock the second waits,
// sees the committed change and is refused; without it both would succeed and
// leave zero super-admins. Exactly one succeeds and one active super-admin
// remains.
func TestGuard_LastSuperAdmin_Concurrent(t *testing.T) {
	for _, op := range []string{"demote", "disable"} {
		t.Run(op, func(t *testing.T) {
			s, pool := newTestStore(t)
			ctx := context.Background()
			a := mkOperator(t, s, "a@inkyank.com", RoleSuperAdmin)
			b := mkOperator(t, s, "b@inkyank.com", RoleSuperAdmin)

			entered := make(chan struct{})
			var once sync.Once
			testHookAfterSuperAdminLock = func() {
				first := false
				once.Do(func() { first = true })
				if first {
					close(entered)
					time.Sleep(300 * time.Millisecond)
				}
			}
			t.Cleanup(func() { testHookAfterSuperAdminLock = nil })

			run := func(actor, target *ProviderUser) error {
				if op == "demote" {
					_, _, err := s.ChangeRole(ctx, actorFor(actor), target.ID, RoleRelayOps)
					return err
				}
				_, err := s.SetDisabled(ctx, actorFor(actor), target.ID, true)
				return err
			}
			errs := make([]error, 2)
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); errs[0] = run(a, b) }()
			<-entered // the first transaction holds the set lock and is paused
			go func() { defer wg.Done(); errs[1] = run(b, a) }()
			wg.Wait()

			ok, last := 0, 0
			for _, err := range errs {
				switch {
				case err == nil:
					ok++
				case errors.Is(err, ErrLastSuperAdmin):
					last++
				default:
					t.Fatalf("unexpected error (deadlock?): %v", err)
				}
			}
			var active int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM provider_users WHERE role = 'super-admin' AND disabled_at IS NULL`).Scan(&active); err != nil {
				t.Fatal(err)
			}
			if ok != 1 || last != 1 || active != 1 {
				t.Fatalf("ok=%d last_super_admin=%d active super-admins=%d, want 1/1/1", ok, last, active)
			}
		})
	}
}
