package scim

import "testing"

// Okta sends a member removal as a targeted filter path, and Okta user ids are
// MIXED CASE (e.g. "00u16w7qu5saDn60H698"). The parser used to lowercase the
// whole path before extracting the id, so the captured value no longer matched
// external_identities.subject (compared byte-for-byte) — every removal resolved
// to zero users and PatchGroup rejected the request with
// 404 "unknown members: …", leaving the user in the group in Zecurity.
//
// The pre-existing integration coverage used the id "h-1", which is already
// lowercase, so it could never catch this.
func TestGroupMemberValues_FilterPathPreservesValueCase(t *testing.T) {
	const oktaID = "00u16w7qu5saDn60H698"

	vals, err := groupMemberValues("REMOVE", `members[value eq "`+oktaID+`"]`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(vals) != 1 {
		t.Fatalf("expected exactly one member value, got %d: %v", len(vals), vals)
	}
	if vals[0] != oktaID {
		t.Fatalf("member id case was folded: got %q want %q", vals[0], oktaID)
	}
}

// The ATTRIBUTE NAME stays case-insensitive (RFC 7644 §3.10) and surrounding
// whitespace is tolerated — providers are inconsistent about both — while the
// value keeps its case.
func TestGroupMemberValues_FilterPathAttributeCaseAndSpacing(t *testing.T) {
	const oktaID = "00uAbCdEfG123"
	for _, path := range []string{
		`members[value eq "` + oktaID + `"]`,
		`Members[Value eq "` + oktaID + `"]`,
		`MEMBERS[ value  eq  "` + oktaID + `" ]`,
	} {
		vals, err := groupMemberValues("REMOVE", path, nil)
		if err != nil {
			t.Fatalf("path %q: unexpected error: %v", path, err)
		}
		if len(vals) != 1 || vals[0] != oktaID {
			t.Fatalf("path %q: got %v want [%s]", path, vals, oktaID)
		}
	}
}

// A filtered path is only meaningful for remove; add/replace must still be
// refused rather than silently treated as a removal.
func TestGroupMemberValues_FilterPathRejectedForNonRemove(t *testing.T) {
	for _, op := range []string{"ADD", "REPLACE"} {
		if _, err := groupMemberValues(op, `members[value eq "00uAbC"]`, nil); err == nil {
			t.Fatalf("op %s: expected a filtered-path rejection", op)
		}
	}
}

// End-to-end through the Operations parser, which is what the HTTP handler
// actually calls — the shape Okta PUTs on "remove user from pushed group".
func TestPatchGroupFromOps_OktaRemoveShapePreservesCase(t *testing.T) {
	const oktaID = "00u16w7qu5saDn60H698"
	p, err := patchGroupFromOps([]map[string]any{
		{"op": "remove", "path": `members[value eq "` + oktaID + `"]`},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(p.Ops) != 1 || p.Ops[0].Op != "REMOVE" {
		t.Fatalf("expected one REMOVE op, got %+v", p.Ops)
	}
	if len(p.Ops[0].Values) != 1 || p.Ops[0].Values[0] != oktaID {
		t.Fatalf("member id not carried through intact: %v", p.Ops[0].Values)
	}
}
