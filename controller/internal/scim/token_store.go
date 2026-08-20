package scim

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrTokenNotFound = errors.New("scim token not found")
	ErrTokenExpired  = errors.New("scim token expired")
	ErrTokenRevoked  = errors.New("scim token revoked")
	ErrTokenLimit    = errors.New("scim token limit exceeded")
	ErrInvalidScope  = errors.New("invalid scim token scope")
)

const (
	// A SCIM bearer token is generated from 32 random bytes = 256 bits.
	tokenBytes = 32

	// ADR-025 allows at most two active tokens for a scope.
	maxActiveTokens = 2

	// Default rotation grace period.
	defaultRotationGrace = 24 * time.Hour
)

// Token represents the metadata of a SCIM token.
//
// Plaintext is deliberately NOT part of this structure. The plaintext token
// exists only during Mint/Rotate and is returned once to the caller.
type Token struct {
	ID           string
	WorkspaceID  string
	ConnectionID string
	Label        *string
	CreatedBy    *string
	CreatedAt    time.Time
	LastUsedAt   *time.Time
	ExpiresAt    *time.Time
	RevokedAt    *time.Time
}

// MintResult contains the token metadata plus the plaintext bearer token.
//
// Plaintext is returned only when the token is initially minted/rotated.
// It must never be persisted or returned by List/Lookup.
type MintResult struct {
	Token     Token
	Plaintext string
}

// Store owns SCIM bearer-token persistence and cryptographic lookup.
type Store struct {
	pool       *pgxpool.Pool
	hashKey    []byte
	grace      time.Duration
	now        func() time.Time
	randomRead func([]byte) error
}

// NewStore creates a SCIM token store.
//
// hashKey must be the value of SCIM_TOKEN_HASH_KEY. It must be different from
// PKI_MASTER_SECRET.
//
// grace controls the rotation grace period. If zero, 24 hours is used.
func NewStore(pool *pgxpool.Pool, hashKey []byte, grace time.Duration) (*Store, error) {
	if pool == nil {
		return nil, errors.New("scim token store: nil database pool")
	}

	if len(hashKey) == 0 {
		return nil, errors.New("scim token store: empty hash key")
	}

	if grace <= 0 {
		grace = defaultRotationGrace
	}

	keyCopy := append([]byte(nil), hashKey...)

	return &Store{
		pool:    pool,
		hashKey: keyCopy,
		grace:   grace,
		now:     time.Now,
		randomRead: func(b []byte) error {
			_, err := rand.Read(b)
			return err
		},
	}, nil
}

const tokenColumns = `
	id,
	workspace_id,
	connection_id,
	label,
	created_by,
	created_at,
	last_used_at,
	expires_at,
	revoked_at
`

// generateToken creates the plaintext bearer token.
//
// 32 random bytes gives us 256 bits of entropy. base64.RawURLEncoding
// makes the value safe to place in an Authorization header.
func (s *Store) generateToken() (string, string, error) {
	raw := make([]byte, tokenBytes)

	if err := s.randomRead(raw); err != nil {
		return "", "", fmt.Errorf("generate scim token: %w", err)
	}

	plaintext := base64.RawURLEncoding.EncodeToString(raw)

	return plaintext, s.hashToken(plaintext), nil
}

