package discoverylimit

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ParseTrustedProxies parses DISCOVERY_TRUSTED_PROXY_CIDRS: CIDRs or single
// addresses separated by commas or whitespace. Empty means no trusted proxy:
// the connection address is authoritative. Any malformed entry is an error, so
// the controller refuses to start rather than trust the wrong hops.
func ParseTrustedProxies(s string) ([]netip.Prefix, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		entries := strings.Fields(part)
		if len(entries) == 0 {
			return nil, fmt.Errorf("trusted proxies: empty entry in %q", s)
		}
		for _, entry := range entries {
			if !strings.Contains(entry, "/") {
				a, err := netip.ParseAddr(entry)
				if err != nil {
					return nil, fmt.Errorf("trusted proxies: %q: %w", entry, err)
				}
				a = a.Unmap()
				out = append(out, netip.PrefixFrom(a, a.BitLen()))
				continue
			}
			p, err := netip.ParsePrefix(entry)
			if err != nil {
				return nil, fmt.Errorf("trusted proxies: %q: %w", entry, err)
			}
			if p != p.Masked() {
				return nil, fmt.Errorf("trusted proxies: %q has host bits set (did you mean %s?)", entry, p.Masked())
			}
			out = append(out, p)
		}
	}
	return out, nil
}

// IPResolver decides which client address a discovery request is charged to.
type IPResolver struct {
	trusted []netip.Prefix
}

func NewIPResolver(trusted []netip.Prefix) IPResolver { return IPResolver{trusted: trusted} }

func (r IPResolver) isTrusted(a netip.Addr) bool {
	for _, p := range r.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientAddr returns the connection's remote address. Only when that address
// is a configured trusted proxy is X-Forwarded-For consulted, read from the
// right: the first address that is not itself a trusted proxy is the client.
// Hops to the left of it were written by the client and are ignored, so a
// spoofed header can't pick the bucket. A malformed hop stops the walk at the
// proxy. X-Real-IP and Forwarded are never consulted. The reverse proxy must
// append to (or overwrite) X-Forwarded-For, never pass a client's value
// through as the right-most hop.
func (r IPResolver) ClientAddr(req *http.Request) netip.Addr {
	remote := parseHost(req.RemoteAddr)
	if !remote.IsValid() || len(r.trusted) == 0 || !r.isTrusted(remote) {
		return remote
	}
	var hops []string
	for _, v := range req.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(v, ",")...)
	}
	leftmost := remote
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			return remote
		}
		a = a.Unmap()
		if !r.isTrusted(a) {
			return a
		}
		leftmost = a
	}
	return leftmost
}

func parseHost(hostport string) netip.Addr {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap()
}

// ClientKey is the rate-limit bucket for a client address: an IPv4 address on
// its own, an IPv6 address by its /64 (one host usually holds a whole /64).
func ClientKey(a netip.Addr) string {
	if !a.IsValid() {
		return "unknown"
	}
	a = a.Unmap()
	if a.Is4() {
		return "4:" + a.String()
	}
	p, _ := a.Prefix(64)
	return "6:" + p.String()
}
