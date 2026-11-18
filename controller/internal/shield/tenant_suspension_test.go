package shield

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"github.com/yourorg/ztna/controller/internal/tenantstate"
)

// Sprint 23 S1 (D-45): a new shield can't enroll while its tenant is
// suspended; after resume it can.
func TestShieldEnroll_RefusedWhileTenantSuspended(t *testing.T) {
	f := newShieldBindingFixture(t)
	token := f.mustReissue()
	if _, err := f.pool.Exec(f.ctx, `UPDATE workspaces SET suspended_at = NOW(), suspended_by = 'op@x', suspension_reason = 'test' WHERE id = $1`, f.wsID); err != nil {
		t.Fatal(err)
	}
	resp, err := f.svc(f.pki, f.rdb).Enroll(f.ctx, f.request(token))
	refusedShieldEnrollment(t, resp, err, codes.FailedPrecondition)
	if !strings.Contains(err.Error(), tenantstate.Message) {
		t.Fatalf("refusal %v, want %q", err, tenantstate.Message)
	}
	if _, err := f.pool.Exec(f.ctx, `UPDATE workspaces SET suspended_at = NULL, suspended_by = NULL, suspension_reason = NULL WHERE id = $1`, f.wsID); err != nil {
		t.Fatal(err)
	}
	resp, err = f.svc(f.pki, f.rdb).Enroll(f.ctx, f.request(f.mustReissue()))
	f.enrolledAs(resp, err)
}

// D-08 preserved: an existing shield's heartbeat is still accepted while its
// tenant is suspended (status stays 'active', D-34; the shield is not
// gated by suspension).
func TestShieldHeartbeat_UnaffectedByTenantSuspension(t *testing.T) {
	db, ctx := setupHeartbeatTestDB(t)
	must := func(q string, args ...any) string {
		t.Helper()
		var id string
		if err := db.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
		return id
	}
	wsID := must(`INSERT INTO workspaces (slug, name, trust_domain, status, suspended_at, suspended_by, suspension_reason)
		VALUES ('shield-susp','shieldsusp','shieldsusp.example.com','active', NOW(), 'op@x', 'test') RETURNING id`)
	rnID := must(`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1,'rn','office') RETURNING id`, wsID)
	connID := must(`INSERT INTO connectors (tenant_id, remote_network_id, name, status) VALUES ($1,$2,'conn','active') RETURNING id`, wsID, rnID)
	shieldID := must(`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status, lan_ip)
		VALUES ($1,$2,$3,'sh','active','10.0.0.5') RETURNING id`, wsID, rnID, connID)

	svc := &service{db: db}
	if _, _, err := svc.UpdateShieldHealth(ctx, shieldID, connID, "active", "v2", "10.0.0.5", 1000); err != nil {
		t.Fatalf("heartbeat of a suspended tenant's shield refused: %v (D-08)", err)
	}
	var version string
	if err := db.QueryRow(ctx, `SELECT COALESCE(version, '') FROM shields WHERE id = $1`, shieldID).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != "v2" {
		t.Fatalf("shield version %q, want v2 (heartbeat recorded)", version)
	}
}
