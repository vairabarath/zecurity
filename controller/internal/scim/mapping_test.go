package scim

import "testing"

func TestExtractSubjectClaim(t *testing.T) {
	claims := map[string]any{"sub": "abc123", "oid": "xyz789", "email": "a@b.c"}

	// Default claim is "sub".
	if got := ExtractSubjectClaim(claims, ""); got != "abc123" {
		t.Fatalf("default subjectClaim = %q, want %q", got, "abc123")
	}
	// Explicit claim name.
	if got := ExtractSubjectClaim(claims, "oid"); got != "xyz789" {
		t.Fatalf("subjectClaim(oid) = %q, want %q", got, "xyz789")
	}
	// Numeric OIDC claims (e.g. some IdPs) normalize to a stable string.
	withNum := map[string]any{"sub": float64(42)}
	if got := ExtractSubjectClaim(withNum, ""); got != "42" {
		t.Fatalf("numeric subjectClaim = %q, want %q", got, "42")
	}
	// Missing claim yields empty (never panics).
	if got := ExtractSubjectClaim(claims, "missing"); got != "" {
		t.Fatalf("missing subjectClaim = %q, want empty", got)
	}
	// Whitespace is trimmed.
	ws := map[string]any{"sub": "  spaced  "}
	if got := ExtractSubjectClaim(ws, ""); got != "spaced" {
		t.Fatalf("whitespace subjectClaim = %q, want %q", got, "spaced")
	}
}

func TestExtractScimIdentifier(t *testing.T) {
	resource := map[string]any{"externalId": "ext-1", "userName": "jdoe"}

	// Default attribute is "externalId".
	if got := ExtractScimIdentifier(resource, ""); got != "ext-1" {
		t.Fatalf("default scimIdentifier = %q, want %q", got, "ext-1")
	}
	// Explicit attribute name.
	if got := ExtractScimIdentifier(resource, "userName"); got != "jdoe" {
		t.Fatalf("scimIdentifier(userName) = %q, want %q", got, "jdoe")
	}
	// Missing attribute yields empty.
	if got := ExtractScimIdentifier(resource, "nope"); got != "" {
		t.Fatalf("missing scimIdentifier = %q, want empty", got)
	}
}

func TestValidateMappingConfig(t *testing.T) {
	// Empty values resolve to defaults and are valid.
	if err := ValidateMappingConfig("", ""); err != nil {
		t.Fatalf("empty config should be valid (defaults differ): %v", err)
	}
	// Distinct, valid overrides.
	if err := ValidateMappingConfig("oid", "externalId"); err != nil {
		t.Fatalf("distinct config should be valid: %v", err)
	}
	// Hardcode that equates both extractors is rejected — must NOT silently
	// assume sub == externalId.
	if err := ValidateMappingConfig("sub", "sub"); err == nil {
		t.Fatalf("config equating subjectClaim and scimIdentifier must be rejected")
	}
}

func TestResolveMappingDefaults(t *testing.T) {
	sc, si := ResolveMapping("", "")
	if sc != DefaultSubjectClaim || si != DefaultScimIdentifier {
		t.Fatalf("ResolveMapping defaults = (%q,%q), want (%q,%q)", sc, si, DefaultSubjectClaim, DefaultScimIdentifier)
	}
	// Non-empty values are preserved.
	sc, si = ResolveMapping("oid", "employeeNumber")
	if sc != "oid" || si != "employeeNumber" {
		t.Fatalf("ResolveMapping preserved = (%q,%q), want (oid,employeeNumber)", sc, si)
	}
}
