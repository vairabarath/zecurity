package policy

import "testing"

// Bug 4 regression: the per-connector relay address must be built in Go via
// net.JoinHostPort so IPv6 observed IPs are bracketed. The previous SQL path
// (`host(observed_ip) || ':9093'`) produced unparseable "::1:9093".
func TestResolveConnectorRelayAddr(t *testing.T) {
	cases := []struct {
		name         string
		publicAddr   string
		observedHost string
		want         string
	}{
		{"public addr wins as-is", "relay.example.com:9093", "10.0.0.1", "relay.example.com:9093"},
		{"ipv4 observed joined with port", "", "8.8.8.8", "8.8.8.8:9093"},
		{"ipv6 observed is bracketed", "", "2001:db8::1", "[2001:db8::1]:9093"},
		{"ipv6 loopback is bracketed", "", "::1", "[::1]:9093"},
		{"no relay coordinates", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveConnectorRelayAddr(tc.publicAddr, tc.observedHost)
			if got != tc.want {
				t.Fatalf("resolveConnectorRelayAddr(%q, %q) = %q, want %q",
					tc.publicAddr, tc.observedHost, got, tc.want)
			}
		})
	}
}
