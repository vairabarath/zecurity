package discovery

import (
	"errors"
	"testing"
)

// O-7 (live run 2026-10-29): targets the connector never scans are refused
// up front; anything it would scan still passes.
func TestCheckScanTarget(t *testing.T) {
	for _, target := range []string{
		"127.0.0.1", "127.0.0.0/8", "127.1.2.0/24", "224.0.0.1", "239.255.255.250", "224.0.0.0/4",
		"0.0.0.0", "::1", "ff02::1", "ff00::/8", "::",
		"", "not-an-ip", "10.0.0.0/33", "10.0.0.1/", "localhost",
	} {
		if err := CheckScanTarget(target); !errors.Is(err, ErrInvalidScanTarget) {
			t.Errorf("CheckScanTarget(%q) = %v, want ErrInvalidScanTarget", target, err)
		}
	}
	for _, target := range []string{
		"192.168.1.44", "10.0.0.0/30", "10.0.0.0/8", "0.0.0.0/0", "100.64.0.1", "fd00::1", "2001:db8::/64", "::/0",
	} {
		if err := CheckScanTarget(target); err != nil {
			t.Errorf("CheckScanTarget(%q) = %v, want nil", target, err)
		}
	}
}

// F-5 (live run 2026-10-29): a service the shield couldn't name is named by
// protocol and port; the shield is named by its name, not its ID.
func TestPromotedResourceName(t *testing.T) {
	for _, c := range []struct{ service, want string }{
		{"SSH", "SSH on shield-nika"},
		{"", "tcp/8813 on shield-nika"},
		{"   ", "tcp/8813 on shield-nika"},
	} {
		if got := PromotedResourceName(c.service, "tcp", 8813, "shield-nika"); got != c.want {
			t.Errorf("PromotedResourceName(%q) = %q, want %q", c.service, got, c.want)
		}
	}
}
