package bootstrap_test

// Sprint 23 S2 (F7, D-44): provider tenant creation and the first-admin
// invitation. DB-backed: must run, not skip, in the gate.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/bootstrap"
	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/invitation"
	"github.com/yourorg/ztna/controller/internal/pki"
)

const (
	freePlanID   = "00000000-0000-0000-0001-000000000001"
	legacyPlanID = "00000000-0000-0000-0001-000000000002"
)

type tenantEnv struct {
	pool   *pgxpool.Pool
	svc    *bootstrap.Service
	google string // platform Google connection (031_ seed)
}

func newTenantEnv(t *testing.T) *tenantEnv {
	t.Helper()
	pool := newDB(t)
	t.Setenv("PKI_MASTER_SECRET", "s2-tenant-create-test-secret")
	ctx := context.Background()
	p, err := pki.Init(ctx, pool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}
	e := &tenantEnv{pool: pool, svc: &bootstrap.Service{Pool: pool, PKIService: p}}
	if err := pool.QueryRow(ctx, `SELECT id::text FROM identity_connections
		WHERE tenant_id IS NULL AND provider = 'google'`).Scan(&e.google); err != nil {
		t.Fatal(err)
	}
	return e
}

// signIn is a first platform (Google) sign-in of a never-seen identity.
func (e *tenantEnv) signIn(email, subject string, verified bool) (*identity.PrincipalCore, error) {
	return e.svc.Provision(context.Background(), identity.ProvisionInput{
		Email: email, Provider: "google", Subject: subject, Name: subject,
		ConnectionID: e.google, Issuer: "https://accounts.google.com", EmailVerified: verified,
	})
}

func (e *tenantEnv) create(t *testing.T, slug, email string, plan *string) *bootstrap.CreatedTenant {
	t.Helper()
	ct, err := e.svc.CreateTenant(context.Background(), bootstrap.CreateTenantInput{
		Slug: slug, Name: strings.ToUpper(slug[:1]) + slug[1:] + " Inc", PlanID: plan, AdminEmail: email,
	}, nil)
	if err != nil {
		t.Fatalf("create tenant %s: %v", slug, err)
	}
	return ct
}

