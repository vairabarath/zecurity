package scim

import (
	"fmt"
	"strings"

	"github.com/yourorg/ztna/controller/internal/auth/mapping"
)

// Canonical Identity Key extraction (ADR-025 §3.1).
//
// The login path (OIDC) and the provisioning path (SCIM) each read a different
// provider attribute and resolve it to ONE canonical key — the value stored in
// external_identities.subject. The invariant is that, for the same person,
// subjectClaim(OIDC) and scimIdentifier(SCIM) resolve to the SAME value. That
// correspondence is a per-connection configuration responsibility validated at
// connection setup (and by the Phase 5 round-trip), NOT an assumption baked
// into code. Never hardcode sub == externalId.

// DefaultSubjectClaim is the OIDC claim read at login when a connection does
// not override it.
const DefaultSubjectClaim = "sub"

// DefaultScimIdentifier is the SCIM attribute read at provisioning when a
// connection does not override it.
const DefaultScimIdentifier = "externalId"

// asString coerces an arbitrary claim/attribute value to a string. SCIM and
// OIDC attributes are JSON and may arrive as string, float64 (numbers), or
// bool; we normalize to a single string so the canonical key is stable.
func asString(v any) string {
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
// map using the configured claim name. It delegates to the shared
// auth/mapping.ExtractSubjectClaim so the OIDC login path and the SCIM
// provisioning path use one identical implementation (ADR-025 §3.1). An empty
// claimName falls back to the default ("sub"); a configured-but-missing claim
// yields "" (caller fails closed). See internal/auth/mapping.
func ExtractSubjectClaim(claims map[string]any, claimName string) string {
	return mapping.ExtractSubjectClaim(claims, claimName)
}

// ExtractScimIdentifier returns the Canonical Identity Key from a SCIM User
// resource map using the configured attribute name. An empty attrName falls
// back to the default ("externalId"). Missing or empty values yield "".
func ExtractScimIdentifier(resource map[string]any, attrName string) string {
	if attrName == "" {
		attrName = DefaultScimIdentifier
	}
	return strings.TrimSpace(asString(resource[attrName]))
}

// ValidateMappingConfig checks the per-connection mapping configuration for
// sanity BEFORE any identity is linked. It enforces the ADR-025 rules:
//
//   - subjectClaim and scimIdentifier must be non-empty (defaults are valid).
//   - Neither may be a hardcode that silently equates the two paths. We
//     specifically reject the degenerate case where an operator has set BOTH
//     extractors to the literal same token that would make every identity
//     collapse onto one key — e.g. configuring both to a constant — by
//     refusing a configuration that resolves to an empty key. The real
//     equivalence check (subjectClaim value == scimIdentifier value for the
//     same person) is proven only by the Phase 5 round-trip.
//
// It does NOT assume sub == externalId; it returns the canonical defaults when
// the connection left them unset.
func ValidateMappingConfig(subjectClaim, scimIdentifier string) error {
	if strings.TrimSpace(subjectClaim) == "" {
		subjectClaim = DefaultSubjectClaim
	}
	if strings.TrimSpace(scimIdentifier) == "" {
		scimIdentifier = DefaultScimIdentifier
	}
	if subjectClaim == scimIdentifier {
		return fmt.Errorf("mapping config invalid: subjectClaim and scimIdentifier must differ "+
			"(got %q for both); they resolve different identity paths and must not be identical", subjectClaim)
	}
	return nil
}

// ResolveMapping returns the effective (claim, identifier) pair for a
// connection, applying defaults for empty values. It is the single source of
// truth for what the engine reads, and never equates the two.
func ResolveMapping(subjectClaim, scimIdentifier string) (string, string) {
	if strings.TrimSpace(subjectClaim) == "" {
		subjectClaim = DefaultSubjectClaim
	}
	if strings.TrimSpace(scimIdentifier) == "" {
		scimIdentifier = DefaultScimIdentifier
	}
	return subjectClaim, scimIdentifier
}
