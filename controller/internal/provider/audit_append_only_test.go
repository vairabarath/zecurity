package provider

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// F17: provider_audit_logs is append-only in the database (migration 051),
// and state-changing actions record the changed fields before and after.

func TestAuditLog_DatabaseRefusesUpdateDeleteTruncate(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	admin := mkOperator(t, s, "admin@inkyank.com", RoleSuperAdmin)
	uid := admin.ID
	if err := s.InsertAudit(ctx, AuditEntry{
		ProviderUserID: &uid, ProviderEmail: admin.Email, Action: "relay.drain",
		TargetType: "relay", TargetID: "r1", Details: map[string]any{"reason": "original"}, IPAddress: "203.0.113.9",
	}); err != nil {
		t.Fatalf("insert must still work: %v", err)
	}
	var id string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM provider_audit_logs WHERE target_id = 'r1'`).Scan(&id); err != nil {
		t.Fatal(err)
	}

	refused := []struct{ name, sql string }{
		{"update details", `UPDATE provider_audit_logs SET details = '{"reason":"rewritten"}' WHERE id = '` + id + `'`},
		{"update actor", `UPDATE provider_audit_logs SET provider_email = 'someone@else' WHERE id = '` + id + `'`},
		{"update all rows", `UPDATE provider_audit_logs SET ip_address = NULL`},
		{"delete one", `DELETE FROM provider_audit_logs WHERE id = '` + id + `'`},
		{"delete all", `DELETE FROM provider_audit_logs`},
		{"truncate", `TRUNCATE provider_audit_logs`},
		{"truncate via cascade", `TRUNCATE provider_users CASCADE`},
	}
	for _, c := range refused {
		_, err := pool.Exec(ctx, c.sql)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" || !strings.Contains(pgErr.Message, "append-only") {
			t.Fatalf("%s: err=%v, want SQLSTATE 42501 append-only", c.name, err)
		}
	}

	var email, details string
	var n int
	if err := pool.QueryRow(ctx, `SELECT provider_email, details::text FROM provider_audit_logs WHERE id = $1`, id).Scan(&email, &details); err != nil {
		t.Fatalf("row gone after refused statements: %v", err)
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM provider_users`).Scan(&n)
	if email != admin.Email || !strings.Contains(details, "original") || n != 1 {
		t.Fatalf("row or users changed: email=%s details=%s users=%d", email, details, n)
	}
}

// auditChange returns the before/after of the newest row for action.
func auditChange(t *testing.T, pool *pgxpool.Pool, action string) (before, after map[string]any, raw string) {
	t.Helper()
	// Not lastAudit: system actions (recover_admin) have no provider_user_id.
	if err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(details::text, '') FROM provider_audit_logs WHERE action = $1 ORDER BY created_at DESC LIMIT 1`,
		action).Scan(&raw); err != nil {
		t.Fatalf("no %s audit row: %v", action, err)
	}
	var d struct {
		Before map[string]any `json:"before"`
		After  map[string]any `json:"after"`
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("%s details: %v", action, err)
	}
	return d.Before, d.After, raw
}

func wantChange(t *testing.T, pool *pgxpool.Pool, action string, before, after map[string]any) {
	t.Helper()
	b, a, raw := auditChange(t, pool, action)
	if !reflect.DeepEqual(b, before) || !reflect.DeepEqual(a, after) {
		t.Fatalf("%s before/after:\n got  %v -> %v\n want %v -> %v\n (%s)", action, b, a, before, after, raw)
	}
	for _, secret := range []string{"argon2id", "password_hash", "ciphertext", "nonce", "secret"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("%s details contain %q: %s", action, secret, raw)
		}
	}
}

func TestAuditChange_OperatorLifecycle(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	admin := mkOperator(t, s, "admin@inkyank.com", RoleSuperAdmin)
	act := actorFor(admin)

	hash, err := HashPassword(ctx, "temporary-password-1")
	if err != nil {
		t.Fatal(err)
	}
	ops, err := s.CreateOperator(ctx, act, "ops@inkyank.com", RoleRelayOps, hash)
	if err != nil {
		t.Fatal(err)
	}
	wantChange(t, pool, AuditOperatorCreate, nil,
		map[string]any{"role": RoleRelayOps, "status": "active", "must_change_password": true})

	if _, _, err := s.ChangeRole(ctx, act, ops.ID, RoleSuperAdmin); err != nil {
		t.Fatal(err)
	}
	wantChange(t, pool, AuditOperatorRoleChange,
		map[string]any{"role": RoleRelayOps}, map[string]any{"role": RoleSuperAdmin})

	if _, err := s.SetDisabled(ctx, act, ops.ID, true); err != nil {
		t.Fatal(err)
	}
	wantChange(t, pool, AuditOperatorDisable, map[string]any{"status": "active"}, map[string]any{"status": "disabled"})

	if _, err := s.SetDisabled(ctx, act, ops.ID, false); err != nil {
		t.Fatal(err)
	}
	wantChange(t, pool, AuditOperatorEnable, map[string]any{"status": "disabled"}, map[string]any{"status": "active"})

	// Enabled with a cleared credential (must_change_password false); a reset sets it.
	if err := s.ResetOperatorPassword(ctx, act, ops.ID, hash); err != nil {
		t.Fatal(err)
	}
	wantChange(t, pool, AuditOperatorPasswordReset,
		map[string]any{"must_change_password": false}, map[string]any{"must_change_password": true})
}

func TestAuditChange_TOTPEnrolResetAndRecover(t *testing.T) {
	s, pool := newTestStore(t)
	ctx := context.Background()
	admin := mkOperator(t, s, "admin@inkyank.com", RoleSuperAdmin)
	other := mkOperator(t, s, "other@inkyank.com", RoleSuperAdmin)

	// Enrol: pending -> enabled.
	if err := s.SetPendingTOTP(ctx, other.ID, "c2VhbGVk", "bm9uY2U="); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmTOTP(ctx, other.ID, 1000, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	wantChange(t, pool, AuditTOTPEnroll, map[string]any{"totp": "pending"}, map[string]any{"totp": "enabled"})

	// Reset by another super-admin: enabled -> none.
	if err := s.ResetOperatorTOTP(ctx, actorFor(admin), other.ID, "lost phone"); err != nil {
		t.Fatal(err)
	}
	wantChange(t, pool, AuditTOTPReset, map[string]any{"totp": "enabled"}, map[string]any{"totp": "none"})

	// Reset of a pending (unconfirmed) enrolment: pending -> none.
	if err := s.SetPendingTOTP(ctx, other.ID, "c2VhbGVk", "bm9uY2U="); err != nil {
		t.Fatal(err)
	}
	if err := s.ResetOperatorTOTP(ctx, actorFor(admin), other.ID, "abandoned enrolment"); err != nil {
		t.Fatal(err)
	}
	wantChange(t, pool, AuditTOTPReset, map[string]any{"totp": "pending"}, map[string]any{"totp": "none"})

	// Break-glass recovery of an enrolled super-admin.
	if err := s.SetPendingTOTP(ctx, admin.ID, "c2VhbGVk", "bm9uY2U="); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConfirmTOTP(ctx, admin.ID, 1000, "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	hash, err := HashPassword(ctx, "recovery-password-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecoverAdmin(ctx, admin.Email, hash); err != nil {
		t.Fatal(err)
	}
	wantChange(t, pool, AuditRecoverAdmin,
		map[string]any{"totp": "enabled", "must_change_password": false},
		map[string]any{"totp": "none", "must_change_password": true})
}
