package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"net"
	"strings"
	"testing"
	"time"

	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/pki"
)

const testRelayID = "550e8400-e29b-41d4-a716-446655440000"

func TestCanonicalRelayID(t *testing.T) {
	if got, err := canonicalRelayID(testRelayID); err != nil || got != testRelayID {
		t.Fatalf("canonical Relay ID rejected: got=%q err=%v", got, err)
	}
	if _, err := canonicalRelayID(strings.ToUpper(testRelayID)); err == nil {
		t.Fatal("uppercase Relay ID accepted")
	}
}

func TestValidateDNSSANs(t *testing.T) {
	if _, err := validateDNSSANs([]string{"relay.example.com"}); err != nil {
		t.Fatalf("valid DNS SAN rejected: %v", err)
	}
	for _, invalid := range [][]string{
		{""},
		{"Relay.example.com"},
		{"*.example.com"},
		{"relay.example.com", "relay.example.com"},
	} {
		if _, err := validateDNSSANs(invalid); err == nil {
			t.Fatalf("invalid DNS SANs accepted: %v", invalid)
		}
	}
}

func TestProvisionCurrentContract(t *testing.T) {
	now := time.Now().UTC()
	fake := &fakePKI{
		result: &pki.RelayCertResult{
			CertificatePEM:    "relay-cert",
			IntermediateCAPEM: "intermediate-cert",
			NotBefore:         now,
			NotAfter:          now.Add(time.Hour),
		},
	}
	service := NewService(fake, time.Hour)

	response, err := service.Provision(context.Background(), &relaypb.ProvisionRequest{
		ProvisioningToken: "ignored-until-future-auth",
		RelayId:           testRelayID,
		CsrDer:            makeCSRDER(t),
		DnsSans:           []string{"relay.example.com"},
		IpSans:            []string{"203.0.113.10"},
	})
	if err != nil {
		t.Fatalf("Provision rejected valid request: %v", err)
	}
	if fake.relayID != testRelayID || fake.dnsSAN != "relay.example.com" || fake.ipSAN != "203.0.113.10" {
		t.Fatalf("unexpected PKI request: relay=%q dns=%q ip=%q", fake.relayID, fake.dnsSAN, fake.ipSAN)
	}
	if response.RelayId != testRelayID || response.SpiffeId != appmeta.RelaySPIFFEID(testRelayID) {
		t.Fatalf("unexpected Provision response identity: %+v", response)
	}
}

type fakePKI struct {
	pki.Service
	result  *pki.RelayCertResult
	relayID string
	dnsSAN  string
	ipSAN   string
}

func (f *fakePKI) SignRelayCert(
	_ context.Context,
	relayID string,
	_ *x509.CertificateRequest,
	dnsNames []string,
	ipAddresses []net.IP,
	_ time.Duration,
) (*pki.RelayCertResult, error) {
	f.relayID = relayID
	f.dnsSAN = dnsNames[0]
	f.ipSAN = ipAddresses[0].String()
	return f.result, nil
}

func makeCSRDER(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CSR key: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	return der
}

func TestValidateIPSANs(t *testing.T) {
	if _, err := validateIPSANs([]string{"203.0.113.10", "2001:db8::1"}); err != nil {
		t.Fatalf("valid IP SANs rejected: %v", err)
	}
	for _, invalid := range [][]string{
		{"not-an-ip"},
		{"2001:0db8::1"},
		{"203.0.113.10", "203.0.113.10"},
	} {
		if _, err := validateIPSANs(invalid); err == nil {
			t.Fatalf("invalid IP SANs accepted: %v", invalid)
		}
	}
}
