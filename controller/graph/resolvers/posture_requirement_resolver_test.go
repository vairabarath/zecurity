package resolvers

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/yourorg/ztna/controller/internal/posture"
)

func TestAddProfileRequirement_UnknownCheck(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)

	profile, err := f.mr.PostureStore.CreateProfile(
		f.ctx,
		uuid.MustParse(f.tenantID),
		"test-unknown-check-add-"+uuid.NewString(),
		true,
	)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() {
		_ = f.mr.PostureStore.DeleteProfile(
			context.Background(),
			uuid.MustParse(f.tenantID),
			profile.ID,
		)
	})

	_, err = f.mr.AddProfileRequirement(f.ctx, profile.ID.String(), "nonexistent.check", false)
	if err == nil {
		t.Fatal("expected error for unknown check")
	}
	if !strings.Contains(err.Error(), "unknown posture check") {
		t.Fatalf("error = %v, want unknown posture check", err)
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("push hook fired %d times, want 0", got)
	}
}

func TestRemoveProfileRequirement_UnknownCheck(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)

	profile, err := f.mr.PostureStore.CreateProfile(
		f.ctx,
		uuid.MustParse(f.tenantID),
		"test-unknown-check-remove-"+uuid.NewString(),
		true,
	)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() {
		_ = f.mr.PostureStore.DeleteProfile(
			context.Background(),
			uuid.MustParse(f.tenantID),
			profile.ID,
		)
	})

	_, err = f.mr.RemoveProfileRequirement(f.ctx, profile.ID.String(), "nonexistent.check")
	if err == nil {
		t.Fatal("expected error for unknown check")
	}
	if !strings.Contains(err.Error(), "unknown posture check") {
		t.Fatalf("error = %v, want unknown posture check", err)
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("push hook fired %d times, want 0", got)
	}
}

func TestAddProfileRequirement_Duplicate(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)
	f.mr.Resolver.PostureEvaluator = posture.NewEvaluator(f.mr.PostureStore, f.notifier)

	profile, err := f.mr.PostureStore.CreateProfile(
		f.ctx,
		uuid.MustParse(f.tenantID),
		"test-duplicate-"+uuid.NewString(),
		true,
	)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() {
		_ = f.mr.PostureStore.DeleteProfile(
			context.Background(),
			uuid.MustParse(f.tenantID),
			profile.ID,
		)
	})

	_, err = f.mr.AddProfileRequirement(f.ctx, profile.ID.String(), posture.CheckFirewall, false)
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("push hook fired %d times after first add, want 1", got)
	}

	_, err = f.mr.AddProfileRequirement(f.ctx, profile.ID.String(), posture.CheckFirewall, false)
	if err == nil {
		t.Fatal("expected duplicate requirement error")
	}
	if !strings.Contains(err.Error(), "duplicate requirement") {
		t.Fatalf("error = %v, want duplicate requirement", err)
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("push hook fired %d times total after duplicate, want 1", got)
	}
}

func TestAddProfileRequirement_Success(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)
	f.mr.Resolver.PostureEvaluator = posture.NewEvaluator(f.mr.PostureStore, f.notifier)

	profile, err := f.mr.PostureStore.CreateProfile(
		f.ctx,
		uuid.MustParse(f.tenantID),
		"test-add-success-"+uuid.NewString(),
		true,
	)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() {
		_ = f.mr.PostureStore.DeleteProfile(
			context.Background(),
			uuid.MustParse(f.tenantID),
			profile.ID,
		)
	})

	got, err := f.mr.AddProfileRequirement(f.ctx, profile.ID.String(), posture.CheckFirewall, true)
	if err != nil {
		t.Fatalf("AddProfileRequirement: %v", err)
	}
	if got.ID != profile.ID.String() {
		t.Fatalf("returned profile ID = %q, want %q", got.ID, profile.ID)
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("push hook fired %d times, want 1", got)
	}
}

func TestRemoveProfileRequirement_Success(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)
	f.mr.Resolver.PostureEvaluator = posture.NewEvaluator(f.mr.PostureStore, f.notifier)

	profile, err := f.mr.PostureStore.CreateProfile(
		f.ctx,
		uuid.MustParse(f.tenantID),
		"test-remove-success-"+uuid.NewString(),
		true,
	)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() {
		_ = f.mr.PostureStore.DeleteProfile(
			context.Background(),
			uuid.MustParse(f.tenantID),
			profile.ID,
		)
	})

	_, err = f.mr.AddProfileRequirement(f.ctx, profile.ID.String(), posture.CheckFirewall, false)
	if err != nil {
		t.Fatalf("AddProfileRequirement: %v", err)
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("push hook fired %d times after add, want 1", got)
	}

	got, err := f.mr.RemoveProfileRequirement(f.ctx, profile.ID.String(), posture.CheckFirewall)
	if err != nil {
		t.Fatalf("RemoveProfileRequirement: %v", err)
	}
	if got.ID != profile.ID.String() {
		t.Fatalf("returned profile ID = %q, want %q", got.ID, profile.ID)
	}
	if got := f.fires.Load(); got != 2 {
		t.Fatalf("push hook fired %d times total, want 2", got)
	}
}

func TestRemoveProfileRequirement_LastEnforceRejected(t *testing.T) {
	f := newACLCoherenceFixture(t)
	f.mr.Resolver.PostureStore = posture.NewStore(f.pool)

	profile, err := f.mr.PostureStore.CreateProfile(
		f.ctx,
		uuid.MustParse(f.tenantID),
		"test-last-enforce-"+uuid.NewString(),
		true,
	)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() {
		_ = f.mr.PostureStore.DeleteProfile(
			context.Background(),
			uuid.MustParse(f.tenantID),
			profile.ID,
		)
	})

	err = f.mr.PostureStore.AddRequirement(
		f.ctx,
		uuid.MustParse(f.tenantID),
		profile.ID,
		posture.Requirement{CheckID: posture.CheckFirewall, AllowUnsupported: false},
	)
	if err != nil {
		t.Fatalf("add requirement: %v", err)
	}

	if _, err := f.pool.Exec(
		f.ctx,
		`UPDATE device_profiles SET mode = 'enforce' WHERE id = $1`,
		profile.ID,
	); err != nil {
		t.Fatalf("set enforce mode: %v", err)
	}

	_, err = f.mr.RemoveProfileRequirement(f.ctx, profile.ID.String(), posture.CheckFirewall)
	if err == nil {
		t.Fatal("expected error removing last requirement from enforced profile")
	}
	if !strings.Contains(err.Error(), "cannot remove the final requirement from an enforced profile") {
		t.Fatalf("error = %v, want last requirement from enforced profile", err)
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("push hook fired %d times, want 0", got)
	}
}
