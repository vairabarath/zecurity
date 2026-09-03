package resolvers

// Live-Postgres integration tests for the PUBLIC login-discovery resolver
// (F7-5): Query.lookupIdpConnections(workspaceSlug).
//
// The security properties under test are the ones that make an UNAUTHENTICATED
// field safe:
//   - scoping is derived from the slug, so one workspace's slug can never
//     surface another workspace's connections;
//   - only ACTIVE connections are offered, because InitiateAuth fails closed on
//     anything else (internal/auth/oidc.go) — a disabled connection is not a
//     login path;
//   - an unknown slug is an empty list, not an error (matching lookupWorkspace).
//
// Skips cleanly when PKI_TEST_DATABASE_URL is unset. Mirrors the harness style
// of idp_update_scim_config_enable_test.go, minus the OIDC fixture (this
// resolver makes no upstream calls).

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/idp"
)

type lookupIdpHarness struct {
	t        *testing.T
	pool     *pgxpool.Pool
	adminDSN string
	dbName   string
	idpStore *idp.Store
}

func newLookupIdpHarness(t *testing.T) *lookupIdpHarness {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := "resolvers_lookupidp_" + strconv.Itoa(os.Getpid()) + "_" + uuid.NewString()[:8]

	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin pool: %v", err)
	}
	defer adminPool.Close()
	_, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	parsed, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse admin dsn: %v", err)
	}
	parsed.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connect test pool: %v", err)
	}

	migDir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	files, err := filepath.Glob(filepath.Join(migDir, "*.sql"))
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read migration %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply migration %s: %v", f, err)
		}
	}

	return &lookupIdpHarness{
		t: t, pool: pool, adminDSN: adminDSN, dbName: dbName, idpStore: idp.NewStore(pool, nil),
	}
}

func (h *lookupIdpHarness) teardown() {
	// Close the test pool FIRST: Postgres refuses to drop a database that still
	// has open connections, and a pool connected to the target database can
	// never drop it. Then issue the DROP from a separate admin connection.
	h.pool.Close()

	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, h.adminDSN)
	if err != nil {
		h.t.Logf("teardown: connect admin pool: %v", err)
		return
	}
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+h.dbName); err != nil {
		h.t.Logf("teardown: drop %s: %v", h.dbName, err)
	}
}

// queryResolver built with NO tenant context anywhere — this field is public,
// and a test that leaked a tenant.Set(...) context would hide that.
func (h *lookupIdpHarness) resolver() *queryResolver {
	return &queryResolver{&Resolver{IdpStore: h.idpStore, Pool: h.pool}}
}

