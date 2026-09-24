package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/valkey-io/valkey-go/valkeycompat"
	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/pki"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Service struct {
	relaypb.UnimplementedRelayServiceServer

	pki       pki.Service
	store     heartbeatStore
	redis     valkeycompat.Cmdable
	jwtSecret string
	certTTL   time.Duration
	notifier  topologyChangeNotifier

	heartbeatDBWriteInterval time.Duration
	labelHoldDown            time.Duration
	onPoolChange             func(ctx context.Context)
}

func NewService(pkiSvc pki.Service, store heartbeatStore, certTTL time.Duration) *Service {
	return &Service{
		pki:                      pkiSvc,
		store:                    store,
		certTTL:                  certTTL,
		heartbeatDBWriteInterval: 5 * time.Minute,
		labelHoldDown:            60 * time.Second,
	}
}

// WithLabelHoldDown overrides RELAY_LABEL_HOLDDOWN_SECS (default 60s). A
// capacity-tier candidate must remain stable for this duration before it is
// promoted to capacity_label and pushed to connectors.
func (s *Service) WithLabelHoldDown(d time.Duration) *Service {
	if d > 0 {
		s.labelHoldDown = d
	}
	return s
}

// WithRelayPoolBroadcaster registers a callback the heartbeat handler invokes
// whenever the eligible relay pool changes (capacity-tier promotion, address
// change, or new active relay). The callback is responsible for building the
// current LabelledRelayList and fanning it out to connected connectors.
// Defined here as a callback so the relay package does not import the
// connector registry. Eviction broadcasts are wired separately via the same
// callback in main.go.
func (s *Service) WithRelayPoolBroadcaster(f func(ctx context.Context)) *Service {
	s.onPoolChange = f
	return s
}

func (s *Service) WithHeartbeatCache(redis valkeycompat.Cmdable, dbWriteInterval time.Duration) *Service {
	s.redis = redis
	if dbWriteInterval > 0 {
		s.heartbeatDBWriteInterval = dbWriteInterval
	}
	return s
}

func (s *Service) WithProvisioningAuth(jwtSecret string) *Service {
	s.jwtSecret = jwtSecret
	return s
}

// WithTransportNotifier wires the transport-plane notifier used to propagate
// relay topology changes (ADR-017). Relay metadata/eviction events call
// NotifyTopologyChange, never NotifyPolicyChange — a relay change must not
// recompile or bump the ACL snapshot (the Track B decoupling invariant).
func (s *Service) WithTransportNotifier(n topologyChangeNotifier) *Service {
	s.notifier = n
	return s
}

