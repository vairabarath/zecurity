package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/appmeta" // WorkspaceTrustDomain for bootstrap INSERT
	"github.com/yourorg/ztna/controller/internal/audit"
	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/pki"
	"github.com/yourorg/ztna/controller/internal/quota"
	"github.com/yourorg/ztna/controller/internal/tenantstate"
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
	//
	// On an ENTERPRISE connection the invite lookup is SCOPED to the workspace
	// that owns the connection. Unscoped, an invite pending in workspace B would
	// let someone signing in through workspace A's IdP join B — the invite is
	// matched on email, which is not an identity key and not workspace proof.
	var pendingWorkspaceID, pendingRole string
	var firstAdmin bool
	var err error
	if in.ConnectionTenantID != nil {
		err = s.Pool.QueryRow(ctx,
			`SELECT wm.workspace_id, wm.role, `+firstAdminInviteSQL+`
			   FROM workspace_members wm
			  WHERE wm.email = $1
			    AND wm.workspace_id = $2
			    AND wm.status = 'invited'
			    AND wm.user_id IS NULL
			    AND `+usableInviteSQL+`
			  LIMIT 1`,
			email, *in.ConnectionTenantID,
		).Scan(&pendingWorkspaceID, &pendingRole, &firstAdmin)
	} else {
		// Platform login is not workspace-scoped, so the email may hold
		// pending invitations in several workspaces, and this first sign-in
		// binds the identity to exactly one of them (ADR-024). Choose
		// deterministically (S2 post-phase fix): the workspace the person
		// signed in for (the invite link and the login page pass its name),
		// then a provider first-admin seat (it has no other way in), then
		// the oldest invitation.
		err = s.Pool.QueryRow(ctx,
			`SELECT wm.workspace_id, wm.role, `+firstAdminInviteSQL+`
			   FROM workspace_members wm
			   JOIN workspaces w ON w.id = wm.workspace_id
			  WHERE wm.email = $1
			    AND wm.status = 'invited'
			    AND wm.user_id IS NULL
			    AND `+usableInviteSQL+`
			  ORDER BY ($2 <> '' AND (lower(w.name) = $2 OR w.slug = $2)) DESC,
			           `+firstAdminInviteSQL+` DESC,
			           wm.invited_at, wm.workspace_id
			  LIMIT 1`,
			email, strings.ToLower(strings.TrimSpace(in.WorkspaceName)),
		).Scan(&pendingWorkspaceID, &pendingRole, &firstAdmin)
	}

	if err == nil {
		// Audit line, NOT a warning: most enterprise IdPs never assert
		// email_verified, so this is the ordinary path for an Okta/Entra
		// invited join, not an exception. It is recorded because the invite
		// carries a role (possibly 'admin') and is matched on email, which is
		// not an identity key.
		//
		// Permitting it is safe: the connection is admin-configured, the
		// directory is the customer's own, and the lookup above is scoped to
		// the workspace that owns the connection. An IdP that EXPLICITLY says
		// email_verified=false never reaches here (providers/oidc.go rejects
		// it at token verification).
		//
		// SCIM does not pass through here — internal/scim uses its own
		// newSCIMProvisioner, so this never fires for directory-driven joins.
		if !in.EmailVerified {
			log.Printf("bootstrap: invited join on an email the IdP did not assert as verified: workspace=%s role=%s email=%q provider=%s subject=%q",
				pendingWorkspaceID, pendingRole, email, in.Provider, in.Subject)
		}
		// S2 (D-44): the provider-issued first-admin invitation grants admin,
		// so it is accepted only for an email the IdP asserted as verified.
		if firstAdmin && !in.EmailVerified {
			log.Printf("bootstrap: first-admin invitation refused: email not verified by the IdP: workspace=%s provider=%s",
				pendingWorkspaceID, in.Provider)
			return nil, identity.ErrNotInvited
		}
		return s.runInvitedUserTransaction(ctx, email, in, pendingWorkspaceID, pendingRole, firstAdmin)
	}
	if !isNoRows(err) {
		return nil, fmt.Errorf("lookup pending invite: %w", err)
	}

	// S2 (D-44): the email holds a provider first-admin seat whose invitation
	// expired (or was replaced). Refuse instead of falling through to
	// self-signup: a new workspace would bind this identity elsewhere for
	// good, and the provider could never hand it the seat. The provider
	// re-issues the invitation (tenantcreate Reinvite).
	expired, err := s.hasExpiredFirstAdminSeat(ctx, email, in.ConnectionTenantID)
	if err != nil {
		return nil, err
	}
	if expired {
		return nil, identity.ErrInvitationExpired
	}

	// No invite. An ENTERPRISE connection must never create a workspace here:
	// it already belongs to one, so provisioning would hand a brand-new
	// workspace (with ADMIN) to anyone in the customer's directory who signs in.
	// Access to THIS workspace is granted by an invite or by SCIM provisioning,
	// neither of which happened — so refuse and let the caller say so.
	if in.ConnectionTenantID != nil {
		return nil, identity.ErrNotInvited
	}

	// Platform tier: a genuine first-time signup creates the workspace.
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

	// Sprint 23 S3 (D-47): the first admin takes the new workspace's first
	// user slot (its plan is the current default, set by the 062 trigger).
	// Checked once the workspace row exists and before its CA is generated.
	tenantID, err := s.createWorkspaceTx(ctx, tx, slug, displayName, nil, func(tenantID string) error {
		if err := quota.ReserveUser(ctx, tx, tenantID, email); err != nil {
			return quotaErr(err)
		}
		return nil
	})
	if err != nil {
		return nil, err
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

	// PENDING-11 P-9: the workspace's admin stream starts with its own
	// creation (the heads exist: 053's AFTER INSERT trigger on workspaces).
	if err := audit.RecordTx(ctx, tx, audit.Entry{
		TenantID:    tenantID,
		ActorUserID: userID,
		ActorEmail:  email,
		Action:      "workspace.create",
		TargetType:  "workspace",
		TargetID:    tenantID,
		Details:     map[string]any{"slug": slug, "name": displayName, "provider": in.Provider},
	}); err != nil {
		return nil, err
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

// createWorkspaceTx is the workspace half of bootstrap, shared by self-signup
// (runBootstrapTransaction) and provider tenant creation (CreateTenant, D-44)
// so both produce the same workspace: the row with its SPIFFE trust domain,
// the workspace CA (key sealed, certificate on the row) and status 'active'.
// The Default resource policy (049_), the audit chain heads (053_) and the
// plan (062_: the current default unless planID is given) come from the
// workspaces INSERT triggers, so they match on both paths too.
// beforeCA, when set, runs once the workspace row exists and before the CA
// is generated (S3: a quota refusal costs no key generation).
func (s *Service) createWorkspaceTx(ctx context.Context, tx pgx.Tx, slug, name string, planID *string, beforeCA func(tenantID string) error) (string, error) {
	// SPIFFE trust domain derived from workspace slug.
	// Required since migration 002 makes trust_domain NOT NULL.
	trustDomain := appmeta.WorkspaceTrustDomain(slug)

	var tenantID string
	err := tx.QueryRow(
		ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain, plan_id)
		 VALUES ($1, $2, 'provisioning', $3, $4)
		 RETURNING id`,
		slug,
		name,
		trustDomain,
		planID,
	).Scan(&tenantID)
	if err != nil {
		return "", fmt.Errorf("insert workspace: %w", err)
	}

	if beforeCA != nil {
		if err := beforeCA(tenantID); err != nil {
			return "", err
		}
	}

	caResult, err := s.PKIService.GenerateWorkspaceCA(ctx, tenantID)
	if err != nil {
		return "", fmt.Errorf("generate workspace CA: %w", err)
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
		return "", fmt.Errorf("insert workspace_ca_keys: %w", err)
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
		return "", fmt.Errorf("activate workspace: %w", err)
	}
	return tenantID, nil
}

// runInvitedUserTransaction creates a user record for an invited person and
// links them to the existing workspace they were invited to. No new workspace
// is created — the invite already assigned them a workspace and role.
func (s *Service) runInvitedUserTransaction(
	ctx context.Context,
	email string,
	in identity.ProvisionInput,
	workspaceID, role string,
	firstAdmin bool,
) (*identity.PrincipalCore, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Sprint 23 S1 (D-45): a suspended tenant gets no new members; checked
	// before anything else so a refusal says "suspended" and writes
	// nothing (no user, no identity link, invitation not consumed).
	if err := tenantstate.Check(ctx, tx, workspaceID); err != nil {
		return nil, err
	}

	// Sprint 23 S3 (D-47): the invited join takes no new slot (the pending
	// invitation already counts the identity), but it goes through the user
	// quota like every user-creating path. It runs BEFORE the first-admin
	// invitation is consumed (a consumed invitation no longer counts), and
	// it locks the workspace row (FOR NO KEY UPDATE), which serialises joins
	// and a concurrent suspension.
	if err := quota.ReserveUser(ctx, tx, workspaceID, email); err != nil {
		return nil, quotaErr(err)
	}
	// S1 again, under that lock: a suspension that committed after the
	// first check is seen now (lock order: workspace row first, no share
	// lock taken before the quota lock, so no upgrade deadlock).
	if err := tenantstate.CheckForShare(ctx, tx, workspaceID); err != nil {
		return nil, err
	}

	// S2 (D-44): consume the first-admin invitation before any write,
	// under its row lock. Only a pending, unexpired invitation for this email is taken; a
	// concurrent sign-in blocks here, then finds it accepted and is refused,
	// so the invitation grants admin exactly once.
	var firstAdminInvitationID string
	if firstAdmin {
		err := tx.QueryRow(ctx,
			`UPDATE invitations
			    SET status = 'accepted'
			  WHERE workspace_id = $1
			    AND email = $2
			    AND role = 'admin'
			    AND status = 'pending'
			    AND expires_at > NOW()
			RETURNING id::text`,
			workspaceID, email,
		).Scan(&firstAdminInvitationID)
		if isNoRows(err) {
			return nil, identity.ErrNotInvited
		}
		if err != nil {
			return nil, fmt.Errorf("accept first-admin invitation: %w", err)
		}
	}

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

	// This first login IS the join: activate the membership row created at
	// invite time and mark the pending invitation used, in this transaction.
	// Leaving them 'invited'/'pending' until /api/invitations/{token}/accept
	// left every user who signed in without the invite link (e.g. straight
	// through the workspace's IdP) shown as invited forever. A later accept of
	// the same link finds nothing pending and returns its usual "already used";
	// the login callback ignores that.
	tag, err := tx.Exec(ctx,
		`UPDATE workspace_members
		    SET user_id = $1, status = 'active', joined_at = NOW()
		  WHERE workspace_id = $2
		    AND email = $3
		    AND status = 'invited'`,
		userID, workspaceID, email,
	)
	if err != nil {
		return nil, fmt.Errorf("activate invited workspace_members: %w", err)
	}
	if firstAdmin {
		if tag.RowsAffected() != 1 {
			return nil, identity.ErrNotInvited
		}
		// PENDING-11 P-9: the tenant's admin stream records who took the
		// first-admin seat (never the token).
		if err := audit.RecordTx(ctx, tx, audit.Entry{
			TenantID:    workspaceID,
			ActorUserID: userID,
			ActorEmail:  email,
			Action:      "invitation.accept",
			TargetType:  "invitation",
			TargetID:    firstAdminInvitationID,
			Details:     map[string]any{"role": "admin", "first_admin": true, "provider": in.Provider},
		}); err != nil {
			return nil, err
		}
	}
	if _, err := tx.Exec(ctx,
		`UPDATE invitations
		    SET status = 'accepted'
		  WHERE workspace_id = $1
		    AND email = $2
		    AND status = 'pending'`,
		workspaceID, email,
	); err != nil {
		return nil, fmt.Errorf("mark invitation accepted: %w", err)
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

// hasExpiredFirstAdminSeat reports whether email holds an unclaimed
// first-admin seat (an 'invited' admin membership with a provider invitation)
// whose invitation is no longer usable. Called only after the usable-invite
// lookup found nothing, so any such seat is expired or replaced. On an
// enterprise connection it is scoped to that connection's workspace.
func (s *Service) hasExpiredFirstAdminSeat(ctx context.Context, email string, workspaceID *string) (bool, error) {
	var found bool
	err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM workspace_members wm
		    WHERE wm.email = $1
		      AND ($2::uuid IS NULL OR wm.workspace_id = $2::uuid)
		      AND wm.role = 'admin'
		      AND wm.status = 'invited'
		      AND wm.user_id IS NULL
		      AND `+firstAdminInviteSQL+`)`,
		email, workspaceID,
	).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("lookup expired first-admin invitation: %w", err)
	}
	return found, nil
}

