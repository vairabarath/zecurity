package posture

// resource_policy_store_integration_test.go -- PENDING-16 Phase 2 verification.
//
// Runs against a real PostgreSQL instance, like the other posture integration
// tests: set PKI_TEST_DATABASE_URL to an admin DSN and the test creates and
// drops its own database. Reuses the harness helpers in
// store_integration_test.go (same package).

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestResourcePolicyStoreIntegration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	dbName := postureTestDBName(t)
	adminPool := postureTestPool(t, ctx, adminDSN)
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

	testDSN, err := postureTestDSN(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test database DSN: %v", err)
	}
	pool := postureTestPool(t, ctx, testDSN)
	defer pool.Close()

	// Phase 1 verification: the full migration chain applies to a fresh database.
	if err := applyPostureTestMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	workspaceA := insertPostureTestWorkspace(t, ctx, pool, "rp-a")
	workspaceB := insertPostureTestWorkspace(t, ctx, pool, "rp-b")
	store := NewStore(pool)

	// ---------------------------------------------------------------- CRUD --

	t.Run("CRUD", func(t *testing.T) {
		if _, err := store.CreateResourcePolicy(ctx, workspaceA, "   "); !errors.Is(err, ErrInvalidPolicyName) {
			t.Fatalf("blank name error = %v, want %v", err, ErrInvalidPolicyName)
		}

		policy, err := store.CreateResourcePolicy(ctx, workspaceA, "  Engineering  ")
		if err != nil {
			t.Fatalf("create resource policy: %v", err)
		}
		if policy.Name != "Engineering" {
			t.Fatalf("policy.Name = %q, want %q (trimmed)", policy.Name, "Engineering")
		}
		if policy.WorkspaceID != workspaceA {
			t.Fatalf("policy.WorkspaceID = %v, want %v", policy.WorkspaceID, workspaceA)
		}
		if policy.CreatedAt.IsZero() || policy.UpdatedAt.IsZero() {
			t.Fatalf("policy timestamps not populated: %+v", policy)
		}

		// UNIQUE (workspace_id, name) surfaces as a domain error.
		if _, err := store.CreateResourcePolicy(ctx, workspaceA, "Engineering"); !errors.Is(err, ErrDuplicatePolicyName) {
			t.Fatalf("duplicate name error = %v, want %v", err, ErrDuplicatePolicyName)
		}
		// The same name in another workspace is fine — uniqueness is per tenant.
		if _, err := store.CreateResourcePolicy(ctx, workspaceB, "Engineering"); err != nil {
			t.Fatalf("same name in other workspace: %v", err)
		}

		got, err := store.GetResourcePolicy(ctx, workspaceA, policy.ID)
		if err != nil {
			t.Fatalf("get resource policy: %v", err)
		}
		if got.ID != policy.ID || got.Name != "Engineering" {
			t.Fatalf("got = %+v, want id %v name Engineering", got, policy.ID)
		}

		// Workspace isolation on read.
		if _, err := store.GetResourcePolicy(ctx, workspaceB, policy.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-workspace get error = %v, want %v", err, ErrNotFound)
		}
		if _, err := store.GetResourcePolicy(ctx, workspaceA, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing get error = %v, want %v", err, ErrNotFound)
		}

		renamed, err := store.UpdateResourcePolicy(ctx, workspaceA, policy.ID, "Engineering Prod")
		if err != nil {
			t.Fatalf("update resource policy: %v", err)
		}
		if renamed.Name != "Engineering Prod" {
			t.Fatalf("renamed.Name = %q", renamed.Name)
		}
		if !renamed.UpdatedAt.After(policy.UpdatedAt) && !renamed.UpdatedAt.Equal(policy.UpdatedAt) {
			t.Fatalf("updated_at moved backwards: %v -> %v", policy.UpdatedAt, renamed.UpdatedAt)
		}
		if _, err := store.UpdateResourcePolicy(ctx, workspaceA, policy.ID, ""); !errors.Is(err, ErrInvalidPolicyName) {
			t.Fatalf("update blank name error = %v, want %v", err, ErrInvalidPolicyName)
		}
		if _, err := store.UpdateResourcePolicy(ctx, workspaceB, policy.ID, "Hijack"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-workspace update error = %v, want %v", err, ErrNotFound)
		}

		policies, err := store.ListResourcePolicies(ctx, workspaceA)
		if err != nil {
			t.Fatalf("list resource policies: %v", err)
		}
		if len(policies) != 1 || policies[0].ID != policy.ID {
			t.Fatalf("workspaceA policies = %+v, want just %v", policies, policy.ID)
		}

		if err := store.DeleteResourcePolicy(ctx, workspaceA, policy.ID); err != nil {
			t.Fatalf("delete resource policy: %v", err)
		}
		if err := store.DeleteResourcePolicy(ctx, workspaceA, policy.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("re-delete error = %v, want %v", err, ErrNotFound)
		}
	})

	// ------------------------------------------------- profile attachments --

	t.Run("ProfileAttachments", func(t *testing.T) {
		policy, err := store.CreateResourcePolicy(ctx, workspaceA, "Profiles")
		if err != nil {
			t.Fatalf("create policy: %v", err)
		}

		// Zero profiles is valid and means Any Device.
		profiles, err := store.ListProfilesForPolicy(ctx, workspaceA, policy.ID)
		if err != nil {
			t.Fatalf("list profiles for empty policy: %v", err)
		}
		if len(profiles) != 0 {
			t.Fatalf("empty policy profiles = %+v, want none", profiles)
		}

		linux, err := store.CreateProfile(ctx, workspaceA, "Corporate Linux", true)
		if err != nil {
			t.Fatalf("create linux profile: %v", err)
		}
		windows, err := store.CreateProfile(ctx, workspaceA, "Corporate Windows", true)
		if err != nil {
			t.Fatalf("create windows profile: %v", err)
		}
		foreign, err := store.CreateProfile(ctx, workspaceB, "Foreign Profile", true)
		if err != nil {
			t.Fatalf("create foreign profile: %v", err)
		}

		// One profile, then a second — multiple attachments are allowed and are
		// what the authorization path will OR together in a later phase.
		if err := store.AddProfileToPolicy(ctx, workspaceA, policy.ID, linux.ID); err != nil {
			t.Fatalf("add linux profile: %v", err)
		}
		if err := store.AddProfileToPolicy(ctx, workspaceA, policy.ID, windows.ID); err != nil {
			t.Fatalf("add windows profile: %v", err)
		}

		profiles, err = store.ListProfilesForPolicy(ctx, workspaceA, policy.ID)
		if err != nil {
			t.Fatalf("list profiles: %v", err)
		}
		if len(profiles) != 2 {
			t.Fatalf("profiles = %+v, want 2", profiles)
		}
		// ORDER BY name — "Corporate Linux" before "Corporate Windows".
		if profiles[0].ID != linux.ID || profiles[1].ID != windows.ID {
			t.Fatalf("profiles not name-ordered: %v, %v", profiles[0].Name, profiles[1].Name)
		}

		// Duplicate attachment rejected.
		if err := store.AddProfileToPolicy(ctx, workspaceA, policy.ID, linux.ID); !errors.Is(err, ErrDuplicateProfileBinding) {
			t.Fatalf("duplicate binding error = %v, want %v", err, ErrDuplicateProfileBinding)
		}

		// Cross-workspace policy/profile rejected in both directions.
		if err := store.AddProfileToPolicy(ctx, workspaceA, policy.ID, foreign.ID); !errors.Is(err, ErrWorkspaceMismatch) {
			t.Fatalf("cross-workspace profile error = %v, want %v", err, ErrWorkspaceMismatch)
		}
		if err := store.AddProfileToPolicy(ctx, workspaceB, policy.ID, foreign.ID); !errors.Is(err, ErrWorkspaceMismatch) {
			t.Fatalf("cross-workspace policy error = %v, want %v", err, ErrWorkspaceMismatch)
		}
		if err := store.AddProfileToPolicy(ctx, workspaceA, uuid.New(), linux.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing policy error = %v, want %v", err, ErrNotFound)
		}
		if err := store.AddProfileToPolicy(ctx, workspaceA, policy.ID, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing profile error = %v, want %v", err, ErrNotFound)
		}

		// Removal, down to zero profiles again.
		if err := store.RemoveProfileFromPolicy(ctx, workspaceA, policy.ID, windows.ID); err != nil {
			t.Fatalf("remove windows profile: %v", err)
		}
		if err := store.RemoveProfileFromPolicy(ctx, workspaceA, policy.ID, windows.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("re-remove error = %v, want %v", err, ErrNotFound)
		}
		if err := store.RemoveProfileFromPolicy(ctx, workspaceB, policy.ID, linux.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-workspace remove error = %v, want %v", err, ErrNotFound)
		}
		if err := store.RemoveProfileFromPolicy(ctx, workspaceA, policy.ID, linux.ID); err != nil {
			t.Fatalf("remove linux profile: %v", err)
		}

		profiles, err = store.ListProfilesForPolicy(ctx, workspaceA, policy.ID)
		if err != nil {
			t.Fatalf("list profiles after removal: %v", err)
		}
		if len(profiles) != 0 {
			t.Fatalf("profiles after removal = %+v, want none", profiles)
		}
	})

	// ---------------------------------------------------------- assignment --

	t.Run("ResourceAssignment", func(t *testing.T) {
		policy, err := store.CreateResourcePolicy(ctx, workspaceA, "Assignment")
		if err != nil {
			t.Fatalf("create policy: %v", err)
		}
		other, err := store.CreateResourcePolicy(ctx, workspaceA, "Assignment Other")
		if err != nil {
			t.Fatalf("create other policy: %v", err)
		}
		foreignPolicy, err := store.CreateResourcePolicy(ctx, workspaceB, "Assignment Foreign")
		if err != nil {
			t.Fatalf("create foreign policy: %v", err)
		}

		resourceID := insertPostureTestResource(t, ctx, pool, workspaceA, "assign")

		// Unassigned resource: policy lookup is (nil, nil), not an error.
		got, err := store.GetResourcePolicyForResource(ctx, workspaceA, resourceID)
		if err != nil {
			t.Fatalf("lookup unassigned resource: %v", err)
		}
		if got != nil {
			t.Fatalf("unassigned resource policy = %+v, want nil", got)
		}
		// A resource that does not exist is distinguishable from "no policy".
		if _, err := store.GetResourcePolicyForResource(ctx, workspaceA, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing resource lookup error = %v, want %v", err, ErrNotFound)
		}
		if _, err := store.GetResourcePolicyForResource(ctx, workspaceB, resourceID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-workspace lookup error = %v, want %v", err, ErrNotFound)
		}

		if err := store.AssignResourcePolicy(ctx, workspaceA, resourceID, policy.ID); err != nil {
			t.Fatalf("assign resource policy: %v", err)
		}

		got, err = store.GetResourcePolicyForResource(ctx, workspaceA, resourceID)
		if err != nil {
			t.Fatalf("lookup assigned resource: %v", err)
		}
		if got == nil || got.ID != policy.ID {
			t.Fatalf("assigned policy = %+v, want %v", got, policy.ID)
		}

		resourceIDs, err := store.ListResourceIDsForPolicy(ctx, workspaceA, policy.ID)
		if err != nil {
			t.Fatalf("list resources for policy: %v", err)
		}
		if len(resourceIDs) != 1 || resourceIDs[0] != resourceID {
			t.Fatalf("policy resources = %v, want [%v]", resourceIDs, resourceID)
		}
		// Reverse lookup is workspace-scoped too.
		foreignIDs, err := store.ListResourceIDsForPolicy(ctx, workspaceB, policy.ID)
		if err != nil {
			t.Fatalf("cross-workspace reverse lookup: %v", err)
		}
		if len(foreignIDs) != 0 {
			t.Fatalf("cross-workspace policy resources = %v, want none", foreignIDs)
		}

		// Re-assigning the same policy is a no-op success.
		if err := store.AssignResourcePolicy(ctx, workspaceA, resourceID, policy.ID); err != nil {
			t.Fatalf("idempotent re-assign: %v", err)
		}

		// A second, different policy is rejected — never silently replaced.
		if err := store.AssignResourcePolicy(ctx, workspaceA, resourceID, other.ID); !errors.Is(err, ErrResourceAlreadyAssigned) {
			t.Fatalf("second policy error = %v, want %v", err, ErrResourceAlreadyAssigned)
		}
		still, err := store.GetResourcePolicyForResource(ctx, workspaceA, resourceID)
		if err != nil {
			t.Fatalf("lookup after rejected assign: %v", err)
		}
		if still == nil || still.ID != policy.ID {
			t.Fatalf("policy changed despite rejection: %+v", still)
		}

		// Cross-workspace assignment rejected in both directions.
		if err := store.AssignResourcePolicy(ctx, workspaceA, resourceID, foreignPolicy.ID); !errors.Is(err, ErrWorkspaceMismatch) {
			t.Fatalf("cross-workspace policy assign error = %v, want %v", err, ErrWorkspaceMismatch)
		}
		if err := store.AssignResourcePolicy(ctx, workspaceB, resourceID, foreignPolicy.ID); !errors.Is(err, ErrWorkspaceMismatch) {
			t.Fatalf("cross-workspace resource assign error = %v, want %v", err, ErrWorkspaceMismatch)
		}
		if err := store.AssignResourcePolicy(ctx, workspaceA, uuid.New(), policy.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing resource assign error = %v, want %v", err, ErrNotFound)
		}
		if err := store.AssignResourcePolicy(ctx, workspaceA, resourceID, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing policy assign error = %v, want %v", err, ErrNotFound)
		}

		// Deleting an assigned policy is refused, so a resource can never be
		// silently stripped of the policy its access depends on.
		if err := store.DeleteResourcePolicy(ctx, workspaceA, policy.ID); !errors.Is(err, ErrPolicyAssigned) {
			t.Fatalf("delete assigned policy error = %v, want %v", err, ErrPolicyAssigned)
		}

		if err := store.UnassignResourcePolicy(ctx, workspaceA, resourceID); err != nil {
			t.Fatalf("unassign resource policy: %v", err)
		}
		got, err = store.GetResourcePolicyForResource(ctx, workspaceA, resourceID)
		if err != nil {
			t.Fatalf("lookup after unassign: %v", err)
		}
		if got != nil {
			t.Fatalf("policy still assigned after unassign: %+v", got)
		}
		// Unassigning again is a no-op success; a missing resource still errors.
		if err := store.UnassignResourcePolicy(ctx, workspaceA, resourceID); err != nil {
			t.Fatalf("idempotent unassign: %v", err)
		}
		if err := store.UnassignResourcePolicy(ctx, workspaceA, uuid.New()); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing resource unassign error = %v, want %v", err, ErrNotFound)
		}
		if err := store.UnassignResourcePolicy(ctx, workspaceB, resourceID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("cross-workspace unassign error = %v, want %v", err, ErrNotFound)
		}

		// Now the policy is free to delete, and its profile bindings cascade.
		profile, err := store.CreateProfile(ctx, workspaceA, "Cascade Probe", true)
		if err != nil {
			t.Fatalf("create cascade profile: %v", err)
		}
		if err := store.AddProfileToPolicy(ctx, workspaceA, policy.ID, profile.ID); err != nil {
			t.Fatalf("attach cascade profile: %v", err)
		}
		if err := store.DeleteResourcePolicy(ctx, workspaceA, policy.ID); err != nil {
			t.Fatalf("delete unassigned policy: %v", err)
		}
		var bindingCount int
		if err := pool.QueryRow(
			ctx,
			`SELECT COUNT(*) FROM resource_policy_profile_bindings
			  WHERE device_resource_policy_id = $1`,
			policy.ID,
		).Scan(&bindingCount); err != nil {
			t.Fatalf("count cascaded bindings: %v", err)
		}
		if bindingCount != 0 {
			t.Fatalf("bindings after policy delete = %d, want 0", bindingCount)
		}
		// The profile itself survives — deleting a policy is not a profile delete.
		if _, err := store.GetProfile(ctx, workspaceA, profile.ID); err != nil {
			t.Fatalf("profile removed by policy delete: %v", err)
		}
	})

	// ------------------------------------------- concurrent assignment race --

	t.Run("ConcurrentAssignment", func(t *testing.T) {
		first, err := store.CreateResourcePolicy(ctx, workspaceA, "Race One")
		if err != nil {
			t.Fatalf("create first policy: %v", err)
		}
		second, err := store.CreateResourcePolicy(ctx, workspaceA, "Race Two")
		if err != nil {
			t.Fatalf("create second policy: %v", err)
		}
		resourceID := insertPostureTestResource(t, ctx, pool, workspaceA, "race")

		// Two different policies assigned at once: the FOR UPDATE row lock must
		// let exactly one win, so the resource never ends up with two policies.
		var (
			wg   sync.WaitGroup
			mu   sync.Mutex
			errs []error
		)
		for _, policyID := range []uuid.UUID{first.ID, second.ID} {
			wg.Add(1)
			go func(id uuid.UUID) {
				defer wg.Done()
				err := store.AssignResourcePolicy(ctx, workspaceA, resourceID, id)
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}(policyID)
		}
		wg.Wait()

		succeeded := 0
		for _, err := range errs {
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, ErrResourceAlreadyAssigned):
			default:
				t.Fatalf("unexpected concurrent assign error: %v", err)
			}
		}
		if succeeded != 1 {
			t.Fatalf("concurrent assigns succeeded = %d, want exactly 1 (errs=%v)", succeeded, errs)
		}

		assigned, err := store.GetResourcePolicyForResource(ctx, workspaceA, resourceID)
		if err != nil {
			t.Fatalf("lookup after race: %v", err)
		}
		if assigned == nil || (assigned.ID != first.ID && assigned.ID != second.ID) {
			t.Fatalf("post-race policy = %+v, want one of the two", assigned)
		}
	})

	// ------------------------------- database-level tenant safety (Phase 1) --

	t.Run("DatabaseRejectsCrossWorkspaceRows", func(t *testing.T) {
		policyA, err := store.CreateResourcePolicy(ctx, workspaceA, "DB Guard A")
		if err != nil {
			t.Fatalf("create policy: %v", err)
		}
		profileB, err := store.CreateProfile(ctx, workspaceB, "DB Guard Profile B", true)
		if err != nil {
			t.Fatalf("create profile: %v", err)
		}
		resourceB := insertPostureTestResource(t, ctx, pool, workspaceB, "dbguard")

		// Bypass the store entirely: the composite foreign keys must refuse a
		// cross-workspace assignment even when the application layer is not in
		// the way.
		if _, err := pool.Exec(
			ctx,
			`UPDATE resources SET device_resource_policy_id = $1 WHERE id = $2`,
			policyA.ID,
			resourceB,
		); err == nil {
			t.Fatal("database allowed a workspace-B resource to reference a workspace-A policy")
		}

		if _, err := pool.Exec(
			ctx,
			`INSERT INTO resource_policy_profile_bindings (
			     device_resource_policy_id, profile_id, workspace_id
			 ) VALUES ($1, $2, $3)`,
			policyA.ID,
			profileB.ID,
			workspaceA,
		); err == nil {
			t.Fatal("database allowed a workspace-A policy to bind a workspace-B profile")
		}
	})

	// ----------------------------------------- legacy model left untouched --

	t.Run("LegacyBindingsPreserved", func(t *testing.T) {
		// Phase 1-3 must not disturb resource_profile_bindings. Prove the legacy
		// path still works end to end alongside the new model.
		resourceID := insertPostureTestResource(t, ctx, pool, workspaceA, "legacy")
		profile, err := store.CreateProfile(ctx, workspaceA, "Legacy Profile", true)
		if err != nil {
			t.Fatalf("create legacy profile: %v", err)
		}

		if _, err := store.CreateResourceBinding(ctx, workspaceA, resourceID, profile.ID); err != nil {
			t.Fatalf("legacy CreateResourceBinding: %v", err)
		}

		legacy, err := store.ListResourceBindingsForWorkspace(ctx, workspaceA)
		if err != nil {
			t.Fatalf("legacy ListResourceBindingsForWorkspace: %v", err)
		}
		found := false
		for _, binding := range legacy {
			if binding.ResourceID == resourceID && binding.ProfileID == profile.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("legacy binding missing from %+v", legacy)
		}

		// The new model and the legacy model are independent: attaching the same
		// profile to a policy does not touch the legacy row, and vice versa.
		policy, err := store.CreateResourcePolicy(ctx, workspaceA, "Coexistence")
		if err != nil {
			t.Fatalf("create coexistence policy: %v", err)
		}
		if err := store.AddProfileToPolicy(ctx, workspaceA, policy.ID, profile.ID); err != nil {
			t.Fatalf("attach profile to policy: %v", err)
		}
		if err := store.AssignResourcePolicy(ctx, workspaceA, resourceID, policy.ID); err != nil {
			t.Fatalf("assign policy to legacy-bound resource: %v", err)
		}

		legacyAfter, err := store.ListResourceBindingsForWorkspace(ctx, workspaceA)
		if err != nil {
			t.Fatalf("legacy list after new-model writes: %v", err)
		}
		if len(legacyAfter) != len(legacy) {
			t.Fatalf("legacy binding count changed: %d -> %d", len(legacy), len(legacyAfter))
		}

		if err := store.DeleteResourceBinding(ctx, workspaceA, resourceID, profile.ID); err != nil {
			t.Fatalf("legacy DeleteResourceBinding: %v", err)
		}
	})
}
