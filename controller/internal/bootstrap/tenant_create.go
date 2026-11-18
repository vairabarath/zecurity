package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yourorg/ztna/controller/internal/audit"
	"github.com/yourorg/ztna/controller/internal/invitation"
	"github.com/yourorg/ztna/controller/internal/quota"
	"github.com/yourorg/ztna/controller/internal/tenantstate"
)

// Provider tenant creation (Sprint 23 S2, F7, D-44). A super-admin creates a
// workspace through the SAME workspace bootstrap as self-signup
// (createWorkspaceTx) but with no first user; the first administrator gets a
// single-use, time-limited invitation that grants the admin role and is taken
// at their first sign-in with a matching verified email (Provision).

var (
	ErrInvalidSlug  = errors.New("invalid slug")
	ErrInvalidName  = errors.New("invalid name")
	ErrInvalidEmail = errors.New("invalid email")
	ErrSlugTaken    = errors.New("slug taken")
	ErrPlanNotFound = errors.New("plan not found")

	ErrTenantNotFound   = errors.New("tenant not found")
	ErrTenantNotActive  = errors.New("tenant not active")
	ErrFirstAdminExists = errors.New("tenant already has an admin")
	ErrEmailHasAccount  = errors.New("email already has an account in this tenant")
)

// FirstAdminInvitationTTL is how long the first-admin invitation is valid
// (the same 7 days as an invitation sent by a tenant admin).
const FirstAdminInvitationTTL = 7 * 24 * time.Hour

