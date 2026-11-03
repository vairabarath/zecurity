package scim

import "testing"

func TestProfileFor(t *testing.T) {
	// Known providers resolve to their built-in profile.
	for _, key := range []string{"okta", "entra", "jumpcloud", "keycloak"} {
		p := ProfileFor(key)
		if p.Key != key {
			t.Fatalf("ProfileFor(%q).Key = %q, want %q", key, p.Key, key)
		}
		if p.DefaultSubjectClaim == "" || p.DefaultScimIdentifier == "" {
			t.Fatalf("ProfileFor(%q) left a default empty", key)
		}
	}

	// Unknown provider falls back to Generic.
	g := ProfileFor("some-unknown-idp")
	if g.Key != GenericProfileKey {
		t.Fatalf("ProfileFor(unknown).Key = %q, want %q", g.Key, GenericProfileKey)
	}

	// Empty provider also falls back to Generic.
	if ProfileFor("").Key != GenericProfileKey {
		t.Fatalf("ProfileFor(\"\").Key should be %q", GenericProfileKey)
	}
}

func TestSupportsProbeLifecycle(t *testing.T) {
	// Entra supports create+delete → full lifecycle available.
	if !ProfileFor("entra").SupportsProbeLifecycle() {
		t.Fatalf("entra should support the probe lifecycle")
	}
	// A hand-trimmed profile that cannot delete must NOT support lifecycle.
	noDelete := Profile{Capabilities: SCIMCapabilities{CreateUser: true, DeleteUser: false}}
	if noDelete.SupportsProbeLifecycle() {
		t.Fatalf("a profile without delete must not claim lifecycle support")
	}
}
