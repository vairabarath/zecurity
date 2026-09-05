package scim

import "testing"

// DeriveGroupExternalID is the fallback Canonical Identity Key used only when
// the IdP pushes a group without an externalId (Okta "Push Groups" by name).
// These tests pin both the happy path and the documented limitations, so the
// trade-offs stay visible rather than being rediscovered in production.
func TestDeriveGroupExternalID(t *testing.T) {
	cases := []struct {
		name        string
		displayName string
		want        string
	}{
		{"simple", "hermes", "hermes"},
		{"spaces become hyphens", "Marketing Team", "marketing-team"},
		{"lowercased", "ENGINEERING", "engineering"},
		{"runs of separators collapse", "R&D  /  Ops", "r-d-ops"},
		{"leading/trailing junk trimmed", "  --Sales--  ", "sales"},
		{"digits kept", "Tier 1 Support", "tier-1-support"},
		{"empty input", "", ""},
		{"whitespace only", "   ", ""},
		{"punctuation only derives nothing", "!!!", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DeriveGroupExternalID(tc.displayName); got != tc.want {
				t.Fatalf("DeriveGroupExternalID(%q) = %q, want %q", tc.displayName, got, tc.want)
			}
		})
	}
}

// The derived key contains no characters that would need escaping in a
// /Groups/{id} path segment — the pre-fix implementation left "/" and "&"
// intact, which would have broken path-based lookups.
func TestDeriveGroupExternalID_PathSafe(t *testing.T) {
	got := DeriveGroupExternalID("R&D / Ops #1")
	for _, r := range got {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-'
		if !ok {
			t.Fatalf("derived key %q contains non-path-safe rune %q", got, r)
		}
	}
}

// Documented limitation #2: names that normalise to the same slug collide.
// They hit UNIQUE (workspace_id, connection_id, external_id) and surface as a
// 409 rather than silently merging two distinct IdP groups into one row.
// Asserted here so the collision is a known, tested property.
func TestDeriveGroupExternalID_CollidesOnEquivalentNames(t *testing.T) {
	a := DeriveGroupExternalID("Marketing Team")
	b := DeriveGroupExternalID("marketing team")
	c := DeriveGroupExternalID("Marketing-Team")
	if a != b || b != c {
		t.Fatalf("expected these to collide (documented limitation): %q %q %q", a, b, c)
	}
}

// Documented limitation #1: the key is derived from a MUTABLE display name, so
// renaming the group at the IdP yields a different key. This test exists to
// make that behaviour explicit — if a future change makes the key stable across
// renames (e.g. by deriving from the IdP's immutable group id), this test
// should be updated deliberately, not silently.
func TestDeriveGroupExternalID_NotStableAcrossRename(t *testing.T) {
	before := DeriveGroupExternalID("Marketing Team")
	after := DeriveGroupExternalID("Marketing")
	if before == after {
		t.Fatalf("expected rename to change the derived key (documented limitation), both were %q", before)
	}
}