// slugPattern: one DNS label. The trust domain is "ws-<slug>.zecurity.in", so
// "ws-" + slug must fit a 63-byte label: at most 60 characters.
var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,58}[a-z0-9])?$`)

// CreateTenantInput is a validated-at-entry request to create a tenant.
type CreateTenantInput struct {
	Slug       string
	Name       string
	PlanID     *string // nil: the current default plan (062_ trigger)
	AdminEmail string
}

// CreatedTenant is what CreateTenant made. Invitation carries the token for
// the invitation email only; callers must never log, audit or return it.
type CreatedTenant struct {
	WorkspaceID string
	Slug        string
	Name        string
	TrustDomain string
	PlanID      string
	PlanName    string
	AdminEmail  string
	Invitation  *invitation.Invitation
}

// CreateTenantAudit is called inside the creation transaction, as the last
// write, so the provider audit row commits with the tenant (F17).
type CreateTenantAudit func(ctx context.Context, tx pgx.Tx, t *CreatedTenant) error

// NormalizeTenantInput validates and normalizes a create request.
func NormalizeTenantInput(in CreateTenantInput) (CreateTenantInput, error) {
	in.Slug = strings.TrimSpace(in.Slug)
	if !slugPattern.MatchString(in.Slug) {
		return in, ErrInvalidSlug
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || utf8.RuneCountInString(in.Name) > 100 || strings.IndexFunc(in.Name, unicode.IsControl) >= 0 {
		return in, ErrInvalidName
	}
	email := strings.ToLower(strings.TrimSpace(in.AdminEmail))
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || len(email) > 254 {
		return in, ErrInvalidEmail
	}
	in.AdminEmail = email
	return in, nil
}

// CreateTenant creates the workspace and its first-admin invitation in one
// transaction with the caller's audit.
func (s *Service) CreateTenant(ctx context.Context, in CreateTenantInput, auditFn CreateTenantAudit) (*CreatedTenant, error) {
	in, err := NormalizeTenantInput(in)
	if err != nil {
		return nil, err
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	if in.PlanID != nil {
		// FOR SHARE: the plan can't be deleted before this workspace commits
		// (the FK is RESTRICT anyway; this turns the race into a clear error).
		var name string
		err := tx.QueryRow(ctx, `SELECT name FROM plans WHERE id = $1 FOR SHARE`, *in.PlanID).Scan(&name)
		if isNoRows(err) {
			return nil, ErrPlanNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("lookup plan: %w", err)
		}
	}

	// Same-slug creates race on the UNIQUE index: the loser's insert waits
	// for the winner's commit and then fails with a unique violation, mapped
	// to ErrSlugTaken (the transaction is abandoned, nothing is written).
	tenantID, err := s.createWorkspaceTx(ctx, tx, in.Slug, in.Name, in.PlanID, nil)
	if err != nil {
		if isSlugViolation(err) {
			return nil, ErrSlugTaken
		}
		return nil, err
	}

	t := &CreatedTenant{WorkspaceID: tenantID, Slug: in.Slug, Name: in.Name, AdminEmail: in.AdminEmail}
	if err := tx.QueryRow(ctx,
		`SELECT w.trust_domain, w.plan_id::text, p.name
		   FROM workspaces w JOIN plans p ON p.id = w.plan_id
		  WHERE w.id = $1`, tenantID,
	).Scan(&t.TrustDomain, &t.PlanID, &t.PlanName); err != nil {
		return nil, fmt.Errorf("read created workspace: %w", err)
	}

	inv, err := insertFirstAdminInvitation(ctx, tx, tenantID, in.AdminEmail)
	if err != nil {
		return nil, err
	}
	inv.WorkspaceName = in.Name
	t.Invitation = inv

	// PENDING-11 P-9: the tenant's own admin stream starts with its creation,
	// as on self-signup. The actor is the provider, not a tenant user.
	if err := audit.RecordTx(ctx, tx, audit.Entry{
		TenantID:   tenantID,
		ActorEmail: "provider",
		Action:     "workspace.create",
		TargetType: "workspace",
		TargetID:   tenantID,
		Details: map[string]any{
			"slug": in.Slug, "name": in.Name, "source": "provider",
			"plan_id": t.PlanID, "first_admin_email": in.AdminEmail, "invitation_id": inv.ID,
		},
	}); err != nil {
		return nil, err
	}

	if auditFn != nil {
		if err := auditFn(ctx, tx, t); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		if isSlugViolation(err) {
			return nil, ErrSlugTaken
		}
		return nil, fmt.Errorf("commit tenant creation: %w", err)
	}
	return t, nil
}

// insertFirstAdminInvitation writes the provider-issued invitation (no tenant
// sender, role admin; 067_) and its 'invited' membership row with role admin.
func insertFirstAdminInvitation(ctx context.Context, tx pgx.Tx, workspaceID, email string) (*invitation.Invitation, error) {
	// Sprint 23 S3 (D-47): the first-admin invitation takes a user slot like
	// any invitation (CreateTenant and re-issue). Same transaction, workspace
	// row locked; a refusal keeps its *quota.Exceeded.
	if err := quota.ReserveInvitation(ctx, tx, workspaceID, email); err != nil {
		return nil, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}
	token := hex.EncodeToString(raw)

	var inv invitation.Invitation
	err := tx.QueryRow(ctx,
		`INSERT INTO invitations (email, workspace_id, invited_by, token, role, expires_at)
		 VALUES ($1, $2, NULL, $3, 'admin', NOW() + make_interval(secs => $4))
		 RETURNING id::text, email, workspace_id::text, token, status, expires_at, created_at`,
		email, workspaceID, token, FirstAdminInvitationTTL.Seconds(),
	).Scan(&inv.ID, &inv.Email, &inv.WorkspaceID, &inv.Token, &inv.Status, &inv.ExpiresAt, &inv.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert first-admin invitation: %w", err)
	}
	inv.Role = "admin"

	// A re-issue to the same email refreshes the unclaimed seat; a row that
	// belongs to an account (user_id set) is never turned into the seat.
	tag, err := tx.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, email, role, status)
		 VALUES ($1, $2, 'admin', 'invited')
		 ON CONFLICT (workspace_id, email) DO UPDATE
		    SET role = 'admin', status = 'invited', invited_at = NOW()
		  WHERE workspace_members.user_id IS NULL`,
		workspaceID, email,
	)
	if err != nil {
		return nil, fmt.Errorf("insert first-admin workspace_member: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, ErrEmailHasAccount
	}
	return &inv, nil
}

// ReissuedInvitation is a re-issued first-admin invitation. As for
// CreatedTenant, Invitation.Token is for the email only.
type ReissuedInvitation struct {
	WorkspaceID   string
	WorkspaceName string
	PreviousEmail string   // "" when there was no earlier invitation
	RevokedIDs    []string // earlier invitations that were still pending
	Invitation    *invitation.Invitation
}

// ReissueAudit is called inside the re-issue transaction, as the last write.
type ReissueAudit func(ctx context.Context, tx pgx.Tx, r *ReissuedInvitation) error