func (h *lookupIdpHarness) seedWorkspace(slug string) (wsID string) {
	wsID = uuid.NewString()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO workspaces (id, slug, name, status, trust_domain)
		 VALUES ($1,$2,$3,'active',$4)`,
		wsID, slug, "Corp "+slug, "td-"+slug,
	); err != nil {
		h.t.Fatalf("seed workspace %s: %v", slug, err)
	}
	return wsID
}

func (h *lookupIdpHarness) seedConn(wsID, provider, displayName, status string) (connID string) {
	connID = uuid.NewString()
	if _, err := h.pool.Exec(context.Background(),
		`INSERT INTO identity_connections
		   (id, tenant_id, protocol, provider, managed, display_name, issuer,
		    client_id, encrypted_client_secret, discovery_url, status)
		 VALUES ($1,$2,'oidc',$3,FALSE,$4,$5,'public-client-id',
		         'SUPER-SECRET-CIPHERTEXT','https://'||$3||'.example.com/.well-known/openid-configuration',$6)`,
		connID, wsID, provider, displayName, "https://"+provider+".example.com/"+connID, status,
	); err != nil {
		h.t.Fatalf("seed connection (%s/%s): %v", provider, status, err)
	}
	return connID
}

// The SHARED platform IdP (tenant_id IS NULL, managed) is seeded by migration
// 031 and constrained to one row per provider, so it is looked up, not created.
func (h *lookupIdpHarness) platformConnID(provider string) (connID string) {
	if err := h.pool.QueryRow(context.Background(),
		`SELECT id::text FROM identity_connections
		  WHERE tenant_id IS NULL AND provider = $1`, provider,
	).Scan(&connID); err != nil {
		h.t.Fatalf("lookup platform connection (%s): %v", provider, err)
	}
	return connID
}

// The enterprise subset — the workspace's own connections. Most assertions here
// are about tenant scoping, which the always-present platform tier would
// otherwise muddy.
func enterpriseOnly(conns []*graph.PublicIdpConnection) []*graph.PublicIdpConnection {
	out := make([]*graph.PublicIdpConnection, 0, len(conns))
	for _, c := range conns {
		if c.Tier == "enterprise" {
			out = append(out, c)
		}
	}
	return out
}

func (h *lookupIdpHarness) setPlatformLogin(wsID string, enabled bool) {
	if _, err := h.pool.Exec(context.Background(),
		`UPDATE workspaces SET platform_login_enabled = $2 WHERE id = $1`, wsID, enabled,
	); err != nil {
		h.t.Fatalf("set platform_login_enabled=%v: %v", enabled, err)
	}
}

// The requested slug returns that workspace's connections and NEVER another
// workspace's, even though both rows live in the same table.
func TestLookupIdpConnections_ScopedToRequestedWorkspace(t *testing.T) {
	h := newLookupIdpHarness(t)
	defer h.teardown()

	acme := h.seedWorkspace("acme")
	beta := h.seedWorkspace("beta")
	acmeConn := h.seedConn(acme, "okta", "Acme Okta", "active")
	betaConn := h.seedConn(beta, "entra", "Beta Entra", "active")

	qr := h.resolver()

	got, err := qr.LookupIdpConnections(context.Background(), "acme")
	if err != nil {
		t.Fatalf("lookup acme: %v", err)
	}
	// The shared platform tier is workspace-agnostic, so scoping is asserted on
	// the enterprise subset; the leak check below still spans everything.
	acmeEnt := enterpriseOnly(got)
	if len(acmeEnt) != 1 {
		t.Fatalf("acme: want 1 enterprise connection, got %d (%+v)", len(acmeEnt), got)
	}
	if acmeEnt[0].ID != acmeConn {
		t.Fatalf("acme: want connection %s, got %s", acmeConn, acmeEnt[0].ID)
	}
	if acmeEnt[0].Provider != "okta" || acmeEnt[0].DisplayName != "Acme Okta" {
		t.Fatalf("acme: unexpected projection %+v", acmeEnt[0])
	}
	for _, c := range got {
		if c.ID == betaConn {
			t.Fatalf("cross-workspace leak: beta connection %s returned for slug acme", betaConn)
		}
	}

	// And symmetrically, so the test cannot pass by returning "the first row".
	gotBeta, err := qr.LookupIdpConnections(context.Background(), "beta")
	if err != nil {
		t.Fatalf("lookup beta: %v", err)
	}
	betaEnt := enterpriseOnly(gotBeta)
	if len(betaEnt) != 1 || betaEnt[0].ID != betaConn {
		t.Fatalf("beta: want only %s, got %+v", betaConn, gotBeta)
	}
	for _, c := range gotBeta {
		if c.ID == acmeConn {
			t.Fatalf("cross-workspace leak: acme connection %s returned for slug beta", acmeConn)
		}
	}
}

// Only active connections are offered: InitiateAuth refuses non-active
// connections, so listing them would render a guaranteed-broken button.
func TestLookupIdpConnections_ActiveOnly(t *testing.T) {
	h := newLookupIdpHarness(t)
	defer h.teardown()

	ws := h.seedWorkspace("acme")
	activeConn := h.seedConn(ws, "okta", "Acme Okta", "active")
	disabledConn := h.seedConn(ws, "entra", "Acme Entra (disabled)", "disabled")
	deletedConn := h.seedConn(ws, "jumpcloud", "Acme JumpCloud (deleted)", "deleted")

	got, err := h.resolver().LookupIdpConnections(context.Background(), "acme")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	ent := enterpriseOnly(got)
	if len(ent) != 1 || ent[0].ID != activeConn {
		t.Fatalf("want only the active connection %s, got %+v", activeConn, got)
	}
	for _, c := range got {
		if c.ID == disabledConn {
			t.Fatalf("disabled connection %s must not be offered as a login path", disabledConn)
		}
		if c.ID == deletedConn {
			t.Fatalf("deleted connection %s must not be offered as a login path", deletedConn)
		}
	}
}

// Unknown slug: empty list, no error — same shape as lookupWorkspace's
// found:false. The login page distinguishes "empty" (Google fallback) from
// "error" (error state), so this must not surface as an error.
func TestLookupIdpConnections_UnknownSlugIsEmptyNotError(t *testing.T) {
	h := newLookupIdpHarness(t)
	defer h.teardown()

	got, err := h.resolver().LookupIdpConnections(context.Background(), "no-such-workspace")
	if err != nil {
		t.Fatalf("unknown slug must not error, got: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("unknown slug must return an empty list, got %+v", got)
	}
}

// A workspace with no connections OF ITS OWN gets only the shared platform
// tier. This is the case that drives the Google fallback on the login page:
// the client falls back when the ENTERPRISE subset is empty, not when the whole
// list is, so the bootstrap entry here is expected rather than a regression.
func TestLookupIdpConnections_WorkspaceWithNoConnections(t *testing.T) {
	h := newLookupIdpHarness(t)
	defer h.teardown()

	h.seedWorkspace("bare")
	googleConn := h.platformConnID("google")

	got, err := h.resolver().LookupIdpConnections(context.Background(), "bare")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if n := len(enterpriseOnly(got)); n != 0 {
		t.Fatalf("want no enterprise connections for a bare workspace, got %d (%+v)", n, got)
	}
	if len(got) != 1 || got[0].ID != googleConn || got[0].Tier != "bootstrap" {
		t.Fatalf("want only the platform tier %s, got %+v", googleConn, got)
	}
}

// The shared platform IdP is offered alongside the workspace's own connections,
// tagged "bootstrap" and ordered AFTER the enterprise tier, mirroring the REST
// discovery endpoint (internal/auth/discovery.go, ADR-024 §0).
func TestLookupIdpConnections_IncludesPlatformTier(t *testing.T) {
	h := newLookupIdpHarness(t)
	defer h.teardown()

	ws := h.seedWorkspace("acme")
	oktaConn := h.seedConn(ws, "okta", "Acme Okta", "active")
	googleConn := h.platformConnID("google")

	got, err := h.resolver().LookupIdpConnections(context.Background(), "acme")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want enterprise + platform, got %d: %+v", len(got), got)
	}
	// Enterprise first — the login page renders [0..] as the primary path.
	if got[0].ID != oktaConn || got[0].Tier != "enterprise" {
		t.Fatalf("want enterprise %s first, got %+v", oktaConn, got[0])
	}
	if got[1].ID != googleConn || got[1].Tier != "bootstrap" {
		t.Fatalf("want bootstrap %s second, got %+v", googleConn, got[1])
	}
}

// platform_login_enabled=false removes the shared IdP as a login path for THIS
// workspace (ADR-024 §5) without touching the workspace's own connections.
func TestLookupIdpConnections_PlatformTierSuppressedWhenDisabled(t *testing.T) {
	h := newLookupIdpHarness(t)
	defer h.teardown()

	ws := h.seedWorkspace("acme")
	oktaConn := h.seedConn(ws, "okta", "Acme Okta", "active")
	googleConn := h.platformConnID("google")
	h.setPlatformLogin(ws, false)

	got, err := h.resolver().LookupIdpConnections(context.Background(), "acme")
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(got) != 1 || got[0].ID != oktaConn {
		t.Fatalf("want only the enterprise connection %s, got %+v", oktaConn, got)
	}
	for _, c := range got {
		if c.ID == googleConn {
			t.Fatalf("platform login is disabled; %s must not be offered", googleConn)
		}
	}
}

// The toggle is PER WORKSPACE: disabling it for one must not hide the shared IdP
// from another. Guards against reading the flag from the wrong tenant.
func TestLookupIdpConnections_PlatformToggleIsPerWorkspace(t *testing.T) {
	h := newLookupIdpHarness(t)
	defer h.teardown()

	acme := h.seedWorkspace("acme")
	beta := h.seedWorkspace("beta")
	h.seedConn(acme, "okta", "Acme Okta", "active")
	h.seedConn(beta, "entra", "Beta Entra", "active")
	googleConn := h.platformConnID("google")
	h.setPlatformLogin(acme, false)

	acmeGot, err := h.resolver().LookupIdpConnections(context.Background(), "acme")
	if err != nil {
		t.Fatalf("lookup acme: %v", err)
	}
	for _, c := range acmeGot {
		if c.Tier == "bootstrap" {
			t.Fatalf("acme disabled platform login, got %+v", c)
		}
	}

	betaGot, err := h.resolver().LookupIdpConnections(context.Background(), "beta")
	if err != nil {
		t.Fatalf("lookup beta: %v", err)
	}
	var sawPlatform bool
	for _, c := range betaGot {
		if c.ID == googleConn && c.Tier == "bootstrap" {
			sawPlatform = true
		}
	}
	if !sawPlatform {
		t.Fatalf("beta still allows platform login; want %s offered, got %+v", googleConn, betaGot)
	}
}
