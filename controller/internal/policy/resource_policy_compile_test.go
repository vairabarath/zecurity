package policy

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yourorg/ztna/controller/internal/posture"
)

// PENDING-16: the ACL compiler and the preview read Resource → Resource
// Policy → Device Profiles. A device in the resource's group with a passing
// firewall report, not approved.
func TestResourcePolicy_CompileAndPreview(t *testing.T) {
	pool, ctx := lifecycleTestPool(t)
	f := newLifecycleFixture(t, pool, ctx, "respolicy")
	rnID, _ := mustInsertRNWithConnector(t, ctx, pool, f.wsID, "rn-pol", "td-pol", "10.42.0.1")
	rID := mustInsertResource(t, ctx, pool, f.wsID, rnID, "res-pol", "10.42.0.10", 443)
	mustAssignResourceToGroup(t, ctx, pool, f.wsID, rID, f.grpID)
	userID := mustInsertUser(t, ctx, pool, f.wsID)
	mustAddGroupMember(t, ctx, pool, f.grpID, userID)
	spiffe := "spiffe://td/client/" + userID
	deviceID := mustInsertClientDevice(t, ctx, pool, f.wsID, userID, spiffe)

	ws, dev, res := uuid.MustParse(f.wsID), uuid.MustParse(deviceID), uuid.MustParse(rID)
	pstore := posture.NewStore(pool)
	profile := func(name, check string) uuid.UUID {
		p, err := pstore.CreateProfile(ctx, ws, name, false)
		if err != nil {
			t.Fatalf("create profile %s: %v", name, err)
		}
		// ADR-031: profiles start with no platform; these devices are Linux.
		if _, err := pstore.SetAllowedPlatforms(ctx, ws, p.ID, []string{"linux"}); err != nil {
			t.Fatalf("allow linux: %v", err)
		}
		if err := pstore.AddRequirement(ctx, ws, p.ID, posture.Requirement{CheckID: check}); err != nil {
			t.Fatalf("add requirement: %v", err)
		}
		return p.ID
	}
	approved := profile("Approved devices", posture.CheckDeviceApproved) // the device fails it
	firewall := profile("Firewall on", posture.CheckFirewall)            // the device passes it

	if err := pstore.InsertReport(ctx, ws, dev, posture.Report{
		ReportID:      uuid.NewString(),
		ClientVersion: "test-client",
		OSInfo:        json.RawMessage(`{"name":"linux","version":"test"}`),
		ReportedAt:    time.Now().UTC(),
		Observations:  []posture.Observation{{CheckID: posture.CheckFirewall, Status: posture.ObservationStatusPass}},
	}); err != nil {
		t.Fatalf("insert report: %v", err)
	}
	notifier := NewNotifier(NewSnapshotCache())
	if _, err := posture.NewEvaluator(pstore, notifier).EvaluateDevice(ctx, ws, dev); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	store := NewStore(pool)
	allowed := func() bool {
		t.Helper()
		compiled, err := CompileACLSnapshot(ctx, store, pstore, notifier, f.wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		for _, e := range compiled.Snapshot.Entries {
			if e.ResourceId == rID {
				return contains(e.AllowedSpiffeIds, spiffe)
			}
		}
		t.Fatal("resource missing from the snapshot")
		return false
	}
	preview := func(profiles ...uuid.UUID) (int, int) {
		t.Helper()
		pv, err := PreviewResourcePolicy(ctx, store, pstore, f.wsID, []uuid.UUID{res}, profiles)
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		return pv.Devices, pv.Passing
	}

	if !allowed() {
		t.Fatal("the Default policy (any device) denied the device")
	}

	pol, err := pstore.CreateResourcePolicy(ctx, ws, "Approved only", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pstore.SetResourcePolicyProfiles(ctx, ws, pol.ID, []uuid.UUID{approved}); err != nil {
		t.Fatal(err)
	}
	if _, err := pstore.SetResourcePolicy(ctx, ws, res, pol.ID); err != nil {
		t.Fatal(err)
	}
	if allowed() {
		t.Fatal("a policy requiring an approved device allowed an unapproved one")
	}

	if _, err := pstore.SetResourcePolicyProfiles(ctx, ws, pol.ID, []uuid.UUID{approved, firewall}); err != nil {
		t.Fatal(err)
	}
	if !allowed() {
		t.Fatal("profiles are OR'd: passing the firewall profile must allow the device")
	}

	if _, err := pstore.SetResourcePolicyProfiles(ctx, ws, pol.ID, []uuid.UUID{approved}); err != nil {
		t.Fatal(err)
	}
	if allowed() {
		t.Fatal("back to approval only: the device must be denied")
	}
	if _, err := pstore.DeleteResourcePolicy(ctx, ws, pol.ID); err != nil {
		t.Fatal(err)
	}
	if !allowed() {
		t.Fatal("after the policy is deleted the resource is in the Default: any device")
	}

	// The preview uses the same decision.
	for _, tc := range []struct {
		name     string
		profiles []uuid.UUID
		passing  int
	}{
		{"approval only", []uuid.UUID{approved}, 0},
		{"firewall only", []uuid.UUID{firewall}, 1},
		{"either", []uuid.UUID{approved, firewall}, 1},
		{"any device", nil, 1},
	} {
		devices, passing := preview(tc.profiles...)
		if devices != 1 || passing != tc.passing {
			t.Errorf("preview %s = %d of %d, want %d of 1", tc.name, passing, devices, tc.passing)
		}
	}
	if _, err := PreviewResourcePolicy(ctx, store, pstore, f.wsID, []uuid.UUID{res}, []uuid.UUID{uuid.New()}); err != posture.ErrPolicyProfileMissing {
		t.Fatalf("preview with an unknown profile err = %v", err)
	}
}
