package graph

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"

	"github.com/yourorg/ztna/controller/internal/rbac"
)

// publicAllowlist contains Query/Mutation fields that are intentionally
// without @requires. Two distinct reasons, both deliberate:
//
//   - Truly public: run before/around login, reachable by any caller.
//   - Authenticated self-service: reachable by any logged-in user, Member
//     included; auth is still enforced upstream by AuthMiddleware on the
//     /graphql route, and the resolver scopes results to the caller's own
//     user/tenant. A capability here would lock Members out of their client.
//
// Anything not here MUST carry @requires or the test fails (deny-by-default).
var publicAllowlist = map[string]bool{
	// --- Truly public (pre-login) ---
	"lookupWorkspace":         true, // workspace discovery during login/signup
	"lookupWorkspacesByEmail": true, // returning-user workspace picker
	"lookupIdpConnections":    true, // login discovery (F7-5), public by design
	"initiateAuth":            true, // starts the OAuth flow
	"invitation":              true, // invite acceptance, before an account exists

	// --- Authenticated self-service (any role, Member included) ---
	"me":        true, // self-profile (+ capabilities of the live role)
	"workspace": true, // the caller's own workspace (scoped WHERE id = tenant)
	"myDevices": true, // the caller's own devices (scoped WHERE user_id AND workspace_id)
}

// expectedCapability pins the capability of every gated Query/Mutation field
// (PENDING-19 §4.1). A schema change that adds a field or moves one to another
// capability fails here until this table is updated on purpose.
var expectedCapability = func() map[string]rbac.Capability {
	m := map[string]rbac.Capability{}
	put := func(c rbac.Capability, names string) {
		for _, n := range strings.Fields(names) {
			m[n] = c
		}
	}
	put(rbac.ConsoleRead, `clientDevices remoteNetworks remoteNetwork connectors connector
		shields shield resources allResources getDiscoveredServices getScanResults groups group
		supportedPostureChecks deviceProfiles devicePostureVisibility idpConnections
		platformLoginEnabled scimTokens scimConflicts scimProviderProfiles connectorLogs users`)
	put(rbac.NetworkWrite, `createRemoteNetwork deleteRemoteNetwork generateConnectorToken
		revokeConnector deleteConnector generateShieldToken revokeShield deleteShield
		createResource updateResource protectResource unprotectResource deleteResource
		forceDeleteResource triggerScan promoteDiscoveredService`)
	put(rbac.AccessWrite, `createGroup updateGroup deleteGroup addGroupMember removeGroupMember
		assignResourceToGroup unassignResourceFromGroup`)
	put(rbac.PostureWrite, `createDeviceProfile updateDeviceProfileMode
		updateDeviceProfileManualTrust addProfileRequirement removeProfileRequirement
		deleteDeviceProfile bindResourceToProfile unbindResourceFromProfile`)
	put(rbac.DeviceWrite, `approveDevice unapproveDevice`)
	put(rbac.DeviceAdmin, `revokeDevice`)
	put(rbac.IdentityAdmin, `createIdpConnection updateIdpConnection setIdpConnectionStatus
		deleteIdpConnection testIdpConnection updateScimConfig setPlatformLoginEnabled
		mintScimToken rotateScimToken revokeScimToken acceptScimConflict rejectScimConflict
		reopenScimConflict grantPermission enableScimBreakGlass`)
	put(rbac.MembersAdmin, `createInvitation setMemberRole`)
	return m
}()

type opField struct {
	typeName string // Query | Mutation
	field    *ast.FieldDefinition
}

// loadSchema parses every *.graphqls file of the package. Globbing (not a
// hand-kept list) is the point: a hand-kept list once missed idp.graphqls and
// posture.graphqls, so ~26 operations were never checked.
func loadSchema(t *testing.T) *ast.SchemaDocument {
	t.Helper()
	files, err := filepath.Glob("*.graphqls")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob schema files: %v (found %d)", err, len(files))
	}
	sort.Strings(files)
	var source strings.Builder
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read schema %s: %v", file, err)
		}
		source.Write(data)
		source.WriteByte('\n')
	}
	doc, gqlErr := parser.ParseSchema(&ast.Source{Input: source.String()})
	if gqlErr != nil {
		t.Fatalf("parse schema: %v", gqlErr)
	}
	return doc
}