// firstAdminInviteSQL reports whether the membership row "wm" IS a
// provider-issued first-admin seat: the row's own role is admin and an admin
// invitation (067_: only without a tenant sender) exists for it. Checking
// the row's role matters: an old, revoked admin invitation must not turn a
// later ordinary invitation for the same email into a "seat" (PR #225
// review: such a member was refused, then self-signed-up elsewhere).
const firstAdminInviteSQL = `(wm.role = 'admin' AND EXISTS (
	SELECT 1 FROM invitations i
	 WHERE i.workspace_id = wm.workspace_id
	   AND i.email = wm.email
	   AND i.role = 'admin'))`

// usableInviteSQL keeps a membership row usable for an invited join. Rows of a
// tenant-sent invitation behave as before; a first-admin row is usable only
// while its invitation is pending and unexpired (D-44: single-use,
// time-limited).
const usableInviteSQL = `(NOT ` + firstAdminInviteSQL + `
	OR EXISTS (
	SELECT 1 FROM invitations i
	 WHERE i.workspace_id = wm.workspace_id
	   AND i.email = wm.email
	   AND i.role = 'admin'
	   AND i.status = 'pending'
	   AND i.expires_at > NOW()))`

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

// quotaErr keeps a *quota.Exceeded as it is (the sign-in shows it) and wraps
// any other quota error (the sign-in fails closed).
func quotaErr(err error) error {
	if _, ok := quota.AsExceeded(err); ok {
		return err
	}
	return fmt.Errorf("quota check: %w", err)
}
