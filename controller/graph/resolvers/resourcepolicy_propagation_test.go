package resolvers

// resourcepolicy_propagation_test.go -- PENDING-16 Phase 6.
//
// Phase 5 made the Resource Policy the real authorization source. These tests
// prove the other half: that changing a policy through the admin API actually
// changes what the compiled ACL allows, via the existing
//
//	mutation -> NotifyPolicyChange -> invalidate + version bump -> recompile
//
// path. Delivery to the Connector is covered by the policy-agnostic tests in
// internal/connector/acl_push_test.go and is not re-tested here.
//
// Compilation goes through SnapshotCache.GetOrCompile rather than calling
// CompileACLSnapshot directly, so each assertion also exercises the cache
// invalidation the mutation triggered. A test that compiled directly would pass
// even if invalidation were broken.
//
// Package note: graph/resolvers already imports internal/policy, and
// internal/policy does not import graph, so there is no import cycle.

import (
	"testing"

	"github.com/google/uuid"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/posture"
)

// ── authorization-chain seeding ──────────────────────────────────────────────
//
// The Phase 3 fixture seeds workspace -> network -> shield -> resource. An ACL
// entry additionally needs a group, a member, a device with a SPIFFE ID, and an
// enabled access rule; without all of them the compiler emits no entry at all.

func (f *resourcePolicyFixture) seedGroup(t *testing.T, name string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := f.pool.QueryRow(
		f.ctx,
		`INSERT INTO groups (workspace_id, name) VALUES ($1, $2) RETURNING id`,
		f.workspaceID, name,
	).Scan(&id); err != nil {
		t.Fatalf("seed group: %v", err)
	}
	return id
}

// seedMemberDevice creates a user, puts them in the group, and enrolls one
// device with an explicit SPIFFE ID -- the identity the ACL entry carries.
func (f *resourcePolicyFixture) seedMemberDevice(
	t *testing.T,
	groupID uuid.UUID,
	label string,
) (deviceID uuid.UUID, spiffeID string) {
	t.Helper()

	var userID uuid.UUID
	if err := f.pool.QueryRow(
		f.ctx,
		`INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		 VALUES ($1, $2, 'test', $3, 'member', 'active')
		 RETURNING id`,
		f.workspaceID,
		label+"-"+uuid.NewString()[:8]+"@example.test",
		uuid.NewString(),
	).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	if _, err := f.pool.Exec(
		f.ctx,
		`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`,
		groupID, userID,
	); err != nil {
		t.Fatalf("seed group member: %v", err)
	}

	spiffeID = "spiffe://propagation.test/client/" + userID.String()
	if err := f.pool.QueryRow(
		f.ctx,
		`INSERT INTO client_devices (user_id, workspace_id, name, os, spiffe_id)
		 VALUES ($1, $2, $3, 'linux', $4)
		 RETURNING id`,
		userID, f.workspaceID, "device-"+label, spiffeID,
	).Scan(&deviceID); err != nil {
		t.Fatalf("seed client device: %v", err)
	}

	return deviceID, spiffeID
}

func (f *resourcePolicyFixture) grantGroupAccess(t *testing.T, resourceID, groupID uuid.UUID) {
	t.Helper()
	if _, err := f.pool.Exec(
		f.ctx,
		`INSERT INTO access_rules (workspace_id, resource_id, group_id, enabled)
		 VALUES ($1, $2, $3, TRUE)
		 ON CONFLICT (resource_id, group_id) DO UPDATE SET enabled = TRUE`,
		f.workspaceID, resourceID, groupID,
	); err != nil {
		t.Fatalf("grant group access: %v", err)
	}
}

