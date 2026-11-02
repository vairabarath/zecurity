// Package rbac holds the workspace admin-console role model (PENDING-19).
//
// Every console operation names a Capability (@requires in the GraphQL schema,
// RequireCapability on REST routes). One table maps each role to its
// capabilities; roles are never listed on fields. Authorization uses the
// caller's live role from users.role (Checker), never the JWT role claim,
// which is display-only.
//
// "Capability" is deliberately not "permission": ADR-025 permissions
// (workspace_permissions, e.g. identity.mapping.break_glass) are explicit
// per-user grants, and no role implies one, Admin included.
package rbac

import (
	"slices"

	"github.com/google/uuid"
)

// Workspace roles, as stored in users.role and workspace_members.role
// (CHECK widened in migration 047).
const (
	RoleAdmin    = "admin"
	RoleViewer   = "viewer"
	RoleDevOps   = "devops"
	RoleSecurity = "security"
	RoleHelpdesk = "helpdesk"
	RoleMember   = "member"
)

// Roles lists every valid role.
var Roles = []string{RoleAdmin, RoleViewer, RoleDevOps, RoleSecurity, RoleHelpdesk, RoleMember}

// Capability is a console operation class. Values equal the GraphQL
// `enum Capability` names.
type Capability string

const (
	ConsoleRead   Capability = "CONSOLE_READ"
	NetworkWrite  Capability = "NETWORK_WRITE"
	AccessWrite   Capability = "ACCESS_WRITE"
	PostureWrite  Capability = "POSTURE_WRITE"
	DeviceWrite   Capability = "DEVICE_WRITE"
	DeviceAdmin   Capability = "DEVICE_ADMIN"
	IdentityAdmin Capability = "IDENTITY_ADMIN"
	MembersAdmin  Capability = "MEMBERS_ADMIN"
)

// AllCapabilities lists every capability in a stable order.
var AllCapabilities = []Capability{
	ConsoleRead, NetworkWrite, AccessWrite, PostureWrite,
	DeviceWrite, DeviceAdmin, IdentityAdmin, MembersAdmin,
}

// table is the role → capability map (PENDING-19 §4). Member has none.
var table = map[string][]Capability{
	RoleAdmin:    AllCapabilities,
	RoleViewer:   {ConsoleRead},
	RoleDevOps:   {ConsoleRead, NetworkWrite},
	RoleSecurity: {ConsoleRead, AccessWrite, PostureWrite},
	RoleHelpdesk: {ConsoleRead, DeviceWrite},
	RoleMember:   {},
}

// ValidRole reports whether role is one of the six workspace roles.
func ValidRole(role string) bool {
	_, ok := table[role]
	return ok
}

// CapabilitiesFor returns a copy of role's capabilities; nil for an unknown role.
func CapabilitiesFor(role string) []Capability {
	caps, ok := table[role]
	if !ok {
		return nil
	}
	return append([]Capability(nil), caps...)
}

// Allows reports whether role holds capability c. Unknown roles hold nothing.
func Allows(role string, c Capability) bool {
	return slices.Contains(table[role], c)
}

// CanonicalID returns the canonical (lower-case, hyphenated) form of a user or
// workspace UUID. Postgres matches uuid input in any case and in {…}/urn
// forms, so every guard that compares ids as strings must compare canonical
// forms, or a re-spelled own id would pass a self check. ok is false for a
// string that is not a UUID.
func CanonicalID(id string) (string, bool) {
	u, err := uuid.Parse(id)
	if err != nil {
		return "", false
	}
	return u.String(), true
}
