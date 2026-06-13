package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"net"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// SignRelayCert signs a relay's CSR with the Platform Intermediate CA.
//
// Phase 2 / Sprint 10.1. The controller never sees the relay's private key —
// the relay host generates the keypair locally and submits only a CSR.
//
// The relay is a platform-level service, so it lives at the global trust
// domain (zecurity.in), not a per-workspace one.
//
// Validation, all fail-closed:
//  1. relayID is a canonical 8-4-4-4-12 lowercase-hex UUID (per the proto).
//  2. CSR self-signature verifies (proof-of-possession).
//  3. Exactly one URI SAN, equal to appmeta.RelaySPIFFEID(relayID).
//  4. Public key is ECDSA P-384 (matches the rest of the deployed PKI).
//  5. Every DNS/IP SAN in the CSR is present in the caller-supplied
//     allowlists. Caller can pass nil to forbid that SAN type entirely.
func (s *serviceImpl) SignRelayCert(
	ctx context.Context,
	relayID string,
	csr *x509.CertificateRequest,
	dnsNames []string,
	ipAddresses []net.IP,
	certTTL time.Duration,
) (*RelayCertResult, error) {
	if s.intermediateKey == nil {
		return nil, fmt.Errorf("intermediate CA not initialized")
	}
	expectedSPIFFE, err := validateRelayCSR(relayID, csr, dnsNames, ipAddresses)
	if err != nil {
		return nil, err
	}

	// 5. Build template.
	serial, err := newSerialNumber()
	if err != nil {
		return nil, fmt.Errorf("generate serial: %w", err)
	}
	spiffeURL, err := url.Parse(expectedSPIFFE)
	if err != nil {
		return nil, fmt.Errorf("parse relay SPIFFE URI: %w", err)
	}

	now := time.Now().UTC()
	notBefore := now.Add(-1 * time.Hour)
	notAfter := now.Add(certTTL)

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   appmeta.PKIRelayCNPrefix + relayID,
			Organization: []string{appmeta.PKIPlatformOrganization},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		URIs:                  []*url.URL{spiffeURL},
		DNSNames:              append([]string(nil), csr.DNSNames...),
		IPAddresses:           append([]net.IP(nil), csr.IPAddresses...),
	}

	certDER, err := x509.CreateCertificate(
		rand.Reader,
		template,
		s.intermediateKey.cert,
		csr.PublicKey,
		s.intermediateKey.privKey,
	)
	if err != nil {
		return nil, fmt.Errorf("sign relay cert: %w", err)
	}

	// 6. Load the Intermediate CA PEM so the relay can configure RELAY_CLIENT_CA.
	var intermediatePEM string
	if err := s.pool.QueryRow(ctx,
		`SELECT certificate_pem FROM ca_intermediate LIMIT 1`,
	).Scan(&intermediatePEM); err != nil {
		return nil, fmt.Errorf("load intermediate CA pem: %w", err)
	}

	return &RelayCertResult{
		CertificatePEM:    encodeCertToPEM(certDER),
		IntermediateCAPEM: intermediatePEM,
		Serial:            serial.Text(16),
		NotBefore:         notBefore,
		NotAfter:          notAfter,
	}, nil
}

func validateRelayCSR(relayID string, csr *x509.CertificateRequest, dnsNames []string, ipAddresses []net.IP) (string, error) {
	parsedRelayID, err := uuid.Parse(relayID)
	if err != nil || parsedRelayID.String() != relayID {
		return "", fmt.Errorf("invalid relay id %q: must be a canonical lowercase UUID", relayID)
	}
	if csr == nil {
		return "", fmt.Errorf("nil CSR")
	}
	if err := csr.CheckSignature(); err != nil {
		return "", fmt.Errorf("csr self-signature: %w", err)
	}

	expectedSPIFFE := appmeta.RelaySPIFFEID(relayID)
	if len(csr.URIs) != 1 || csr.URIs[0].String() != expectedSPIFFE {
		return "", fmt.Errorf("csr URI SAN: want exactly one %q, got %v", expectedSPIFFE, csr.URIs)
	}

	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P384() {
		return "", fmt.Errorf("csr public key: want ECDSA P-384")
	}

	dnsAllow := make(map[string]struct{}, len(dnsNames))
	for _, dnsName := range dnsNames {
		dnsAllow[dnsName] = struct{}{}
	}
	for _, dnsName := range csr.DNSNames {
		if _, ok := dnsAllow[dnsName]; !ok {
			return "", fmt.Errorf("csr DNS SAN %q not in allowlist", dnsName)
		}
	}

	for _, csrIP := range csr.IPAddresses {
		allowed := false
		for _, allowedIP := range ipAddresses {
			if csrIP.Equal(allowedIP) {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", fmt.Errorf("csr IP SAN %s not in allowlist", csrIP)
		}
	}

	return expectedSPIFFE, nil
}
