package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ── Sentinel errors ─────────────────────────────────────────────────────────
//
// gRPC handlers map these to status codes; keeping them as values rather
// than ad-hoc strings lets the call sites use errors.Is.

var (
	errWorkspaceNotFound  = errors.New("workspace not found")
	errInvitationNotFound = errors.New("invitation not found")
	errUserNotInvited     = errors.New("user has no membership in workspace and no invitation provided")
)

// ── Workspace lookup ────────────────────────────────────────────────────────

type workspace struct {
	ID   string
	Slug string
}

func lookupWorkspaceBySlug(ctx context.Context, db *pgxpool.Pool, slug string) (*workspace, error) {
	var ws workspace
	err := db.QueryRow(ctx,
		`SELECT id, slug
		   FROM workspaces
		  WHERE slug = $1
		    AND status = 'active'`,
		slug,
	).Scan(&ws.ID, &ws.Slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errWorkspaceNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query workspace by slug: %w", err)
	}
	return &ws, nil
}

func lookupWorkspaceSlug(ctx context.Context, db *pgxpool.Pool, workspaceID string) (string, error) {
	var slug string
	err := db.QueryRow(ctx,
		`SELECT slug FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&slug)
	if err != nil {
		return "", fmt.Errorf("query workspace slug: %w", err)
	}
	return slug, nil
}

// ── User upsert ─────────────────────────────────────────────────────────────
//
// Schema (migration 001) keys users by (tenant_id, provider_sub). The same
// Google account in two workspaces is two rows. So "upsert" here means:
//   - lookup by (tenant_id, provider, provider_sub)
//   - if found  → update last_login_at, return existing row
//   - if missing and createIfMissing → insert role='member'
//   - if missing and !createIfMissing → errUserNotInvited

type userRow struct {
	ID   string
	Role string
}

func upsertUser(
	ctx context.Context,
	db *pgxpool.Pool,
	tenantID, email, provider, providerSub string,
	createIfMissing bool,
) (*userRow, bool, error) {
	var u userRow
	err := db.QueryRow(ctx,
		`SELECT id, role
		   FROM users
		  WHERE tenant_id = $1
		    AND provider = $2
		    AND provider_sub = $3`,
		tenantID, provider, providerSub,
	).Scan(&u.ID, &u.Role)

	switch {
	case err == nil:
		if _, uErr := db.Exec(ctx,
			`UPDATE users
			    SET last_login_at = NOW(), updated_at = NOW()
			  WHERE id = $1`,
			u.ID,
		); uErr != nil {
			// Last-login bookkeeping should never fail the login;
			// surface it as a log line by returning nil for the error.
			fmt.Printf("warning: update last_login_at for user %s: %v\n", u.ID, uErr)
		}
		return &u, false, nil

	case errors.Is(err, pgx.ErrNoRows):
		if !createIfMissing {
			return nil, false, errUserNotInvited
		}
		err = db.QueryRow(ctx,
			`INSERT INTO users
			   (tenant_id, email, provider, provider_sub, role, status, last_login_at)
			 VALUES ($1, $2, $3, $4, 'member', 'active', NOW())
			 RETURNING id, role`,
			tenantID, email, provider, providerSub,
		).Scan(&u.ID, &u.Role)
		if err != nil {
			return nil, false, fmt.Errorf("insert user: %w", err)
		}
		return &u, true, nil

	default:
		return nil, false, fmt.Errorf("lookup user: %w", err)
	}
}

// ── Invitation lookup / accept ──────────────────────────────────────────────

type invitation struct {
	ID          string
	Email       string
	WorkspaceID string
	Status      string
	ExpiresAt   time.Time
}

func getInvitationByToken(ctx context.Context, db *pgxpool.Pool, token string) (*invitation, error) {
	var inv invitation
	err := db.QueryRow(ctx,
		`SELECT id, email, workspace_id, status, expires_at
		   FROM invitations
		  WHERE token = $1`,
		token,
	).Scan(&inv.ID, &inv.Email, &inv.WorkspaceID, &inv.Status, &inv.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errInvitationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query invitation: %w", err)
	}
	return &inv, nil
}

func markInvitationAccepted(ctx context.Context, db *pgxpool.Pool, invitationID string) error {
	tag, err := db.Exec(ctx,
		`UPDATE invitations
		    SET status = 'accepted'
		  WHERE id = $1
		    AND status = 'pending'
		    AND expires_at > NOW()`,
		invitationID,
	)
	if err != nil {
		return fmt.Errorf("update invitation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("invitation %s not pending or already expired", invitationID)
	}
	return nil
}

// ── Client device persistence ──────────────────────────────────────────────

func insertClientDevice(
	ctx context.Context,
	db *pgxpool.Pool,
	userID, workspaceID, name, os string,
) (string, error) {
	var id string
	err := db.QueryRow(ctx,
		`INSERT INTO client_devices (user_id, workspace_id, name, os)
		 VALUES ($1, $2, $3, $4)
		 RETURNING id`,
		userID, workspaceID, name, os,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert client_device: %w", err)
	}
	return id, nil
}

func updateClientDeviceCert(
	ctx context.Context,
	db *pgxpool.Pool,
	deviceID, certSerial string,
	notAfter time.Time,
	spiffeID string,
) error {
	_, err := db.Exec(ctx,
		`UPDATE client_devices
		    SET cert_serial = $1,
		        cert_not_after = $2,
		        spiffe_id = $3
		  WHERE id = $4`,
		certSerial, notAfter, spiffeID, deviceID,
	)
	if err != nil {
		return fmt.Errorf("update client_device cert: %w", err)
	}
	return nil
}
