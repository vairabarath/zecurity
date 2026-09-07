package scim

import "testing"

// Okta deactivates a user (app unassignment) with a PATCH that has NO path and
// an object value: {"op":"replace","value":{"active":false}}. That is the
// RFC 7644 §3.5.2 whole-resource shape.
//
// applyPatchValue's ""/"emails" branch only handled a bare STRING value, so the
// object was silently dropped: p.Active stayed nil, dispatchActive() no-opped,
// and the request degraded into a plain attribute update — 2xx returned,
// users.updated_at bumped, user left ACTIVE. Observed live against Okta.
func TestUserPatch_NoPathObjectDeactivates(t *testing.T) {
	p, err := patchFromOps([]map[string]any{
		{"op": "replace", "value": map[string]any{"active": false}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Active == nil {
		t.Fatal("Active must be set: a no-path object value carrying active=false is a deactivation")
	}
	if *p.Active {
		t.Fatalf("Active must be false, got %v", *p.Active)
	}
}

// The reactivation direction must work through the same shape.
func TestUserPatch_NoPathObjectReactivates(t *testing.T) {
	p, err := patchFromOps([]map[string]any{
		{"op": "replace", "value": map[string]any{"active": true}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Active == nil || !*p.Active {
		t.Fatalf("expected Active=true, got %v", p.Active)
	}
}

// The explicit-path form that already worked must keep working.
func TestUserPatch_ExplicitActivePathStillWorks(t *testing.T) {
	p, err := patchFromOps([]map[string]any{
		{"op": "replace", "path": "active", "value": false},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Active == nil || *p.Active {
		t.Fatalf("expected Active=false, got %v", p.Active)
	}
}

// A no-path object may carry several attributes at once; all must be applied,
// not just the first.
func TestUserPatch_NoPathObjectAppliesEveryAttribute(t *testing.T) {
	p, err := patchFromOps([]map[string]any{
		{"op": "replace", "value": map[string]any{"active": false, "userName": "new@example.com"}},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Active == nil || *p.Active {
		t.Fatalf("active not applied: %v", p.Active)
	}
	if p.Email != "new@example.com" {
		t.Fatalf("userName not applied: %q", p.Email)
	}
}

// A bare STRING with no path keeps its previous meaning (an email change) —
// the new object branch must not have changed that.
func TestUserPatch_NoPathBareStringStillEmail(t *testing.T) {
	p, err := patchFromOps([]map[string]any{
		{"op": "replace", "value": "someone@example.com"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Email != "someone@example.com" {
		t.Fatalf("expected the bare string to set Email, got %q", p.Email)
	}
	if p.Active != nil {
		t.Fatalf("a bare string must not touch Active, got %v", *p.Active)
	}
}
