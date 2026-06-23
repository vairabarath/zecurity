package resource

import "testing"

func aclStrPtr(s string) *string { return &s }
func aclIntPtr(i int) *int       { return &i }

// TestACLRelevantUpdate is the DB-free seam that decides whether an UpdateResource
// touched a field the ACL compiler actually reads (name, protocol, port_from).
// ACL-irrelevant fields (description, port_to, remote_network_id) must NOT count.
func TestACLRelevantUpdate(t *testing.T) {
	cases := []struct {
		name  string
		input UpdateInput
		want  bool
	}{
		{"name only", UpdateInput{Name: aclStrPtr("web")}, true},
		{"protocol only", UpdateInput{Protocol: aclStrPtr("tcp")}, true},
		{"port_from only", UpdateInput{PortFrom: aclIntPtr(443)}, true},
		{"description only", UpdateInput{Description: aclStrPtr("note")}, false},
		{"port_to only", UpdateInput{PortTo: aclIntPtr(9000)}, false},
		{"remote_network only", UpdateInput{RemoteNetworkID: aclStrPtr("rn-1")}, false},
		{"empty input", UpdateInput{}, false},
		{"irrelevant + relevant", UpdateInput{Description: aclStrPtr("note"), Protocol: aclStrPtr("udp")}, true},
		{"all irrelevant", UpdateInput{Description: aclStrPtr("n"), PortTo: aclIntPtr(1), RemoteNetworkID: aclStrPtr("rn")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ACLRelevantUpdate(tc.input); got != tc.want {
				t.Fatalf("ACLRelevantUpdate(%+v) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}
