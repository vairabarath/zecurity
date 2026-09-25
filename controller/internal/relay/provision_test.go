package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"net"
	"net/url"
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
	store := defaultRelayStore()
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
	if fake.relayID != testRelayID ||
		len(fake.dnsNames) != 1 || fake.dnsNames[0] != "relay.example.com" ||
		len(fake.ipAddrs) != 1 || !fake.ipAddrs[0].Equal(net.ParseIP("203.0.113.10")) {
		t.Fatalf("unexpected PKI request: relay=%q dns=%v ip=%v", fake.relayID, fake.dnsNames, fake.ipAddrs)
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
	service := newProvisionTestService(fake, defaultRelayStore(), rdb)
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

// TestProvisionRejectsUnregisteredRelay — an unregistered relay is rejected
// BEFORE the token is burned (Phase C moved the row lookup ahead of the burn).
func TestProvisionRejectsUnregisteredRelay(t *testing.T) {
	ctx := context.Background()
	rdb := newProvisionTestValkey(t)
	token, jti := storeProvisioningToken(t, ctx, rdb, testRelayID)
	fake := newFakePKI()
	store := &fakeProvisionStore{loadErr: ErrRelayNotFound}
	service := newProvisionTestService(fake, store, rdb)
	req := validProvisionRequest(t)
	req.ProvisioningToken = token

	_, err := service.Provision(ctx, req)
	assertStatusCode(t, err, codes.FailedPrecondition)
	if fake.signCalls != 0 {
		t.Fatalf("SignRelayCert calls = %d, want 0", fake.signCalls)
	}
	if !jtiPresent(t, ctx, rdb, jti) {
		t.Fatal("provisioning token was consumed by a rejected request")
	}
}

type fakePKI struct {
	pki.Service
	result    *pki.RelayCertResult
	relayID   string
	dnsNames  []string // allowlist the signer received
	ipAddrs   []net.IP // allowlist the signer received
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
	f.dnsNames = append([]string(nil), dnsNames...)
	f.ipAddrs = append([]net.IP(nil), ipAddresses...)
	return f.result, nil
}

type fakeProvisionStore struct {
	row           *RelayRow // returned by LoadRelayByID; nil + nil loadErr = not found
	loadErr       error
	markErr       error
	markedRelayID string
}

// pendingRelayStore returns a store whose relay row is awaiting provisioning
// with the given operator-registered SAN allowlists.
func pendingRelayStore(dnsAllowlist, ipAllowlist []string) *fakeProvisionStore {
	return &fakeProvisionStore{row: &RelayRow{
		ID:           testRelayID,
		Status:       "pending",
		DNSAllowlist: dnsAllowlist,
		IPAllowlist:  ipAllowlist,
	}}
}

func (f *fakeProvisionStore) LoadRelayByID(_ context.Context, id string) (*RelayRow, error) {
	if f.loadErr != nil {
		return nil, f.loadErr
	}
	if f.row == nil || f.row.ID != id {
		return nil, ErrRelayNotFound
	}
	return f.row, nil
}

func (f *fakeProvisionStore) MarkProvisioned(_ context.Context, id, _ string, _ time.Time, _, _ string) error {
	f.markedRelayID = id
	return f.markErr
}

func (f *fakeProvisionStore) RelayCertStatus(context.Context, string, string) (bool, bool, error) {
	return false, false, nil
}

func (f *fakeProvisionStore) RecordRenewedCert(context.Context, string, string, string, time.Time) (int, error) {
	return 0, ErrRelayNotFound
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
		CsrDer:   makeRelayCSRDER(t, testRelayID, []string{"relay.example.com"}, []string{"203.0.113.10"}),
		DnsSans:  []string{"relay.example.com"},
		IpSans:   []string{"203.0.113.10"},
		Version:  "1.0.0",
		Hostname: "relay-test",
	}
}

// defaultRelayStore matches validProvisionRequest's SANs.
func defaultRelayStore() *fakeProvisionStore {
	return pendingRelayStore([]string{"relay.example.com"}, []string{"203.0.113.10"})
}

// jtiPresent reports whether the provisioning token is still unused.
func jtiPresent(t *testing.T, ctx context.Context, rdb valkeycompat.Cmdable, jti string) bool {
	t.Helper()
	n, err := rdb.Exists(ctx, provisioningJTIPrefix+jti).Result()
	if err != nil {
		t.Fatalf("check provisioning JTI: %v", err)
	}
	return n == 1
}

func assertStatusCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if got := status.Code(err); got != want {
		t.Fatalf("status code = %v, want %v; err=%v", got, want, err)
	}
}

// makeRelayCSRDER builds a P-384 CSR carrying the relay SPIFFE URI SAN for
// uriRelayID plus the given DNS names (verbatim) and IP SANs.
func makeRelayCSRDER(t *testing.T, uriRelayID string, dnsNames, ips []string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CSR key: %v", err)
	}
	spiffeURI, err := url.Parse(appmeta.RelaySPIFFEID(uriRelayID))
	if err != nil {
		t.Fatalf("parse SPIFFE URI: %v", err)
	}
	template := &x509.CertificateRequest{
		URIs:     []*url.URL{spiffeURI},
		DNSNames: dnsNames,
	}
	for _, value := range ips {
		ip := net.ParseIP(value)
		if ip == nil {
			t.Fatalf("bad test IP %q", value)
		}
		template.IPAddresses = append(template.IPAddresses, ip)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, template, key)
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