// Provision validates a single-use provisioning token and signs a
// Relay-generated CSR. The relay has no client certificate yet, so the
// provisioning token is its bootstrap credential.
//
// Every validation — token, relay row status, request SANs, and each check
// SignRelayCert would make on the CSR — runs BEFORE the token is burned, so a
// rejected request never consumes it. The certificate's DNS/IP SANs are bound
// to the operator-registered allowlists on the relay row (not to the SANs the
// relay asks for), and the signer receives those stored allowlists and
// enforces them independently.
func (s *Service) Provision(ctx context.Context, req *relaypb.ProvisionRequest) (*relaypb.ProvisionResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}

	relayID, err := canonicalRelayID(req.RelayId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	requestDNS, err := canonicalRequestDNSSANs(req.DnsSans)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	requestIPs, err := parseRequestIPSANs(req.IpSans)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	csr, err := x509.ParseCertificateRequest(req.CsrDer)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse Relay CSR: %v", err)
	}
	if req.ProvisioningToken == "" {
		return nil, status.Error(
			codes.Unauthenticated,
			"provisioning token required",
		)
	}

	if s.jwtSecret == "" || s.redis == nil || s.store == nil {
		return nil, status.Error(
			codes.FailedPrecondition,
			"relay provisioning authentication is not configured",
		)
	}

	claims, err := VerifyProvisioningToken(
		s.jwtSecret,
		req.ProvisioningToken,
	)
	if err != nil {
		return nil, status.Error(
			codes.PermissionDenied,
			"invalid provisioning token",
		)
	}
	if claims.RelayID != relayID {
		return nil, status.Error(
			codes.PermissionDenied,
			"token relay mismatch",
		)
	}

	// Operator-registered row: must exist and still be awaiting provisioning.
	// MarkProvisioned keeps its own status guard for a row revoked after this read.
	row, err := s.store.LoadRelayByID(ctx, relayID)
	if errors.Is(err, ErrRelayNotFound) {
		return nil, status.Error(codes.FailedPrecondition, "relay not registered")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "load Relay registration")
	}
	if row.Status != "pending" {
		return nil, status.Error(codes.FailedPrecondition, "relay is not awaiting provisioning")
	}

	allow, err := newRelaySANAllowlist(row.DNSAllowlist, row.IPAllowlist)
	if err != nil {
		log.Printf("relay provision: relay=%s stored SAN allowlist invalid: %v", relayID, err)
		return nil, status.Error(codes.FailedPrecondition, "relay SAN allowlist is invalid")
	}
	for _, name := range requestDNS {
		if !allow.allowsDNS(name) {
			log.Printf("relay provision: relay=%s requested DNS SAN %q not in allowlist", relayID, name)
			return nil, status.Error(codes.PermissionDenied, "requested SAN not in relay allowlist")
		}
	}
	for _, ip := range requestIPs {
		if !allow.allowsIP(ip) {
			log.Printf("relay provision: relay=%s requested IP SAN %s not in allowlist", relayID, ip)
			return nil, status.Error(codes.PermissionDenied, "requested SAN not in relay allowlist")
		}
	}
	if err := precheckRelayCSR(relayID, csr, allow); err != nil {
		log.Printf("relay provision: relay=%s CSR rejected before token burn: %v", relayID, err)
		return nil, err
	}

	// All validation passed — only now consume the single-use token.
	burnedRelayID, ok, err := BurnProvisioningJTI(
		ctx,
		s.redis,
		claims.ID,
	)

	if err != nil {
		return nil, status.Error(
			codes.Internal,
			"burn provisioning token",
		)
	}
	if !ok {
		return nil, status.Error(
			codes.PermissionDenied,
			"provisioning token already used or unknown",
		)
	}
	if burnedRelayID != relayID {
		return nil, status.Error(
			codes.PermissionDenied,
			"token relay mismatch",
		)
	}

	// The signer gets the STORED allowlists (never the relay-supplied SANs) and
	// re-validates the CSR against them independently.
	cert, err := s.pki.SignRelayCert(ctx, relayID, csr, allow.dnsNames, allow.ipAddresses, s.certTTL)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign Relay certificate: %v", err)
	}

	if err := s.store.MarkProvisioned(ctx, relayID, cert.Serial, cert.NotAfter, req.Version, req.Hostname); err != nil {
		if errors.Is(err, ErrRelayNotFound) {
			return nil, status.Error(
				codes.FailedPrecondition,
				"relay not registered",
			)
		}
		return nil, status.Error(
			codes.Internal,
			"record Relay provisioning",
		)
	}

	return &relaypb.ProvisionResponse{
		CertificatePem:    []byte(cert.CertificatePEM),
		IntermediateCaPem: []byte(cert.IntermediateCAPEM),
		RelayId:           relayID,
		SpiffeId:          appmeta.RelaySPIFFEID(relayID),
		CertNotAfterUnix:  cert.NotAfter.Unix(),
		CertNotBeforeUnix: cert.NotBefore.Unix(),
	}, nil
}

func canonicalRelayID(raw string) (string, error) {
	parsed, err := uuid.Parse(raw)
	if err != nil || parsed.String() != raw {
		return "", fmt.Errorf("Relay ID must be a canonical lowercase UUID")
	}
	return parsed.String(), nil
}

func validateDNSSANs(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || value != strings.ToLower(value) || strings.Contains(value, "*") || strings.ContainsAny(value, " /") {
			return nil, fmt.Errorf("invalid DNS SAN %q", value)
		}
		if _, exists := seen[value]; exists {
			return nil, fmt.Errorf("duplicate DNS SAN %q", value)
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result, nil
}

func validateIPSANs(values []string) ([]net.IP, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]net.IP, 0, len(values))
	for _, value := range values {
		ip := net.ParseIP(value)
		if ip == nil || ip.String() != value {
			return nil, fmt.Errorf("invalid or non-canonical IP SAN %q", value)
		}
		if _, exists := seen[value]; exists {
			return nil, fmt.Errorf("duplicate IP SAN %q", value)
		}
		seen[value] = struct{}{}
		result = append(result, ip)
	}
	return result, nil
}

// canonicalDNSName normalizes a DNS SAN for comparison: lowercase (DNS is
// case-insensitive) and at most one trailing root dot removed ("relay.example.com."
// is the same FQDN as "relay.example.com"). It keeps the shape rules of
// validateDNSSANs: no wildcards, no spaces or slashes, no empty labels at the end.
func canonicalDNSName(value string) (string, error) {
	name := strings.TrimSuffix(strings.ToLower(value), ".")
	if name == "" || strings.HasSuffix(name, ".") || strings.Contains(name, "*") || strings.ContainsAny(name, " /") {
		return "", fmt.Errorf("invalid DNS SAN %q", value)
	}
	return name, nil
}

