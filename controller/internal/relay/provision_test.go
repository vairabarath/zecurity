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

	"github.com/alicebob/miniredis/v2"
	"github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeycompat"
	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/pki"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testRelayID = "550e8400-e29b-41d4-a716-446655440000"
const otherTestRelayID = "550e8400-e29b-41d4-a716-446655440001"
const testProvisioningSecret = "test-relay-provisioning-secret"

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

func TestProvisionValidToken(t *testing.T) {
	ctx := context.Background()
	rdb := newProvisionTestValkey(t)
	token, _ := storeProvisioningToken(t, ctx, rdb, testRelayID)
	fake := newFakePKI()
	store := &fakeProvisionStore{}
	service := newProvisionTestService(fake, store, rdb)
	req := validProvisionRequest(t)
	req.ProvisioningToken = token

	response, err := service.Provision(ctx, req)
	if err != nil {
		t.Fatalf("Provision rejected valid request: %v", err)
	}
	if fake.signCalls != 1 {
		t.Fatalf("SignRelayCert calls = %d, want 1", fake.signCalls)
	}
	if store.markedRelayID != testRelayID {
		t.Fatalf("marked relay = %q, want %q", store.markedRelayID, testRelayID)
	}
	if fake.relayID != testRelayID || fake.dnsSAN != "relay.example.com" || fake.ipSAN != "203.0.113.10" {
		t.Fatalf("unexpected PKI request: relay=%q dns=%q ip=%q", fake.relayID, fake.dnsSAN, fake.ipSAN)
	}
	if response.RelayId != testRelayID || response.SpiffeId != appmeta.RelaySPIFFEID(testRelayID) {
		t.Fatalf("unexpected Provision response identity: %+v", response)
	}
}

func TestProvisionRequiresToken(t *testing.T) {
	fake := newFakePKI()
	service := NewService(fake, &fakeProvisionStore{}, time.Hour)

	_, err := service.Provision(context.Background(), validProvisionRequest(t))
	assertStatusCode(t, err, codes.Unauthenticated)
	if fake.signCalls != 0 {
		t.Fatalf("SignRelayCert calls = %d, want 0", fake.signCalls)
	}
}

func TestProvisionRejectsWrongRelayToken(t *testing.T) {
	ctx := context.Background()
	rdb := newProvisionTestValkey(t)
	token, _ := storeProvisioningToken(t, ctx, rdb, otherTestRelayID)
	fake := newFakePKI()
	service := newProvisionTestService(fake, &fakeProvisionStore{}, rdb)
	req := validProvisionRequest(t)
	req.ProvisioningToken = token

	_, err := service.Provision(ctx, req)
	assertStatusCode(t, err, codes.PermissionDenied)
	if fake.signCalls != 0 {
		t.Fatalf("SignRelayCert calls = %d, want 0", fake.signCalls)
	}
}

func TestProvisionRejectsReplayedToken(t *testing.T) {
	ctx := context.Background()
	rdb := newProvisionTestValkey(t)
	token, _ := storeProvisioningToken(t, ctx, rdb, testRelayID)
	fake := newFakePKI()
	service := newProvisionTestService(fake, &fakeProvisionStore{}, rdb)
	req := validProvisionRequest(t)
	req.ProvisioningToken = token

	if _, err := service.Provision(ctx, req); err != nil {
		t.Fatalf("first Provision failed: %v", err)
	}
	_, err := service.Provision(ctx, req)
	assertStatusCode(t, err, codes.PermissionDenied)
	if fake.signCalls != 1 {
		t.Fatalf("SignRelayCert calls = %d, want 1", fake.signCalls)
	}
}

