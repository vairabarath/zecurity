package resolvers

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/yourorg/ztna/controller/graph"
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
		_, _ = f.mr.PostureStore.DeleteProfile(
			context.Background(),
			uuid.MustParse(f.tenantID),
			profile.ID, true,
		)
	})

	_, err = f.mr.AddProfileRequirement(f.ctx, profile.ID.String(), "nonexistent.check", false, nil, nil)
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
		_, _ = f.mr.PostureStore.DeleteProfile(
			context.Background(),
			uuid.MustParse(f.tenantID),
			profile.ID, true,
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
		_, _ = f.mr.PostureStore.DeleteProfile(
			context.Background(),
			uuid.MustParse(f.tenantID),
			profile.ID, true,
		)
	})

	_, err = f.mr.AddProfileRequirement(f.ctx, profile.ID.String(), posture.CheckFirewall, false, nil, nil)
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	if got := f.fires.Load(); got != 1 {
		t.Fatalf("push hook fired %d times after first add, want 1", got)
	}

	_, err = f.mr.AddProfileRequirement(f.ctx, profile.ID.String(), posture.CheckFirewall, false, nil, nil)
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
		_, _ = f.mr.PostureStore.DeleteProfile(
			context.Background(),
			uuid.MustParse(f.tenantID),
			profile.ID, true,
		)
	})

	got, err := f.mr.AddProfileRequirement(f.ctx, profile.ID.String(), posture.CheckFirewall, true, nil, nil)
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
		_, _ = f.mr.PostureStore.DeleteProfile(
			context.Background(),
			uuid.MustParse(f.tenantID),
			profile.ID, true,
		)
	})

	_, err = f.mr.AddProfileRequirement(f.ctx, profile.ID.String(), posture.CheckFirewall, false, nil, nil)
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

