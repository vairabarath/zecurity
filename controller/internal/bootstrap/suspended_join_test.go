package bootstrap_test

import (
	"context"
	"testing"

	"github.com/yourorg/ztna/controller/internal/bootstrap"
	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/invitation"
	"github.com/yourorg/ztna/controller/internal/tenantstate"
)

// Sprint 23 S1 (D-45), review finding: an invited join into a suspended
// tenant is refused inside the provisioning transaction, before anything is
// written: no user, no identity link, the invitation stays pending and the
// membership invited. After resume the same join works.
func TestInvitedJoin_RefusedWhileSuspended(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	var ws string
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (slug, name, status, trust_domain)
		VALUES ('susp', 'susp', 'active', 'susp.test') RETURNING id::text`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	conn, err := idp.NewStore(pool, plainSecrets{}).CreateWorkspaceConnection(ctx, ws, idp.CreateInput{
		Provider: "oidc", DisplayName: "Corp IdP", Issuer: "https://idp.susp.test", ClientID: "c", ClientSecret: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	var adminID string
	if err := pool.QueryRow(ctx, `INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		VALUES ($1, 'admin@susp.test', 'google', 'admin', 'admin', 'active') RETURNING id::text`, ws).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	inv, err := invitation.NewStore(pool).CreateInvitation(ctx, "bob@susp.test", ws, adminID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET suspended_at = NOW(), suspended_by = 'op', suspension_reason = 'r' WHERE id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	svc := &bootstrap.Service{Pool: pool}
	in := identity.ProvisionInput{Email: "bob@susp.test", Provider: "oidc", Subject: "bob", ConnectionID: conn.ID,
		Issuer: "https://idp.susp.test", ConnectionTenantID: &ws, EmailVerified: true}
	if _, err := svc.Provision(ctx, in); !tenantstate.IsSuspended(err) {
		t.Fatalf("join while suspended: err = %v, want ErrSuspended", err)
	}
	var users, links int
	var invStatus, memberStatus string
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM users WHERE email = 'bob@susp.test'),
		(SELECT count(*) FROM external_identities WHERE subject = 'bob'),
		(SELECT status FROM invitations WHERE id = $1),
		(SELECT status FROM workspace_members WHERE workspace_id = $2 AND email = 'bob@susp.test')`, inv.ID, ws).
		Scan(&users, &links, &invStatus, &memberStatus); err != nil {
		t.Fatal(err)
	}
	if users != 0 || links != 0 || invStatus != "pending" || memberStatus != "invited" {
		t.Fatalf("refused join wrote users=%d links=%d invitation=%s member=%s", users, links, invStatus, memberStatus)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspaces SET suspended_at = NULL, suspended_by = NULL, suspension_reason = NULL WHERE id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if p, err := svc.Provision(ctx, in); err != nil || p.TenantID != ws {
		t.Fatalf("join after resume: %+v %v", p, err)
	}
}
