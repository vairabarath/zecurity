package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"net"
	"net/url"
	"strings"
	"testing"

	"github.com/yourorg/ztna/controller/internal/appmeta"
)

const testRelayID = "550e8400-e29b-41d4-a716-446655440000"

func TestValidateRelayCSR(t *testing.T) {
	dnsName := "relay.example.com"
	ipAddress := net.ParseIP("203.0.113.10")
	validCSR := makeRelayCSR(t, elliptic.P384(), testRelayID, []string{dnsName}, []net.IP{ipAddress})

	if _, err := validateRelayCSR(testRelayID, validCSR, []string{dnsName}, []net.IP{ipAddress}); err != nil {
		t.Fatalf("valid Relay CSR rejected: %v", err)
	}

	tests := []struct {
		name      string
		relayID   string
		csr       *x509.CertificateRequest
		dnsNames  []string
		ipAddrs   []net.IP
		wantError string
	}{
		{
			name:      "uppercase UUID",
			relayID:   strings.ToUpper(testRelayID),
			csr:       validCSR,
			dnsNames:  []string{dnsName},
			ipAddrs:   []net.IP{ipAddress},
			wantError: "canonical lowercase UUID",
		},
		{
			name:      "wrong SPIFFE identity",
			relayID:   testRelayID,
			csr:       makeRelayCSR(t, elliptic.P384(), "11111111-1111-1111-1111-111111111111", nil, nil),
			wantError: "csr URI SAN",
		},
		{
			name:      "P-256 key",
			relayID:   testRelayID,
			csr:       makeRelayCSR(t, elliptic.P256(), testRelayID, nil, nil),
			wantError: "ECDSA P-384",
		},
		{
			name:      "DNS SAN not allowed",
			relayID:   testRelayID,
			csr:       validCSR,
			ipAddrs:   []net.IP{ipAddress},
			wantError: "DNS SAN",
		},
		{
			name:      "IP SAN not allowed",
			relayID:   testRelayID,
			csr:       validCSR,
			dnsNames:  []string{dnsName},
			wantError: "IP SAN",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := validateRelayCSR(tt.relayID, tt.csr, tt.dnsNames, tt.ipAddrs)
			if err == nil || !strings.Contains(err.Error(), tt.wantError) {
				t.Fatalf("expected error containing %q, got %v", tt.wantError, err)
			}
		})
	}
}

func makeRelayCSR(t *testing.T, curve elliptic.Curve, relayID string, dnsNames []string, ipAddresses []net.IP) *x509.CertificateRequest {
	t.Helper()

	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatalf("generate CSR key: %v", err)
	}
	spiffeURI, err := url.Parse(appmeta.RelaySPIFFEID(relayID))
	if err != nil {
		t.Fatalf("parse Relay SPIFFE URI: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		URIs:        []*url.URL{spiffeURI},
		DNSNames:    dnsNames,
		IPAddresses: ipAddresses,
	}, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		t.Fatalf("parse CSR: %v", err)
	}
	return csr
}