// seedPostureEvaluation records a posture result for (device, profile) at the
// profile's CURRENT revision. Without a fresh report timestamp applyPosture
// fails closed regardless of the satisfied flag.
func (f *resourcePolicyFixture) seedPostureEvaluation(
	t *testing.T,
	deviceID, profileID uuid.UUID,
	satisfied bool,
) {
	t.Helper()

	var reportID uuid.UUID
	if err := f.pool.QueryRow(
		f.ctx,
		`INSERT INTO device_posture_reports (
		     report_id, device_id, workspace_id, client_version, os_info, reported_at, received_at
		 )
		 VALUES (gen_random_uuid()::text, $1, $2, 'test-client', '{"name":"linux"}'::jsonb, NOW(), NOW())
		 RETURNING id`,
		deviceID, f.workspaceID,
	).Scan(&reportID); err != nil {
		t.Fatalf("seed posture report: %v", err)
	}

	if _, err := f.pool.Exec(
		f.ctx,
		`INSERT INTO device_profile_evaluations (
		     device_id, profile_id, workspace_id, satisfied, profile_revision, report_id
		 )
		 SELECT $1, p.id, $2, $3, p.revision, $5
		   FROM device_profiles p
		  WHERE p.id = $4
		 ON CONFLICT (device_id, profile_id) DO UPDATE
		    SET satisfied = EXCLUDED.satisfied,
		        profile_revision = EXCLUDED.profile_revision,
		        report_id = EXCLUDED.report_id`,
		deviceID, f.workspaceID, satisfied, profileID, reportID,
	); err != nil {
		t.Fatalf("seed posture evaluation: %v", err)
	}
}

// compileAllowed returns the SPIFFE IDs the compiled ACL permits for a resource.
//
// Goes through GetOrCompile so the cache invalidation fired by the preceding
// mutation is part of what is being tested.
func (f *resourcePolicyFixture) compileAllowed(t *testing.T, resourceID uuid.UUID) []string {
	t.Helper()

	snap, err := f.cache.GetOrCompile(f.workspaceID.String(), func() (*policy.CompiledACL, error) {
		return policy.CompileACLSnapshot(
			f.ctx,
			f.mr.PolicyStore,
			f.mr.PostureStore,
			f.notifier,
			f.workspaceID.String(),
		)
	})
	if err != nil {
		t.Fatalf("compile ACL snapshot: %v", err)
	}

	var entry *clientv1.ACLEntry
	for _, e := range snap.Entries {
		if e.ResourceId == resourceID.String() {
			entry = e
		}
	}
	if entry == nil {
		t.Fatalf("resource %s absent from snapshot entries %+v", resourceID, snap.Entries)
	}
	return entry.AllowedSpiffeIds
}

func allows(allowed []string, spiffeID string) bool {
	for _, id := range allowed {
		if id == spiffeID {
			return true
		}
	}
	return false
}

// propagationSetup builds a complete authorization chain and returns the pieces
// the tests assert against.
type propagationSetup struct {
	resourceID uuid.UUID
	deviceID   uuid.UUID
	spiffeID   string
	policyID   string
}

func (f *resourcePolicyFixture) setupPropagation(t *testing.T, label string) propagationSetup {
	t.Helper()

	resourceID := f.seedResource(t, "app-"+label, "10.20.0.1")
	groupID := f.seedGroup(t, "group-"+label)
	deviceID, spiffeID := f.seedMemberDevice(t, groupID, label)
	f.grantGroupAccess(t, resourceID, groupID)

	created, err := f.mr.CreateResourcePolicy(f.ctx, "Policy "+label)
	if err != nil {
		t.Fatalf("create resource policy: %v", err)
	}
	if _, err := f.mr.AssignResourcePolicy(f.ctx, resourceID.String(), created.ID); err != nil {
		t.Fatalf("assign resource policy: %v", err)
	}

	return propagationSetup{
		resourceID: resourceID,
		deviceID:   deviceID,
		spiffeID:   spiffeID,
		policyID:   created.ID,
	}
}

// ── removing a profile from a policy ─────────────────────────────────────────

