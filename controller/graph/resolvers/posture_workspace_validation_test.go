package resolvers

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/yourorg/ztna/controller/internal/posture"
)

// Cross-workspace profile delete must fail without notifying policy.
func TestDeleteDeviceProfileRejectsCrossWorkspaceProfile(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)

	var otherWorkspaceID uuid.UUID
	slug := "posture-delete-cross-ws-" + uuid.NewString()
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $1, 'active', $1 || '.test')
		 RETURNING id`,
		slug,
	).Scan(&otherWorkspaceID); err != nil {
		t.Fatalf("create other workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id = $1`, otherWorkspaceID)
	})

	profile, err := f.mr.PostureStore.CreateProfile(f.ctx, otherWorkspaceID, "cross-ws-delete", true)
	if err != nil {
		t.Fatalf("create other-workspace profile: %v", err)
	}

	_, err = f.mr.DeleteDeviceProfile(f.ctx, profile.ID.String())
	if err == nil || !strings.Contains(err.Error(), "deleteDeviceProfile: profile not found") {
		t.Fatalf("cross-workspace delete error = %v", err)
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("rejected cross-workspace delete fired push hook %d times, want 0", got)
	}
}

// Invalid profile UUID for delete must fail without notifying policy.
func TestDeleteDeviceProfileRejectsInvalidUUID(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)

	_, err := f.mr.DeleteDeviceProfile(f.ctx, "not-a-uuid")
	if err == nil || !strings.Contains(err.Error(), "invalid profile id") {
		t.Fatalf("invalid UUID delete error = %v", err)
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("invalid UUID delete fired push hook %d times, want 0", got)
	}
}

// Cross-workspace add requirement must fail without notifying policy.
func TestAddProfileRequirementRejectsCrossWorkspaceProfile(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)

	var otherWorkspaceID uuid.UUID
	slug := "posture-add-req-cross-ws-" + uuid.NewString()
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $1, 'active', $1 || '.test')
		 RETURNING id`,
		slug,
	).Scan(&otherWorkspaceID); err != nil {
		t.Fatalf("create other workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id = $1`, otherWorkspaceID)
	})

	profile, err := f.mr.PostureStore.CreateProfile(f.ctx, otherWorkspaceID, "cross-ws-add-req", true)
	if err != nil {
		t.Fatalf("create other-workspace profile: %v", err)
	}

	_, err = f.mr.AddProfileRequirement(f.ctx, profile.ID.String(), posture.CheckLUKS, true)
	if err == nil || !strings.Contains(err.Error(), "addProfileRequirement: profile not found") {
		t.Fatalf("cross-workspace add requirement error = %v", err)
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("rejected cross-workspace add requirement fired push hook %d times, want 0", got)
	}
}

// Invalid profile UUID for add requirement must fail without notifying policy.
func TestAddProfileRequirementRejectsInvalidProfileUUID(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)

	_, err := f.mr.AddProfileRequirement(f.ctx, "not-a-uuid", posture.CheckLUKS, false)
	if err == nil || !strings.Contains(err.Error(), "invalid profile id") {
		t.Fatalf("invalid UUID add requirement error = %v", err)
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("invalid UUID add requirement fired push hook %d times, want 0", got)
	}
}

// Cross-workspace remove requirement must fail without notifying policy.
func TestRemoveProfileRequirementRejectsCrossWorkspaceProfile(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)

	var otherWorkspaceID uuid.UUID
	slug := "posture-remove-req-cross-ws-" + uuid.NewString()
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $1, 'active', $1 || '.test')
		 RETURNING id`,
		slug,
	).Scan(&otherWorkspaceID); err != nil {
		t.Fatalf("create other workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id = $1`, otherWorkspaceID)
	})

	profile, err := f.mr.PostureStore.CreateProfile(f.ctx, otherWorkspaceID, "cross-ws-remove-req", true)
	if err != nil {
		t.Fatalf("create other-workspace profile: %v", err)
	}

	_, err = f.mr.RemoveProfileRequirement(f.ctx, profile.ID.String(), posture.CheckLUKS)
	if err == nil || !strings.Contains(err.Error(), "removeProfileRequirement: requirement or profile not found") {
		t.Fatalf("cross-workspace remove requirement error = %v", err)
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("rejected cross-workspace remove requirement fired push hook %d times, want 0", got)
	}
}

// Invalid profile UUID for remove requirement must fail without notifying policy.
func TestRemoveProfileRequirementRejectsInvalidProfileUUID(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)

	_, err := f.mr.RemoveProfileRequirement(f.ctx, "not-a-uuid", posture.CheckLUKS)
	if err == nil || !strings.Contains(err.Error(), "invalid profile id") {
		t.Fatalf("invalid UUID remove requirement error = %v", err)
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("invalid UUID remove requirement fired push hook %d times, want 0", got)
	}
}

// Invalid resource UUID for bind must fail before database access or notification.
func TestBindResourceToProfileRejectsInvalidResourceUUID(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)

	_, err := f.mr.BindResourceToProfile(f.ctx, uuid.NewString(), "not-a-uuid")
	if err == nil || !strings.Contains(err.Error(), "invalid resource id") {
		t.Fatalf("invalid resource UUID bind error = %v", err)
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("invalid resource UUID bind fired push hook %d times, want 0", got)
	}
}

// Invalid resource UUID for unbind must fail before database access or notification.
func TestUnbindResourceFromProfileRejectsInvalidResourceUUID(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)

	_, err := f.mr.UnbindResourceFromProfile(f.ctx, uuid.NewString(), "not-a-uuid")
	if err == nil || !strings.Contains(err.Error(), "invalid resource id") {
		t.Fatalf("invalid resource UUID unbind error = %v", err)
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("invalid resource UUID unbind fired push hook %d times, want 0", got)
	}
}
