package resolvers

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yourorg/ztna/controller/internal/posture"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// ADR-030: approveDevice / unapproveDevice are audited, scoped to the
// caller's workspace, refuse a revoked device, and re-evaluate posture at once.
func TestApproveDevice(t *testing.T) {
	f := newCertHistoryFixture(t)
	store := posture.NewStore(f.pool)
	f.mr.PostureEvaluator = posture.NewEvaluator(store, f.mr.PolicyNotifier)

	dev := f.scalar(t, `INSERT INTO client_devices (user_id, workspace_id, name, os)
		VALUES ($1, $2, 'laptop-1', 'linux') RETURNING id::text`, f.user, f.ws)
	ws, devID := uuid.MustParse(f.ws), uuid.MustParse(dev)
	if err := store.InsertReport(f.ctx, ws, devID, posture.Report{
		ReportID:      uuid.NewString(),
		ClientVersion: "test",
		OSInfo:        json.RawMessage(`{"name":"linux"}`),
		ReportedAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("insert report: %v", err)
	}
	profile, err := store.CreateProfile(f.ctx, ws, "Company devices", true)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if err := store.AddRequirement(f.ctx, ws, profile.ID, posture.Requirement{CheckID: posture.CheckDeviceApproved}); err != nil {
		t.Fatalf("add requirement: %v", err)
	}
	satisfied := func(t *testing.T) bool {
		t.Helper()
		ev, err := store.LatestEvaluation(f.ctx, ws, devID, profile.ID)
		if err != nil {
			t.Fatalf("latest evaluation: %v", err)
		}
		return ev.Satisfied
	}
	audits := func(t *testing.T, action string) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(f.ctx,
			`SELECT count(*) FROM audit_logs WHERE action = $1 AND target_id = $2 AND tenant_id = $3`,
			action, dev, f.ws).Scan(&n); err != nil {
			t.Fatalf("count audits: %v", err)
		}
		return n
	}

	got, err := f.mr.ApproveDevice(f.ctx, dev)
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if got.ApprovedAt == nil || got.ApprovedByEmail == nil || *got.ApprovedByEmail != "owner@certhist.test" {
		t.Fatalf("approved device = %+v", got)
	}
	if !satisfied(t) {
		t.Fatal("approval must re-evaluate the device at once")
	}
	if _, err := f.mr.ApproveDevice(f.ctx, dev); err != nil {
		t.Fatalf("approve again: %v", err)
	}
	if n := audits(t, "device.approve"); n != 1 {
		t.Fatalf("device.approve audits = %d, want 1 (repeat is a no-op)", n)
	}

	got, err = f.mr.UnapproveDevice(f.ctx, dev)
	if err != nil {
		t.Fatalf("unapprove: %v", err)
	}
	if got.ApprovedAt != nil || got.ApprovedByEmail != nil {
		t.Fatalf("unapproved device = %+v", got)
	}
	if satisfied(t) {
		t.Fatal("removing approval must re-evaluate the device at once")
	}
	if n := audits(t, "device.unapprove"); n != 1 {
		t.Fatalf("device.unapprove audits = %d, want 1", n)
	}

	// Another workspace's admin can't approve this device.
	other := f.scalar(t, `INSERT INTO workspaces (slug, name, status, trust_domain)
		VALUES ('other', 'other', 'active', 'other.test') RETURNING id::text`)
	otherCtx := tenant.Set(f.ctx, tenant.TenantContext{TenantID: other, UserID: uuid.NewString(), Role: "admin"})
	if _, err := f.mr.ApproveDevice(otherCtx, dev); err == nil || !strings.Contains(err.Error(), "device not found") {
		t.Fatalf("cross-workspace approve error = %v, want device not found", err)
	}

	f.exec(t, `UPDATE client_devices SET revoked_at = NOW() WHERE id = $1`, dev)
	if _, err := f.mr.ApproveDevice(f.ctx, dev); err == nil || !strings.Contains(err.Error(), "device is revoked") {
		t.Fatalf("approve revoked error = %v, want device is revoked", err)
	}
	if n := audits(t, "device.approve"); n != 1 {
		t.Fatalf("refused approve must not audit, device.approve audits = %d", n)
	}
	if _, err := f.mr.ApproveDevice(f.ctx, "not-a-uuid"); err == nil {
		t.Fatal("approve with a malformed id must fail")
	}
}