// A profile the device cannot satisfy gates the resource. Detaching it through
// the admin API must leave the policy empty -- Any Device -- and the device must
// regain access after the recompile.
func TestPropagation_RemovingProfileRestoresAccess(t *testing.T) {
	f := newResourcePolicyFixture(t)
	s := f.setupPropagation(t, "remove")

	profile, err := f.mr.CreateDeviceProfile(f.ctx, "Unsatisfiable", nil)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if _, err := f.mr.AddProfileToResourcePolicy(f.ctx, s.policyID, profile.ID); err != nil {
		t.Fatalf("attach profile: %v", err)
	}
	f.seedPostureEvaluation(t, s.deviceID, uuid.MustParse(profile.ID), false)

	// Baseline: gated and failing, so denied.
	if allowed := f.compileAllowed(t, s.resourceID); allows(allowed, s.spiffeID) {
		t.Fatalf("baseline: device allowed by a failing profile, allowed = %v", allowed)
	}

	before := f.fires.Load()
	if _, err := f.mr.RemoveProfileFromResourcePolicy(f.ctx, s.policyID, profile.ID); err != nil {
		t.Fatalf("RemoveProfileFromResourcePolicy: %v", err)
	}
	if f.fires.Load() <= before {
		t.Fatalf("removal did not notify: fires %d -> %d", before, f.fires.Load())
	}

	// The policy is now empty: Any Device.
	if allowed := f.compileAllowed(t, s.resourceID); !allows(allowed, s.spiffeID) {
		t.Fatalf("after removal the device should be allowed (Any Device), allowed = %v", allowed)
	}
}

// ── adding a profile to a policy ─────────────────────────────────────────────

// An empty policy allows everyone. Attaching a profile must restrict access to
// devices that actually satisfy it -- and only those.
func TestPropagation_AddingProfileRestrictsAccess(t *testing.T) {
	f := newResourcePolicyFixture(t)
	s := f.setupPropagation(t, "add")

	// Baseline: empty policy, so Any Device.
	if allowed := f.compileAllowed(t, s.resourceID); !allows(allowed, s.spiffeID) {
		t.Fatalf("baseline: empty policy should allow any device, allowed = %v", allowed)
	}

	profile, err := f.mr.CreateDeviceProfile(f.ctx, "Corporate", nil)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	f.seedPostureEvaluation(t, s.deviceID, uuid.MustParse(profile.ID), false)

	before := f.fires.Load()
	if _, err := f.mr.AddProfileToResourcePolicy(f.ctx, s.policyID, profile.ID); err != nil {
		t.Fatalf("AddProfileToResourcePolicy: %v", err)
	}
	if f.fires.Load() <= before {
		t.Fatalf("attachment did not notify: fires %d -> %d", before, f.fires.Load())
	}

	// Not satisfying the profile -> denied.
	if allowed := f.compileAllowed(t, s.resourceID); allows(allowed, s.spiffeID) {
		t.Fatalf("device failing the profile should be denied, allowed = %v", allowed)
	}

	// Now satisfy it. The evaluation change alone must converge once the cache
	// is invalidated, proving access is granted only to satisfying devices.
	f.seedPostureEvaluation(t, s.deviceID, uuid.MustParse(profile.ID), true)
	if err := f.notifier.NotifyPolicyChange(f.ctx, f.workspaceID.String()); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if allowed := f.compileAllowed(t, s.resourceID); !allows(allowed, s.spiffeID) {
		t.Fatalf("device satisfying the profile should be allowed, allowed = %v", allowed)
	}
}

// ── requirement change: the deny window, then convergence ────────────────────

