package connector

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"testing"
	"time"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/spiffe"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Certificate audit bug 7: the unary SPIFFE interceptor verified a connector's
// certificate chain but never its revocation, so a revoked connector whose
// certificate had not yet expired could still call every connector unary RPC
// — including proxying its shields' certificate renewals. Every connector
// unary RPC must now be refused for a revoked connector, and refused (fail
// closed) when the connector's standing cannot be established.

const (
	testConnectorTD = "ws-bug7.zecurity.in"
	testConnectorID = "6f1c2a7e-3d4b-4c5a-9e8f-0a1b2c3d4e5f"
)

// connectorUnaryMethods are every unary RPC a connector certificate reaches
// through the interceptor: its own, and the shield RPCs it proxies.
var connectorUnaryMethods = []string{
	pb.ConnectorService_RenewCert_FullMethodName,
	pb.ConnectorService_Goodbye_FullMethodName,
	shieldpb.ShieldService_RenewCert_FullMethodName,
	shieldpb.ShieldService_Goodbye_FullMethodName,
}

func makeTestConnectorLeaf(t *testing.T, issuer *x509.Certificate, issuerKey *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate connector key: %v", err)
	}
	uri, err := url.Parse(fmt.Sprintf("spiffe://%s/connector/%s", testConnectorTD, testConnectorID))
	if err != nil {
		t.Fatalf("parse connector SPIFFE URI: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(12),
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{uri},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer, &key.PublicKey, issuerKey)
	if err != nil {
		t.Fatalf("create connector certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse connector certificate: %v", err)
	}
	return cert
}

type connectorCall struct {
	called   bool
	role     string
	entityID string
	td       string
}

// connectorInterceptorFixture presents a connector certificate that chains to
// its workspace CA; standing is what the store reports for the connector.
func connectorInterceptorFixture(t *testing.T, standing *ConnectorStanding, standingErr error) (context.Context, grpc.UnaryServerInterceptor, *mockWorkspaceStore, *connectorCall, grpc.UnaryHandler) {
	t.Helper()
	workspaceCA, workspaceKey := makeTestCA(t)
	leaf := makeTestConnectorLeaf(t, workspaceCA, workspaceKey)
	store := &mockWorkspaceStore{workspaceCA: workspaceCA, standing: standing, standingErr: standingErr}
	validator := func(context.Context, string) bool { return true }
	call := &connectorCall{}
	handler := func(ctx context.Context, req any) (any, error) {
		call.called = true
		call.role = spiffe.Role(ctx)
		call.entityID = spiffe.EntityID(ctx)
		call.td = spiffe.TrustDomain(ctx)
		return "ok", nil
	}
	return relayPeerContext(t, leaf), UnarySPIFFEInterceptor(validator, store, nil), store, call, handler
}

func TestUnarySPIFFEInterceptor_RejectsRevokedConnectorOnEveryUnaryRPC(t *testing.T) {
	revokedAt := time.Now().Add(-time.Minute)
	cases := map[string]*ConnectorStanding{
		"status revoked":                      {Status: "revoked", RevokedAt: &revokedAt},
		"status revoked without revoked_at":   {Status: "revoked"},
		"revoked_at set, status active":       {Status: "active", RevokedAt: &revokedAt},
		"revoked_at set, status disconnected": {Status: "disconnected", RevokedAt: &revokedAt},
	}
	for name, standing := range cases {
		for _, method := range connectorUnaryMethods {
			t.Run(name+" "+method, func(t *testing.T) {
				ctx, interceptor, _, call, handler := connectorInterceptorFixture(t, standing, nil)
				resp, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, handler)
				if got := status.Code(err); got != codes.PermissionDenied {
					t.Fatalf("code = %v (err %v), want PermissionDenied", got, err)
				}
				if resp != nil || call.called {
					t.Fatal("a revoked connector reached the handler")
				}
			})
		}
	}
}