// operations returns every Query and Mutation field. Base types live in
// doc.Definitions; `extend type` blocks live in doc.Extensions. Both are walked.
func operations(doc *ast.SchemaDocument) []opField {
	var out []opField
	for _, defs := range [][]*ast.Definition{doc.Definitions, doc.Extensions} {
		for _, def := range defs {
			if def.Kind != ast.Object || (def.Name != "Query" && def.Name != "Mutation") {
				continue
			}
			for _, f := range def.Fields {
				out = append(out, opField{def.Name, f})
			}
		}
	}
	return out
}

// requiredCapability returns the @requires capability of a field, if any.
func requiredCapability(f *ast.FieldDefinition) (rbac.Capability, bool) {
	d := f.Directives.ForName("requires")
	if d == nil {
		return "", false
	}
	arg := d.Arguments.ForName("capability")
	if arg == nil || arg.Value == nil {
		return "", false
	}
	return rbac.Capability(arg.Value.Raw), true
}

// TestAllFieldsAuthorized: every Query/Mutation field either carries
// @requires with the capability pinned in expectedCapability, or is in the
// public allowlist (deny-by-default).
func TestAllFieldsAuthorized(t *testing.T) {
	doc := loadSchema(t)
	ops := operations(doc)

	seen := map[string]bool{}
	gated := 0
	for _, op := range ops {
		name := op.field.Name
		seen[name] = true
		if op.field.Directives.ForName("hasRole") != nil {
			t.Errorf("%s.%s uses the removed @hasRole directive; use @requires", op.typeName, name)
		}
		c, has := requiredCapability(op.field)
		if publicAllowlist[name] {
			if has {
				t.Errorf("%s.%s is allowlisted as public but carries @requires(%s); pick one", op.typeName, name, c)
			}
			continue
		}
		if !has {
			t.Errorf("%s.%s has no @requires directive and is not in publicAllowlist — "+
				"add @requires(capability: X) or explicitly allowlist it", op.typeName, name)
			continue
		}
		gated++
		want, ok := expectedCapability[name]
		if !ok {
			t.Errorf("%s.%s requires %s but is not in expectedCapability; add it on purpose", op.typeName, name, c)
			continue
		}
		if c != want {
			t.Errorf("%s.%s requires %s, expected %s (PENDING-19 §4.1)", op.typeName, name, c, want)
		}
	}
	for name := range expectedCapability {
		if !seen[name] {
			t.Errorf("expectedCapability lists %q, which is not in the schema", name)
		}
	}

	// 73 console operations + setMemberRole (PENDING-19 §4.1). A drop below
	// means schema collection broke and a pass would be meaningless.
	const wantGated = 74
	if gated != wantGated {
		t.Fatalf("authz check found %d gated operations, want %d — schema collection broken or "+
			"an operation was added/removed without updating expectedCapability", gated, wantGated)
	}
}

// TestCapabilityEnumMatchesRBAC: the GraphQL Capability enum and rbac's
// capabilities are the same set, so a schema value can't silently map to no
// role (deny) or a Go capability can't be unreachable.
func TestCapabilityEnumMatchesRBAC(t *testing.T) {
	var gql []string
	for _, v := range AllCapability {
		gql = append(gql, string(v))
	}
	var goCaps []string
	for _, c := range rbac.AllCapabilities {
		goCaps = append(goCaps, string(c))
	}
	sort.Strings(gql)
	sort.Strings(goCaps)
	if strings.Join(gql, ",") != strings.Join(goCaps, ",") {
		t.Fatalf("GraphQL Capability %v != rbac.AllCapabilities %v", gql, goCaps)
	}
	var roles []string
	for _, r := range AllRole {
		roles = append(roles, strings.ToLower(string(r)))
	}
	sort.Strings(roles)
	want := append([]string(nil), rbac.Roles...)
	sort.Strings(want)
	if strings.Join(roles, ",") != strings.Join(want, ",") {
		t.Fatalf("GraphQL Role %v != rbac.Roles %v", roles, want)
	}
}

