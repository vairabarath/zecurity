package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Operator lifecycle (Sprint 21 Phase U; Decision Record D-27, D-25, D-14).
//
// Every mutation runs in ONE transaction together with its provider_audit_logs
// row: a failed audit rolls the change back, and a refused or failed change
// writes no audit row. Role change, disable, enable and password reset bump
// session_generation, so the affected operator's existing tokens are rejected
// on their next request (RequireProvider). Passwords never reach this layer in
// plaintext and never appear in audit details.
//
// Lock order: operations that can reduce the number of active super-admins
// (role change, disable) lock the active super-admin set FIRST, in id order,
// and the target row second. A consistent order means two concurrent demotions
// serialize (the second sees the first's commit) instead of deadlocking.

// AuditActor identifies who performed an operator mutation, for the audit row.
type AuditActor struct {
	UserID string
	Email  string
	IP     string
}

var (
	ErrCannotModifySelf = errors.New("operators cannot disable, demote or reset themselves")
	ErrLastSuperAdmin   = errors.New("change would leave no active super-admin")
	ErrAlreadyExists    = errors.New("provider user already exists")
	ErrExistsDisabled   = errors.New("provider user exists but is disabled; enable it instead")
	ErrAccountDisabled  = errors.New("provider user is disabled; enable it first")
	ErrInvalidRole      = errors.New("invalid provider role")
	ErrInvalidEmail     = errors.New("invalid email address")
)

// Operator audit actions (Phase U). Details never contain credentials.
const (
	AuditOperatorCreate        = "provider_user.create"
	AuditOperatorRoleChange    = "provider_user.role_change"
	AuditOperatorDisable       = "provider_user.disable"
	AuditOperatorEnable        = "provider_user.enable"
	AuditOperatorPasswordReset = "provider_user.password_reset"
)

const maxEmailLength = 254

// ValidRole reports whether role is one of the two provider roles (D-05).
func ValidRole(role string) bool {
	return role == RoleSuperAdmin || role == RoleRelayOps
}

// NormalizeOperatorEmail applies the lightweight Phase U validation — trimmed,
// lowercased, no whitespace, exactly one "@" with non-empty local and domain
// parts, at most 254 characters — and returns the normalized address. This is
// validation, not verification (there is no email infrastructure).
func NormalizeOperatorEmail(raw string) (string, error) {
	email := normalizeEmail(raw)
	if email == "" || len(email) > maxEmailLength || strings.ContainsAny(email, " \t\r\n") {
		return "", ErrInvalidEmail
	}
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") {
		return "", ErrInvalidEmail
	}
	return email, nil
}

// withTx runs fn in a transaction, committing only if fn returns nil.
func (s *Store) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// testHookAfterSuperAdminLock, when set by tests, runs right after a role
// change or disable has locked the active super-admin set. It lets the
// concurrency test force two transactions to overlap deterministically. Always
// nil in production.
var testHookAfterSuperAdminLock func()

func afterSuperAdminLock() {
	if testHookAfterSuperAdminLock != nil {
		testHookAfterSuperAdminLock()
	}
}