func (e *tenantEnv) scalar(t *testing.T, q string, args ...any) string {
	t.Helper()
	var v string
	if err := e.pool.QueryRow(context.Background(), q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

// workspaceShape is what self-signup and provider creation must agree on.
func (e *tenantEnv) workspaceShape(t *testing.T, ws string) string {
	return e.scalar(t, `
		SELECT concat_ws('|',
		  w.status,
		  (w.trust_domain = 'ws-' || w.slug || '.zecurity.in')::text,
		  (w.ca_cert_pem LIKE '-----BEGIN CERTIFICATE-----%')::text,
		  (SELECT count(*) FROM workspace_ca_keys k WHERE k.tenant_id = w.id AND k.certificate_pem = w.ca_cert_pem)::text,
		  (SELECT string_agg(name || ':' || is_default::text, ',') FROM resource_policies p WHERE p.workspace_id = w.id),
		  w.plan_id::text,
		  (SELECT count(*) FROM audit_logs a WHERE a.tenant_id = w.id AND a.action = 'workspace.create')::text)
		  FROM workspaces w WHERE w.id = $1`, ws)
}

// The created workspace matches a self-signup one: status, trust domain,
// workspace CA, Default policy, default plan and its creation audit.
func TestCreateTenant_EquivalentToSelfSignup(t *testing.T) {
	e := newTenantEnv(t)
	self, err := e.svc.Provision(context.Background(), identity.ProvisionInput{
		Email: "founder@self.test", Provider: "google", Subject: "founder", WorkspaceName: "Self Co",
		ConnectionID: e.google, Issuer: "https://accounts.google.com", EmailVerified: true,
	})
	if err != nil {
		t.Fatalf("self-signup: %v", err)
	}
	ct := e.create(t, "acme", "first@acme.test", nil)

	want := "active|true|true|1|Default:true|" + freePlanID + "|1"
	if got := e.workspaceShape(t, self.TenantID); got != want {
		t.Fatalf("self-signup workspace = %s, want %s", got, want)
	}
	if got := e.workspaceShape(t, ct.WorkspaceID); got != want {
		t.Fatalf("provider-created workspace = %s, want %s (same as self-signup)", got, want)
	}
	if ct.TrustDomain != "ws-acme.zecurity.in" || ct.PlanID != freePlanID || ct.PlanName != "Free" {
		t.Fatalf("created = %+v", ct)
	}
	// No first user: only the invited admin seat.
	if n := e.scalar(t, `SELECT count(*)::text FROM users WHERE tenant_id = $1`, ct.WorkspaceID); n != "0" {
		t.Fatalf("users in a provider-created workspace = %s, want 0", n)
	}
	if got := e.scalar(t, `SELECT role || '|' || status || '|' || (user_id IS NULL)::text FROM workspace_members WHERE workspace_id = $1`, ct.WorkspaceID); got != "admin|invited|true" {
		t.Fatalf("first-admin membership = %s", got)
	}
	if got := e.scalar(t, `SELECT role || '|' || status || '|' || (invited_by IS NULL)::text || '|' ||
		(expires_at BETWEEN NOW() + INTERVAL '6 days 23 hours' AND NOW() + INTERVAL '7 days 1 hour')::text
		FROM invitations WHERE workspace_id = $1`, ct.WorkspaceID); got != "admin|pending|true|true" {
		t.Fatalf("first-admin invitation = %s", got)
	}

	// A chosen plan is honoured.
	leg := legacyPlanID
	if ct2 := e.create(t, "globex", "a@globex.test", &leg); ct2.PlanID != legacyPlanID || ct2.PlanName != "Legacy" {
		t.Fatalf("plan_id not honoured: %+v", ct2)
	}
}

func TestCreateTenant_Validation(t *testing.T) {
	e := newTenantEnv(t)
	ctx := context.Background()
	missing := "11111111-1111-1111-1111-111111111111"
	cases := []struct {
		name string
		in   bootstrap.CreateTenantInput
		want error
	}{
		{"uppercase slug", bootstrap.CreateTenantInput{Slug: "Acme", Name: "A", AdminEmail: "a@a.test"}, bootstrap.ErrInvalidSlug},
		{"leading dash", bootstrap.CreateTenantInput{Slug: "-acme", Name: "A", AdminEmail: "a@a.test"}, bootstrap.ErrInvalidSlug},
		{"dot", bootstrap.CreateTenantInput{Slug: "ac.me", Name: "A", AdminEmail: "a@a.test"}, bootstrap.ErrInvalidSlug},
		{"too long", bootstrap.CreateTenantInput{Slug: strings.Repeat("a", 61), Name: "A", AdminEmail: "a@a.test"}, bootstrap.ErrInvalidSlug},
		{"empty name", bootstrap.CreateTenantInput{Slug: "acme", Name: "  ", AdminEmail: "a@a.test"}, bootstrap.ErrInvalidName},
		{"bad email", bootstrap.CreateTenantInput{Slug: "acme", Name: "A", AdminEmail: "not-an-email"}, bootstrap.ErrInvalidEmail},
		{"display-name email", bootstrap.CreateTenantInput{Slug: "acme", Name: "A", AdminEmail: "Bob <b@a.test>"}, bootstrap.ErrInvalidEmail},
		{"missing plan", bootstrap.CreateTenantInput{Slug: "acme", Name: "A", AdminEmail: "a@a.test", PlanID: &missing}, bootstrap.ErrPlanNotFound},
	}
	for _, c := range cases {
		if _, err := e.svc.CreateTenant(ctx, c.in, nil); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	if n := e.scalar(t, `SELECT count(*)::text FROM workspaces`); n != "0" {
		t.Fatalf("a refused create left %s workspaces", n)
	}
	// 60 characters is the longest slug ("ws-" + slug fits a DNS label).
	e.create(t, strings.Repeat("a", 60), "a@a.test", nil)
	// Email is normalised.
	ct := e.create(t, "norm", "  Mixed@Case.Test ", nil)
	if ct.AdminEmail != "mixed@case.test" || ct.Invitation.Email != "mixed@case.test" {
		t.Fatalf("email not normalised: %q / %q", ct.AdminEmail, ct.Invitation.Email)
	}
}

// Two creates with the same slug: exactly one wins, the other is slug_taken
// and leaves nothing behind.
func TestCreateTenant_SlugRace(t *testing.T) {
	e := newTenantEnv(t)
	ctx := context.Background()
	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.svc.CreateTenant(ctx, bootstrap.CreateTenantInput{
				Slug: "race", Name: "Race", AdminEmail: "admin@race.test",
			}, nil)
		}(i)
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case errors.Is(err, bootstrap.ErrSlugTaken):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("%d creates won, want exactly 1", won)
	}
	if got := e.scalar(t, `SELECT count(*)::text || '|' || (SELECT count(*) FROM invitations)::text || '|' ||
		(SELECT count(*) FROM workspace_ca_keys)::text FROM workspaces`); got != "1|1|1" {
		t.Fatalf("workspaces|invitations|ca keys = %s, want 1|1|1", got)
	}
}

// The first-admin invitation: refused for a non-matching or unverified
// email; taken once by the matching verified email, which becomes admin.
func TestFirstAdminInvitation_EmailBindingAndSingleUse(t *testing.T) {
	e := newTenantEnv(t)
	ctx := context.Background()
	ct := e.create(t, "acme", "first@acme.test", nil)

	// Non-matching email: never joins the tenant (it self-signs-up instead).
	other, err := e.signIn("someone@else.test", "other", true)
	if err != nil {
		t.Fatalf("non-matching sign-in: %v", err)
	}
	if other.TenantID == ct.WorkspaceID {
		t.Fatal("a non-matching email joined the provider-created tenant")
	}
	// Matching email the IdP did not verify: refused, invitation untouched.
	if _, err := e.signIn("first@acme.test", "unverified", false); !errors.Is(err, identity.ErrNotInvited) {
		t.Fatalf("unverified matching email: err = %v, want ErrNotInvited", err)
	}
	if st := e.scalar(t, `SELECT status FROM invitations WHERE id = $1`, ct.Invitation.ID); st != "pending" {
		t.Fatalf("invitation after refused sign-ins = %s, want pending", st)
	}

	// The matching verified email takes the admin seat.
	p, err := e.signIn("First@Acme.Test", "first", true)
	if err != nil {
		t.Fatalf("first admin sign-in: %v", err)
	}
	if p.TenantID != ct.WorkspaceID || p.Role != "admin" {
		t.Fatalf("first admin joined %s as %s, want %s as admin", p.TenantID, p.Role, ct.WorkspaceID)
	}
	if got := e.scalar(t, `SELECT u.role || '|' || wm.role || '|' || wm.status || '|' || i.status
		FROM users u JOIN workspace_members wm ON wm.user_id = u.id
		JOIN invitations i ON i.workspace_id = u.tenant_id AND i.email = u.email
		WHERE u.id = $1`, p.UserID); got != "admin|admin|active|accepted" {
		t.Fatalf("after accept user|member|status|invitation = %s", got)
	}
	if n := e.scalar(t, `SELECT count(*)::text FROM audit_logs WHERE tenant_id = $1 AND action = 'invitation.accept'
		AND target_id = $2 AND NOT details::text LIKE '%' || $3 || '%'`, ct.WorkspaceID, ct.Invitation.ID, ct.Invitation.Token); n != "1" {
		t.Fatalf("invitation.accept audits without the token = %s, want 1", n)
	}

	// Single use: another identity with the same email does not join.
	again, err := e.signIn("first@acme.test", "second-account", true)
	if err != nil {
		t.Fatalf("second sign-in: %v", err)
	}
	if again.TenantID == ct.WorkspaceID {
		t.Fatal("the first-admin invitation was used twice")
	}
	// The admin UI's accept endpoint never takes an admin invitation.
	if err := invitation.NewStore(e.pool).AcceptInvitation(ctx, ct.Invitation.Token, ct.WorkspaceID, p.UserID, "first@acme.test"); !errors.Is(err, invitation.ErrNotFound) {
		t.Fatalf("accept endpoint on the admin invitation: err = %v, want ErrNotFound", err)
	}
	if n := e.scalar(t, `SELECT count(*)::text FROM users WHERE tenant_id = $1`, ct.WorkspaceID); n != "1" {
		t.Fatalf("users in the tenant = %s, want 1", n)
	}
}

// Concurrent first sign-ins of two identities with the invited email: the
// invitation grants admin exactly once.
func TestFirstAdminInvitation_ConcurrentSingleUse(t *testing.T) {
	e := newTenantEnv(t)
	ct := e.create(t, "acme", "first@acme.test", nil)
	const n = 6
	var wg sync.WaitGroup
	res := make([]*identity.PrincipalCore, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res[i], errs[i] = e.signIn("first@acme.test", "acct-"+string(rune('a'+i)), true)
		}(i)
	}
	wg.Wait()
	joined := 0
	for i := range res {
		if errs[i] == nil && res[i].TenantID == ct.WorkspaceID {
			joined++
		}
	}
	if joined != 1 {
		t.Fatalf("%d sign-ins joined the tenant, want exactly 1 (errs %v)", joined, errs)
	}
	if got := e.scalar(t, `SELECT count(*)::text FROM users WHERE tenant_id = $1 AND role = 'admin'`, ct.WorkspaceID); got != "1" {
		t.Fatalf("admins = %s, want 1", got)
	}
}

