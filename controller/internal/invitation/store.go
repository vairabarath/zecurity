package invitation

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("invitation not found")

// Invitation mirrors the invitations table row.
type Invitation struct {
	ID            string
	Email         string
	WorkspaceID   string
	WorkspaceName string // populated by joins in Get queries
	InvitedBy     string
	Token         string
	Status        string
	ExpiresAt     time.Time
	CreatedAt     time.Time
}

// Store owns all DB operations for the invitations table.
type Store struct {
	db *pgxpool.Pool
}

func NewStore(db *pgxpool.Pool) *Store { return &Store{db: db} }

// CreateInvitation inserts a new pending invitation and returns the full row.
// Token is 32 random bytes encoded as lowercase hex (64 hex chars, 256 bits entropy).
func (s *Store) CreateInvitation(ctx context.Context, email, workspaceID, invitedBy string) (*Invitation, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}
	token := hex.EncodeToString(raw)

	var inv Invitation
	err := s.db.QueryRow(ctx,
		`INSERT INTO invitations (email, workspace_id, invited_by, token)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id, email, workspace_id, invited_by, token, status, expires_at, created_at`,
		email, workspaceID, invitedBy, token,
	).Scan(
		&inv.ID, &inv.Email, &inv.WorkspaceID, &inv.InvitedBy,
		&inv.Token, &inv.Status, &inv.ExpiresAt, &inv.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert invitation: %w", err)
	}
	return &inv, nil
}

// GetByToken returns the invitation for the given token, joining workspace
// name for display. Returns ErrNotFound when no row exists.
func (s *Store) GetByToken(ctx context.Context, token string) (*Invitation, error) {
	var inv Invitation
	err := s.db.QueryRow(ctx,
		`SELECT i.id, i.email, i.workspace_id, w.name, i.invited_by,
		        i.token, i.status, i.expires_at, i.created_at
		   FROM invitations i
		   JOIN workspaces w ON w.id = i.workspace_id
		  WHERE i.token = $1`,
		token,
	).Scan(
		&inv.ID, &inv.Email, &inv.WorkspaceID, &inv.WorkspaceName, &inv.InvitedBy,
		&inv.Token, &inv.Status, &inv.ExpiresAt, &inv.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query invitation: %w", err)
	}
	return &inv, nil
}

// AcceptInvitation marks the invitation as accepted and adds the user to the
// workspace as MEMBER. The caller must already be authenticated to the invited
// workspace (JWT tenant_id == invitation workspace_id — enforced in handler).
func (s *Store) AcceptInvitation(ctx context.Context, token, workspaceID, userID string) error {
	tag, err := s.db.Exec(ctx,
		`UPDATE invitations
		    SET status = 'accepted'
		  WHERE token = $1
		    AND workspace_id = $2
		    AND status = 'pending'
		    AND expires_at > NOW()`,
		token, workspaceID,
	)
	if err != nil {
		return fmt.Errorf("accept invitation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}

	_, err = s.db.Exec(ctx,
		`INSERT INTO workspace_users (workspace_id, user_id, role)
		 VALUES ($1, $2, 'member')
		 ON CONFLICT (workspace_id, user_id) DO NOTHING`,
		workspaceID, userID,
	)
	if err != nil {
		return fmt.Errorf("add user to workspace: %w", err)
	}
	return nil
}
