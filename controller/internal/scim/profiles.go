// Package scim contains the provider-agnostic SCIM engine for ADR-025:
// built-in provider profiles, Canonical Identity Key extraction, and the
// fail-closed mapping-validation gate. It imports no identity-mutation or
// outbox code — those are layered on in later phases (see MappingGate for the
// seam where Phase 5 plugs in the real round-trip).
package scim

// SCIMCapabilities describes what a provider's SCIM 2.0 endpoint supports.
// Used by the mapping-validation probe to choose the active lifecycle
// (POST→GET→verify→DELETE) versus a read-only fallback (ADR-025 §3.1).
type SCIMCapabilities struct {
	// CreateUser indicates the provider supports SCIM User creation.
	CreateUser bool
	// DeleteUser indicates the provider supports SCIM User deletion.
	DeleteUser bool
	// PatchUser indicates the provider supports SCIM PATCH.
	PatchUser bool
	// PaginationOK indicates the provider paginates list responses sanely.
	PaginationOK bool
}

// Profile is a built-in, data-only description of one IdP's SCIM behavior.
//
// It contains NO business logic and NO handler type — the engine reads these
// defaults and the per-connection overrides (subject_claim / scim_identifier
// columns) and runs the same code path for every provider. Generic SCIM 2.0
// is always available as the fallback for any unrecognized provider.
type Profile struct {
	// Key is the provider label (e.g. "okta", "entra").
	Key string
	// DisplayName is the human-readable provider name.
	DisplayName string
	// DefaultSubjectClaim is the OIDC claim this provider uses as the stable
	// login identifier (e.g. Entra → "oid"). Overridable per connection.
	DefaultSubjectClaim string
	// DefaultScimIdentifier is the SCIM attribute this provider uses as the
	// stable provisioning identifier (e.g. "externalId"). Overridable per
	// connection. It is NOT assumed equal to DefaultSubjectClaim.
	DefaultScimIdentifier string
	// Capabilities describes the provider's SCIM 2.0 feature support.
	Capabilities SCIMCapabilities
	// Quirks are known provider-specific behaviors the engine must accommodate
	// (e.g. attribute naming, tombstone handling). Data only.
	Quirks []string
}

// BuiltinProfiles maps a provider label to its built-in Profile.
//
// Unknown providers fall back to GenericSCIM (see ProfileFor). There are no
// per-provider handler types — every profile drives the same engine.
var BuiltinProfiles = map[string]Profile{
	"okta": {
		Key:                    "okta",
		DisplayName:            "Okta",
		DefaultSubjectClaim:    "sub",
		DefaultScimIdentifier:  "externalId",
		Capabilities:           SCIMCapabilities{CreateUser: true, DeleteUser: true, PatchUser: true, PaginationOK: true},
		Quirks:                 []string{"userName is email-formatted", "externalId may equal okta login"},
	},
	"entra": {
		Key:                    "entra",
		DisplayName:            "Microsoft Entra ID",
		DefaultSubjectClaim:    "oid",
		DefaultScimIdentifier:  "externalId",
		Capabilities:           SCIMCapabilities{CreateUser: true, DeleteUser: true, PatchUser: true, PaginationOK: true},
		Quirks:                 []string{"subject is oid not sub", "ImmutableId may be used as externalId"},
	},
	"jumpcloud": {
		Key:                    "jumpcloud",
		DisplayName:            "JumpCloud",
		DefaultSubjectClaim:    "sub",
		DefaultScimIdentifier:  "externalId",
		Capabilities:           SCIMCapabilities{CreateUser: true, DeleteUser: true, PatchUser: true, PaginationOK: true},
		Quirks:                 []string{},
	},
	"keycloak": {
		Key:                    "keycloak",
		DisplayName:            "Keycloak",
		DefaultSubjectClaim:    "sub",
		DefaultScimIdentifier:  "externalId",
		Capabilities:           SCIMCapabilities{CreateUser: true, DeleteUser: true, PatchUser: true, PaginationOK: true},
		Quirks:                 []string{"username is authoritative local id"},
	},
	"generic": {
		Key:                    "generic",
		DisplayName:            "Generic SCIM 2.0",
		DefaultSubjectClaim:    "sub",
		DefaultScimIdentifier:  "externalId",
		Capabilities:           SCIMCapabilities{CreateUser: true, DeleteUser: true, PatchUser: true, PaginationOK: true},
		Quirks:                 []string{"behavior depends on the concrete implementation"},
	},
}

// GenericProfileKey is the fallback provider label for any unrecognized IdP.
const GenericProfileKey = "generic"

// ProfileFor returns the built-in Profile for a provider label, falling back to
// the Generic SCIM 2.0 profile for unknown providers. It never returns nil.
func ProfileFor(provider string) Profile {
	if provider == "" {
		return BuiltinProfiles[GenericProfileKey]
	}
	if p, ok := BuiltinProfiles[provider]; ok {
		return p
	}
	return BuiltinProfiles[GenericProfileKey]
}

// SupportsProbeLifecycle reports whether the profile's capabilities allow the
// full active probe-user round-trip (POST→GET→verify→DELETE). If the provider
// cannot safely create and delete a probe user, only a read-only fallback is
// permissible (ADR-025 §3.1).
func (p Profile) SupportsProbeLifecycle() bool {
	return p.Capabilities.CreateUser && p.Capabilities.DeleteUser
}