// ReissueFirstAdminInvitation gives a provider-created tenant whose first
// admin never signed in a new single-use, 7-day first-admin invitation
// (S2, D-44). Every earlier first-admin invitation stops working (pending
// ones are marked 'expired'). adminEmail "" re-sends to the previous
// invitee; another address moves the seat to it. Refused once the tenant
// has an admin account: from then on its admins invite people themselves.
func (s *Service) ReissueFirstAdminInvitation(ctx context.Context, workspaceID, adminEmail string, auditFn ReissueAudit) (*ReissuedInvitation, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	r := &ReissuedInvitation{WorkspaceID: workspaceID}
	// The workspace row lock serialises re-issues (and a concurrent first
	// sign-in's seat check below) per tenant.
	var status string
	err = tx.QueryRow(ctx, `SELECT name, status FROM workspaces WHERE id = $1 FOR UPDATE`, workspaceID).Scan(&r.WorkspaceName, &status)
	if isNoRows(err) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("lock workspace: %w", err)
	}
	if status != "active" {
		return nil, ErrTenantNotActive
	}
	// S1 (D-45): no new admin invitation while the provider has the tenant
	// suspended (suspension keeps status 'active', D-34).
	if err := tenantstate.Check(ctx, tx, workspaceID); err != nil {
		if tenantstate.IsSuspended(err) {
			return nil, ErrTenantNotActive
		}
		return nil, err
	}

	err = tx.QueryRow(ctx,
		`SELECT email FROM invitations
		  WHERE workspace_id = $1 AND role = 'admin'
		  ORDER BY created_at DESC, id DESC LIMIT 1`, workspaceID).Scan(&r.PreviousEmail)
	if err != nil && !isNoRows(err) {
		return nil, fmt.Errorf("lookup previous invitation: %w", err)
	}
	email := r.PreviousEmail
	if strings.TrimSpace(adminEmail) != "" {
		in, err := NormalizeTenantInput(CreateTenantInput{Slug: "x", Name: "x", AdminEmail: adminEmail})
		if err != nil {
			return nil, err
		}
		email = in.AdminEmail
	}
	if email == "" {
		return nil, ErrInvalidEmail
	}

	rows, err := tx.Query(ctx,
		`UPDATE invitations SET status = 'expired'
		  WHERE workspace_id = $1 AND role = 'admin' AND status = 'pending'
		RETURNING id::text`, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("revoke earlier invitations: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		r.RevokedIDs = append(r.RevokedIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("revoke earlier invitations: %w", err)
	}
	// Only a provider-created tenant whose first admin never signed in has
	// a seat to re-issue (D-44 covers the FIRST admin). A tenant without
	// any provider admin invitation (self-signup), or whose admin
	// invitation was ever accepted, is refused even if all its admins were
	// later deleted (PR #225 review).
	var providerCreated, claimed bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM invitations WHERE workspace_id = $1 AND role = 'admin'),
		        EXISTS (SELECT 1 FROM invitations WHERE workspace_id = $1 AND role = 'admin' AND status = 'accepted')`,
		workspaceID).Scan(&providerCreated, &claimed); err != nil {
		return nil, fmt.Errorf("check first-admin seat: %w", err)
	}
	if !providerCreated || claimed {
		return nil, ErrFirstAdminExists
	}

	// The admin check runs after the revoke on purpose: a first sign-in that
	// consumed an earlier invitation held its row lock, so the UPDATE above
	// waited for it, and this statement (READ COMMITTED) sees its committed
	// admin. A tenant with an admin account has nothing to re-issue.
	var hasAdmin bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE tenant_id = $1 AND role = 'admin' AND status <> 'deleted')`,
		workspaceID).Scan(&hasAdmin); err != nil {
		return nil, fmt.Errorf("check admins: %w", err)
	}
	if hasAdmin {
		return nil, ErrFirstAdminExists
	}
	// A seat moved to another email: drop the old unclaimed seat.
	if _, err := tx.Exec(ctx,
		`DELETE FROM workspace_members
		  WHERE workspace_id = $1 AND role = 'admin' AND status = 'invited'
		    AND user_id IS NULL AND email <> $2`, workspaceID, email); err != nil {
		return nil, fmt.Errorf("drop earlier first-admin seat: %w", err)
	}

	inv, err := insertFirstAdminInvitation(ctx, tx, workspaceID, email)
	if err != nil {
		return nil, err
	}
	inv.WorkspaceName = r.WorkspaceName
	r.Invitation = inv

	if err := audit.RecordTx(ctx, tx, audit.Entry{
		TenantID:   workspaceID,
		ActorEmail: "provider",
		Action:     "invitation.create",
		TargetType: "invitation",
		TargetID:   inv.ID,
		Details: map[string]any{
			"role": "admin", "first_admin": true, "reissued": true, "email": email,
			"previous_email": r.PreviousEmail, "revoked_invitation_ids": r.RevokedIDs,
		},
	}); err != nil {
		return nil, err
	}
	if auditFn != nil {
		if err := auditFn(ctx, tx, r); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit re-issue: %w", err)
	}
	return r, nil
}

func isSlugViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" &&
		(pgErr.ConstraintName == "workspaces_slug_key" || pgErr.ConstraintName == "workspaces_trust_domain_key")
}
