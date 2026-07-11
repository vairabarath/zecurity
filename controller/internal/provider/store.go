package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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
}

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

// GetByEmail returns the ACTIVE provider user for an email, or
// ErrProviderUserNotFound (also when the user exists but is disabled).
// This is the allowlist check RequireProvider gates on.
func (s *Store) GetByEmail(ctx context.Context, email string) (*ProviderUser, error) {
	u := &ProviderUser{}
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, role, disabled_at, created_at
                 FROM provider_users
                WHERE email = $1 AND disabled_at IS NULL`,
		normalizeEmail(email),
	).Scan(&u.ID, &u.Email, &u.Role, &u.DisabledAt, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrProviderUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get provider user by email: %w", err)
	}
	return u, nil
}

// List returns all provider users (including disabled), newest first.
func (s *Store) List(ctx context.Context) ([]ProviderUser, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, email, role, disabled_at, created_at
                 FROM provider_users
                ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list provider users: %w", err)
	}
	defer rows.Close()

	var out []ProviderUser
	for rows.Next() {
		var u ProviderUser
		if err := rows.Scan(&u.ID, &u.Email, &u.Role, &u.DisabledAt, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan provider user: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// Create inserts a new provider user. The role CHECK constraint in migration
// 025 rejects any value outside the allowed enum.
func (s *Store) Create(ctx context.Context, email, role string) (*ProviderUser, error) {
	u := &ProviderUser{}
	err := s.pool.QueryRow(ctx,
		`INSERT INTO provider_users (email, role)
               VALUES ($1, $2)
               RETURNING id, email, role, disabled_at, created_at`,
		normalizeEmail(email), role,
	).Scan(&u.ID, &u.Email, &u.Role, &u.DisabledAt, &u.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("create provider user: %w", err)
	}
	return u, nil
}

// Disable soft-disables a provider user (revokes access, preserves audit FK).
func (s *Store) Disable(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE provider_users SET disabled_at = NOW(), updated_at = NOW()
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

// UpsertSuperAdmin idempotently ensures an email exists as an active
// super-admin. Used by the PROVIDER_BOOTSTRAP_EMAILS seed on startup.
func (s *Store) UpsertSuperAdmin(ctx context.Context, email string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO provider_users (email, role)
               VALUES ($1, 'super-admin')
               ON CONFLICT (email) DO UPDATE
                  SET role        = 'super-admin',
                      disabled_at = NULL,
                      updated_at  = NOW()`,
		normalizeEmail(email),
	)
	if err != nil {
		return fmt.Errorf("upsert super-admin %q: %w", email, err)
	}
	return nil
}

// InsertAudit appends one provider action to provider_audit_logs. Append-only:
// this is the only write path, and there is deliberately no update/delete.
func (s *Store) InsertAudit(ctx context.Context, e AuditEntry) error {
	var details []byte
	if e.Details != nil {
		b, err := json.Marshal(e.Details)
		if err != nil {
			return fmt.Errorf("marshal audit details: %w", err)
		}
		details = b
	}
	_, err := s.pool.Exec(ctx,
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
