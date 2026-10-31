package discoverylimit

import (
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestParseTrustedProxies(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"", 0, false},
		{"   ", 0, false},
		{"10.0.0.0/8", 1, false},
		{"10.0.0.0/8, 192.168.1.5", 2, false},              // a bare address is a /32
		{"10.0.0.0/8 2001:db8::/32\t172.16.0.1", 3, false}, // space and tab separated
		{"10.0.0.0/33", 0, true},
		{"not-an-ip", 0, true},
		{"10.0.0.0/8,,192.168.0.0/16", 0, true}, // an empty entry is a typo, not "nothing"
		{"10.0.0.1/8", 0, true},                 // host bits set: ambiguous, refused
	} {
		got, err := ParseTrustedProxies(tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%q: err=%v, wantErr=%v", tc.in, err, tc.wantErr)
		}
		if !tc.wantErr && len(got) != tc.want {
			t.Fatalf("%q: %d prefixes, want %d", tc.in, len(got), tc.want)
		}
	}
}

func clientAddr(t *testing.T, r IPResolver, remote string, xff ...string) string {
	t.Helper()
	req := httptest.NewRequest("GET", "/x", nil)
	req.RemoteAddr = remote
	for _, v := range xff {
		req.Header.Add("X-Forwarded-For", v)
	}
	req.Header.Set("X-Real-IP", "6.6.6.6") // never consulted
	return r.ClientAddr(req).String()
}

func TestClientAddr_NoTrustedProxies_ConnectionAddressWins(t *testing.T) {
	r := NewIPResolver(nil)
	if got := clientAddr(t, r, "203.0.113.7:5555", "1.2.3.4"); got != "203.0.113.7" {
		t.Fatalf("got %s, want the connection address (X-Forwarded-For ignored)", got)
	}
	if got := clientAddr(t, r, "[::ffff:203.0.113.7]:5555"); got != "203.0.113.7" {
		t.Fatalf("IPv4-mapped address: got %s", got)
	}
}

func TestClientAddr_TrustedProxy(t *testing.T) {
	trusted, err := ParseTrustedProxies("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	r := NewIPResolver(trusted)
	for _, tc := range []struct {
		name   string
		remote string
		xff    []string
		want   string
	}{
		{"proxy appends the client", "10.0.0.1:443", []string{"198.51.100.9"}, "198.51.100.9"},
		{"client spoofs a header, proxy appends the real address", "10.0.0.1:443", []string{"9.9.9.9, 198.51.100.9"}, "198.51.100.9"},
		{"two trusted hops", "10.0.0.1:443", []string{"198.51.100.9, 10.0.0.2"}, "198.51.100.9"},
		{"header split over several lines", "10.0.0.1:443", []string{"9.9.9.9", "198.51.100.9"}, "198.51.100.9"},
		{"untrusted peer: its header is ignored", "203.0.113.7:443", []string{"198.51.100.9"}, "203.0.113.7"},
		{"trusted peer without a header", "10.0.0.1:443", nil, "10.0.0.1"},
		{"malformed right-most entry: fall back to the proxy", "10.0.0.1:443", []string{"198.51.100.9, garbage"}, "10.0.0.1"},
		{"every hop trusted: the left-most one", "10.0.0.1:443", []string{"10.0.0.3, 10.0.0.2"}, "10.0.0.3"},
		{"IPv6 client behind the proxy", "10.0.0.1:443", []string{"2001:db8:1:2::5"}, "2001:db8:1:2::5"},
	} {
		if got := clientAddr(t, r, tc.remote, tc.xff...); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
	}
}

func TestClientKey_Grouping(t *testing.T) {
	k := func(s string) string { return ClientKey(netip.MustParseAddr(s)) }
	if k("203.0.113.7") == k("203.0.113.8") {
		t.Fatal("two IPv4 addresses must have their own buckets")
	}
	if k("2001:db8:1:2::5") != k("2001:db8:1:2:ffff:ffff:ffff:ffff") {
		t.Fatal("addresses in one IPv6 /64 must share a bucket")
	}
	if k("2001:db8:1:2::5") == k("2001:db8:1:3::5") {
		t.Fatal("different IPv6 /64s must have their own buckets")
	}
	if k("::ffff:203.0.113.7") != k("203.0.113.7") {
		t.Fatal("an IPv4-mapped address is the IPv4 address")
	}
	if got := ClientKey(netip.Addr{}); got != "unknown" {
		t.Fatalf("invalid address key = %q", got)
	}
}
