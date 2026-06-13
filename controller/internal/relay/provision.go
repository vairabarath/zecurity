package relay

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/pki"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Service struct {
	relaypb.UnimplementedRelayServiceServer

	pki     pki.Service
	certTTL time.Duration
}

func NewService(pkiSvc pki.Service, certTTL time.Duration) *Service {
	return &Service{pki: pkiSvc, certTTL: certTTL}
}

// Provision validates and signs a Relay-generated CSR.
//
// Provisioning currently uses server-authenticated TLS. ProvisioningToken is
// reserved for a future authenticated operator flow and is ignored.
func (s *Service) Provision(ctx context.Context, req *relaypb.ProvisionRequest) (*relaypb.ProvisionResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}

	relayID, err := canonicalRelayID(req.RelayId)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	dnsSANs, err := validateDNSSANs(req.DnsSans)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	ipSANs, err := validateIPSANs(req.IpSans)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	csr, err := x509.ParseCertificateRequest(req.CsrDer)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse Relay CSR: %v", err)
	}

	cert, err := s.pki.SignRelayCert(ctx, relayID, csr, dnsSANs, ipSANs, s.certTTL)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "sign Relay certificate: %v", err)
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

var _ relaypb.RelayServiceServer = (*Service)(nil)