// lockActiveSuperAdmins locks every active super-admin row in id order and
// returns their ids.
func lockActiveSuperAdmins(ctx context.Context, tx pgx.Tx) ([]string, error) {
	rows, err := tx.Query(ctx,
		`SELECT id FROM provider_users
          WHERE role = 'super-admin' AND disabled_at IS NULL
          ORDER BY id
            FOR UPDATE`)
	if err != nil {
		return nil, fmt.Errorf("lock super-admins: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan super-admin: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// lockTarget loads and locks the target row (including disabled rows).
func lockTarget(ctx context.Context, tx pgx.Tx, id string) (*ProviderUser, error) {
	u, err := scanProviderUser(tx.QueryRow(ctx,
		`SELECT `+providerUserColumns+` FROM provider_users WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProviderUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock provider user: %w", err)
	}
	return u, nil
}

func (s *Store) auditOperator(ctx context.Context, tx pgx.Tx, actor AuditActor, action string, target *ProviderUser, details map[string]any) error {
	uid := actor.UserID
	return s.InsertAuditTx(ctx, tx, AuditEntry{
		ProviderUserID: &uid,
		ProviderEmail:  actor.Email,
		Action:         action,
		TargetType:     "provider_user",
		TargetID:       target.ID,
		Details:        details,
		IPAddress:      actor.IP,
	})
}

// CreateOperator inserts a new operator with a temporary-password hash and
// must_change_password = TRUE. The email must already be normalized
// (NormalizeOperatorEmail). An existing active email → ErrAlreadyExists; an
// existing disabled email → ErrExistsDisabled (enable it instead).
func (s *Store) CreateOperator(ctx context.Context, actor AuditActor, email, role, passwordHash string) (*ProviderUser, error) {
	if !ValidRole(role) {
		return nil, ErrInvalidRole
	}
	var created *ProviderUser
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		u, err := scanProviderUser(tx.QueryRow(ctx,
			`INSERT INTO provider_users (email, role, password_hash, must_change_password)
                  VALUES ($1, $2, $3, TRUE)
             ON CONFLICT (email) DO NOTHING
               RETURNING `+providerUserColumns,
			email, role, passwordHash))
		if errors.Is(err, pgx.ErrNoRows) {
			var disabled bool
			if err := tx.QueryRow(ctx,
				`SELECT disabled_at IS NOT NULL FROM provider_users WHERE email = $1`, email).Scan(&disabled); err != nil {
				return fmt.Errorf("classify existing provider user: %w", err)
			}
			if disabled {
				return ErrExistsDisabled
			}
			return ErrAlreadyExists
		}
		if err != nil {
			return fmt.Errorf("insert provider user: %w", err)
		}
		if err := s.auditOperator(ctx, tx, actor, AuditOperatorCreate, u,
			map[string]any{"email": u.Email, "role": u.Role}); err != nil {
			return err
		}
		created = u
		return nil
	})
	if err != nil {
		return nil, err
	}
	return created, nil
}

// ChangeRole sets the operator's role and bumps session_generation. The same
// role is a no-op (changed=false: no update, no audit, no bump). Self-changes
// and demoting the last active super-admin are refused.
func (s *Store) ChangeRole(ctx context.Context, actor AuditActor, id, role string) (*ProviderUser, bool, error) {
	if !ValidRole(role) {
		return nil, false, ErrInvalidRole
	}
	if id == actor.UserID {
		return nil, false, ErrCannotModifySelf
	}
	var (
		result  *ProviderUser
		changed bool
	)
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		active, err := lockActiveSuperAdmins(ctx, tx)
		if err != nil {
			return err
		}
		afterSuperAdminLock()
		target, err := lockTarget(ctx, tx, id)
		if err != nil {
			return err
		}
		if target.Role == role {
			result = target
			return nil
		}
		if target.Role == RoleSuperAdmin && target.DisabledAt == nil && len(active) <= 1 {
			return ErrLastSuperAdmin
		}
		u, err := scanProviderUser(tx.QueryRow(ctx,
			`UPDATE provider_users
                SET role = $2, session_generation = session_generation + 1, updated_at = NOW()
              WHERE id = $1
          RETURNING `+providerUserColumns, id, role))
		if err != nil {
			return fmt.Errorf("change provider role: %w", err)
		}
		if err := s.auditOperator(ctx, tx, actor, AuditOperatorRoleChange, u,
			map[string]any{"email": u.Email, "from": target.Role, "to": u.Role}); err != nil {
			return err
		}
		result, changed = u, true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return result, changed, nil
}

// SetDisabled disables or enables an operator.
//
//   - Disable: sets disabled_at and bumps session_generation. Refused for self
//     and for the last active super-admin.
//   - Enable: clears disabled_at, CLEARS password_hash (so the old credential
//     is never trusted again — the operator cannot sign in until an explicit
//     reset-password) and bumps session_generation. No password is generated.
//   - Already in the requested state: changed=false — no update, no audit, no
//     bump; in particular enabling an enabled operator never touches its password.
func (s *Store) SetDisabled(ctx context.Context, actor AuditActor, id string, disabled bool) (bool, error) {
	if disabled && id == actor.UserID {
		return false, ErrCannotModifySelf
	}
	var changed bool
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var active []string
		if disabled {
			var err error
			if active, err = lockActiveSuperAdmins(ctx, tx); err != nil {
				return err
			}
			afterSuperAdminLock()
		}
		target, err := lockTarget(ctx, tx, id)
		if err != nil {
			return err
		}
		if (target.DisabledAt != nil) == disabled {
			return nil // already in the requested state
		}
		var (
			action  string
			details = map[string]any{"email": target.Email}
		)
		if disabled {
			if target.Role == RoleSuperAdmin && len(active) <= 1 {
				return ErrLastSuperAdmin
			}
			if _, err := tx.Exec(ctx,
				`UPDATE provider_users
                    SET disabled_at = NOW(), session_generation = session_generation + 1, updated_at = NOW()
                  WHERE id = $1`, id); err != nil {
				return fmt.Errorf("disable provider user: %w", err)
			}
			action = AuditOperatorDisable
		} else {
			if _, err := tx.Exec(ctx,
				`UPDATE provider_users
                    SET disabled_at = NULL, password_hash = NULL, must_change_password = FALSE,
                        session_generation = session_generation + 1, updated_at = NOW()
                  WHERE id = $1`, id); err != nil {
				return fmt.Errorf("enable provider user: %w", err)
			}
			action = AuditOperatorEnable
			details["credential_cleared"] = true
		}
		if err := s.auditOperator(ctx, tx, actor, action, target, details); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return changed, nil
}

// ResetOperatorPassword stores a new temporary-password hash with
// must_change_password = TRUE and bumps session_generation. Refused for self
// (use POST /provider/auth/password) and for a disabled operator
// (ErrAccountDisabled — enable first, then reset).
func (s *Store) ResetOperatorPassword(ctx context.Context, actor AuditActor, id, passwordHash string) error {
	if id == actor.UserID {
		return ErrCannotModifySelf
	}
	return s.withTx(ctx, func(tx pgx.Tx) error {
		target, err := lockTarget(ctx, tx, id)
		if err != nil {
			return err
		}
		if target.DisabledAt != nil {
			return ErrAccountDisabled
		}
		if _, err := tx.Exec(ctx,
			`UPDATE provider_users
                SET password_hash = $2, must_change_password = TRUE, password_changed_at = NOW(),
                    session_generation = session_generation + 1, updated_at = NOW()
              WHERE id = $1`, id, passwordHash); err != nil {
			return fmt.Errorf("reset provider password: %w", err)
		}
		return s.auditOperator(ctx, tx, actor, AuditOperatorPasswordReset, target,
			map[string]any{"email": target.Email})
	})
}