func TestProvisionRejectsUnregisteredRelay(t *testing.T) {
	ctx := context.Background()
	rdb := newProvisionTestValkey(t)
	token, _ := storeProvisioningToken(t, ctx, rdb, testRelayID)
	fake := newFakePKI()
	store := &fakeProvisionStore{markErr: ErrRelayNotFound}
	service := newProvisionTestService(fake, store, rdb)
	req := validProvisionRequest(t)
	req.ProvisioningToken = token

	_, err := service.Provision(ctx, req)
	assertStatusCode(t, err, codes.FailedPrecondition)
	if fake.signCalls != 1 {
		t.Fatalf("SignRelayCert calls = %d, want 1", fake.signCalls)
	}
}

type fakePKI struct {
	pki.Service
	result    *pki.RelayCertResult
	relayID   string
	dnsSAN    string
	ipSAN     string
	signCalls int
}

func (f *fakePKI) SignRelayCert(
	_ context.Context,
	relayID string,
	_ *x509.CertificateRequest,
	dnsNames []string,
	ipAddresses []net.IP,
	_ time.Duration,
) (*pki.RelayCertResult, error) {
	f.signCalls++
	f.relayID = relayID
	f.dnsSAN = dnsNames[0]
	f.ipSAN = ipAddresses[0].String()
	return f.result, nil
}

type fakeProvisionStore struct {
	markErr       error
	markedRelayID string
}

func (f *fakeProvisionStore) MarkProvisioned(_ context.Context, id, _ string, _ time.Time, _, _ string) error {
	f.markedRelayID = id
	return f.markErr
}

func (f *fakeProvisionStore) RecordHeartbeat(context.Context, string, string, time.Time, string, string, string, int, string, string, uint32, uint32) error {
	return nil
}

func (f *fakeProvisionStore) ListConnectorsForRelay(context.Context, string) (map[string][]string, error) {
	return nil, nil
}

func (f *fakeProvisionStore) EvaluateCapacityLabel(context.Context, string, time.Duration) (CapacityLabelTransition, error) {
	return CapacityLabelTransition{}, nil
}

func newFakePKI() *fakePKI {
	now := time.Now().UTC()
	return &fakePKI{result: &pki.RelayCertResult{
		CertificatePEM:    "relay-cert",
		IntermediateCAPEM: "intermediate-cert",
		Serial:            "2a",
		NotBefore:         now,
		NotAfter:          now.Add(time.Hour),
	}}
}

func newProvisionTestService(fake *fakePKI, store heartbeatStore, rdb valkeycompat.Cmdable) *Service {
	return NewService(fake, store, time.Hour).
		WithHeartbeatCache(rdb, time.Minute).
		WithProvisioningAuth(testProvisioningSecret)
}

func newProvisionTestValkey(t *testing.T) valkeycompat.Cmdable {
	t.Helper()
	server := miniredis.RunT(t)
	client, err := valkey.NewClient(valkey.ClientOption{
		InitAddress:  []string{server.Addr()},
		DisableCache: true,
	})
	if err != nil {
		t.Fatalf("create test Valkey client: %v", err)
	}
	t.Cleanup(client.Close)
	return valkeycompat.NewAdapter(client)
}

func storeProvisioningToken(t *testing.T, ctx context.Context, rdb valkeycompat.Cmdable, relayID string) (string, string) {
	t.Helper()
	token, jti, err := IssueProvisioningToken(testProvisioningSecret, relayID, time.Hour)
	if err != nil {
		t.Fatalf("issue provisioning token: %v", err)
	}
	if err := StoreProvisioningJTI(ctx, rdb, jti, relayID, time.Hour); err != nil {
		t.Fatalf("store provisioning JTI: %v", err)
	}
	return token, jti
}

func validProvisionRequest(t *testing.T) *relaypb.ProvisionRequest {
	t.Helper()
	return &relaypb.ProvisionRequest{
		RelayId:  testRelayID,
		CsrDer:   makeCSRDER(t),
		DnsSans:  []string{"relay.example.com"},
		IpSans:   []string{"203.0.113.10"},
		Version:  "1.0.0",
		Hostname: "relay-test",
	}
}

func assertStatusCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("status code = %v, want %v; err=%v", got, want, err)
	}
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
