package connector

import (
	"testing"
)

// Sprint 20 Fix 16 (F46): which enrolled shields revoking a connector would
// strand. Real Postgres (throwaway database); set ENROLLMENT_TEST_DATABASE_URL.
func TestStrandedShields(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)

	var tenantID, netA, netB string
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ('f46', 'f46', 'active', 'ws-f46.zecurity.in') RETURNING id`,
	).Scan(&tenantID); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	for _, n := range []*string{&netA, &netB} {
		if err := pool.QueryRow(ctx,
			`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, gen_random_uuid()::text, 'other') RETURNING id`,
			tenantID,
		).Scan(n); err != nil {
			t.Fatalf("insert remote_network: %v", err)
		}
	}
	mkConn := func(net, status string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO connectors (tenant_id, remote_network_id, name, status, revoked_at)
			 VALUES ($1, $2, gen_random_uuid()::text, $3, CASE WHEN $3 = 'revoked' THEN NOW() END) RETURNING id`,
			tenantID, net, status,
		).Scan(&id); err != nil {
			t.Fatalf("insert connector(%s): %v", status, err)
		}
		return id
	}
	mkShield := func(conn, net, name, status string) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status)
			 VALUES ($1, $2, $3, $4, $5)`,
			tenantID, net, conn, name, status,
		); err != nil {
			t.Fatalf("insert shield(%s): %v", name, err)
		}
	}
	names := func(connID string) []string {
		t.Helper()
		got, err := StrandedShields(ctx, pool, tenantID, connID)
		if err != nil {
			t.Fatalf("StrandedShields: %v", err)
		}
		var out []string
		for _, s := range got {
			out = append(out, s.Name)
		}
		return out
	}

	a := mkConn(netA, "active")
	mkShield(a, netA, "s-active", "active")
	mkShield(a, netA, "s-offline", "disconnected")
	mkShield(a, netA, "s-pending", "pending") // no certificate yet
	mkShield(a, netA, "s-revoked", "revoked") // already cut off

	if got := names(a); len(got) != 2 || got[0] != "s-active" || got[1] != "s-offline" {
		t.Fatalf("last connector: stranded %v, want [s-active s-offline]", got)
	}
	// A connector in another remote network cannot take them over.
	mkConn(netB, "active")
	if got := names(a); len(got) != 2 {
		t.Fatalf("other network's connector counted: %v", got)
	}
	// A revoked peer in the same network cannot either.
	mkConn(netA, "revoked")
	if got := names(a); len(got) != 2 {
		t.Fatalf("revoked peer counted: %v", got)
	}
	// A disconnected peer can come back: nothing is stranded.
	b := mkConn(netA, "disconnected")
	if got := names(a); len(got) != 0 {
		t.Fatalf("with a disconnected peer: stranded %v, want none", got)
	}
	// Tenant-scoped.
	if got, _ := StrandedShields(ctx, pool, "00000000-0000-0000-0000-000000000000", b); len(got) != 0 {
		t.Fatalf("cross-tenant: %v", got)
	}
}
