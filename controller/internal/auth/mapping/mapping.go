// Package mapping holds the canonical Canonical Identity Key extraction for
// ADR-025 §3.1. It is a leaf package: it imports nothing from internal/auth,
// internal/scim, internal/identity, or internal/idp, so both the OIDC login
// path (internal/auth/providers) and the SCIM provisioning path
// (internal/scim) can share the exact same subject-claim extraction logic
// without an import cycle.
//
// The two sides of the mapping are intentionally symmetric:
//
//	OIDC login:   configured subjectClaim → ExtractSubjectClaim(claims)
//	SCIM provision: configured scimIdentifier → ExtractScimIdentifier(resource)
//
// For the same person they MUST resolve to the same value, but the code never
// assumes sub == externalId. See internal/scim/mapping.go for the SCIM side.
package mapping

import (
	"fmt"
	"strings"
)

// DefaultSubjectClaim is the OIDC claim read at login when a connection does
// not configure an override.
const DefaultSubjectClaim = "sub"

// AsString coerces an arbitrary claim/attribute value to a string. OIDC and
// SCIM attributes are JSON and may arrive as string, float64 (numbers), or
// bool; we normalize to a single string so the canonical key is stable.
func AsString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case fmt.Stringer:
		return t.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

// ExtractSubjectClaim returns the Canonical Identity Key from the OIDC claims
// map using the configured claim name. An empty claimName falls back to the
// default ("sub"). Missing or empty values yield "".
//
// IMPORTANT (ADR-025 §3.1 fail-closed contract): this NEVER falls back to
// "sub" when an explicit, non-empty claimName was configured but is
// missing/empty in the claims — it returns "", and the caller must fail
// closed. Only the EMPTY-claimName case maps to "sub" (the legacy default).
func ExtractSubjectClaim(claims map[string]any, claimName string) string {
	if claimName == "" {
		claimName = DefaultSubjectClaim
	}
	return strings.TrimSpace(AsString(claims[claimName]))
}