// Fail closed: a connector whose standing cannot be established is not trusted.
func TestUnarySPIFFEInterceptor_ConnectorFailsClosedWithoutStanding(t *testing.T) {
	cases := []struct {
		name     string
		standing *ConnectorStanding
		err      error
		want     codes.Code
	}{
		{"connector row missing (deleted or wrong trust domain)", nil, nil, codes.PermissionDenied},
		{"standing unreadable", nil, errors.New("database unavailable"), codes.Unavailable},
		{"error alongside a standing", &ConnectorStanding{Status: "active"}, errors.New("partial read"), codes.Unavailable},
	}
	for _, tc := range cases {
		for _, method := range connectorUnaryMethods {
			t.Run(tc.name+" "+method, func(t *testing.T) {
				ctx, interceptor, _, call, handler := connectorInterceptorFixture(t, tc.standing, tc.err)
				_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, handler)
				if got := status.Code(err); got != tc.want {
					t.Fatalf("code = %v (err %v), want %v", got, err, tc.want)
				}
				if call.called {
					t.Fatal("the handler ran without an established connector standing")
				}
			})
		}
	}
}

// Valid connectors keep working: identity is injected and the standing is
// read once per call.
func TestUnarySPIFFEInterceptor_ValidConnectorAllowed(t *testing.T) {
	for _, st := range []string{"active", "disconnected", "pending"} {
		for _, method := range connectorUnaryMethods {
			t.Run(st+" "+method, func(t *testing.T) {
				ctx, interceptor, store, call, handler := connectorInterceptorFixture(t, &ConnectorStanding{Status: st}, nil)
				resp, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, handler)
				if err != nil {
					t.Fatalf("valid connector refused: %v", err)
				}
				if resp != "ok" || !call.called {
					t.Fatal("valid connector did not reach the handler")
				}
				if call.role != appmeta.SPIFFERoleConnector || call.entityID != testConnectorID || call.td != testConnectorTD {
					t.Fatalf("identity = %+v", call)
				}
				if store.standingCalls != 1 {
					t.Fatalf("standing read %d times, want 1", store.standingCalls)
				}
			})
		}
	}
}

// The certificate is still verified first: an untrusted certificate is
// refused as unauthenticated, before its claimed connector is looked up.
func TestUnarySPIFFEInterceptor_ConnectorCertificateCheckedBeforeStanding(t *testing.T) {
	otherCA, otherKey := makeTestCA(t)
	workspaceCA, _ := makeTestCA(t)
	leaf := makeTestConnectorLeaf(t, otherCA, otherKey)
	store := &mockWorkspaceStore{workspaceCA: workspaceCA, standing: &ConnectorStanding{Status: "active"}}
	interceptor := UnarySPIFFEInterceptor(func(context.Context, string) bool { return true }, store, nil)
	_, err := interceptor(relayPeerContext(t, leaf), nil,
		&grpc.UnaryServerInfo{FullMethod: pb.ConnectorService_RenewCert_FullMethodName},
		func(context.Context, any) (any, error) { t.Fatal("handler reached"); return nil, nil })
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", got)
	}
	if store.standingCalls != 0 {
		t.Fatal("standing read for an unverified certificate")
	}
}

// Relay enforcement is unchanged and never consults connector standing;
// bootstrap RPCs stay certificate-free.
func TestUnarySPIFFEInterceptor_ConnectorStandingDoesNotAffectRelaysOrBootstrap(t *testing.T) {
	ctx, validator, store, info, handler := relayInterceptorFixture(t)
	mock := store.(*mockWorkspaceStore)
	mock.standingErr = errors.New("connector standing must not be consulted")
	revChecker := NewRelayRevocationChecker(func(context.Context) ([]string, error) { return nil, nil })
	if err := revChecker.Refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := UnarySPIFFEInterceptor(validator, store, revChecker)(ctx, nil, info, handler); err != nil {
		t.Fatalf("valid relay refused: %v", err)
	}
	if mock.standingCalls != 0 {
		t.Fatal("relay call consulted connector standing")
	}

	for _, method := range []string{
		pb.ConnectorService_Enroll_FullMethodName,
		shieldpb.ShieldService_Enroll_FullMethodName,
		relaypb.RelayService_Provision_FullMethodName,
	} {
		called := false
		_, err := UnarySPIFFEInterceptor(validator, store, nil)(context.Background(), nil,
			&grpc.UnaryServerInfo{FullMethod: method},
			func(context.Context, any) (any, error) { called = true; return "ok", nil })
		if err != nil || !called {
			t.Fatalf("%s: bootstrap RPC blocked: %v", method, err)
		}
	}
	if mock.standingCalls != 0 {
		t.Fatal("bootstrap RPC consulted connector standing")
	}
}
