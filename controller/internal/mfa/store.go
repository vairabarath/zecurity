package mfa

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the Postgres Repo (table user_totp, migration 061).
type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Get(ctx context.Context, userID string) (*Record, error) {
	rec := Record{UserID: userID}
	err := s.pool.QueryRow(ctx,
		`SELECT workspace_id::text, secret_ciphertext, secret_nonce, enabled_at, last_step
		   FROM user_totp WHERE user_id = $1`, userID,
	).Scan(&rec.WorkspaceID, &rec.Ciphertext, &rec.Nonce, &rec.EnabledAt, &rec.LastStep)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("mfa: get totp: %w", err)
	}
	return &rec, nil
}

func (s *Store) BeginEnrollment(ctx context.Context, userID, workspaceID, ciphertext, nonce string) error {
	// The user must belong to the workspace; a confirmed authenticator is
	// never overwritten (the DO UPDATE's WHERE leaves it alone → 0 rows).
	tag, err := s.pool.Exec(ctx,
		`INSERT INTO user_totp (user_id, workspace_id, secret_ciphertext, secret_nonce)
		 SELECT u.id, u.tenant_id, $3, $4
		   FROM users u
		  WHERE u.id = $1 AND u.tenant_id = $2
		 ON CONFLICT (user_id) DO UPDATE
		    SET secret_ciphertext = EXCLUDED.secret_ciphertext,
		        secret_nonce      = EXCLUDED.secret_nonce,
		        last_step         = NULL,
		        created_at        = NOW()
		  WHERE user_totp.enabled_at IS NULL`,
		userID, workspaceID, ciphertext, nonce)
	if err != nil {
		return fmt.Errorf("mfa: begin enrollment: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyEnrolled
	}
	return nil
}

func (s *Store) AcceptStep(ctx context.Context, userID string, step int64) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`UPDATE user_totp
		    SET last_step  = $2,
		        enabled_at = COALESCE(enabled_at, NOW())
		  WHERE user_id = $1 AND (last_step IS NULL OR last_step < $2)`,
		userID, step)
	if err != nil {
		return false, fmt.Errorf("mfa: accept step: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

func (s *Store) Delete(ctx context.Context, workspaceID, userID string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM user_totp WHERE user_id = $1 AND workspace_id = $2`, userID, workspaceID)
	if err != nil {
		return false, fmt.Errorf("mfa: delete totp: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// EnrolledUsers returns which of the workspace's users have a confirmed
// authenticator (for the console's user list).
func (s *Store) EnrolledUsers(ctx context.Context, workspaceID string) (map[string]bool, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT user_id::text FROM user_totp WHERE workspace_id = $1 AND enabled_at IS NOT NULL`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("mfa: list enrolled: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
