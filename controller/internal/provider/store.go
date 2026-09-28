package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrProviderUserNotFound is returned when no active provider user matches.
// A soft-disabled user (disabled_at IS NOT NULL) is treated as not found.
var ErrProviderUserNotFound = errors.New("provider user not found")

// ProviderUser mirrors a row in provider_users. Provider identities are a
// separate security domain from tenant users — no tenant_id, ever.
type ProviderUser struct {
	ID         string
	Email      string
	Role       string // "super-admin" | "relay-ops"
	DisabledAt *time.Time
	CreatedAt  time.Time

	// Local-account fields (Sprint 21 Phase H, migration 037). PasswordHash and
	// SessionGeneration are never serialized or returned by any API.
	PasswordHash       string `json:"-"`
	MustChangePassword bool   // login yields a password-change-only token
	SessionGeneration  int64  `json:"-"` // embedded in every provider JWT; bump = revoke all
	LastLoginAt        *time.Time
}

// Provider identity audit actions (Sprint 21 Phase H). Passwords never appear
// in Details.
const (
	AuditBootstrapCreate = "provider_user.bootstrap_create"
	AuditPasswordChange  = "provider_user.password_change"
	AuditSessionLogin    = "provider_session.login"
	AuditSessionLogout   = "provider_session.logout"
	AuditRateLimit       = "provider_auth.rate_limit"

	// SystemBootstrapActor is provider_email for audit rows written by the
	// create-only bootstrap (no provider user performed the action).
	SystemBootstrapActor = "system:bootstrap"
)

