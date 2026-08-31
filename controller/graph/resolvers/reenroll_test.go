package resolvers

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/db"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// ADR-024. Certificate expiry stays a hard trust boundary: renewal needs a valid
// cert, an expired one must re-enrol. Before this, the ONLY way to re-enrol was
// revoke + create-new, which mints a new connector id and therefore orphans every
// shield bound to the old one — the cascade ADR-024 records as instance 1. So the
// recovery from one failure created another.
//
// The assertion that carries the fix is NOT "status became pending". It is that the
// connector KEEPS ITS ID and its shields still resolve afterwards. A test that only
// checked the status would pass against a revoke-and-recreate implementation, which
// is the thing being replaced.
//
// Self-contained: seeds its own workspace, so unlike its neighbours it needs no
// RESOURCE_TEST_SHIELD_ID.
func TestReenrollKeepsIdentityAndBindings(t *testing.T) {
	dsn := os.Getenv("RESOURCE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("RESOURCE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	_, _ = pool.Exec(ctx, `DELETE FROM workspaces WHERE slug = 'reenroll-test'`)

	must := func(q string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
		return id
	}

	wsID := must(`INSERT INTO workspaces (slug, name, trust_domain, status)
		VALUES ('reenroll-test','reenroll','reenroll.example.com','active') RETURNING id`)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id = $1`, wsID) })

	rnID := must(`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1,'rn','office') RETURNING id`, wsID)

	// 'disconnected' is the expired-cert state: the connector cannot heartbeat, so
	// the disconnect watcher has already marked it.
	connID := must(`INSERT INTO connectors (tenant_id, remote_network_id, name, status, lan_addr, cert_serial, cert_not_after)
		VALUES ($1,$2,'conn','disconnected','10.0.0.1:9091','abc123', NOW() - INTERVAL '1 day') RETURNING id`, wsID, rnID)
	shieldID := must(`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status, trust_domain)
		VALUES ($1,$2,$3,'sh','disconnected','reenroll.example.com') RETURNING id`, wsID, rnID, connID)
	resID := must(`INSERT INTO resources (tenant_id, remote_network_id, shield_id, name, host, protocol, port_from, port_to, status, pending_action)
		VALUES ($1,$2,$3,'db','10.0.0.5','tcp',5432,5432,'protected','apply') RETURNING id`, wsID, rnID, shieldID)

	mr := &mutationResolver{&Resolver{Pool: pool, TenantDB: db.NewTenantDB(pool)}}
	tctx := tenant.Set(ctx, tenant.TenantContext{
		TenantID: wsID,
		UserID:   "00000000-0000-0000-0000-000000000000",
		Role:     "admin",
		Email:    "reenroll-test@example.com",
	})

	connStatus := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT status FROM connectors WHERE id = $1`, connID).Scan(&s); err != nil {
			t.Fatalf("read connector: %v", err)
		}
		return s
	}

	// ── The recovery path ────────────────────────────────────────────────────
	ok, err := mr.ReenrollConnector(tctx, connID)
	if err != nil || !ok {
		t.Fatalf("ReenrollConnector: ok=%v err=%v", ok, err)
	}
	if got := connStatus(); got != "pending" {
		t.Fatalf("status = %q, want pending — Enroll accepts only a pending connector", got)
	}

	// THE assertion: identity and bindings survive. This is what revoke+create-new
	// destroys, and the entire reason this mutation exists.
	var stillThere bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM connectors WHERE id = $1 AND tenant_id = $2)`, connID, wsID,
	).Scan(&stillThere); err != nil || !stillThere {
		t.Fatalf("connector id %s no longer exists — re-enrolment must PRESERVE the identity", connID)
	}
	var boundConn string
	if err := pool.QueryRow(ctx, `SELECT connector_id::text FROM shields WHERE id = $1`, shieldID).Scan(&boundConn); err != nil {
		t.Fatalf("shield lookup: %v — its connector was destroyed", err)
	}
	if boundConn != connID {
		t.Fatalf("shield now points at %s, want %s — its binding was broken", boundConn, connID)
	}
	var resStatus string
	var shieldSet bool
	if err := pool.QueryRow(ctx,
		`SELECT status, shield_id IS NOT NULL FROM resources WHERE id = $1`, resID,
	).Scan(&resStatus, &shieldSet); err != nil {
		t.Fatalf("read resource: %v", err)
	}
	if resStatus != "protected" || !shieldSet {
		t.Fatalf("resource is status=%q shield_set=%v, want protected/true — re-enrolment must not demote resources", resStatus, shieldSet)
	}

	// Credential columns must be cleared, or a stale serial outlives the cert.
	var serialSet, notAfterSet, jtiSet bool
	if err := pool.QueryRow(ctx,
		`SELECT cert_serial IS NOT NULL, cert_not_after IS NOT NULL, enrollment_token_jti IS NOT NULL
		   FROM connectors WHERE id = $1`, connID,
	).Scan(&serialSet, &notAfterSet, &jtiSet); err != nil {
		t.Fatalf("read cert columns: %v", err)
	}
	if serialSet || notAfterSet || jtiSet {
		t.Fatalf("stale credential columns left set: serial=%v not_after=%v jti=%v", serialSet, notAfterSet, jtiSet)
	}

	// Idempotent: a second click just re-arms, so the operator can get a fresh token.
	if ok, err := mr.ReenrollConnector(tctx, connID); err != nil || !ok {
		t.Fatalf("second ReenrollConnector: ok=%v err=%v — must be idempotent", ok, err)
	}

	// ── The two refusals that keep expiry a hard boundary ────────────────────

	// ACTIVE is refused: a pending connector is excluded from ACL snapshots
	// (policy/store.go `c.status = 'active'`), so this would be an outage
	// disguised as a repair.
	if _, err := pool.Exec(ctx, `UPDATE connectors SET status = 'active' WHERE id = $1`, connID); err != nil {
		t.Fatalf("set active: %v", err)
	}
	if _, err := mr.ReenrollConnector(tctx, connID); err == nil {
		t.Fatal("re-enrolled an ACTIVE connector — that removes it from ACL snapshots and takes its resources offline")
	}
	if got := connStatus(); got != "active" {
		t.Fatalf("status = %q after a refused call, want active — the refusal must not half-apply", got)
	}

	// REVOKED is refused: revocation is absolute and this must never become a way
	// to un-revoke a component.
	if _, err := pool.Exec(ctx, `UPDATE connectors SET status = 'revoked' WHERE id = $1`, connID); err != nil {
		t.Fatalf("set revoked: %v", err)
	}
	if _, err := mr.ReenrollConnector(tctx, connID); err == nil {
		t.Fatal("re-enrolled a REVOKED connector — revocation must be absolute")
	}
	if got := connStatus(); got != "revoked" {
		t.Fatalf("status = %q after a refused call, want revoked", got)
	}

	// ── The shield mirror ────────────────────────────────────────────────────
	if ok, err := mr.ReenrollShield(tctx, shieldID); err != nil || !ok {
		t.Fatalf("ReenrollShield: ok=%v err=%v", ok, err)
	}
	var shieldStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM shields WHERE id = $1`, shieldID).Scan(&shieldStatus); err != nil {
		t.Fatalf("read shield: %v", err)
	}
	if shieldStatus != "pending" {
		t.Fatalf("shield status = %q, want pending", shieldStatus)
	}
	// The resource stays bound across the shield's re-enrolment too — that is the
	// point of keeping the shield id.
	if err := pool.QueryRow(ctx,
		`SELECT status, shield_id IS NOT NULL FROM resources WHERE id = $1`, resID,
	).Scan(&resStatus, &shieldSet); err != nil {
		t.Fatalf("read resource: %v", err)
	}
	if resStatus != "protected" || !shieldSet {
		t.Fatalf("resource is status=%q shield_set=%v after shield re-enrolment, want protected/true", resStatus, shieldSet)
	}

	if _, err := pool.Exec(ctx, `UPDATE shields SET status = 'revoked' WHERE id = $1`, shieldID); err != nil {
		t.Fatalf("revoke shield: %v", err)
	}
	if _, err := mr.ReenrollShield(tctx, shieldID); err == nil {
		t.Fatal("re-enrolled a REVOKED shield — revocation must be absolute")
	}
}