// TestRoleOperationMatrix: role × operation for every gated field, generated
// from the capability table and checked against the role definitions in
// PENDING-19 §3, written out independently here.
func TestRoleOperationMatrix(t *testing.T) {
	doc := loadSchema(t)
	isQuery := map[string]bool{}
	for _, op := range operations(doc) {
		isQuery[op.field.Name] = op.typeName == "Query"
	}
	// What each role may do, stated from §3 rather than from rbac's table.
	allowed := func(role, op string, c rbac.Capability) bool {
		switch role {
		case rbac.RoleAdmin:
			return true
		case rbac.RoleViewer:
			return isQuery[op]
		case rbac.RoleDevOps:
			return isQuery[op] || c == rbac.NetworkWrite
		case rbac.RoleSecurity:
			return isQuery[op] || c == rbac.AccessWrite || c == rbac.PostureWrite
		case rbac.RoleHelpdesk:
			return isQuery[op] || op == "approveDevice" || op == "unapproveDevice"
		case rbac.RoleMember:
			return false
		}
		return false
	}
	for op, c := range expectedCapability {
		for _, role := range rbac.Roles {
			got := rbac.Allows(role, c)
			if want := allowed(role, op, c); got != want {
				t.Errorf("role %s on %s (%s): allowed=%v, want %v", role, op, c, got, want)
			}
		}
	}
	// Spot checks the design note calls out by name.
	for _, tc := range []struct {
		role string
		c    rbac.Capability
		want bool
	}{
		{rbac.RoleHelpdesk, rbac.DeviceAdmin, false},  // R-8: revokeDevice is Admin-only
		{rbac.RoleDevOps, rbac.NetworkWrite, true},    // R-4: install tokens
		{rbac.RoleSecurity, rbac.MembersAdmin, false}, // R-10: invites Admin-only
		{rbac.RoleViewer, rbac.ConsoleRead, true},
		{rbac.RoleMember, rbac.ConsoleRead, false},
		{"", rbac.ConsoleRead, false},
		{"owner", rbac.ConsoleRead, false},
	} {
		if got := rbac.Allows(tc.role, tc.c); got != tc.want {
			t.Errorf("Allows(%q, %s) = %v, want %v", tc.role, tc.c, got, tc.want)
		}
	}
}

// secretFieldName matches field names that would carry a secret.
var secretFieldName = regexp.MustCompile(`(?i)(secret|password|privatekey|private_key|token$|tokenvalue|plaintext|^key$|pem$)`)

// readSafeFields are names matching secretFieldName that are metadata, not
// secrets. Each entry is a deliberate decision.
var readSafeFields = map[string]bool{
	"ScimProviderProfile.key": true, // provider name: "okta" | "entra" | …
}

// TestConsoleReadReturnsNoSecrets (PENDING-19 §6): no type reachable from a
// CONSOLE_READ query has a field whose name says it carries a secret. Viewer
// can read every query, so a secret on a read path would leak to Viewer.
func TestConsoleReadReturnsNoSecrets(t *testing.T) {
	doc := loadSchema(t)
	types := map[string]*ast.Definition{}
	for _, defs := range [][]*ast.Definition{doc.Definitions, doc.Extensions} {
		for _, d := range defs {
			if prev, ok := types[d.Name]; ok {
				merged := *prev
				merged.Fields = append(append(ast.FieldList{}, prev.Fields...), d.Fields...)
				types[d.Name] = &merged
			} else {
				types[d.Name] = d
			}
		}
	}
	visited := map[string]bool{}
	var walk func(typeName, path string)
	walk = func(typeName, path string) {
		def, ok := types[typeName]
		if !ok || visited[typeName] || (def.Kind != ast.Object && def.Kind != ast.Interface && def.Kind != ast.Union) {
			return
		}
		visited[typeName] = true
		for _, f := range def.Fields {
			if secretFieldName.MatchString(f.Name) && !readSafeFields[typeName+"."+f.Name] {
				t.Errorf("%s.%s (via %s) looks like a secret on a CONSOLE_READ path", typeName, f.Name, path)
			}
			walk(f.Type.Name(), path+"."+f.Name)
		}
		for _, member := range def.Types {
			walk(member, path)
		}
	}
	for _, op := range operations(doc) {
		if c, ok := requiredCapability(op.field); ok && c == rbac.ConsoleRead {
			walk(op.field.Type.Name(), op.field.Name)
		}
	}
	if len(visited) == 0 {
		t.Fatal("no types reached from CONSOLE_READ queries; walk is broken")
	}
}
