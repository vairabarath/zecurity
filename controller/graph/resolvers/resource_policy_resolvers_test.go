package resolvers

import (
	"testing"

	"github.com/google/uuid"
	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/posture"
)

// PENDING-16: through every resource-policy operation, another workspace's
// policy or resource gets the same answer as a missing or malformed one, and
// nothing changes or notifies.
func TestResourcePolicyIsolation(t *testing.T) {
	f := newIsoFixture(t)
	f.mr.PostureStore = posture.NewStore(f.pool)
	f.qr.PostureStore = f.mr.PostureStore
	wsB := uuid.MustParse(f.b.id)
	bPolicy, err := f.mr.PostureStore.CreateResourcePolicy(f.b.ctx, wsB, "B only", nil)
	if err != nil {
		t.Fatal(err)
	}
	aPolicy, err := f.mr.CreateResourcePolicy(f.a.ctx, "A policy", nil)
	if err != nil {
		t.Fatal(err)
	}
	vA, vB := f.notifier.Version(f.a.id), f.notifier.Version(f.b.id)
	name := "taken over"

	policyIDs := []struct{ what, id string }{{"foreign", bPolicy.ID.String()}, {"missing", missingID}, {"malformed", "not-a-uuid"}}
	for _, p := range policyIDs {
		for _, c := range []struct {
			op   string
			call func() error
		}{
			{"updateResourcePolicy", func() error { _, err := f.mr.UpdateResourcePolicy(f.a.ctx, p.id, &name, nil); return err }},
			{"setResourcePolicyProfiles", func() error { _, err := f.mr.SetResourcePolicyProfiles(f.a.ctx, p.id, nil); return err }},
			{"deleteResourcePolicy", func() error { _, err := f.mr.DeleteResourcePolicy(f.a.ctx, p.id); return err }},
			{"setResourcePolicy", func() error { _, err := f.mr.SetResourcePolicy(f.a.ctx, f.a.res, p.id); return err }},
		} {
			err := c.call()
			if err == nil || presented(f.a.ctx, err) != "resource policy not found" {
				t.Errorf("%s on a %s policy: client sees %q", c.op, p.what, presentedOrNil(f, err))
			}
		}
		if got, err := f.qr.ResourcePolicy(f.a.ctx, p.id); got != nil || err != nil {
			t.Errorf("resourcePolicy(%s) = %v, %v; want nil, nil", p.what, got, err)
		}
	}
	for _, r := range []struct{ what, id string }{{"foreign", f.b.res}, {"missing", missingID}, {"malformed", "not-a-uuid"}} {
		_, err := f.mr.SetResourcePolicy(f.a.ctx, r.id, aPolicy.ID)
		if err == nil || presented(f.a.ctx, err) != "resource not found" {
			t.Errorf("setResourcePolicy on a %s resource: client sees %q", r.what, presentedOrNil(f, err))
		}
	}
	if got, _ := f.mr.PostureStore.GetResourcePolicy(f.b.ctx, wsB, bPolicy.ID); got == nil || got.Name != "B only" {
		t.Fatalf("B's policy changed: %+v", got)
	}
	if f.notifier.Version(f.a.id) != vA || f.notifier.Version(f.b.id) != vB {
		t.Fatal("a refused operation bumped a policy version")
	}
	list, err := f.qr.ResourcePolicies(f.a.ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("A lists %d policies (%v), want its Default + A policy", len(list), err)
	}
}

func presentedOrNil(f *isoFixture, err error) string {
	if err == nil {
		return "<no error>"
	}
	return presented(f.a.ctx, err)
}