// AuditEntry is one append-only provider_audit_logs row. Details is a free-form
// context snapshot (name/TTL/SANs, granted role, …) stored as JSONB.
type AuditEntry struct {
	ProviderUserID *string
	ProviderEmail  string
	Action         string // dotted verb, e.g. "relay.create"
	TargetType     string
	TargetID       string
	Details        map[string]any
	IPAddress      string
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// normalizeEmail matches ADR-005 normalization used in bootstrap.go / client store.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// providerUserColumns / scanProviderUser keep every read of provider_users in
// one shape. password_hash is NULL for accounts without a local password.
const providerUserColumns = `id, email, role, disabled_at, created_at,
       COALESCE(password_hash, ''), must_change_password, session_generation, last_login_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanProviderUser(r rowScanner) (*ProviderUser, error) {
	u := &ProviderUser{}
	err := r.Scan(&u.ID, &u.Email, &u.Role, &u.DisabledAt, &u.CreatedAt,
		&u.PasswordHash, &u.MustChangePassword, &u.SessionGeneration, &u.LastLoginAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// GetByEmail returns the ACTIVE provider user for an email, or
// ErrProviderUserNotFound (also when the user exists but is disabled).
func (s *Store) GetByEmail(ctx context.Context, email string) (*ProviderUser, error) {
	u, err := scanProviderUser(s.pool.QueryRow(ctx,
		`SELECT `+providerUserColumns+`
                 FROM provider_users
                WHERE email = $1 AND disabled_at IS NULL`,
		normalizeEmail(email),
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProviderUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get provider user by email: %w", err)
	}
	return u, nil
}

// GetByEmailForLogin returns the provider user for an email INCLUDING disabled
// rows (DisabledAt set) and the password hash, or ErrProviderUserNotFound. The
// LocalPasswordAuthenticator turns every non-match into the same error.
func (s *Store) GetByEmailForLogin(ctx context.Context, email string) (*ProviderUser, error) {
	u, err := scanProviderUser(s.pool.QueryRow(ctx,
		`SELECT `+providerUserColumns+` FROM provider_users WHERE email = $1`,
		normalizeEmail(email),
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProviderUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get provider user for login: %w", err)
	}
	return u, nil
}

// GetByID returns the provider user by id INCLUDING disabled rows, or
// ErrProviderUserNotFound. RequireProvider uses it so it can distinguish a
// disabled operator (403) from a revoked session (401).
func (s *Store) GetByID(ctx context.Context, id string) (*ProviderUser, error) {
	u, err := scanProviderUser(s.pool.QueryRow(ctx,
		`SELECT `+providerUserColumns+` FROM provider_users WHERE id = $1`, id,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProviderUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get provider user by id: %w", err)
	}
	return u, nil
}

// List returns all provider users (including disabled), newest first.
func (s *Store) List(ctx context.Context) ([]ProviderUser, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+providerUserColumns+`
                 FROM provider_users
                ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list provider users: %w", err)
	}
	defer rows.Close()

	var out []ProviderUser
	for rows.Next() {
		u, err := scanProviderUser(rows)
		if err != nil {
			return nil, fmt.Errorf("scan provider user: %w", err)
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// Create inserts a new provider user. The role CHECK constraint in migration
// 025 rejects any value outside the allowed enum.
func (s *Store) Create(ctx context.Context, email, role string) (*ProviderUser, error) {
	u, err := scanProviderUser(s.pool.QueryRow(ctx,
		`INSERT INTO provider_users (email, role)
               VALUES ($1, $2)
               RETURNING `+providerUserColumns,
		normalizeEmail(email), role,
	))
	if err != nil {
		return nil, fmt.Errorf("create provider user: %w", err)
	}
	return u, nil
}

// SetPassword stores a new Argon2id hash and, in the SAME statement, bumps
// session_generation — so every password change or reset invalidates every
// existing session immediately (AT-CORE-2). mustChange marks a temporary
// password. Returns the updated row (with the new generation) for IssueSession.
func (s *Store) SetPassword(ctx context.Context, id, passwordHash string, mustChange bool) (*ProviderUser, error) {
	u, err := scanProviderUser(s.pool.QueryRow(ctx,
		`UPDATE provider_users
                SET password_hash        = $2,
                    must_change_password = $3,
                    password_changed_at  = NOW(),
                    session_generation   = session_generation + 1,
                    updated_at           = NOW()
              WHERE id = $1
          RETURNING `+providerUserColumns,
		id, passwordHash, mustChange,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProviderUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("set provider password: %w", err)
	}
	return u, nil
}

// BumpSessionGeneration revokes every outstanding token for the user (logout).
func (s *Store) BumpSessionGeneration(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE provider_users
                SET session_generation = session_generation + 1, updated_at = NOW()
              WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("bump provider session generation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProviderUserNotFound
	}
	return nil
}

// RecordLogin stamps last_login_at after a successful login.
func (s *Store) RecordLogin(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE provider_users SET last_login_at = NOW() WHERE id = $1`, id); err != nil {
		return fmt.Errorf("record provider login: %w", err)
	}
	return nil
}

// HasSuperAdmin reports whether any super-admin row exists (active or
// disabled). Bootstrap uses it as a cheap early exit; the authoritative,
// race-safe check is inside CreateBootstrapSuperAdminIfNone.
func (s *Store) HasSuperAdmin(ctx context.Context) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM provider_users WHERE role = 'super-admin')`).Scan(&exists); err != nil {
		return false, fmt.Errorf("check super-admin: %w", err)
	}
	return exists, nil
}

// BootstrapResult is the outcome of CreateBootstrapSuperAdminIfNone.
type BootstrapResult int

const (
	// BootstrapCreated: no super-admin existed; the bootstrap account was created.
	BootstrapCreated BootstrapResult = iota
	// BootstrapSkippedAdminExists: a super-admin (in any state) already exists;
	// the bootstrap environment is ignored.
	BootstrapSkippedAdminExists
	// BootstrapEmailTaken: no super-admin exists but the email belongs to an
	// existing non-super-admin row. Nothing is changed — bootstrap never
	// promotes, overwrites or re-enables an existing account.
	BootstrapEmailTaken
)

// bootstrapLockKey serializes bootstrap attempts (pg_advisory_xact_lock), so
// concurrent starts can never create two bootstrap super-admins.
const bootstrapLockKey int64 = 0x7a65635f626f6f74 // "zec_boot"

// CreateBootstrapSuperAdminIfNone implements the create-only bootstrap (D-26):
// in one transaction, under an advisory lock, it creates the bootstrap
// super-admin (must_change_password = TRUE) only while no super-admin exists,
// and writes the provider_user.bootstrap_create audit row with it.
func (s *Store) CreateBootstrapSuperAdminIfNone(ctx context.Context, email, passwordHash string) (BootstrapResult, *ProviderUser, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, nil, fmt.Errorf("begin bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, bootstrapLockKey); err != nil {
		return 0, nil, fmt.Errorf("bootstrap lock: %w", err)
	}
	var exists bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM provider_users WHERE role = 'super-admin')`).Scan(&exists); err != nil {
		return 0, nil, fmt.Errorf("check super-admin: %w", err)
	}
	if exists {
		return BootstrapSkippedAdminExists, nil, nil
	}
	u, err := scanProviderUser(tx.QueryRow(ctx,
		`INSERT INTO provider_users (email, role, password_hash, must_change_password)
              VALUES ($1, 'super-admin', $2, TRUE)
         ON CONFLICT (email) DO NOTHING
           RETURNING `+providerUserColumns,
		normalizeEmail(email), passwordHash,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return BootstrapEmailTaken, nil, nil
	}
	if err != nil {
		return 0, nil, fmt.Errorf("insert bootstrap super-admin: %w", err)
	}
	if err := s.InsertAuditTx(ctx, tx, AuditEntry{
		ProviderEmail: SystemBootstrapActor,
		Action:        AuditBootstrapCreate,
		TargetType:    "provider_user",
		TargetID:      u.ID,
		Details:       map[string]any{"email": u.Email, "role": u.Role},
	}); err != nil {
		return 0, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, fmt.Errorf("commit bootstrap: %w", err)
	}
	return BootstrapCreated, u, nil
}

// Disable soft-disables a provider user (revokes access, preserves audit FK)
// and bumps session_generation so every outstanding token dies at once.
func (s *Store) Disable(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE provider_users
                SET disabled_at = NOW(), session_generation = session_generation + 1, updated_at = NOW()
              WHERE id = $1 AND disabled_at IS NULL`,
		id,
	)
	if err != nil {
		return fmt.Errorf("disable provider user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProviderUserNotFound
	}
	return nil
}

// dbExec is the subset of pgx used to write an audit row. Both *pgxpool.Pool and
// pgx.Tx satisfy it, so an audit can be written standalone or folded into a
// caller's transaction.
type dbExec interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// InsertAudit appends one provider action to provider_audit_logs on the pool.
// Append-only: this is the only write path; there is deliberately no update/delete.
func (s *Store) InsertAudit(ctx context.Context, e AuditEntry) error {
	return s.InsertAuditTx(ctx, s.pool, e)
}

// InsertAuditTx appends one provider action using the supplied executor. Pass a
// pgx.Tx to make the audit row commit atomically with the caller's transaction
// (a failed audit then rolls the whole operation back); pass the pool for a
// standalone write.
func (s *Store) InsertAuditTx(ctx context.Context, db dbExec, e AuditEntry) error {
	var details []byte
	if e.Details != nil {
		b, err := json.Marshal(e.Details)
		if err != nil {
			return fmt.Errorf("marshal audit details: %w", err)
		}
		details = b
	}
	_, err := db.Exec(ctx,
		`INSERT INTO provider_audit_logs
                   (provider_user_id, provider_email, action, target_type, target_id, details, ip_address)
               VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		e.ProviderUserID, e.ProviderEmail, e.Action, e.TargetType, e.TargetID, details, nullIfEmpty(e.IPAddress),
	)
	if err != nil {
		return fmt.Errorf("insert provider audit: %w", err)
	}
	return nil
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
