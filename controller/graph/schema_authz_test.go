package graph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

// publicAllowlist contains Query/Mutation fields that are intentionally
// unguarded by @hasRole. Two distinct reasons, both deliberate:
//
//   - Truly public: run before/around login, reachable by any caller.
//   - Authenticated-but-not-admin: reachable by any logged-in member; auth is
//     still enforced upstream by AuthMiddleware on the /graphql route, and the
//     resolver scopes results to the caller's own user/tenant. These are NOT
//     admin operations, so @hasRole([ADMIN]) would wrongly lock members out.
//
// Anything not here MUST carry @hasRole or the test fails (deny-by-default).
var publicAllowlist = map[string]bool{
	// --- Truly public (pre-login) ---
	"lookupWorkspace":         true, // workspace discovery during login/signup
	"lookupWorkspacesByEmail": true, // returning-user workspace picker
	"initiateAuth":            true, // starts the OAuth flow
	"invitation":              true, // invite acceptance, before an account exists

	// --- Authenticated, member-scoped (not admin) ---
	"me":        true, // self-profile
	"workspace": true, // the caller's own workspace (scoped WHERE id = tenant)
	"myDevices": true, // the caller's own devices (scoped WHERE user_id AND workspace_id)
}

// TestAllFieldsAuthorized asserts that every Query and Mutation field
// either carries @hasRole or is in the public allowlist.
//
// This turns the authz model from allow-by-default (a forgotten
// annotation silently opens a field) to deny-by-default (you must
// explicitly justify every public field).
func TestAllFieldsAuthorized(t *testing.T) {
	schemaFiles := []string{
		"schema.graphqls",
		"connector.graphqls",
		"shield.graphqls",
		"resource.graphqls",
		"discovery.graphqls",
		"client.graphqls",
		"policy.graphqls",
		"log.graphqls",
	}

	var source strings.Builder
	for _, file := range schemaFiles {
		data, err := os.ReadFile(filepath.Join("graph", file))
		if err != nil {
			// Try relative to test dir (go test runs from package dir)
			data, err = os.ReadFile(file)
			if err != nil {
				t.Fatalf("read schema %s: %v", file, err)
			}
		}
		source.Write(data)
		source.WriteByte('\n')
	}

	doc, gqlErr := parser.ParseSchema(&ast.Source{
		Input: source.String(),
	})
	if gqlErr != nil {
		t.Fatalf("parse schema: %v", gqlErr)
	}

	// Collect all fields from Query and Mutation types. The base types live in
	// schema.graphqls (parsed into doc.Definitions); every other file declares
	// `extend type Query`/`extend type Mutation`, which gqlparser parses into
	// doc.Extensions — NOT doc.Definitions. We MUST walk both, or the check
	// silently skips ~30 fields (generateConnectorToken et al.) and passes
	// vacuously.
	targetTypes := map[string]bool{"Query": true, "Mutation": true}

	defs := make([]*ast.Definition, 0, len(doc.Definitions)+len(doc.Extensions))
	defs = append(defs, doc.Definitions...)
	defs = append(defs, doc.Extensions...)

	checked := 0
	for _, def := range defs {
		if def.Kind != ast.Object || !targetTypes[def.Name] {
			continue
		}

		for _, field := range def.Fields {
			checked++
			if publicAllowlist[field.Name] {
				continue
			}

			// Check for @hasRole directive on the field.
			hasRole := false
			for _, dir := range field.Directives {
				if dir.Name == "hasRole" {
					hasRole = true
					break
				}
			}

			if !hasRole {
				t.Errorf(
					"%s.%s has no @hasRole directive and is not in publicAllowlist — "+
						"either add @hasRole(roles: [ADMIN]) or explicitly allowlist it",
					def.Name, field.Name,
				)
			}
		}
	}

	// Safety floor: guard against the test silently going vacuous (e.g. a schema
	// file gets renamed and dropped, or extends stop being walked). The schema has
	// ~35 Query/Mutation fields; if we ever inspect fewer than this, something is
	// wrong with collection itself, not with the annotations.
	const minExpectedFields = 20
	if checked < minExpectedFields {
		t.Fatalf(
			"authz check only inspected %d Query/Mutation fields (expected >= %d) — "+
				"schema collection is likely broken (extends not walked, or files missing); "+
				"a passing result here would be meaningless",
			checked, minExpectedFields,
		)
	}
}
