package tenantexport

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Download tokens (2026-11-18 design): an emailed link valid until the purge
// (purpose "email") and a single-use, short-lived token handed out by an
// export-only sign-in (purpose "signin"). Only SHA-256(token) is stored; a
// token is never logged, audited or returned by a provider API.

// SigninTokenTTL is how long an export-only sign-in token lasts.
const SigninTokenTTL = 15 * time.Minute

var (
	// ErrInvalidToken: unknown, expired, used, or the tenant is no longer
	// awaiting deletion. One error for all, so a caller learns nothing.
	ErrInvalidToken = errors.New("tenantexport: invalid or expired download link")
)

// IssuedToken is a token for one admin; the caller emails or redirects with
// it and never logs it.
type IssuedToken struct {
	UserID    string
	Email     string
	Token     string
	ExpiresAt time.Time
}

func newToken() (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	h := sha256.Sum256([]byte(tok))
	return tok, h[:], nil
}

// IssueEmailTokensTx issues an emailed download token, valid until expires,
// to every active admin of the workspace (in the caller's transaction).
func IssueEmailTokensTx(ctx context.Context, tx pgx.Tx, workspaceID string, expires time.Time) ([]IssuedToken, error) {
	rows, err := tx.Query(ctx, `SELECT id::text, email FROM users
		  WHERE tenant_id = $1 AND role = 'admin' AND status = 'active' ORDER BY created_at, id`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("tenantexport: list admins: %w", err)
	}
	type admin struct{ id, email string }
	var admins []admin
	for rows.Next() {
		var a admin
		if err := rows.Scan(&a.id, &a.email); err != nil {
			rows.Close()
			return nil, err
		}
		admins = append(admins, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]IssuedToken, 0, len(admins))
	for _, a := range admins {
		tok, h, err := newToken()
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO tenant_export_tokens (token_sha256, workspace_id, purpose, user_id, expires_at)
			VALUES ($1, $2, 'email', $3, $4)`, h, workspaceID, a.id, expires); err != nil {
			return nil, fmt.Errorf("tenantexport: store token: %w", err)
		}
		out = append(out, IssuedToken{UserID: a.id, Email: a.email, Token: tok, ExpiresAt: expires})
	}
	return out, nil
}

// IssueSigninToken issues a single-use export-only sign-in token for an admin
// of a tenant awaiting deletion. It refuses (ErrInvalidToken) unless the
// tenant is suspended with a scheduled deletion and userID is an active admin.
func IssueSigninToken(ctx context.Context, db interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}, workspaceID, userID string) (IssuedToken, error) {
	tok, h, err := newToken()
	if err != nil {
		return IssuedToken{}, err
	}
	var expires time.Time
	err = db.QueryRow(ctx, `
		INSERT INTO tenant_export_tokens (token_sha256, workspace_id, purpose, user_id, expires_at)
		SELECT $1, w.id, 'signin', u.id, LEAST(NOW() + make_interval(secs => $4), w.delete_after)
		  FROM workspaces w JOIN users u ON u.tenant_id = w.id
		 WHERE w.id = $2 AND u.id = $3
		   AND w.suspended_at IS NOT NULL AND w.delete_after IS NOT NULL AND w.delete_after > NOW()
		   AND u.role = 'admin' AND u.status = 'active'
		RETURNING expires_at`, h, workspaceID, userID, SigninTokenTTL.Seconds()).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return IssuedToken{}, ErrInvalidToken
	}
	if err != nil {
		return IssuedToken{}, fmt.Errorf("tenantexport: store token: %w", err)
	}
	return IssuedToken{UserID: userID, Token: tok, ExpiresAt: expires}, nil
}

// Grant is what a valid token allows: downloading this tenant's archive.
type Grant struct {
	WorkspaceID string
	Slug        string
	Name        string
	UserID      string
	UserEmail   string
	Purpose     string
	DeleteAfter time.Time
}

// lookup resolves a token (without consuming it) while the tenant is
// suspended, still scheduled for deletion and the purge hasn't started.
func lookup(ctx context.Context, tx pgx.Tx, token string, lock bool) (Grant, []byte, error) {
	if len(token) < 20 || len(token) > 100 {
		return Grant{}, nil, ErrInvalidToken
	}
	h := sha256.Sum256([]byte(token))
	q := `SELECT w.id::text, w.slug, w.name, u.id::text, u.email, t.purpose, w.delete_after
		  FROM tenant_export_tokens t
		  JOIN workspaces w ON w.id = t.workspace_id
		  JOIN users u ON u.id = t.user_id AND u.tenant_id = w.id
		 WHERE t.token_sha256 = $1
		   AND t.expires_at > NOW() AND t.used_at IS NULL
		   AND w.suspended_at IS NOT NULL AND w.delete_after IS NOT NULL
		   AND u.role = 'admin' AND u.status = 'active'
		   AND NOT EXISTS (SELECT 1 FROM tenant_purges p WHERE p.workspace_id = w.id)`
	if lock {
		q += ` FOR UPDATE OF t`
	}
	var g Grant
	err := tx.QueryRow(ctx, q, h[:]).Scan(&g.WorkspaceID, &g.Slug, &g.Name, &g.UserID, &g.UserEmail, &g.Purpose, &g.DeleteAfter)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, nil, ErrInvalidToken
	}
	if err != nil {
		return Grant{}, nil, fmt.Errorf("tenantexport: look up token: %w", err)
	}
	return g, h[:], nil
}

// Redeem validates a token for a download in tx. A sign-in token is consumed
// (single use); an emailed link stays valid until the purge.
func Redeem(ctx context.Context, tx pgx.Tx, token string) (Grant, error) {
	g, h, err := lookup(ctx, tx, token, true)
	if err != nil {
		return Grant{}, err
	}
	if g.Purpose == "signin" {
		if _, err := tx.Exec(ctx, `UPDATE tenant_export_tokens SET used_at = NOW() WHERE token_sha256 = $1`, h); err != nil {
			return Grant{}, fmt.Errorf("tenantexport: consume token: %w", err)
		}
	}
	return g, nil
}

// Inspect validates a token without consuming it (the download page shows
// the workspace and the deadline).
func Inspect(ctx context.Context, tx pgx.Tx, token string) (Grant, error) {
	g, _, err := lookup(ctx, tx, token, false)
	return g, err
}
