package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/appmeta" // WorkspaceTrustDomain for bootstrap INSERT
	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/pki"
)

// Service is the workspace-creating provisioner for the identity pipeline
// (PENDING-04 / ADR-024). It is the JIT-create leg the Linker invokes on a
// resolver miss: a first-time user gets a brand-new workspace (as admin), an
// invited user joins the workspace they were invited to (as member). In both
// cases the canonical users row AND its external_identities link are written in
// ONE transaction, so an authenticated login never leaves a user without an
// identity key.
//
// Identity RESOLUTION for a returning user lives in internal/identity (the
// Resolver, keyed on external_identities); this package only provisions the
// never-seen identity. It satisfies identity.Provisioner.
type Service struct {
	Pool       *pgxpool.Pool
	PKIService pki.Service
}

// Provision creates (or joins) the workspace membership for a never-before-seen
// external identity and links it. It is only called after the Resolver reports
// no existing user for (ConnectionID, Subject) — see identity.Service.
//
// Email is normalized to lowercase at entry so it matches stored invites
// regardless of admin input casing. See: ADR-005 Email Normalization.
func (s *Service) Provision(ctx context.Context, in identity.ProvisionInput) (*identity.PrincipalCore, error) {
	email := strings.ToLower(strings.TrimSpace(in.Email))

	// The connection already proved this identity cryptographically, so any
	// provider the resolver hands us is legitimate. Guard only the empty case
	// so a misconfigured caller can't create a provider-less user.
	if in.Provider == "" {
		return nil, fmt.Errorf("provision: provider is required")
	}
	if in.ConnectionID == "" || in.Subject == "" {
		return nil, fmt.Errorf("provision: connection_id and subject are required")
	}

	// New user — check workspace_members for a pending invite by email before
	// creating a new workspace. Invited users join an existing workspace as
	// their assigned role; only truly first-time signups get a new workspace as
	// 'admin'. (Email here is an invite-matching hint, never an identity key.)
	var pendingWorkspaceID, pendingRole string
	err := s.Pool.QueryRow(ctx,
		`SELECT workspace_id, role
		   FROM workspace_members
		  WHERE email = $1
		    AND status = 'invited'
		    AND user_id IS NULL
		  LIMIT 1`,
		email,
	).Scan(&pendingWorkspaceID, &pendingRole)

	if err == nil {
		return s.runInvitedUserTransaction(ctx, email, in, pendingWorkspaceID, pendingRole)
	}
	if !isNoRows(err) {
		return nil, fmt.Errorf("lookup pending invite: %w", err)
	}

	return s.runBootstrapTransaction(ctx, email, in)
}

