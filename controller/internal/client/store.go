package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/identity"
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
// Identity is keyed on external_identities (tenant_id, connection_id, subject)
// — the ADR-024 key, never email — resolved via the shared identity.Resolver so
// the CLI and web flow agree on "who is this". The CLI logs in workspace-first,
// so tenantID scopes the lookup. "upsert" here means:
//   - resolve (tenant, connection, subject) → canonical user
//   - if found  → lifecycle-gate, update last_login_at, return existing row
//   - if missing and createIfMissing → insert user + external_identities (one tx)
//   - if missing and !createIfMissing → errUserNotInvited
//
// Unlike the web flow (bootstrap), the CLI never creates a workspace: an
// unresolved identity either joins the known workspace as 'member' (invited) or
// is rejected.

type userRow struct {
	ID   string
	Role string
}

func upsertUser(
	ctx context.Context,
	db *pgxpool.Pool,
	tenantID, email, provider, providerSub, connectionID, issuer string,
	createIfMissing bool,
) (row *userRow, generation int, created bool, err error) {
	email = strings.ToLower(strings.TrimSpace(email))

	core, found, err := identity.NewResolver(db).Resolve(ctx, connectionID, providerSub, tenantID)
	if err != nil {
		return nil, 0, false, fmt.Errorf("resolve identity: %w", err)
	}

	if found {
		// Fail closed on a suspended/locked/deleted canonical user.
		if err := identity.CheckLifecycle(core.Status); err != nil {
			return nil, 0, false, err
		}
		if _, uErr := db.Exec(ctx,
			`UPDATE users
			    SET last_login_at = NOW(), updated_at = NOW()
			  WHERE id = $1`,
			core.UserID,
		); uErr != nil {
			// Last-login bookkeeping should never fail the login; log and proceed.
			fmt.Printf("warning: update last_login_at for user %s: %v\n", core.UserID, uErr)
		}
		return &userRow{ID: core.UserID, Role: core.Role}, core.Generation, false, nil
	}

	if !createIfMissing {
		return nil, 0, false, errUserNotInvited
	}

	// JIT-create the member and its identity link atomically.
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, 0, false, fmt.Errorf("begin user tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var u userRow
	if err := tx.QueryRow(ctx,
		`INSERT INTO users
		   (tenant_id, email, provider, provider_sub, role, status, last_login_at)
		 VALUES ($1, $2, $3, $4, 'member', 'active', NOW())
		 RETURNING id, role`,
		tenantID, email, provider, providerSub,
	).Scan(&u.ID, &u.Role); err != nil {
		return nil, 0, false, fmt.Errorf("insert user: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO external_identities (tenant_id, user_id, connection_id, issuer, subject)
		 VALUES ($1, $2, $3, $4, $5)`,
		tenantID, u.ID, connectionID, issuer, providerSub,
	); err != nil {
		return nil, 0, false, fmt.Errorf("insert external_identity: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, 0, false, fmt.Errorf("commit user tx: %w", err)
	}
	return &u, 1, true, nil
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

// revokeClientDevice marks a client device as revoked. The ownership fields
// (user_id, workspace_id) are included in the WHERE clause as defense-in-depth
// alongside the handler-side check — even a handler bug or a spoofed device_id
// cannot revoke a row that doesn't belong to the caller. Idempotent on already-
// revoked rows: the AND revoked_at IS NULL clause makes repeated calls a no-op
// and preserves the original revocation timestamp for audit.
func revokeClientDevice(ctx context.Context, db *pgxpool.Pool, deviceID, userID, workspaceID string) error {
	_, err := db.Exec(ctx,
		`UPDATE client_devices
		    SET revoked_at = NOW()
		  WHERE id           = $1
		    AND user_id      = $2
		    AND workspace_id = $3
		    AND revoked_at IS NULL`,
		deviceID, userID, workspaceID,
	)
	if err != nil {
		return fmt.Errorf("revoke client_device: %w", err)
	}
	return nil
}

// revokeUserDevices revokes every one of a user's client devices within a
// workspace, scoped by (user_id, workspace_id) and gated with
// AND revoked_at IS NULL. It returns the number of rows actually affected.
//
// Idempotent on replay: re-running against already-revoked rows affects 0 rows,
// so callers can use the 0 count to skip downstream side effects (audit,
// notify) on at-least-once redelivery without producing duplicate entries.
//
// Intentionally pool-based (autocommit), not transactional with the audit
// write: a security revocation must never be blocked by a transient failure of
// the audit table. The durable enforcement path is revoked_at → workspace CRL
// (connectors poll it independently); the audit row is best-effort context.
func revokeUserDevices(
	ctx context.Context,
	db *pgxpool.Pool,
	userID, workspaceID string,
) (int64, error) {
	tag, err := db.Exec(ctx,
		`UPDATE client_devices
		    SET revoked_at = NOW()
		  WHERE user_id      = $1
		    AND workspace_id = $2
		    AND revoked_at IS NULL`,
		userID, workspaceID,
	)
	if err != nil {
		return 0, fmt.Errorf("revoke user devices: %w", err)
	}
	return tag.RowsAffected(), nil
}

// markUserDevicesReEnrollRequired sets status = 're_enroll_required' on every
// one of a user's devices within a workspace. Unlike revokeUserDevices, this
// does NOT filter on revoked_at — a reactivated user's devices are typically
// already revoked (from the prior suspend's revokeUserDevices call), and
// re_enroll_required must still be set so deviceGate reports the recoverable
// RE_ENROLL_REQUIRED directive instead of the terminal REVOKED one (status
// takes priority over revoked_at — see Track2-Device-Trust-Directive.md D-C).
//
// Idempotent on replay: gated on status <> 're_enroll_required', so
// re-running against already-marked rows affects 0 rows. Returns the number
// of rows actually affected.
func markUserDevicesReEnrollRequired(
	ctx context.Context,
	db *pgxpool.Pool,
	userID, workspaceID string,
) (int64, error) {
	tag, err := db.Exec(ctx,
		`UPDATE client_devices
		    SET status = 're_enroll_required'
		  WHERE user_id      = $1
		    AND workspace_id = $2
		    AND status      <> 're_enroll_required'`,
		userID, workspaceID,
	)
	if err != nil {
		return 0, fmt.Errorf("mark user devices re-enroll required: %w", err)
	}
	return tag.RowsAffected(), nil
}