// An expired first-admin invitation is not honoured at sign-in, and the
// sign-in is refused with ErrInvitationExpired (which is also an
// ErrNotInvited) instead of creating a workspace for that email: a new
// workspace would bind the identity elsewhere for good.
func TestFirstAdminInvitation_Expires(t *testing.T) {
	e := newTenantEnv(t)
	ct := e.create(t, "acme", "first@acme.test", nil)
	if _, err := e.pool.Exec(context.Background(),
		`UPDATE invitations SET expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, ct.Invitation.ID); err != nil {
		t.Fatal(err)
	}
	_, err := e.signIn("first@acme.test", "late", true)
	if !errors.Is(err, identity.ErrInvitationExpired) || !errors.Is(err, identity.ErrNotInvited) {
		t.Fatalf("sign-in after expiry: err = %v, want ErrInvitationExpired (an ErrNotInvited)", err)
	}
	if got := e.scalar(t, `SELECT (SELECT count(*) FROM workspaces)::text || '|' || (SELECT count(*) FROM users)::text || '|' ||
		(SELECT status FROM workspace_members WHERE workspace_id = $1) || '|' ||
		(SELECT status FROM invitations WHERE id = $2)`, ct.WorkspaceID, ct.Invitation.ID); got != "1|0|invited|pending" {
		t.Fatalf("after refused expired sign-in workspaces|users|seat|invitation = %s, want 1|0|invited|pending", got)
	}
	// Other emails still self-sign-up as before.
	if p, err := e.signIn("someone@else.test", "else", true); err != nil || p.TenantID == ct.WorkspaceID {
		t.Fatalf("unrelated self-signup: %v", err)
	}
}

// Re-issue (same email): earlier links stop working, the new one grants the
// seat once; the tenant and provider streams record it without the token.
func TestReissueFirstAdminInvitation_SameEmail(t *testing.T) {
	e := newTenantEnv(t)
	ctx := context.Background()
	ct := e.create(t, "acme", "first@acme.test", nil)
	if _, err := e.pool.Exec(ctx, `UPDATE invitations SET expires_at = NOW() - INTERVAL '1 second' WHERE id = $1`, ct.Invitation.ID); err != nil {
		t.Fatal(err)
	}
	r, err := e.svc.ReissueFirstAdminInvitation(ctx, ct.WorkspaceID, "", nil)
	if err != nil {
		t.Fatalf("reissue: %v", err)
	}
	if r.PreviousEmail != "first@acme.test" || r.Invitation.Email != "first@acme.test" || r.Invitation.Token == ct.Invitation.Token ||
		r.Invitation.ExpiresAt.Before(time.Now().Add(6*24*time.Hour)) || r.WorkspaceName != ct.Name {
		t.Fatalf("reissued = %+v", r)
	}
	if got := e.scalar(t, `SELECT string_agg(status, ',' ORDER BY created_at) FROM invitations WHERE workspace_id = $1`, ct.WorkspaceID); got != "expired,pending" {
		t.Fatalf("invitation statuses = %s, want expired,pending", got)
	}
	// The old link no longer works anywhere: its row is not the latest
	// usable one, and the accept endpoint never takes admin invitations.
	if st := e.scalar(t, `SELECT status FROM invitations WHERE id = $1`, ct.Invitation.ID); st != "expired" {
		t.Fatalf("old invitation status = %s, want expired (revoked by the re-issue)", st)
	}
	if n := e.scalar(t, `SELECT count(*)::text FROM workspace_members WHERE workspace_id = $1`, ct.WorkspaceID); n != "1" {
		t.Fatalf("seats = %s, want 1", n)
	}
	p, err := e.signIn("first@acme.test", "first", true)
	if err != nil || p.TenantID != ct.WorkspaceID || p.Role != "admin" {
		t.Fatalf("sign-in with the re-issued invitation: %+v, %v", p, err)
	}
	if st := e.scalar(t, `SELECT status FROM invitations WHERE id = $1`, r.Invitation.ID); st != "accepted" {
		t.Fatalf("new invitation = %s, want accepted", st)
	}
	if n := e.scalar(t, `SELECT count(*)::text FROM audit_logs WHERE tenant_id = $1 AND action = 'invitation.create'
		AND (details->>'reissued')::boolean AND position($2 in details::text) = 0`, ct.WorkspaceID, r.Invitation.Token); n != "1" {
		t.Fatalf("invitation.create (reissued, no token) audits = %s", n)
	}
	// Once the seat is taken there is nothing to re-issue.
	if _, err := e.svc.ReissueFirstAdminInvitation(ctx, ct.WorkspaceID, "", nil); !errors.Is(err, bootstrap.ErrFirstAdminExists) {
		t.Fatalf("reissue after the admin signed in: err = %v, want ErrFirstAdminExists", err)
	}
}

// Re-issue while the first link is still pending revokes it at once.
func TestReissueFirstAdminInvitation_RevokesPendingLink(t *testing.T) {
	e := newTenantEnv(t)
	ctx := context.Background()
	ct := e.create(t, "acme", "first@acme.test", nil)
	r, err := e.svc.ReissueFirstAdminInvitation(ctx, ct.WorkspaceID, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.RevokedIDs) != 1 || r.RevokedIDs[0] != ct.Invitation.ID {
		t.Fatalf("revoked = %v, want [%s]", r.RevokedIDs, ct.Invitation.ID)
	}
	// The join consumes the newest usable invitation, never the revoked one.
	if _, err := e.signIn("first@acme.test", "first", true); err != nil {
		t.Fatal(err)
	}
	if got := e.scalar(t, `SELECT (SELECT status FROM invitations WHERE id = $1) || '|' || (SELECT status FROM invitations WHERE id = $2)`,
		ct.Invitation.ID, r.Invitation.ID); got != "expired|accepted" {
		t.Fatalf("old|new = %s, want expired|accepted", got)
	}
}

// Re-issue to another email moves the seat: the old email no longer has one
// (it self-signs-up as anyone would), the new email takes it.
func TestReissueFirstAdminInvitation_NewEmail(t *testing.T) {
	e := newTenantEnv(t)
	ctx := context.Background()
	ct := e.create(t, "acme", "typo@acme.test", nil)
	r, err := e.svc.ReissueFirstAdminInvitation(ctx, ct.WorkspaceID, " Right@Acme.Test ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Invitation.Email != "right@acme.test" || r.PreviousEmail != "typo@acme.test" {
		t.Fatalf("reissued = %+v", r)
	}
	if got := e.scalar(t, `SELECT string_agg(email || ':' || role || ':' || status, ',') FROM workspace_members WHERE workspace_id = $1`, ct.WorkspaceID); got != "right@acme.test:admin:invited" {
		t.Fatalf("seats = %s", got)
	}
	if p, err := e.signIn("typo@acme.test", "typo", true); err != nil || p.TenantID == ct.WorkspaceID {
		t.Fatalf("old email after the seat moved: %+v, %v", p, err)
	}
	if p, err := e.signIn("right@acme.test", "right", true); err != nil || p.TenantID != ct.WorkspaceID || p.Role != "admin" {
		t.Fatalf("new email: %+v, %v", p, err)
	}
}

func TestReissueFirstAdminInvitation_Refusals(t *testing.T) {
	e := newTenantEnv(t)
	ctx := context.Background()
	self, err := e.signIn("owner@self.test", "owner", true)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, ws, email string
		want            error
	}{
		{"self-signup tenant has an admin", self.TenantID, "x@y.test", bootstrap.ErrFirstAdminExists},
		{"unknown tenant", "11111111-1111-1111-1111-111111111111", "x@y.test", bootstrap.ErrTenantNotFound},
	}
	ct := e.create(t, "acme", "first@acme.test", nil)
	cases = append(cases, struct {
		name, ws, email string
		want            error
	}{"bad email", ct.WorkspaceID, "not an email", bootstrap.ErrInvalidEmail})
	for _, c := range cases {
		if _, err := e.svc.ReissueFirstAdminInvitation(ctx, c.ws, c.email, nil); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	// An email that already has a (non-admin) account in the tenant is never
	// turned into the invited admin seat; nothing changes.
	var uid string
	if err := e.pool.QueryRow(ctx, `INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		VALUES ($1, 'member@acme.test', 'scim', 's1', 'member', 'active') RETURNING id::text`, ct.WorkspaceID).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO workspace_members (workspace_id, user_id, email, role, status, joined_at)
		VALUES ($1, $2, 'member@acme.test', 'member', 'active', NOW())`, ct.WorkspaceID, uid); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ReissueFirstAdminInvitation(ctx, ct.WorkspaceID, "member@acme.test", nil); !errors.Is(err, bootstrap.ErrEmailHasAccount) {
		t.Fatalf("re-issue to an account's email: err = %v, want ErrEmailHasAccount", err)
	}
	if got := e.scalar(t, `SELECT role || '|' || status FROM workspace_members WHERE workspace_id = $1 AND email = 'member@acme.test'`, ct.WorkspaceID); got != "member|active" {
		t.Fatalf("member row after refused re-issue = %s", got)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE workspaces SET status = 'provisioning' WHERE id = $1`, ct.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.ReissueFirstAdminInvitation(ctx, ct.WorkspaceID, "", nil); !errors.Is(err, bootstrap.ErrTenantNotActive) {
		t.Fatalf("inactive tenant: %v", err)
	}
	if n := e.scalar(t, `SELECT count(*)::text FROM invitations WHERE workspace_id = $1`, ct.WorkspaceID); n != "1" {
		t.Fatalf("refused re-issues wrote invitations: %s", n)
	}
}

// Only the provider-issued first-admin invitation can grant admin: a tenant
// admin's invitation is always 'member', and the database refuses an admin
// invitation with a tenant sender (067_).
func TestFirstAdminInvitation_OnlyProviderGrantsAdmin(t *testing.T) {
	e := newTenantEnv(t)
	ctx := context.Background()
	self, err := e.signIn("owner@self.test", "owner", true)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := invitation.NewStore(e.pool).CreateInvitation(ctx, "dana@self.test", self.TenantID, self.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Role != "member" {
		t.Fatalf("tenant invitation role = %q, want member", inv.Role)
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO invitations (email, workspace_id, invited_by, token, role)
		VALUES ('x@self.test', $1, $2, 'tok-admin-from-tenant', 'admin')`, self.TenantID, self.UserID); err == nil {
		t.Fatal("database accepted an admin invitation sent by a tenant user")
	}
	if _, err := e.pool.Exec(ctx, `INSERT INTO invitations (email, workspace_id, invited_by, token, role)
		VALUES ('y@self.test', $1, NULL, 'tok-member-no-sender', 'member')`, self.TenantID); err == nil {
		t.Fatal("database accepted a member invitation with no sender")
	}
	// The member invitation joins as member (unchanged behaviour).
	p, err := e.signIn("dana@self.test", "dana", true)
	if err != nil {
		t.Fatal(err)
	}
	if p.TenantID != self.TenantID || p.Role != "member" {
		t.Fatalf("member invite joined %s as %s", p.TenantID, p.Role)
	}
}

