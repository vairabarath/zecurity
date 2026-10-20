package connector

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"testing"
	"time"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/spiffe"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Certificate audit bug 10 (connector): RenewCert signed whatever public key
// the renewal CSR carried. Renewal is authenticated with the current
// certificate, but nothing tied the new certificate's key to it, so whoever
// briefly held the connector's key could move its identity onto a key of
// their own and keep renewing it. The connector reuses its key on renewal.

func newConnectorKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

// connectorCSRWithKey is a renewal/enrollment CSR for connectorID signed by key.
func connectorCSRWithKey(t *testing.T, connectorID, trustDomain string, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	uri, err := url.Parse(appmeta.ConnectorSPIFFEID(trustDomain, connectorID))
	if err != nil {
		t.Fatalf("parse SPIFFE URI: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: appmeta.PKIConnectorCNPrefix + connectorID},
		URIs:    []*url.URL{uri},
	}, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	return der
}

func parsePEMCert(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("certificate PEM does not decode")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return cert
}

func (f *bindingFixture) requestWithKey(token string, key *ecdsa.PrivateKey) *pb.EnrollRequest {
	return &pb.EnrollRequest{
		EnrollmentToken: token,
		CsrDer:          connectorCSRWithKey(f.t, f.connID, f.td, key),
		Version:         "1.0.0",
		Hostname:        "binding-host",
	}
}

// enrollWithKey enrolls the connector with key and returns its certificate.
func (f *bindingFixture) enrollWithKey(key *ecdsa.PrivateKey) *x509.Certificate {
	f.t.Helper()
	f.key = key
	resp, err := f.handler(f.pki).Enroll(f.ctx, f.requestWithKey(f.mustReissue(), key))
	if err != nil {
		f.t.Fatalf("enroll: %v", err)
	}
	return parsePEMCert(f.t, resp.CertificatePem)
}

// withPeer attaches leaf as the mTLS peer certificate, as the gRPC server does
// for a connection the SPIFFE interceptor verified.
func withPeer(ctx context.Context, leaf *x509.Certificate) context.Context {
	return peer.NewContext(ctx, &peer.Peer{AuthInfo: credentials.TLSInfo{
		State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}},
	}})
}

// renewWith renews presenting leaf (nil: no peer certificate) with a CSR
// signed by csrKey.
func (f *bindingFixture) renewWith(h *EnrollmentHandler, leaf *x509.Certificate, csrKey *ecdsa.PrivateKey) (*pb.RenewCertResponse, error) {
	ctx := spiffe.WithIdentity(f.ctx, fmt.Sprintf("spiffe://%s/connector/%s", f.td, f.connID),
		appmeta.SPIFFERoleConnector, f.connID, f.td)
	if leaf != nil {
		ctx = withPeer(ctx, leaf)
	}
	return h.RenewCert(ctx, &pb.RenewCertRequest{PublicKeyDer: connectorCSRWithKey(f.t, f.connID, f.td, csrKey)})
}

type connectorCertState struct {
	serial  string
	history int
}

func (f *bindingFixture) certState() connectorCertState {
	f.t.Helper()
	var st connectorCertState
	if err := f.pool.QueryRow(f.ctx,
		`SELECT COALESCE(cert_serial, ''),
		        (SELECT count(*) FROM connector_certificates WHERE connector_id = $1)
		   FROM connectors WHERE id = $1`, f.connID).Scan(&st.serial, &st.history); err != nil {
		f.t.Fatalf("read connector certificate state: %v", err)
	}
	return st
}

// refusedRenewal asserts no certificate was returned and nothing recorded.
func (f *bindingFixture) refusedRenewal(resp *pb.RenewCertResponse, err error, before connectorCertState) {
	f.t.Helper()
	if err == nil || resp != nil {
		f.t.Fatalf("renewal was not refused (err %v)", err)
	}
	if after := f.certState(); after != before {
		f.t.Fatalf("certificate state changed by a refused renewal: %+v → %+v", before, after)
	}
}

func TestConnectorRenewalKey_SameKeyRenews(t *testing.T) {
	f := newBindingFixture(t)
	leaf := f.enrollWithKey(newConnectorKey(t))
	resp, err := f.renewWith(f.handler(f.pki), leaf, f.key)
	if err != nil {
		t.Fatalf("renewal with the existing key refused: %v", err)
	}
	renewed := parsePEMCert(t, resp.CertificatePem)
	if !f.key.PublicKey.Equal(renewed.PublicKey) {
		t.Fatal("the renewed certificate is not for the connector's existing key")
	}
	if got := f.certState().serial; got != renewed.SerialNumber.Text(16) {
		t.Fatalf("recorded serial %s, want the renewed %s", got, renewed.SerialNumber.Text(16))
	}
}

func TestConnectorRenewalKey_DifferentKeyRefused(t *testing.T) {
	f := newBindingFixture(t)
	leaf := f.enrollWithKey(newConnectorKey(t))
	before := f.certState()
	attacker := newConnectorKey(t)
	resp, err := f.renewWith(f.handler(f.pki), leaf, attacker)
	f.refusedRenewal(resp, err, before)
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", got)
	}
}

// Without the authenticated peer certificate there is nothing to compare the
// CSR with: fail closed.
func TestConnectorRenewalKey_MissingPeerCertificateFailsClosed(t *testing.T) {
	f := newBindingFixture(t)
	f.enrollWithKey(newConnectorKey(t))
	before := f.certState()
	resp, err := f.renewWith(f.handler(f.pki), nil, f.key)
	f.refusedRenewal(resp, err, before)
}

func TestConnectorRenewalKey_UnreadablePeerKeyFailsClosed(t *testing.T) {
	f := newBindingFixture(t)
	f.enrollWithKey(newConnectorKey(t))
	before := f.certState()
	broken := &x509.Certificate{SerialNumber: big.NewInt(1), NotAfter: time.Now().Add(time.Hour)} // no public key
	resp, err := f.renewWith(f.handler(f.pki), broken, f.key)
	f.refusedRenewal(resp, err, before)
}

// selfSignedLeaf is a stand-in presented certificate for key. RenewCert only
// compares keys; the chain was verified by the SPIFFE interceptor.
func selfSignedLeaf(t *testing.T, key *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(7), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return leaf
}