// canonicalRequestDNSSANs normalizes the relay-requested DNS SANs and rejects
// duplicates after normalization.
func canonicalRequestDNSSANs(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		name, err := canonicalDNSName(value)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("duplicate DNS SAN %q", value)
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	return result, nil
}

// parseRequestIPSANs parses the relay-requested IP SANs into net.IP values.
// Any valid textual form is accepted ("2001:DB8:0:0::1" == "2001:db8::1");
// duplicates are detected on the parsed value.
func parseRequestIPSANs(values []string) ([]net.IP, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]net.IP, 0, len(values))
	for _, value := range values {
		ip := net.ParseIP(value)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP SAN %q", value)
		}
		key := ip.String()
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("duplicate IP SAN %q", value)
		}
		seen[key] = struct{}{}
		result = append(result, ip)
	}
	return result, nil
}

// relaySANAllowlist is the operator-registered SAN allowlist of one relay row,
// in the form handed to the signer: canonical DNS names and parsed IPs. An empty
// list forbids that SAN type entirely.
type relaySANAllowlist struct {
	dnsNames    []string
	dnsSet      map[string]struct{}
	ipAddresses []net.IP
}

func newRelaySANAllowlist(dnsAllowlist, ipAllowlist []string) (*relaySANAllowlist, error) {
	a := &relaySANAllowlist{dnsSet: make(map[string]struct{}, len(dnsAllowlist))}
	for _, value := range dnsAllowlist {
		name, err := canonicalDNSName(value)
		if err != nil {
			return nil, err
		}
		if _, exists := a.dnsSet[name]; exists {
			continue
		}
		a.dnsSet[name] = struct{}{}
		a.dnsNames = append(a.dnsNames, name)
	}
	for _, value := range ipAllowlist {
		ip := net.ParseIP(value)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP allowlist entry %q", value)
		}
		a.ipAddresses = append(a.ipAddresses, ip)
	}
	return a, nil
}

// allowsDNS reports whether a canonical DNS name is in the allowlist.
func (a *relaySANAllowlist) allowsDNS(canonical string) bool {
	_, ok := a.dnsSet[canonical]
	return ok
}

// allowsIP compares parsed IP values (net.IP.Equal), the same predicate the
// signer uses — never raw strings.
func (a *relaySANAllowlist) allowsIP(ip net.IP) bool {
	for _, allowed := range a.ipAddresses {
		if ip.Equal(allowed) {
			return true
		}
	}
	return false
}

// precheckRelayCSR mirrors every check pki.SignRelayCert performs on the CSR.
// The signer runs only after the provisioning token is burned, so without this
// a request the signer would reject would still consume the token. The signer
// keeps its own checks; this is the pre-burn copy.
//
// CSR DNS SANs must already be in canonical form: the signer copies CSR names
// verbatim into the certificate and matches them exactly against the
// (canonical) allowlist, so a mixed-case or trailing-dot name would pass a
// normalized comparison here and then fail at the signer after the burn.
func precheckRelayCSR(relayID string, csr *x509.CertificateRequest, allow *relaySANAllowlist) error {
	if err := csr.CheckSignature(); err != nil {
		return status.Errorf(codes.InvalidArgument, "Relay CSR signature invalid: %v", err)
	}
	expectedSPIFFE := appmeta.RelaySPIFFEID(relayID)
	if len(csr.URIs) != 1 || csr.URIs[0].String() != expectedSPIFFE {
		return status.Errorf(codes.InvalidArgument, "Relay CSR must carry exactly one URI SAN %q", expectedSPIFFE)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P384() {
		return status.Error(codes.InvalidArgument, "Relay CSR public key must be ECDSA P-384")
	}
	for _, name := range csr.DNSNames {
		canonical, err := canonicalDNSName(name)
		if err != nil {
			return status.Error(codes.InvalidArgument, err.Error())
		}
		if canonical != name {
			return status.Errorf(codes.InvalidArgument, "Relay CSR DNS SAN %q must be in canonical form %q", name, canonical)
		}
		if !allow.allowsDNS(canonical) {
			return status.Error(codes.PermissionDenied, "Relay CSR SAN not in relay allowlist")
		}
	}
	for _, ip := range csr.IPAddresses {
		if !allow.allowsIP(ip) {
			return status.Error(codes.PermissionDenied, "Relay CSR SAN not in relay allowlist")
		}
	}
	return nil
}

var _ relaypb.RelayServiceServer = (*Service)(nil)