// hashToken calculates HMAC-SHA256 using the dedicated SCIM token key.
//
// We intentionally do NOT use a normal SHA-256 hash here. The additional
// secret key means someone who obtains the database cannot simply hash
// candidate tokens and compare them with token_hash.
func (s *Store) hashToken(token string) string {
	mac := hmac.New(sha256.New, s.hashKey)
	_, _ = mac.Write([]byte(token))

	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// Mint creates a new token for exactly one (workspace, connection) scope.
//
// The identity_connections row is locked before inspecting existing tokens.
// This serializes concurrent mint/rotate operations for the same connection.
func (s *Store) Mint(
	ctx context.Context,
	workspaceID string,
	connectionID string,
	label *string,
	createdBy *string,
	expiresAt *time.Time,
) (*MintResult, error) {
	if workspaceID == "" || connectionID == "" {
		return nil, ErrInvalidScope
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("scim token mint begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// Lock the parent connection, NOT scim_tokens.
	//
	// This is the serialization point for all token operations belonging
	// to this identity connection.
	var lockedID string

	err = tx.QueryRow(
		ctx,
		`SELECT id
		 FROM identity_connections
		 WHERE id = $1
		   AND tenant_id = $2
		   AND status = 'active'
		 FOR UPDATE`,
		connectionID,
		workspaceID,
	).Scan(&lockedID)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidScope
	}
	if err != nil {
		return nil, fmt.Errorf("lock identity connection: %w", err)
	}

	// Remove tokens that are already revoked or expired from the active
	// calculation. We don't delete them because their history is useful.
	now := s.now()

	activeCount, err := s.activeTokenCount(ctx, tx, workspaceID, connectionID, now)
	if err != nil {
		return nil, err
	}

	if activeCount >= maxActiveTokens {
		if err := s.applyThirdTokenRule(
			ctx,
			tx,
			workspaceID,
			connectionID,
			now,
		); err != nil {
			return nil, err
		}
	}

	plaintext, tokenHash, err := s.generateToken()
	if err != nil {
		return nil, err
	}

	var token Token

	row := tx.QueryRow(
		ctx,
		`INSERT INTO scim_tokens (
			workspace_id,
			connection_id,
			token_hash,
			label,
			created_by,
			expires_at
		)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING `+tokenColumns,
		workspaceID,
		connectionID,
		tokenHash,
		label,
		createdBy,
		expiresAt,
	)

	if err := scanToken(row, &token); err != nil {
		return nil, fmt.Errorf("insert scim token: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit scim token mint: %w", err)
	}

	return &MintResult{
		Token:     token,
		Plaintext: plaintext,
	}, nil
}

// activeTokenCount returns the number of currently usable tokens for a scope.
func (s *Store) activeTokenCount(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	connectionID string,
	now time.Time,
) (int, error) {
	var count int

	err := tx.QueryRow(
		ctx,
		`SELECT COUNT(*)
		 FROM scim_tokens
		 WHERE workspace_id = $1
		   AND connection_id = $2
		   AND revoked_at IS NULL
		   AND (expires_at IS NULL OR expires_at > $3)`,
		workspaceID,
		connectionID,
		now,
	).Scan(&count)

	if err != nil {
		return 0, fmt.Errorf("count active scim tokens: %w", err)
	}

	return count, nil
}

// applyThirdTokenRule enforces the ADR-025 two-token limit.
//
// When two active tokens already exist:
//
//	oldest token → revoked
//	remaining token → expiry shortened to grace window
//	new token     → inserted by Mint
//
// The existing expiry is NEVER extended.
func (s *Store) applyThirdTokenRule(
	ctx context.Context,
	tx pgx.Tx,
	workspaceID string,
	connectionID string,
	now time.Time,
) error {
	type activeToken struct {
		id        string
		expiresAt *time.Time
	}

	rows, err := tx.Query(
		ctx,
		`SELECT id, expires_at
		 FROM scim_tokens
		 WHERE workspace_id = $1
		   AND connection_id = $2
		   AND revoked_at IS NULL
		   AND (expires_at IS NULL OR expires_at > $3)
		 ORDER BY created_at ASC, id ASC
		 FOR UPDATE`,
		workspaceID,
		connectionID,
		now,
	)
	if err != nil {
		return fmt.Errorf("select active scim tokens: %w", err)
	}
	defer rows.Close()

	var tokens []activeToken

	for rows.Next() {
		var t activeToken

		if err := rows.Scan(&t.id, &t.expiresAt); err != nil {
			return fmt.Errorf("scan active scim token: %w", err)
		}

		tokens = append(tokens, t)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate active scim tokens: %w", err)
	}

	if len(tokens) < maxActiveTokens {
		return nil
	}

	// Oldest token is revoked.
	if _, err := tx.Exec(
		ctx,
		`UPDATE scim_tokens
		 SET revoked_at = $3
		 WHERE id = $1
		   AND workspace_id = $2
		   AND revoked_at IS NULL`,
		tokens[0].id,
		workspaceID,
		now,
	); err != nil {
		return fmt.Errorf("revoke oldest scim token: %w", err)
	}

	// The second token receives the grace window.
	//
	// Important:
	//
	//     newExpiry = MIN(existingExpiry, now + grace)
	//
	// If the token was already going to expire in 2 hours, we keep 2 hours.
	// We never turn it into 24 hours.
	graceExpiry := now.Add(s.grace)

	if _, err := tx.Exec(
		ctx,
		`UPDATE scim_tokens
		 SET expires_at = CASE
			 WHEN expires_at IS NULL THEN $3
			 WHEN expires_at > $3 THEN $3
			 ELSE expires_at
		 END
		 WHERE id = $1
		   AND workspace_id = $2
		   AND revoked_at IS NULL`,
		tokens[1].id,
		workspaceID,
		graceExpiry,
	); err != nil {
		return fmt.Errorf("set scim token grace expiry: %w", err)
	}

	return nil
}

// Rotate creates a replacement token using the same scope.
//
// Rotation uses the same parent connection lock as Mint, so concurrent
// rotations cannot violate the two-token invariant.
func (s *Store) Rotate(
	ctx context.Context,
	workspaceID string,
	connectionID string,
	label *string,
	createdBy *string,
	expiresAt *time.Time,
) (*MintResult, error) {
	return s.Mint(
		ctx,
		workspaceID,
		connectionID,
		label,
		createdBy,
		expiresAt,
	)
}

// Revoke explicitly revokes a token.
//
// The workspace + connection pair is part of the WHERE clause so a caller
// cannot revoke a token belonging to another scope.
func (s *Store) Revoke(
	ctx context.Context,
	workspaceID string,
	connectionID string,
	tokenID string,
) error {
	if workspaceID == "" || connectionID == "" || tokenID == "" {
		return ErrInvalidScope
	}

	tag, err := s.pool.Exec(
		ctx,
		`UPDATE scim_tokens
		 SET revoked_at = COALESCE(revoked_at, NOW())
		 WHERE id = $1
		   AND workspace_id = $2
		   AND connection_id = $3`,
		tokenID,
		workspaceID,
		connectionID,
	)
	if err != nil {
		return fmt.Errorf("revoke scim token: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return ErrTokenNotFound
	}

	return nil
}

// Lookup authenticates a plaintext bearer token.
//
// The lookup never searches by plaintext. It first calculates the HMAC,
// then searches by token_hash.
//
// A successful lookup updates last_used_at and returns the token's scope.
func (s *Store) Lookup(
	ctx context.Context,
	plaintext string,
) (*Token, error) {
	if plaintext == "" {
		return nil, ErrTokenNotFound
	}

	tokenHash := s.hashToken(plaintext)
	now := s.now()

	var token Token

	row := s.pool.QueryRow(
		ctx,
		`UPDATE scim_tokens
		 SET last_used_at = $2
		 WHERE token_hash = $1
		   AND revoked_at IS NULL
		   AND (expires_at IS NULL OR expires_at > $2)
		 RETURNING `+tokenColumns,
		tokenHash,
		now,
	)

	if err := scanToken(row, &token); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Deliberately don't reveal whether the token was:
			// missing, expired, or revoked.
			return nil, ErrTokenNotFound
		}

		return nil, fmt.Errorf("lookup scim token: %w", err)
	}

	return &token, nil
}

// List returns token metadata for exactly one scope.
//
// Plaintext tokens are never available through this method.
func (s *Store) List(
	ctx context.Context,
	workspaceID string,
	connectionID string,
) ([]Token, error) {
	if workspaceID == "" || connectionID == "" {
		return nil, ErrInvalidScope
	}

	rows, err := s.pool.Query(
		ctx,
		`SELECT `+tokenColumns+`
		 FROM scim_tokens
		 WHERE workspace_id = $1
		   AND connection_id = $2
		 ORDER BY created_at DESC`,
		workspaceID,
		connectionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list scim tokens: %w", err)
	}
	defer rows.Close()

	var tokens []Token

	for rows.Next() {
		var token Token

		if err := scanToken(rows, &token); err != nil {
			return nil, fmt.Errorf("scan scim token: %w", err)
		}

		tokens = append(tokens, token)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate scim tokens: %w", err)
	}

	return tokens, nil
}

func scanToken(row interface{ Scan(...any) error }, token *Token) error {
	return row.Scan(
		&token.ID,
		&token.WorkspaceID,
		&token.ConnectionID,
		&token.Label,
		&token.CreatedBy,
		&token.CreatedAt,
		&token.LastUsedAt,
		&token.ExpiresAt,
		&token.RevokedAt,
	)
}

// ParseTokenID validates and normalizes a token UUID used by administrative
// operations.
func ParseTokenID(value string) (string, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return "", fmt.Errorf("invalid token id: %w", err)
	}

	return id.String(), nil
}
