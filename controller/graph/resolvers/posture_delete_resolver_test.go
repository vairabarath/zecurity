package resolvers

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/yourorg/ztna/controller/internal/posture"
)

func TestDeleteDeviceProfile_Success(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)

	workspaceID := uuid.MustParse(f.tenantID)
	profile, err := f.mr.PostureStore.CreateProfile(
		f.ctx,
		workspaceID,
		"delete-me-"+uuid.NewString(),
	)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}

	ok, err := f.mr.DeleteDeviceProfile(f.ctx, profile.ID.String())
	if err != nil {
		t.Fatalf("DeleteDeviceProfile: %v", err)
	}
	if !ok {
		t.Fatal("DeleteDeviceProfile returned false, want true")
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("push hook fired %d times after successful delete, want 1", got)
	}
}

func TestDeleteDeviceProfile_NotFound(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)

	// Non-existent profile ID
	_, err := f.mr.DeleteDeviceProfile(f.ctx, uuid.NewString())
	if err == nil || !strings.Contains(err.Error(), "profile not found") {
		t.Fatalf("DeleteDeviceProfile non-existent error = %v, want 'profile not found'", err)
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("push hook fired %d times on not-found delete, want 0", got)
	}

	// Cross-workspace: create a profile in another workspace and try to delete it
	// from the fixture's workspace. The workspace_id filter in the DELETE query
	// should prevent finding it.
	var otherWorkspaceID uuid.UUID
	slug := "posture-cross-ws-delete-" + uuid.NewString()
	if err := f.pool.QueryRow(
		f.ctx,
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

	otherProfile, err := f.mr.PostureStore.CreateProfile(
		f.ctx,
		otherWorkspaceID,
		"cross-ws-"+uuid.NewString(),
	)
	if err != nil {
		t.Fatalf("create other-workspace profile: %v", err)
	}

	_, err = f.mr.DeleteDeviceProfile(f.ctx, otherProfile.ID.String())
	if err == nil || !strings.Contains(err.Error(), "profile not found") {
		t.Fatalf("cross-workspace delete error = %v, want 'profile not found'", err)
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("push hook fired %d times on cross-workspace delete, want 0", got)
	}
}

func TestDeleteDeviceProfile_NoNotifyOnStoreError(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)

	workspaceID := uuid.MustParse(f.tenantID)
	profile, err := f.mr.PostureStore.CreateProfile(
		f.ctx,
		workspaceID,
		"delete-fail-"+uuid.NewString(),
	)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() {
		_ = f.mr.PostureStore.DeleteProfile(
			context.Background(), workspaceID, profile.ID,
		)
	})

	// Use a cancelled context so the DB query fails with a non-ErrNotFound error.
	cancelCtx, cancel := context.WithCancel(f.ctx)
	cancel()

	_, err = f.mr.DeleteDeviceProfile(cancelCtx, profile.ID.String())
	if err == nil {
		t.Fatal("DeleteDeviceProfile with cancelled context should fail")
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("push hook fired %d times on store error, want 0", got)
	}
}
