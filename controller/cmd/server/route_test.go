package main

import "testing"

// TestRequestSelectsOnlyPublicFields exercises the server-owned public-routing
// predicate. The contract: a request is public ONLY if it is a single
// query/mutation whose every root field is in publicRootFields. Everything else
// is fail-closed to protected (false).
func TestRequestSelectsOnlyPublicFields(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		// --- public: the three pre-login root fields ---
		{"named public query", `{"query":"query LookupWorkspace { lookupWorkspace(slug:\"acme\") { id } }"}`, true},
		{"unnamed public query", `{"query":"{ lookupWorkspace(slug:\"acme\") { id } }"}`, true},
		{"public mutation initiateAuth", `{"query":"mutation { initiateAuth(provider:\"google\") { redirectUrl } }"}`, true},
		{"public lookupWorkspacesByEmail", `{"query":"query { lookupWorkspacesByEmail(email:\"a@b.com\") { workspaces { id } } }"}`, true},

		// --- protected: ordinary protected operation ---
		{"protected mutation", `{"query":"mutation { generateConnectorToken(remoteNetworkId:\"1\", connectorName:\"c\") { connectorId } }"}`, false},
		{"authenticated workspace field", `{"query":"{ workspace { id } }"}`, false},
		{"authenticated me field", `{"query":"{ me { id } }"}`, false},

		// --- anti-spoof: public OPERATION NAME wrapping a protected field ---
		{"name spoof", `{"query":"query LookupWorkspace { generateConnectorToken(remoteNetworkId:\"1\", connectorName:\"c\") { connectorId } }"}`, false},

		// --- anti-smuggle: public field beside a protected field ---
		{"smuggle public+protected", `{"query":"{ lookupWorkspace(slug:\"acme\") { id } me { id } }"}`, false},

		// --- alias cannot disguise a protected field as public ---
		{"aliased protected field", `{"query":"{ lookupWorkspace: generateConnectorToken(remoteNetworkId:\"1\", connectorName:\"c\") { connectorId } }"}`, false},

		// --- structural fail-closed cases ---
		{"multiple operations", `{"query":"query A { lookupWorkspace(slug:\"a\") { id } } query B { me { id } }", "operationName":"A"}`, false},
		{"subscription", `{"query":"subscription { lookupWorkspace { id } }"}`, false},
		{"introspection root", `{"query":"{ __schema { types { name } } }"}`, false},
		{"empty query (APQ hash-only)", `{"extensions":{"persistedQuery":{"version":1,"sha256Hash":"abc"}}}`, false},
		{"blank query string", `{"query":"   "}`, false},
		{"batch array body", `[{"query":"{ lookupWorkspace(slug:\"a\") { id } }"}]`, false},
		{"malformed json", `{"query":`, false},
		{"unparseable graphql", `{"query":"{ this is not valid"}`, false},
		{"empty selection set", `{"query":"query {}"}`, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestSelectsOnlyPublicFields([]byte(tc.body)); got != tc.want {
				t.Errorf("requestSelectsOnlyPublicFields() = %v, want %v\nbody: %s", got, tc.want, tc.body)
			}
		})
	}
}
