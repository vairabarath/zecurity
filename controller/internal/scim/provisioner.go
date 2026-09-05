package scim

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/identity"
)

// scimProvisioner satisfies identity.Provisioner for the directory-provisioning
// path. Unlike the bootstrap provisioner (internal/bootstrap) it does NOT create
// a workspace — the connection already belongs to a known workspace, and SCIM
// users are plain members of that workspace. It writes the canonical users row
// AND its external_identities link in ONE transaction so a provisioned identity
// is never left without its identity key (ADR-024 invariant #4).
//
// It is constructed per-request by DirectoryService with the resolved
// (workspaceID, connectionID) so the workspace boundary is never derived from
// the SCIM payload (ADR-025 §10).
type scimProvisioner struct {
	pool      *pgxpool.Pool
	tenantID  string
	connID    string
	syncInst  string
	publisher identity.EventPublisher
}

var _ identity.Provisioner = (*scimProvisioner)(nil)

// newSCIMProvisioner builds a provisioner bound to one (workspace, connection,
// sync instance) scope. A nil publisher is accepted (callers pass the directory
// service's publisher, which may be NopPublisher in tests).
func newSCIMProvisioner(pool *pgxpool.Pool, tenantID, connID, syncInst string, pub identity.EventPublisher) *scimProvisioner {
	if pub == nil {
		pub = identity.NopPublisher{}
	}
	return &scimProvisioner{pool: pool, tenantID: tenantID, connID: connID, syncInst: syncInst, publisher: pub}
}

// Provision creates the canonical user for a never-seen SCIM identity. It mirrors
// the atomicity contract of bootstrap.insertExternalIdentity but omits workspace
// creation, CA generation, and workspace_members insertion — a SCIM-provisioned
// member joins an existing workspace owned by the connection.
func (p *scimProvisioner) Provision(ctx context.Context, in identity.ProvisionInput) (*identity.PrincipalCore, error) {
	if p.tenantID == "" {
		return nil, fmt.Errorf("scim provision: tenantID is required")
	}
	if in.ConnectionID == "" || in.Subject == "" {
		return nil, fmt.Errorf("scim provision: connection_id and subject are required")
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if email == "" {
		// The canonical key (subject) is the identity anchor; email is a hint.
		// A directory may push a user with no email — that is allowed.
		email = in.Subject
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("scim provision begin: %w", err)
	}
	defer tx.Rollback(ctx)

	var userID string
	err = tx.QueryRow(ctx,
		`INSERT INTO users
		   (tenant_id, email, provider, provider_sub, role, status,
		    provisioned_by, provisioning_owner, sync_instance_id)
		 VALUES ($1, $2, $3, $4, 'member', 'active', 'scim', 'scim', $5)
		 RETURNING id`,
		p.tenantID, email, in.Provider, in.Subject, nullIfUUID(p.syncInst),
	).Scan(&userID)
	if err != nil {
		return nil, fmt.Errorf("scim insert user: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO external_identities
		   (tenant_id, user_id, connection_id, issuer, subject, sync_instance_id)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		p.tenantID, userID, in.ConnectionID, in.Issuer, in.Subject, nullIfUUID(p.syncInst),
	); err != nil {
		return nil, fmt.Errorf("scim insert external_identity: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("scim provision commit: %w", err)
	}

	_ = p.publisher.Publish(ctx, identity.Event{
		TenantID:    p.tenantID,
		ActorUserID: userID,
		ActorEmail:  email,
		Action:      "identity.user.provisioned",
		TargetType:  "user",
		TargetID:    userID,
		Details:     map[string]any{"provider": in.Provider, "connection_id": p.connID, "source": "scim"},
	})

	return &identity.PrincipalCore{
		UserID:     userID,
		TenantID:   p.tenantID,
		Role:       "member",
		Email:      email,
		Status:     "active",
		Generation: 1,
	}, nil
}

// nullIfUUID returns nil for an empty UUID string, else the string. Used for the
// nullable sync_instance_id columns.
func nullIfUUID(s string) any {
	if s == "" {
		return nil
	}
	return s
}