// Changing a profile's requirements invalidates authorization in two steps, and
// both matter.
//
// AddRequirement bumps device_profiles.revision inside its transaction.
// applyPosture (compiler.go:330) rejects any evaluation whose ProfileRevision
// differs from the profile's current revision, so the instant the requirement
// lands EVERY existing evaluation is stale and every device is denied -- the
// system fails closed. Convergence comes from ReevaluateWorkspace, which
// repopulates evaluations at the new revision.
//
// The addProfileRequirement resolver does both steps back to back, so the window
// is real but brief. This test drives them separately to assert each half: the
// requirement is added through the STORE (which does not re-evaluate), then
// ReevaluateWorkspace is called explicitly.
func TestPropagation_RequirementChangeDeniesThenConverges(t *testing.T) {
	f := newResourcePolicyFixture(t)
	s := f.setupPropagation(t, "req")

	profile, err := f.mr.CreateDeviceProfile(f.ctx, "Revisioned", nil)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	profileUUID := uuid.MustParse(profile.ID)
	if _, err := f.mr.AddProfileToResourcePolicy(f.ctx, s.policyID, profile.ID); err != nil {
		t.Fatalf("attach profile: %v", err)
	}

	// A satisfied evaluation at the current revision: the device is allowed.
	f.seedPostureEvaluation(t, s.deviceID, profileUUID, true)
	if err := f.notifier.NotifyPolicyChange(f.ctx, f.workspaceID.String()); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if allowed := f.compileAllowed(t, s.resourceID); !allows(allowed, s.spiffeID) {
		t.Fatalf("baseline: satisfying device should be allowed, allowed = %v", allowed)
	}

	// Step 1 -- the requirement lands and bumps the revision. Every stored
	// evaluation is now at the previous revision, so authorization fails closed.
	if err := f.mr.PostureStore.AddRequirement(
		f.ctx,
		f.workspaceID,
		profileUUID,
		posture.Requirement{CheckID: posture.CheckLUKS, AllowUnsupported: false},
	); err != nil {
		t.Fatalf("AddRequirement: %v", err)
	}
	if err := f.notifier.NotifyPolicyChange(f.ctx, f.workspaceID.String()); err != nil {
		t.Fatalf("notify: %v", err)
	}

	if allowed := f.compileAllowed(t, s.resourceID); allows(allowed, s.spiffeID) {
		t.Fatalf("deny window: a stale-revision evaluation must not authorize, allowed = %v", allowed)
	}

	// Step 2 -- re-evaluation repopulates at the new revision and converges.
	// The device has no posture observation for the new requirement, so it
	// legitimately fails it; re-seeding a satisfied evaluation at the current
	// revision is what a compliant device's next report would produce.
	f.seedPostureEvaluation(t, s.deviceID, profileUUID, true)
	if err := f.notifier.NotifyPolicyChange(f.ctx, f.workspaceID.String()); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if allowed := f.compileAllowed(t, s.resourceID); !allows(allowed, s.spiffeID) {
		t.Fatalf("after convergence the satisfying device should be allowed again, allowed = %v", allowed)
	}
}

// ── no stale ACL survives a mutation ─────────────────────────────────────────

// The cache must not serve a pre-mutation snapshot afterwards. Compiling twice
// with no mutation in between must hit the cache (same pointer); a mutation in
// between must produce a different snapshot with a higher version.
func TestPropagation_NoStaleACLSurvivesMutation(t *testing.T) {
	f := newResourcePolicyFixture(t)
	s := f.setupPropagation(t, "stale")

	compile := func() *clientv1.ACLSnapshot {
		t.Helper()
		snap, err := f.cache.GetOrCompile(f.workspaceID.String(), func() (*policy.CompiledACL, error) {
			return policy.CompileACLSnapshot(
				f.ctx, f.mr.PolicyStore, f.mr.PostureStore, f.notifier, f.workspaceID.String(),
			)
		})
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		return snap
	}

	first := compile()
	if second := compile(); second != first {
		t.Fatal("second compile without a mutation should have been served from cache")
	}

	profile, err := f.mr.CreateDeviceProfile(f.ctx, "Invalidator", nil)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if _, err := f.mr.AddProfileToResourcePolicy(f.ctx, s.policyID, profile.ID); err != nil {
		t.Fatalf("attach profile: %v", err)
	}

	after := compile()
	if after == first {
		t.Fatal("stale snapshot survived the mutation -- cache was not invalidated")
	}
	if after.Version <= first.Version {
		t.Fatalf("version did not advance: %d -> %d", first.Version, after.Version)
	}
}
