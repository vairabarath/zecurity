package bootstrap_test

import (
	"context"
	"testing"

	"github.com/yourorg/ztna/controller/internal/bootstrap"
	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/invitation"
	"github.com/yourorg/ztna/controller/internal/quota"
)

// Sprint 23 S3 (D-47): user-creating paths of sign-in and invitations.
//   - an invited join takes no new slot (its pending invitation already
//     counts the identity), so it succeeds at the limit and over a lowered
//     one; accepting the invitation link does too;
//   - a join whose invitation expired is a new identity: refused at the
//     limit with quota_exceeded, and nothing is created.
func TestQuota_InvitedJoinAndAccept(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	var ws, adminID string
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (slug, name, status, trust_domain)
		VALUES ('q', 'q', 'active', 'q.test') RETURNING id::text`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	conn, err := idp.NewStore(pool, plainSecrets{}).CreateWorkspaceConnection(ctx, ws, idp.CreateInput{
		Provider: "oidc", DisplayName: "Corp IdP", Issuer: "https://idp.q.test", ClientID: "c", ClientSecret: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		VALUES ($1, 'admin@q.test', 'google', 'admin', 'admin', 'active') RETURNING id::text`, ws).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	setUsers := func(n int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_quota_overrides (workspace_id, key, limit_value, reason, set_by)
			VALUES ($1, 'users', $2, 'test', 'op') ON CONFLICT (workspace_id, key) DO UPDATE SET limit_value = EXCLUDED.limit_value`, ws, n); err != nil {
			t.Fatal(err)
		}
	}
	invites := invitation.NewStore(pool)
	setUsers(2)
	inv, err := invites.CreateInvitation(ctx, "dana@q.test", ws, adminID)
	if err != nil {
		t.Fatalf("invite below the limit: %v", err)
	}
	if _, err := invites.CreateInvitation(ctx, "erin@q.test", ws, adminID); err == nil {
		t.Fatal("a third identity was invited past the limit")
	} else if qe, ok := quota.AsExceeded(err); !ok || qe.Key != quota.KeyUsers || qe.Limit != 2 || qe.Used != 2 {
		t.Fatalf("over the limit: %v", err)
	}

	// Lowered below usage: the invited identity still joins (no new slot).
	setUsers(1)
	svc := &bootstrap.Service{Pool: pool}
	p, err := svc.Provision(ctx, identity.ProvisionInput{
		Email: "dana@q.test", Provider: "oidc", Subject: "dana", ConnectionID: conn.ID,
		Issuer: "https://idp.q.test", ConnectionTenantID: &ws, EmailVerified: true,
	})
	if err != nil {
		t.Fatalf("invited join over a lowered limit: %v", err)
	}
	// Accepting a (pending) invitation link takes no slot either.
	inv2, err := func() (*invitation.Invitation, error) {
		setUsers(5)
		return invites.CreateInvitation(ctx, "dana@q.test", ws, adminID)
	}()
	if err != nil {
		t.Fatal(err)
	}
	setUsers(1)
	if err := invites.AcceptInvitation(ctx, inv2.Token, ws, p.UserID, "dana@q.test"); err != nil {
		t.Fatalf("accept over a lowered limit: %v", err)
	}
	_ = inv

	// An invitation that expired no longer counts: its identity is new.
	setUsers(5)
	if _, err := invites.CreateInvitation(ctx, "fred@q.test", ws, adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE invitations SET expires_at = NOW() - INTERVAL '1 second' WHERE email = 'fred@q.test'`); err != nil {
		t.Fatal(err)
	}
	setUsers(2) // admin + dana
	_, err = svc.Provision(ctx, identity.ProvisionInput{
		Email: "fred@q.test", Provider: "oidc", Subject: "fred", ConnectionID: conn.ID,
		Issuer: "https://idp.q.test", ConnectionTenantID: &ws, EmailVerified: true,
	})
	if qe, ok := quota.AsExceeded(err); !ok || qe.Key != quota.KeyUsers {
		t.Fatalf("join on an expired invitation at the limit: %v, want quota_exceeded users", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE tenant_id = $1 AND email = 'fred@q.test'`, ws).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused join created %d users (err %v)", n, err)
	}
}

// A brand-new workspace's first admin takes the first slot of the default
// plan; a default plan with no user slot refuses the signup (and creates no
// workspace).
func TestQuota_FirstSignup(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE plans SET limits = jsonb_set(limits, '{users}', '0') WHERE is_default`); err != nil {
		t.Fatal(err)
	}
	var connID string
	if err := pool.QueryRow(ctx, `INSERT INTO identity_connections (provider, managed, display_name, issuer)
		VALUES ('google', true, 'Google', 'https://accounts.google.com') ON CONFLICT DO NOTHING RETURNING id::text`).Scan(&connID); err != nil {
		if err := pool.QueryRow(ctx, `SELECT id::text FROM identity_connections WHERE provider = 'google' AND tenant_id IS NULL`).Scan(&connID); err != nil {
			t.Fatal(err)
		}
	}
	svc := &bootstrap.Service{Pool: pool}
	in := identity.ProvisionInput{Email: "founder@new.test", Provider: "google", Subject: "founder", ConnectionID: connID,
		Issuer: "https://accounts.google.com", WorkspaceName: "New Co"}
	_, err := svc.Provision(ctx, in)
	if qe, ok := quota.AsExceeded(err); !ok || qe.Key != quota.KeyUsers || qe.Limit != 0 {
		t.Fatalf("signup on a zero-user default plan: %v, want quota_exceeded users", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workspaces`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("refused signup left %d workspaces (err %v)", n, err)
	}
}
