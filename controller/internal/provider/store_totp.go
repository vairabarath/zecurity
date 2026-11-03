package provider

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// TOTP state in provider_users (Sprint 22 Phase T, 042_provider_totp.sql).
// Like operator mutations, every credential change commits together with its
// audit row and a session_generation bump, so the affected operator's tokens
// stop working on their next request.

var (
	// ErrTOTPAlreadyEnabled: enrolment can't restart once TOTP is confirmed.
	ErrTOTPAlreadyEnabled = errors.New("totp already enabled")
	// ErrTOTPNotStarted: confirm without a pending secret.
	ErrTOTPNotStarted = errors.New("totp enrolment not started")
	// ErrTOTPNotEnrolled: reset of an operator with no TOTP (pending or enabled).
	ErrTOTPNotEnrolled = errors.New("operator has no totp enrolment")
	// ErrNotSuperAdmin: recover-admin targets super-admins only.
	ErrNotSuperAdmin = errors.New("provider user is not a super-admin")
)

// TOTPState is the sealed secret and the replay marker of one operator.
type TOTPState struct {
	Ciphertext string
	Nonce      string
	EnabledAt  *time.Time
	LastStep   *int64
}

// GetTOTPState loads an operator's TOTP state. Ciphertext is "" when there is
// no secret (neither pending nor enabled).
func (s *Store) GetTOTPState(ctx context.Context, id string) (*TOTPState, error) {
	st := &TOTPState{}
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(totp_secret_ciphertext, ''), COALESCE(totp_secret_nonce, ''), totp_enabled_at, totp_last_step
           FROM provider_users WHERE id = $1`, id,
	).Scan(&st.Ciphertext, &st.Nonce, &st.EnabledAt, &st.LastStep)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProviderUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get totp state: %w", err)
	}
	return st, nil
}

// SetPendingTOTP stores a new sealed secret for an operator who hasn't
// confirmed TOTP yet, replacing any earlier pending one.
func (s *Store) SetPendingTOTP(ctx context.Context, id, ciphertext, nonce string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE provider_users
            SET totp_secret_ciphertext = $2, totp_secret_nonce = $3, totp_last_step = NULL, updated_at = NOW()
          WHERE id = $1 AND totp_enabled_at IS NULL`, id, ciphertext, nonce)
	if err != nil {
		return fmt.Errorf("set pending totp: %w", err)
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.GetByID(ctx, id); err != nil {
			return err
		}
		return ErrTOTPAlreadyEnabled
	}
	return nil
}

// ConfirmTOTP enables a pending enrolment whose code matched at step: sets
// totp_enabled_at and totp_last_step, bumps session_generation and writes the
// provider_user.totp_enroll audit row, in one transaction. Returns the
// updated row (its generation is the one the new session must carry).
func (s *Store) ConfirmTOTP(ctx context.Context, id string, step int64, ip string) (*ProviderUser, error) {
	var updated *ProviderUser
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		u, err := scanProviderUser(tx.QueryRow(ctx,
			`UPDATE provider_users
                SET totp_enabled_at = NOW(), totp_last_step = $2,
                    session_generation = session_generation + 1, updated_at = NOW()
              WHERE id = $1 AND totp_enabled_at IS NULL AND totp_secret_ciphertext IS NOT NULL
          RETURNING `+providerUserColumns, id, step))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTOTPNotStarted
		}
		if err != nil {
			return fmt.Errorf("confirm totp: %w", err)
		}
		uid := u.ID
		if err := s.InsertAuditTx(ctx, tx, AuditEntry{
			ProviderUserID: &uid,
			ProviderEmail:  u.Email,
			Action:         AuditTOTPEnroll,
			TargetType:     "provider_user",
			TargetID:       u.ID,
			Details:        map[string]any{"email": u.Email},
			IPAddress:      ip,
		}); err != nil {
			return err
		}
		updated = u
		return nil
	})
	return updated, err
}

// AcceptTOTPStep records step as the last accepted one, only if it is newer
// than the stored one. False means another request already used this step or
// a later one: the code is a replay.
func (s *Store) AcceptTOTPStep(ctx context.Context, id string, step int64) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE provider_users SET totp_last_step = $2
          WHERE id = $1 AND totp_enabled_at IS NOT NULL
            AND (totp_last_step IS NULL OR totp_last_step < $2)`, id, step)
	if err != nil {
		return false, fmt.Errorf("accept totp step: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ResetOperatorTOTP clears another operator's TOTP (pending or enabled) so
// they must enrol again (OQ-1). Refused for self and for a disabled operator,
// like password reset; an operator with no enrolment → ErrTOTPNotEnrolled.
// The reason is required by the handler and stored in the audit row.
func (s *Store) ResetOperatorTOTP(ctx context.Context, actor AuditActor, id, reason string) error {
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
		tag, err := tx.Exec(ctx,
			`UPDATE provider_users
                SET totp_secret_ciphertext = NULL, totp_secret_nonce = NULL,
                    totp_enabled_at = NULL, totp_last_step = NULL,
                    session_generation = session_generation + 1, updated_at = NOW()
              WHERE id = $1 AND (totp_enabled_at IS NOT NULL OR totp_secret_ciphertext IS NOT NULL)`, id)
		if err != nil {
			return fmt.Errorf("reset operator totp: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrTOTPNotEnrolled
		}
		return s.auditOperator(ctx, tx, actor, AuditTOTPReset, target,
			map[string]any{"email": target.Email, "reason": reason})
	})
}

// RecoverAdmin is the break-glass reset behind `zecurity-controller provider
// recover-admin` (ADR-029, D-26 note): for an active super-admin, a new
// temporary-password hash with must_change_password, TOTP cleared, sessions
// ended, audited as system:cli. Returns whether a TOTP enrolment was cleared.
func (s *Store) RecoverAdmin(ctx context.Context, email, passwordHash string) (bool, error) {
	var hadTOTP bool
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var id, role string
		var disabledAt, enabledAt *time.Time
		var ct *string
		err := tx.QueryRow(ctx,
			`SELECT id::text, role, disabled_at, totp_enabled_at, totp_secret_ciphertext
               FROM provider_users WHERE email = $1 FOR UPDATE`, normalizeEmail(email),
		).Scan(&id, &role, &disabledAt, &enabledAt, &ct)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrProviderUserNotFound
		}
		if err != nil {
			return fmt.Errorf("lock provider user: %w", err)
		}
		if role != RoleSuperAdmin {
			return ErrNotSuperAdmin
		}
		if disabledAt != nil {
			return ErrAccountDisabled
		}
		hadTOTP = enabledAt != nil || ct != nil
		if _, err := tx.Exec(ctx,
			`UPDATE provider_users
                SET password_hash = $2, must_change_password = TRUE, password_changed_at = NOW(),
                    totp_secret_ciphertext = NULL, totp_secret_nonce = NULL,
                    totp_enabled_at = NULL, totp_last_step = NULL,
                    session_generation = session_generation + 1, updated_at = NOW()
              WHERE id = $1`, id, passwordHash); err != nil {
			return fmt.Errorf("recover admin: %w", err)
		}
		return s.InsertAuditTx(ctx, tx, AuditEntry{
			ProviderEmail: SystemCLIActor,
			Action:        AuditRecoverAdmin,
			TargetType:    "provider_user",
			TargetID:      id,
			Details:       map[string]any{"email": normalizeEmail(email), "totp_cleared": hadTOTP},
		})
	})
	return hadTOTP, err
}