func (s *Service) runBootstrapTransaction(
	ctx context.Context,
	email string,
	in identity.ProvisionInput,
) (*identity.PrincipalCore, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Workspace display name: the caller's chosen name, else the user's name.
	displayName := in.WorkspaceName
	if displayName == "" {
		displayName = in.Name
	}
	if displayName == "" {
		displayName = email
	}
	slug := slugify(displayName)

	// SPIFFE trust domain derived from workspace slug.
	// Required since migration 002 makes trust_domain NOT NULL.
	trustDomain := appmeta.WorkspaceTrustDomain(slug)

	var tenantID string
	err = tx.QueryRow(
		ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $2, 'provisioning', $3)
		 RETURNING id`,
		slug,
		displayName,
		trustDomain,
	).Scan(&tenantID)
	if err != nil {
		return nil, fmt.Errorf("insert workspace: %w", err)
	}

	var userID string
	err = tx.QueryRow(
		ctx,
		`INSERT INTO users
		 (tenant_id, email, provider, provider_sub, role, status)
		 VALUES ($1, $2, $3, $4, 'admin', 'active')
		 RETURNING id`,
		tenantID,
		email,
		in.Provider,
		in.Subject,
	).Scan(&userID)
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}

	if err := insertExternalIdentity(ctx, tx, tenantID, userID, in); err != nil {
		return nil, err
	}

	caResult, err := s.PKIService.GenerateWorkspaceCA(ctx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("generate workspace CA: %w", err)
	}

	_, err = tx.Exec(
		ctx,
		`INSERT INTO workspace_ca_keys
		 (tenant_id, encrypted_private_key, nonce, key_algorithm,
		  certificate_pem, not_before, not_after)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		tenantID,
		caResult.EncryptedPrivateKey,
		caResult.Nonce,
		caResult.KeyAlgorithm,
		caResult.CertificatePEM,
		caResult.NotBefore,
		caResult.NotAfter,
	)
	if err != nil {
		return nil, fmt.Errorf("insert workspace_ca_keys: %w", err)
	}

	_, err = tx.Exec(
		ctx,
		`UPDATE workspaces
		 SET status = 'active',
		     ca_cert_pem = $1,
		     updated_at = NOW()
		 WHERE id = $2`,
		caResult.CertificatePEM,
		tenantID,
	)
	if err != nil {
		return nil, fmt.Errorf("activate workspace: %w", err)
	}

	// Insert the admin into workspace_members so the table is the complete
	// record of all members (admins + invited members) for this workspace.
	_, err = tx.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, email, role, status, joined_at)
		 VALUES ($1, $2, $3, 'admin', 'active', NOW())
		 ON CONFLICT (workspace_id, email) DO NOTHING`,
		tenantID, userID, email,
	)
	if err != nil {
		return nil, fmt.Errorf("insert admin workspace_member: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit bootstrap transaction: %w", err)
	}

	return &identity.PrincipalCore{
		UserID:     userID,
		TenantID:   tenantID,
		Role:       "admin",
		Email:      email,
		Status:     "active",
		Generation: 1,
	}, nil
}

// runInvitedUserTransaction creates a user record for an invited person and
// links them to the existing workspace they were invited to. No new workspace
// is created — the invite already assigned them a workspace and role.
func (s *Service) runInvitedUserTransaction(
	ctx context.Context,
	email string,
	in identity.ProvisionInput,
	workspaceID, role string,
) (*identity.PrincipalCore, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var userID string
	err = tx.QueryRow(ctx,
		`INSERT INTO users
		 (tenant_id, email, provider, provider_sub, role, status)
		 VALUES ($1, $2, $3, $4, $5, 'active')
		 RETURNING id`,
		workspaceID, email, in.Provider, in.Subject, role,
	).Scan(&userID)
	if err != nil {
		return nil, fmt.Errorf("insert invited user: %w", err)
	}

	if err := insertExternalIdentity(ctx, tx, workspaceID, userID, in); err != nil {
		return nil, err
	}

	// Link the workspace_members row to the now-known user_id.
	// The full activation (status='active', joined_at) is done by AcceptInvitation
	// after the frontend calls /api/invitations/{token}/accept.
	_, err = tx.Exec(ctx,
		`UPDATE workspace_members
		    SET user_id = $1
		  WHERE workspace_id = $2
		    AND email = $3
		    AND status = 'invited'`,
		userID, workspaceID, email,
	)
	if err != nil {
		return nil, fmt.Errorf("link invited user to workspace_members: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit invited user transaction: %w", err)
	}

	return &identity.PrincipalCore{
		UserID:     userID,
		TenantID:   workspaceID,
		Role:       role,
		Email:      email,
		Status:     "active",
		Generation: 1,
	}, nil
}

// insertExternalIdentity writes the ADR-024 identity link inside the caller's
// provisioning transaction, so the users row and its identity key commit
// atomically. Keyed on (tenant_id, connection_id, subject) — never email.
func insertExternalIdentity(ctx context.Context, tx pgx.Tx, tenantID, userID string, in identity.ProvisionInput) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO external_identities (tenant_id, user_id, connection_id, issuer, subject)
		 VALUES ($1, $2, $3, $4, $5)`,
		tenantID, userID, in.ConnectionID, in.Issuer, in.Subject,
	)
	if err != nil {
		return fmt.Errorf("insert external_identity: %w", err)
	}
	return nil
}

// slugify converts a display name into a URL-safe lowercase slug.
func slugify(name string) string {
	var b strings.Builder
	prev := '-'

	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			prev = r
			continue
		}

		if prev != '-' {
			b.WriteRune('-')
			prev = '-'
		}
	}

	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "workspace"
	}

	return slug
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