// D-4: a confirmed delete of a profile in use returns the policies it was
// removed from and, of those, the ones left with no profiles (now any device).
func TestDeleteDeviceProfileResultNamesPolicies(t *testing.T) {
	f := newIsoFixture(t)
	f.mr.PostureStore = posture.NewStore(f.pool)
	ws := uuid.MustParse(f.a.id)
	newProfile := func(name string) string {
		p, err := f.mr.PostureStore.CreateProfile(f.a.ctx, ws, name, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.mr.PostureStore.AddRequirement(f.a.ctx, ws, p.ID, posture.Requirement{CheckID: posture.CheckFirewall}); err != nil {
			t.Fatal(err)
		}
		return p.ID.String()
	}
	gone, other := newProfile("gone"), newProfile("other")
	only, err := f.mr.CreateResourcePolicy(f.a.ctx, "Only gone", nil)
	if err != nil {
		t.Fatal(err)
	}
	both, err := f.mr.CreateResourcePolicy(f.a.ctx, "Gone or other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.mr.SetResourcePolicyProfiles(f.a.ctx, only.ID, []string{gone}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mr.SetResourcePolicyProfiles(f.a.ctx, both.ID, []string{gone, other}); err != nil {
		t.Fatal(err)
	}

	confirm := true
	res, err := f.mr.DeleteDeviceProfile(f.a.ctx, gone, &confirm)
	if err != nil {
		t.Fatalf("confirmed delete: %v", err)
	}
	names := func(refs []*graph.ResourcePolicyRef) map[string]string {
		m := map[string]string{}
		for _, r := range refs {
			m[r.ID] = r.Name
		}
		return m
	}
	removed, anyDevice := names(res.RemovedFrom), names(res.NowAnyDevice)
	if len(removed) != 2 || removed[only.ID] != "Only gone" || removed[both.ID] != "Gone or other" {
		t.Fatalf("removedFrom = %v, want both policies", removed)
	}
	if len(anyDevice) != 1 || anyDevice[only.ID] != "Only gone" {
		t.Fatalf("nowAnyDevice = %v, want only %q", anyDevice, "Only gone")
	}
}

// Each resource-policy change writes an audit row with its action.
func TestResourcePolicyAudit(t *testing.T) {
	f := newIsoFixture(t)
	f.mr.PostureStore = posture.NewStore(f.pool)
	ws := uuid.MustParse(f.a.id)
	prof, err := f.mr.PostureStore.CreateProfile(f.a.ctx, ws, "audited", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.mr.PostureStore.AddRequirement(f.a.ctx, ws, prof.ID, posture.Requirement{CheckID: posture.CheckFirewall}); err != nil {
		t.Fatal(err)
	}

	pol, err := f.mr.CreateResourcePolicy(f.a.ctx, "Audited", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.mr.SetResourcePolicyProfiles(f.a.ctx, pol.ID, []string{prof.ID.String()}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.mr.SetResourcePolicy(f.a.ctx, f.a.res, pol.ID); err != nil {
		t.Fatal(err)
	}
	confirm := true
	if _, err := f.mr.DeleteDeviceProfile(f.a.ctx, prof.ID.String(), nil); err == nil {
		t.Fatal("deleting a profile in use without confirmation succeeded")
	}
	if _, err := f.mr.DeleteDeviceProfile(f.a.ctx, prof.ID.String(), &confirm); err != nil {
		t.Fatalf("confirmed delete: %v", err)
	}
	if _, err := f.mr.DeleteResourcePolicy(f.a.ctx, pol.ID); err != nil {
		t.Fatal(err)
	}

	rows, err := f.pool.Query(f.a.ctx,
		`SELECT action, target_id, coalesce(details::text, '') FROM audit_logs WHERE tenant_id = $1 ORDER BY created_at, action`, f.a.id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	details := map[string]string{}
	for rows.Next() {
		var action, target, d string
		if err := rows.Scan(&action, &target, &d); err != nil {
			t.Fatal(err)
		}
		got = append(got, action)
		details[action] = d
	}
	want := []string{"resource_policy.create", "resource_policy.profiles", "resource.policy", "device_profile.delete", "resource_policy.delete"}
	if len(got) != len(want) {
		t.Fatalf("audit actions = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("audit actions = %v, want %v", got, want)
		}
	}
	if d := details["device_profile.delete"]; !contains(d, "Audited") {
		t.Fatalf("profile delete audit doesn't name the policies: %s", d)
	}
	if d := details["resource_policy.delete"]; !contains(d, f.a.res) {
		t.Fatalf("policy delete audit doesn't list the moved resource: %s", d)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