// Each single-use guard of the invited join on its own: the lookup normally
// filters a used invitation out, so these are the race's last line. A join
// that read the invitation as usable must still be refused, with nothing
// written, when (a) the invitation is no longer pending, or (b) the
// membership row is no longer 'invited'.
func TestFirstAdminInvitation_JoinGuards(t *testing.T) {
	e := newTenantEnv(t)
	ctx := context.Background()
	in := func(sub string) identity.ProvisionInput {
		return identity.ProvisionInput{Email: "first@acme.test", Provider: "google", Subject: sub,
			ConnectionID: e.google, Issuer: "https://accounts.google.com", EmailVerified: true}
	}

	// (a) invitation already accepted, membership still invited.
	a := e.create(t, "alpha", "first@acme.test", nil)
	if _, err := e.pool.Exec(ctx, `UPDATE invitations SET status = 'accepted' WHERE id = $1`, a.Invitation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RunInvitedJoin(ctx, "first@acme.test", in("a"), a.WorkspaceID, "admin", true); !errors.Is(err, identity.ErrNotInvited) {
		t.Fatalf("join on an accepted invitation: err = %v, want ErrNotInvited", err)
	}

	// (b) invitation pending, membership already active.
	b := e.create(t, "beta", "first@acme.test", nil)
	if _, err := e.pool.Exec(ctx, `UPDATE workspace_members SET status = 'active' WHERE workspace_id = $1`, b.WorkspaceID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.RunInvitedJoin(ctx, "first@acme.test", in("b"), b.WorkspaceID, "admin", true); !errors.Is(err, identity.ErrNotInvited) {
		t.Fatalf("join on an already-active seat: err = %v, want ErrNotInvited", err)
	}
	if got := e.scalar(t, `SELECT (SELECT count(*) FROM users WHERE tenant_id IN ($1, $2))::text || '|' || status
		FROM invitations WHERE id = $3`, a.WorkspaceID, b.WorkspaceID, b.Invitation.ID); got != "0|pending" {
		t.Fatalf("refused joins wrote users|invitation = %s, want 0|pending", got)
	}
}

// The admin UI's accept endpoint never takes a PENDING first-admin
// invitation, even for an account in that workspace with the invited email
// (e.g. one SCIM created): only the invitee's first sign-in can.
func TestFirstAdminInvitation_AcceptEndpointRefusesPending(t *testing.T) {
	e := newTenantEnv(t)
	ctx := context.Background()
	ct := e.create(t, "acme", "first@acme.test", nil)
	var uid string
	if err := e.pool.QueryRow(ctx, `INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		VALUES ($1, 'first@acme.test', 'scim', 'scim-1', 'member', 'active') RETURNING id::text`, ct.WorkspaceID).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	err := invitation.NewStore(e.pool).AcceptInvitation(ctx, ct.Invitation.Token, ct.WorkspaceID, uid, "first@acme.test")
	if !errors.Is(err, invitation.ErrNotFound) {
		t.Fatalf("accept endpoint on a pending admin invitation: err = %v, want ErrNotFound", err)
	}
	if got := e.scalar(t, `SELECT i.status || '|' || wm.status FROM invitations i
		JOIN workspace_members wm ON wm.workspace_id = i.workspace_id AND wm.email = i.email WHERE i.id = $1`, ct.Invitation.ID); got != "pending|invited" {
		t.Fatalf("after refused accept invitation|membership = %s, want pending|invited", got)
	}
}

// Platform sign-in with pending invitations in several workspaces binds the
// identity to exactly one, chosen deterministically (S2 post-phase fix): the
// workspace the person signed in for, then a provider first-admin seat, then
// the oldest invitation.
func TestPlatformInvitedJoin_ChoosesInvitationDeterministically(t *testing.T) {
	e := newTenantEnv(t)
	ctx := context.Background()
	owner, err := e.signIn("owner@beta.test", "owner", true)
	if err != nil {
		t.Fatal(err)
	}
	beta := owner.TenantID
	betaName := e.scalar(t, `SELECT name FROM workspaces WHERE id = $1`, beta)
	inv := invitation.NewStore(e.pool)
	// Member invitations in beta, made (and aged) before the provider seats.
	for _, em := range []string{"a@x.test", "b@x.test", "c@x.test"} {
		if _, err := inv.CreateInvitation(ctx, em, beta, owner.UserID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.pool.Exec(ctx, `UPDATE workspace_members SET invited_at = NOW() - INTERVAL '1 day' WHERE workspace_id = $1`, beta); err != nil {
		t.Fatal(err)
	}
	acme := e.create(t, "acme", "a@x.test", nil)
	e.create(t, "acme2", "b@x.test", nil)

	join := func(email, sub, hint string) *identity.PrincipalCore {
		t.Helper()
		p, err := e.svc.Provision(ctx, identity.ProvisionInput{
			Email: email, Provider: "google", Subject: sub, Name: sub, WorkspaceName: hint,
			ConnectionID: e.google, Issuer: "https://accounts.google.com", EmailVerified: true,
		})
		if err != nil {
			t.Fatalf("%s: %v", email, err)
		}
		return p
	}
	// No hint: the first-admin seat wins over an older member invitation.
	if p := join("a@x.test", "a", ""); p.TenantID != acme.WorkspaceID || p.Role != "admin" {
		t.Fatalf("no hint: joined %s as %s, want acme as admin", p.TenantID, p.Role)
	}
	// Hint names beta (the invite link's workspace): beta wins, even over a seat.
	if p := join("b@x.test", "b", "  "+strings.ToUpper(betaName)+" "); p.TenantID != beta || p.Role != "member" {
		t.Fatalf("beta hint: joined %s as %s, want beta as member", p.TenantID, p.Role)
	}
	// Two member invitations, no seat, no hint: the oldest wins, every time.
	gamma, err := e.signIn("owner@gamma.test", "gowner", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inv.CreateInvitation(ctx, "c@x.test", gamma.TenantID, gamma.UserID); err != nil {
		t.Fatal(err)
	}
	if p := join("c@x.test", "c", ""); p.TenantID != beta {
		t.Fatalf("oldest: joined %s, want beta (older invitation)", p.TenantID)
	}
}
