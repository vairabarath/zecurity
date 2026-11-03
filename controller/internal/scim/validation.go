package scim

import (
	"context"
	"fmt"
)

// MappingState is the result of mapping validation for a connection. It uses
// the ADR-025 terminology exactly:
//
//   - "proven"   — the OIDC subjectClaim and SCIM scimIdentifier were proven to
//     resolve to the same logical user (the Phase 5 active probe-user
//     round-trip POST→GET→verify→DELETE succeeded, or an equivalent check).
//   - "unproven" — the mapping could not be proven. SCIM MUST stay disabled.
//
// There is intentionally no third "broken"/"failed" state here — an
// unverifiable mapping is simply "unproven" and fail-closed.
type MappingState string

const (
	// MappingProven means the mapping equivalence was actively verified.
	MappingProven MappingState = "proven"
	// MappingUnproven means the mapping could not be verified; SCIM disabled.
	MappingUnproven MappingState = "unproven"
)

// MappingGateResult is the outcome of evaluating mapping validation for a
// connection. It is the single contract the GraphQL layer consumes; Phase 5
// fills RoundTripVerified without changing this shape.
type MappingGateResult struct {
	// MappingState is "proven" or "unproven" (ADR-025 §3.1 terminology).
	MappingState MappingState
	// ScimEnabledAllowed is true only when the mapping is proven OR an explicit
	// identity.mapping.break_glass override is in effect. Fail-closed: false
	// when unproven and no override.
	ScimEnabledAllowed bool
	// Reason explains why SCIM is or is not allowed. Always populated.
	Reason string
	// RoundTripVerified is true only when the real POST→GET→verify→DELETE
	// probe actually ran and matched. Phase 4 cannot perform this (the /Users
	// endpoint lands in Phase 5), so it is always false here; Phase 5 sets it
	// via WithRoundTrip, and the gate recomputes the result. This is the seam.
	RoundTripVerified bool
}

// MappingGate evaluates mapping validation for a connection in a fail-closed
// manner. It NEVER claims the mapping is proven without a real round-trip:
// by default RoundTripVerified is false, so ScimEnabledAllowed is false.
type MappingGate struct {
	// Profile supplies provider defaults/capabilities. Resolved via ProfileFor
	// by the caller (or NewMappingGate).
	profile Profile
}

// NewMappingGate builds a gate for a provider. Unknown providers fall back to
// the Generic SCIM 2.0 profile.
func NewMappingGate(provider string) *MappingGate {
	return &MappingGate{profile: ProfileFor(provider)}
}

// Evaluate computes the fail-closed mapping-validation result for a connection.
//
//   - It always starts from the unproven state.
//   - It validates the per-connection mapping config (subjectClaim /
//     scimIdentifier). A malformed config yields unproven + a descriptive
//     reason but NEVER "proven".
//   - It does NOT consult the break_glass permission itself — that decision is
//     made by the caller (the resolver), which knows the actor. If the caller
//     has determined a break_glass override applies, pass breakGlass=true and a
//     reason; the gate will then allow SCIM despite the unproven mapping, while
//     keeping MappingState == "unproven" (the override does not retroactively
//     "prove" the mapping).
//
// The only way MappingState becomes "proven" is via WithRoundTrip (Phase 5),
// which requires RoundTripVerified == true.
func (g *MappingGate) Evaluate(ctx context.Context, subjectClaim, scimIdentifier string, override BreakGlassOverride) (MappingGateResult, error) {
	// Phase 4: the active round-trip cannot run (no /Users endpoint yet), so
	// RoundTripVerified is false. Phase 5 will call WithRoundTrip to attach a
	// verified result to this same gate.
	result := MappingGateResult{
		MappingState:       MappingUnproven,
		RoundTripVerified:  false,
		ScimEnabledAllowed: false,
	}

	// Config sanity first. A bad config is never "proven" and never silently
	// enabled.
	if err := ValidateMappingConfig(subjectClaim, scimIdentifier); err != nil {
		result.Reason = fmt.Sprintf("mapping config invalid: %s", err.Error())
		return result, nil
	}

	if override.Applied && override.Reason != "" {
		// Fail-closed override: SCIM may be enabled DESPITE the unproven
		// mapping, but the mapping remains "unproven" — the break-glass path
		// explicitly permits enabling without verification (ADR-025 §3.2). A
		// reason is mandatory; an override without one is NOT honored.
		result.ScimEnabledAllowed = true
		result.Reason = overrideReason(override)
		return result, nil
	}

	result.Reason = "mapping unproven: SCIM disabled until the identity mapping is verified (run the probe or apply identity.mapping.break_glass)"
	return result, nil
}

// BreakGlassOverride carries the result of the Phase 3 permission check. The
// resolver populates this only after confirming the actor holds
// identity.mapping.break_glass (ADR-025 §3.2) with a mandatory reason.
type BreakGlassOverride struct {
	// Applied is true only when the dedicated permission was held AND a
	// non-empty reason was supplied.
	Applied bool
	// Actor is the user who exercised the override (for audit correlation).
	Actor string
	// Reason is the mandatory free-text justification (never empty when Applied).
	Reason string
	// Workspace and Connection identify the scope of the override.
	Workspace   string
	Connection  string
}

func overrideReason(o BreakGlassOverride) string {
	if o.Reason == "" {
		return "SCIM enabled via identity.mapping.break_glass override"
	}
	return "SCIM enabled via identity.mapping.break_glass override: " + o.Reason
}

// WithRoundTrip returns a new result that reflects a SUCCESSFUL active probe:
// the mapping is "proven" and SCIM is allowed. This is the Phase 5 seam — when
// the /Users round-trip (POST→GET→verify→DELETE) actually runs and matches,
// the caller constructs the gate, attaches this, and obtains a proven result
// without any change to the Phase 4 contract.
//
// If verified is false, this returns the unchanged (unproven, disabled) result.
func (r MappingGateResult) WithRoundTrip(verified bool, subjectClaim, scimIdentifier string) MappingGateResult {
	if !verified {
		return r
	}
	return MappingGateResult{
		MappingState:       MappingProven,
		ScimEnabledAllowed: true,
		RoundTripVerified:  true,
		Reason: "mapping proven: OIDC subjectClaim and SCIM scimIdentifier resolve to the same " +
			"Canonical Identity Key for the probe user (no live OIDC login performed)",
	}
}
