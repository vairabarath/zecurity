package resolvers

import (
	"testing"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/db"
	"github.com/yourorg/ztna/controller/internal/resource"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// Admin changes are audited with the actor and the target (live run
// 2026-10-29, O-2); a refused change leaves no row.
func TestAdminAudit_ChangesRecordedWithActor(t *testing.T) {
	f := newPolicyResolverFixture(t)
	f.mr.ResourceCfg = resource.Config{DB: f.pool}
	f.mr.TenantDB = db.NewTenantDB(f.pool)
	admin := createTestUser(t, f.ctx, f.pool, f.tenantID, "admin@audit.test")
	member := createTestUser(t, f.ctx, f.pool, f.tenantID, "member@audit.test")
	ctx := tenant.Set(f.ctx, tenant.TenantContext{
		TenantID: f.tenantID, UserID: admin, Email: "admin@audit.test", Role: "ADMIN",
	})

	rn, err := f.mr.CreateRemoteNetwork(ctx, "audit-net", graph.NetworkLocationOffice)
	if err != nil {
		t.Fatalf("createRemoteNetwork: %v", err)
	}
	res, err := f.mr.CreateResource(ctx, graph.CreateResourceInput{
		RemoteNetworkID: rn.ID, Name: "audit-web", Host: "10.0.0.5", Protocol: "tcp", PortFrom: 80, PortTo: 80,
	})
	if err != nil {
		t.Fatalf("createResource: %v", err)
	}
	g, err := f.mr.CreateGroup(ctx, "audit-group", nil)
	if err != nil {
		t.Fatalf("createGroup: %v", err)
	}
	if _, err := f.mr.AddGroupMember(ctx, g.ID, member); err != nil {
		t.Fatalf("addGroupMember: %v", err)
	}
	if _, err := f.mr.AssignResourceToGroup(ctx, res.ID, g.ID); err != nil {
		t.Fatalf("assignResourceToGroup: %v", err)
	}
	if _, err := f.mr.RemoveGroupMember(ctx, g.ID, member); err != nil {
		t.Fatalf("removeGroupMember: %v", err)
	}
	// Refused: not a member any more, and an unknown group.
	if _, err := f.mr.RemoveGroupMember(ctx, g.ID, member); err == nil {
		t.Fatal("removing a non-member succeeded")
	}
	if _, err := f.mr.AddGroupMember(ctx, "00000000-0000-0000-0000-000000000000", member); err == nil {
		t.Fatal("adding to an unknown group succeeded")
	}

	want := []struct{ action, targetType, targetID string }{
		{"remote_network.create", "remote_network", rn.ID},
		{"resource.create", "resource", res.ID},
		{"group.create", "group", g.ID},
		{"group.member.add", "group", g.ID},
		{"resource.group.assign", "resource", res.ID},
		{"group.member.remove", "group", g.ID},
	}
	rows, err := f.pool.Query(f.ctx,
		`SELECT action, target_type, target_id, coalesce(actor_user_id::text, ''), actor_email
		   FROM audit_logs WHERE tenant_id = $1 ORDER BY created_at, action`, f.tenantID)
	if err != nil {
		t.Fatalf("audit query: %v", err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var action, targetType, targetID, actorID, actorEmail string
		if err := rows.Scan(&action, &targetType, &targetID, &actorID, &actorEmail); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if i >= len(want) {
			t.Fatalf("extra audit row %s %s/%s (a refused change was audited?)", action, targetType, targetID)
		}
		w := want[i]
		if action != w.action || targetType != w.targetType || targetID != w.targetID {
			t.Errorf("row %d = %s %s/%s, want %s %s/%s", i, action, targetType, targetID, w.action, w.targetType, w.targetID)
		}
		if actorID != admin || actorEmail != "admin@audit.test" {
			t.Errorf("row %d actor = %q %q, want %q admin@audit.test", i, actorID, actorEmail, admin)
		}
		i++
	}
	if i != len(want) {
		t.Fatalf("audit rows = %d, want %d", i, len(want))
	}
}
