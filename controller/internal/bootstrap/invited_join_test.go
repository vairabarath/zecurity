package bootstrap_test

// An invited user who joins by signing in (without the invite link's accept
// call) is an active member, and the invitation is used. DB-backed: must run,
// not skip, in the gate.

import (
	"context"
	"encoding/base64"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/bootstrap"
	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/invitation"
	"github.com/yourorg/ztna/controller/internal/pki"
)

type plainSecrets struct{ pki.Service }

func (plainSecrets) EncryptSecret(p []byte, _ string) (string, string, error) {
	return base64.StdEncoding.EncodeToString(p), "n", nil
}
func (plainSecrets) DecryptSecret(c, _, _ string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(c)
}

func newDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	name := "bootstrap_invjoin_" + strconv.Itoa(os.Getpid()) + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name) })
	u, _ := url.Parse(adminDSN)
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	files, _ := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	sort.Strings(files)
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return pool
}

func TestInvitedJoinActivatesMembership(t *testing.T) {
	pool := newDB(t)
	ctx := context.Background()

	var ws string
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (slug, name, status, trust_domain)
		VALUES ('inv', 'inv', 'active', 'inv.test') RETURNING id::text`).Scan(&ws); err != nil {
		t.Fatal(err)
	}
	conn, err := idp.NewStore(pool, plainSecrets{}).CreateWorkspaceConnection(ctx, ws, idp.CreateInput{
		Provider: "oidc", DisplayName: "Corp IdP", Issuer: "https://idp.inv.test",
		ClientID: "c", ClientSecret: "s",
	})
	if err != nil {
		t.Fatal(err)
	}
	var adminID string
	if err := pool.QueryRow(ctx, `INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		VALUES ($1, 'admin@inv.test', 'google', 'admin', 'admin', 'active') RETURNING id::text`, ws).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	inv, err := invitation.NewStore(pool).CreateInvitation(ctx, "Dana@Inv.Test", ws, adminID)
	if err != nil {
		t.Fatal(err)
	}

	svc := &bootstrap.Service{Pool: pool}
	p, err := svc.Provision(ctx, identity.ProvisionInput{
		Email: "dana@inv.test", Provider: "oidc", Subject: "dana", ConnectionID: conn.ID,
		Issuer: "https://idp.inv.test", ConnectionTenantID: &ws, EmailVerified: true,
	})
	if err != nil {
		t.Fatalf("provision invited user: %v", err)
	}
	if p.TenantID != ws || p.Role != "member" {
		t.Fatalf("joined %s as %s, want %s as member", p.TenantID, p.Role, ws)
	}

	var memberStatus string
	var joined *time.Time
	var linked string
	if err := pool.QueryRow(ctx,
		`SELECT status, joined_at, COALESCE(user_id::text, '') FROM workspace_members
		  WHERE workspace_id = $1 AND email = 'dana@inv.test'`, ws).Scan(&memberStatus, &joined, &linked); err != nil {
		t.Fatal(err)
	}
	if memberStatus != "active" || joined == nil || linked != p.UserID {
		t.Fatalf("membership status=%q joined=%v user=%q, want active, set, %s", memberStatus, joined, linked, p.UserID)
	}
	var invStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM invitations WHERE id = $1`, inv.ID).Scan(&invStatus); err != nil {
		t.Fatal(err)
	}
	if invStatus != "accepted" {
		t.Fatalf("invitation status = %q, want accepted", invStatus)
	}

	// The invite link's later accept finds nothing pending ("already used"),
	// which the login callback ignores; the membership stays active.
	err = invitation.NewStore(pool).AcceptInvitation(ctx, inv.Token, ws, p.UserID, "dana@inv.test")
	if err != invitation.ErrNotFound {
		t.Fatalf("later accept: err = %v, want ErrNotFound", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM workspace_members WHERE workspace_id = $1 AND email = 'dana@inv.test'`, ws).Scan(&memberStatus); err != nil || memberStatus != "active" {
		t.Fatalf("after later accept: %q, %v", memberStatus, err)
	}
}
