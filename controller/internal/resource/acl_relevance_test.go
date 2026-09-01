package resource

import "testing"

func aclStrPtr(s string) *string { return &s }
func aclIntPtr(i int) *int       { return &i }

// TestACLRelevantUpdate is the DB-free seam that decides whether an UpdateResource
// touched a field the ACL compiler actually reads. That set must match the fields
// CompileACLSnapshot emits into an ACLEntry: name, protocol, port_from, hostname,
// resolver, local_target. ACL-irrelevant fields (description, port_to,
// remote_network_id) must NOT count — counting them churns the snapshot version
// and triggers a pointless fan-out push to every connected client.
func TestACLRelevantUpdate(t *testing.T) {
	cases := []struct {
		name  string
		input UpdateInput
		want  bool
	}{
		{"name only", UpdateInput{Name: aclStrPtr("web")}, true},
		{"protocol only", UpdateInput{Protocol: aclStrPtr("tcp")}, true},
		{"port_from only", UpdateInput{PortFrom: aclIntPtr(443)}, true},
		// hostname and resolver reach the wire as of Phase 5, so each must
		// invalidate the snapshot on its own: hostname is what the connector
		// resolves, resolver is how.
		{"hostname only", UpdateInput{Hostname: aclStrPtr("db.internal")}, true},
		{"resolver only", UpdateInput{Resolver: aclStrPtr(`{"type":"dns"}`)}, true},
		// local_target is deliberately NOT ACL-relevant. Only the Shield dials
		// it, and Shields receive shield.v1.ResourceInstruction, never an
		// ACLEntry — so an edit has nothing to invalidate in the ACL plane.
		// If this ever flips to true, check that local_target has not been put
		// back into ACLEntry by mistake.
		{"local_target only", UpdateInput{LocalTarget: aclStrPtr("127.0.0.1")}, false},
		{"description only", UpdateInput{Description: aclStrPtr("note")}, false},
		{"port_to only", UpdateInput{PortTo: aclIntPtr(9000)}, false},
		// Was asserted false, which is what let the bug stand. The compiler reads
		// r.remote_network_id and emits it as ACLEntry.RemoteNetworkId — the routing
		// reference a client follows to find which connectors serve the resource — so
		// moving a resource between networks MUST bump the ACL version.
		{"remote_network only", UpdateInput{RemoteNetworkID: aclStrPtr("rn-1")}, true},
		{"empty input", UpdateInput{}, false},
		{"irrelevant + relevant", UpdateInput{Description: aclStrPtr("note"), Protocol: aclStrPtr("udp")}, true},
		{"all irrelevant", UpdateInput{Description: aclStrPtr("n"), PortTo: aclIntPtr(1), LocalTarget: aclStrPtr("127.0.0.1")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ACLRelevantUpdate(tc.input); got != tc.want {
				t.Fatalf("ACLRelevantUpdate(%+v) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}
