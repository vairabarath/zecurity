package mapping

import "testing"

func TestExtractSubjectClaim_DefaultIsSub(t *testing.T) {
	claims := map[string]any{"sub": "abc123", "oid": "xyz789", "email": "a@b.c"}
	if got := ExtractSubjectClaim(claims, ""); got != "abc123" {
		t.Fatalf("default subjectClaim = %q, want %q", got, "abc123")
	}
}

func TestExtractSubjectClaim_ExplicitClaim(t *testing.T) {
	claims := map[string]any{"sub": "abc123", "oid": "xyz789", "email": "a@b.c"}
	if got := ExtractSubjectClaim(claims, "oid"); got != "xyz789" {
		t.Fatalf("subjectClaim(oid) = %q, want %q", got, "xyz789")
	}
}

func TestExtractSubjectClaim_NumericNormalized(t *testing.T) {
	withNum := map[string]any{"sub": float64(42)}
	if got := ExtractSubjectClaim(withNum, ""); got != "42" {
		t.Fatalf("numeric subjectClaim = %q, want %q", got, "42")
	}
}

func TestExtractSubjectClaim_MissingYieldsEmpty(t *testing.T) {
	claims := map[string]any{"sub": "abc123"}
	if got := ExtractSubjectClaim(claims, "missing"); got != "" {
		t.Fatalf("missing subjectClaim = %q, want empty", got)
	}
}

func TestExtractSubjectClaim_WhitespaceTrimmed(t *testing.T) {
	ws := map[string]any{"sub": "  spaced  "}
	if got := ExtractSubjectClaim(ws, ""); got != "spaced" {
		t.Fatalf("whitespace subjectClaim = %q, want %q", got, "spaced")
	}
}

// TestExtractSubjectClaim_ConfiguredButMissingIsNotSub is the fail-closed
// guarantee: when an EXPLICIT claim is configured but absent, the extractor
// returns "" and never resolves to "sub". The caller is responsible for
// failing authentication.
func TestExtractSubjectClaim_ConfiguredButMissingIsNotSub(t *testing.T) {
	claims := map[string]any{"sub": "abc123"} // "email" configured but absent
	if got := ExtractSubjectClaim(claims, "email"); got != "" {
		t.Fatalf("configured-but-missing claim must yield empty (not sub), got %q", got)
	}
}
