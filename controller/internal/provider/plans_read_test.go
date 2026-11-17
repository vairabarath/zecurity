package provider_test

import (
	"encoding/json"
	"testing"
)

// Plan reads (Sprint 23 B1, D-43): super-admin only; the seeded Free plan is
// the default and every new workspace is on it; tenant detail names the plan.

const (
	freePlanID   = "00000000-0000-0000-0001-000000000001"
	legacyPlanID = "00000000-0000-0000-0001-000000000002"
)

func TestPlanReads_SuperAdminListAndGet(t *testing.T) {
	e := newReadEnv(t)
	r := e.get(t, "/provider/plans", e.superTok)
	if r.code != 200 {
		t.Fatalf("list = %d %s", r.code, r.body)
	}
	var list struct {
		Items []struct {
			ID        string            `json:"id"`
			Name      string            `json:"name"`
			IsDefault bool              `json:"is_default"`
			Tenants   int               `json:"tenants"`
			Limits    map[string]*int64 `json:"limits"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(r.body), &list); err != nil || len(list.Items) != 2 {
		t.Fatalf("list body %s: %v", r.body, err)
	}
	free, legacy := list.Items[0], list.Items[1]
	if free.ID != freePlanID || !free.IsDefault || free.Tenants != 1 || free.Limits["users"] == nil || *free.Limits["users"] != 5 {
		t.Fatalf("Free first, default, with the seeded tenant: %+v", free)
	}
	if legacy.ID != legacyPlanID || legacy.IsDefault || legacy.Tenants != 0 || len(legacy.Limits) != 6 || legacy.Limits["resources"] != nil {
		t.Fatalf("Legacy: %+v", legacy)
	}

	if r := e.get(t, "/provider/plans/"+legacyPlanID, e.superTok); r.code != 200 {
		t.Fatalf("get = %d %s", r.code, r.body)
	}
	if r := e.get(t, "/provider/plans/not-a-uuid", e.superTok); r.code != 400 || readErrCode(t, r) != "invalid_id" {
		t.Fatalf("bad id = %d %s", r.code, r.body)
	}
	if r := e.get(t, "/provider/plans/00000000-0000-0000-0000-0000000000ff", e.superTok); r.code != 404 {
		t.Fatalf("unknown plan = %d %s", r.code, r.body)
	}
}

func TestPlanReads_RelayOpsForbidden(t *testing.T) {
	e := newReadEnv(t)
	for _, path := range []string{"/provider/plans", "/provider/plans/" + freePlanID} {
		if r := e.get(t, path, e.opsTok); r.code != 403 {
			t.Fatalf("relay-ops %s = %d %s, want 403", path, r.code, r.body)
		}
	}
}

func TestPlanReads_TenantDetailNamesThePlan(t *testing.T) {
	e := newReadEnv(t)
	read := func() (id, name string, users *int64) {
		t.Helper()
		r := e.get(t, "/provider/tenants/"+e.tenantID, e.superTok)
		var d struct {
			Plan struct {
				ID     string            `json:"id"`
				Name   string            `json:"name"`
				Limits map[string]*int64 `json:"limits"`
			} `json:"plan"`
		}
		if err := json.Unmarshal([]byte(r.body), &d); err != nil || r.code != 200 {
			t.Fatalf("tenant detail %d %s: %v", r.code, r.body, err)
		}
		return d.Plan.ID, d.Plan.Name, d.Plan.Limits["users"]
	}
	if id, name, users := read(); id != freePlanID || name != "Free" || users == nil || *users != 5 {
		t.Fatalf("seeded tenant: plan %s %s users %v, want Free with 5", id, name, users)
	}
	exec(t, e.pool, `UPDATE workspaces SET plan_id = $2 WHERE id = $1`, e.tenantID, legacyPlanID)
	if id, name, users := read(); id != legacyPlanID || name != "Legacy" || users != nil {
		t.Fatalf("after reassignment: plan %s %s users %v, want Legacy unlimited", id, name, users)
	}
}
