package auditexport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// DeliveryTimeout bounds one delivery, end to end (P-5).
const DeliveryTimeout = 10 * time.Second

// ClientOptions are the dev-only escape hatches (P-6, S3). Both are honoured
// only with ENV=development; see OptionsFromEnv.
type ClientOptions struct {
	// AllowPrivate lets the exporter reach loopback / private addresses
	// (AUDIT_EXPORT_DEV_ALLOW_PRIVATE), for a lab receiver.
	AllowPrivate bool
	// ExtraRootsPEM is added to the system roots (AUDIT_EXPORT_DEV_CA_FILE).
	ExtraRootsPEM []byte
}

// OptionsFromEnv reads the dev overrides; outside ENV=development they are
// ignored with a warning (logf).
func OptionsFromEnv(logf func(string, ...any)) (ClientOptions, error) {
	var o ClientOptions
	allow := os.Getenv("AUDIT_EXPORT_DEV_ALLOW_PRIVATE") == "true"
	caFile := os.Getenv("AUDIT_EXPORT_DEV_CA_FILE")
	if (allow || caFile != "") && os.Getenv("ENV") != "development" {
		logf("audit export: AUDIT_EXPORT_DEV_ALLOW_PRIVATE / AUDIT_EXPORT_DEV_CA_FILE ignored: only honoured with ENV=development")
		return o, nil
	}
	o.AllowPrivate = allow
	if allow {
		logf("DEV: audit export may reach private and loopback addresses (AUDIT_EXPORT_DEV_ALLOW_PRIVATE=true)")
	}
	if caFile != "" {
		pemBytes, err := os.ReadFile(caFile)
		if err != nil {
			return o, fmt.Errorf("AUDIT_EXPORT_DEV_CA_FILE: %w", err)
		}
		if !x509.NewCertPool().AppendCertsFromPEM(pemBytes) {
			return o, errors.New("AUDIT_EXPORT_DEV_CA_FILE: no PEM certificates found")
		}
		o.ExtraRootsPEM = pemBytes
		logf("DEV: audit export trusts the extra CA bundle %s", caFile)
	}
	return o, nil
}

var errAddressBlocked = errors.New("destination address not allowed")

// blockedPrefixes are non-public ranges netip's predicates don't cover.
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network"
	netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, incl. broadcast
	netip.MustParsePrefix("64:ff9b::/96"),    // NAT64 (could embed a private IPv4)
	netip.MustParsePrefix("64:ff9b:1::/48"),  // local-use NAT64
	netip.MustParsePrefix("100::/64"),        // discard
	netip.MustParsePrefix("2001:db8::/32"),   // documentation
	netip.MustParsePrefix("2002::/16"),       // 6to4 (could embed a private IPv4)
}

// AddressAllowed reports whether the exporter may connect to ip: only public
// unicast addresses. IPv4-mapped IPv6 is checked as IPv4.
func AddressAllowed(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

// NewClient returns the exporter's HTTP client (P-6 SSRF guard):
//   - the dialled IP is checked in Dialer.Control, after DNS resolution, so a
//     name that re-resolves to a private address (DNS rebinding) is refused;
//   - no proxy (a proxy would dial on our behalf and skip the check);
//   - redirects are not followed (a 3xx is a failed delivery);
//   - TLS 1.2+, system roots (plus the dev bundle).
func NewClient(o ClientOptions) *http.Client {
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			if o.AllowPrivate {
				return nil
			}
			ap, err := netip.ParseAddrPort(address)
			if err != nil || !AddressAllowed(ap.Addr()) {
				return errAddressBlocked
			}
			return nil
		},
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(o.ExtraRootsPEM) > 0 {
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		roots.AppendCertsFromPEM(o.ExtraRootsPEM)
		tlsCfg.RootCAs = roots
	}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		TLSClientConfig:       tlsCfg,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: DeliveryTimeout,
		MaxIdleConns:          16,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       60 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{
		Transport: tr,
		Timeout:   DeliveryTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ValidateURL checks a sink URL: https, a host, no credentials or fragment,
// and (unless allowPrivate) no literal non-public IP. Hostnames are checked
// again at dial time.
func ValidateURL(raw string, allowPrivate bool) error {
	if len(raw) > 2048 {
		return errors.New("url is too long (max 2048 characters)")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("url is not valid")
	}
	if u.Scheme != "https" {
		return errors.New("url must use https")
	}
	if u.Host == "" || u.Hostname() == "" {
		return errors.New("url must have a host")
	}
	if u.User != nil {
		return errors.New("url must not contain credentials; use the auth header instead")
	}
	if u.Fragment != "" || u.Opaque != "" {
		return errors.New("url must not contain a fragment")
	}
	if ip, err := netip.ParseAddr(strings.Trim(u.Hostname(), "[]")); err == nil && !allowPrivate && !AddressAllowed(ip) {
		return errors.New("url points at a private, loopback or reserved address")
	}
	if !allowPrivate && strings.EqualFold(strings.TrimSuffix(u.Hostname(), "."), "localhost") {
		return errors.New("url points at a private, loopback or reserved address")
	}
	return nil
}

// RedactURL is the URL's scheme and host only, for audit details and logs:
// a path (e.g. /services/T…/B…/<secret>) or query string may carry a token,
// and audit rows are readable by every console role and exported.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

func describeTransportError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err // drop the URL
	}
	switch {
	case errors.Is(err, errAddressBlocked):
		return "destination address not allowed (private, loopback or reserved)"
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		return "delivery timed out"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "destination host name did not resolve"
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return "destination TLS certificate is not trusted"
	}
	var hostErr x509.HostnameError
	if errors.As(err, &hostErr) {
		return "destination TLS certificate does not match the host name"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "could not connect to the destination"
	}
	return "delivery failed"
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