func TestRemoveProfileRequirement_LastRequirementOfProfileInUseRejected(t *testing.T) {
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
		_, _ = f.mr.PostureStore.DeleteProfile(
			context.Background(),
			uuid.MustParse(f.tenantID),
			profile.ID, true,
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

	// PENDING-16: a profile a resource policy uses keeps its last requirement.
	pol, err := f.mr.PostureStore.CreateResourcePolicy(f.ctx, uuid.MustParse(f.tenantID), "last-req-"+uuid.NewString(), nil)
	if err != nil {
		t.Fatalf("create policy: %v", err)
	}
	if _, err := f.mr.PostureStore.SetResourcePolicyProfiles(f.ctx, uuid.MustParse(f.tenantID), pol.ID, []uuid.UUID{profile.ID}); err != nil {
		t.Fatalf("require profile: %v", err)
	}

	_, err = f.mr.RemoveProfileRequirement(f.ctx, profile.ID.String(), posture.CheckFirewall)
	if err == nil {
		t.Fatal("expected error removing the last requirement of a profile in use")
	}
	if got := ErrorPresenter(f.ctx, err).Message; got != posture.ErrLastRequirement.Error() {
		t.Fatalf("client sees %q, want %q", got, posture.ErrLastRequirement.Error())
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("push hook fired %d times, want 0", got)
	}
}

// PENDING-20 G1: the country check needs a valid rule; no other check takes one.
func TestAddProfileRequirement_CountryRule(t *testing.T) {
	f := newACLCoherenceFixture(t)
	store := posture.NewStore(f.pool)
	f.mr.Resolver.PostureStore = store
	f.mr.Resolver.PostureEvaluator = posture.NewEvaluator(store, nil)

	profile, err := store.CreateProfile(f.ctx, uuid.MustParse(f.tenantID), "test-country-"+uuid.NewString(), true)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.DeleteProfile(context.Background(), uuid.MustParse(f.tenantID), profile.ID, true)
	})
	id := profile.ID.String()

	for _, tc := range []struct {
		name      string
		check     string
		countries *graph.CountryRuleInput
		want      string
	}{
		{"country without rule", posture.CheckNetworkCountry, nil, "needs countries"},
		{"rule on another check", posture.CheckFirewall, &graph.CountryRuleInput{Allow: []string{"IN"}, Deny: []string{}}, "countries apply only to"},
		{"bad code", posture.CheckNetworkCountry, &graph.CountryRuleInput{Allow: []string{"India"}, Deny: []string{}}, "not an ISO 3166-1 alpha-2"},
		{"both empty", posture.CheckNetworkCountry, &graph.CountryRuleInput{Allow: []string{}, Deny: []string{}}, "at least one country"},
		{"in both lists", posture.CheckNetworkCountry, &graph.CountryRuleInput{Allow: []string{"IN"}, Deny: []string{"in"}}, "in both the allow and the deny list"},
	} {
		_, err := f.mr.AddProfileRequirement(f.ctx, id, tc.check, false, tc.countries, nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	if _, err := f.mr.AddProfileRequirement(f.ctx, id, posture.CheckNetworkCountry, true,
		&graph.CountryRuleInput{Allow: []string{"IN"}, Deny: []string{}}, nil); err == nil || !strings.Contains(err.Error(), "allowUnsupported does not apply") {
		t.Errorf("allowUnsupported on the country check: err = %v", err)
	}
	if _, err := f.mr.UpdateProfileRequirementCountries(f.ctx, id, graph.CountryRuleInput{Allow: []string{"IN"}, Deny: []string{}}); err == nil ||
		!strings.Contains(err.Error(), "no country check") {
		t.Errorf("update before a country check exists: err = %v", err)
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("refused requirements fired the push hook %d times", got)
	}

	got, err := f.mr.AddProfileRequirement(f.ctx, id, posture.CheckNetworkCountry, false,
		&graph.CountryRuleInput{Allow: []string{"in", "us"}, Deny: []string{"cn"}}, nil)
	if err != nil {
		t.Fatalf("valid country rule: %v", err)
	}
	reqs, err := f.mr.DeviceProfile().Requirements(f.ctx, got)
	if err != nil || len(reqs) != 1 || reqs[0].Countries == nil {
		t.Fatalf("requirements: %+v %v", reqs, err)
	}
	if a, d := strings.Join(reqs[0].Countries.Allow, ","), strings.Join(reqs[0].Countries.Deny, ","); a != "IN,US" || d != "CN" {
		t.Fatalf("stored rule allow=%s deny=%s, want IN,US / CN", a, d)
	}

	// G-10: replace the lists in place.
	if _, err := f.mr.UpdateProfileRequirementCountries(f.ctx, id, graph.CountryRuleInput{Allow: []string{}, Deny: []string{"ru"}}); err != nil {
		t.Fatalf("update: %v", err)
	}
	reqs, _ = f.mr.DeviceProfile().Requirements(f.ctx, got)
	if len(reqs) != 1 || len(reqs[0].Countries.Allow) != 0 || strings.Join(reqs[0].Countries.Deny, ",") != "RU" {
		t.Fatalf("after update: %+v", reqs[0].Countries)
	}
	if _, err := f.mr.UpdateProfileRequirementCountries(f.ctx, id, graph.CountryRuleInput{Allow: []string{}, Deny: []string{}}); err == nil {
		t.Fatal("update to an empty rule accepted")
	}
}

// ADR-031 D-11: a Minimum OS Version check needs a valid minimum; no other
// check takes one; the minimum is stored and returned.
func TestAddProfileRequirement_MinVersion(t *testing.T) {
	f := newACLCoherenceFixture(t)
	store := posture.NewStore(f.pool)
	f.mr.Resolver.PostureStore = store
	f.mr.Resolver.PostureEvaluator = posture.NewEvaluator(store, nil)

	profile, err := store.CreateProfile(f.ctx, uuid.MustParse(f.tenantID), "test-minos-"+uuid.NewString(), true)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.DeleteProfile(context.Background(), uuid.MustParse(f.tenantID), profile.ID, true)
	})
	id := profile.ID.String()
	str := func(s string) *string { return &s }

	for _, tc := range []struct {
		name  string
		check string
		min   *string
		allow bool
		want  string
	}{
		{"no minimum", posture.CheckWindowsMinOS, nil, false, "needs a minimum version"},
		{"blank minimum", posture.CheckWindowsMinOS, str("  "), false, "needs a minimum version"},
		{"minimum on another check", posture.CheckWindowsFirewall, str("10"), false, "minVersion applies only"},
		{"not a version", posture.CheckAndroidMinOS, str("twelve"), false, "numbers separated by dots"},
		{"too many parts", posture.CheckWindowsMinOS, str("10.0.1.2.3"), false, "numbers separated by dots"},
		{"allowUnsupported", posture.CheckIOSMinOS, str("17"), true, "allowUnsupported does not apply"},
	} {
		_, err := f.mr.AddProfileRequirement(f.ctx, id, tc.check, tc.allow, nil, tc.min)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	if got := f.fires.Load(); got != 0 {
		t.Fatalf("refused requirements fired the push hook %d times", got)
	}

	got, err := f.mr.AddProfileRequirement(f.ctx, id, posture.CheckWindowsMinOS, false, nil, str(" 10.0.22631 "))
	if err != nil {
		t.Fatalf("valid minimum: %v", err)
	}
	reqs, err := f.mr.DeviceProfile().Requirements(f.ctx, got)
	if err != nil || len(reqs) != 1 || reqs[0].MinVersion == nil || *reqs[0].MinVersion != "10.0.22631" {
		t.Fatalf("requirements: %+v %v", reqs, err)
	}

	// Replaced in place.
	if _, err := f.mr.UpdateProfileRequirementMinVersion(f.ctx, id, posture.CheckWindowsMinOS, "10.0.26100"); err != nil {
		t.Fatalf("update: %v", err)
	}
	reqs, _ = f.mr.DeviceProfile().Requirements(f.ctx, got)
	if len(reqs) != 1 || *reqs[0].MinVersion != "10.0.26100" {
		t.Fatalf("after update: %+v", reqs)
	}
	for _, tc := range []struct{ check, min, want string }{
		{posture.CheckWindowsMinOS, "ten", "numbers separated by dots"},
		{posture.CheckAndroidMinOS, "13", "doesn't require"},
		{posture.CheckWindowsFirewall, "10", "not a Minimum OS Version check"},
	} {
		if _, err := f.mr.UpdateProfileRequirementMinVersion(f.ctx, id, tc.check, tc.min); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("update %s %q: err = %v, want %q", tc.check, tc.min, err, tc.want)
		}
	}
}
