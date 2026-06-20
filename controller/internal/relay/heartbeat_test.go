package relay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/spiffe"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type fakeHeartbeatStore struct {
	id           string
	certSerial   string
	certNotAfter time.Time
	version      string
	hostname     string
	observedIP   string
	observedPort int
	addressScope string
	publicAddr   string
	err          error
}

func (s *fakeHeartbeatStore) RecordHeartbeat(_ context.Context, id, certSerial string, certNotAfter time.Time, version, hostname, observedIP string, observedPort int, addressScope, publicAddr string) error {
	s.id = id
	s.certSerial = certSerial
	s.certNotAfter = certNotAfter
	s.version = version
	s.hostname = hostname
	s.observedIP = observedIP
	s.observedPort = observedPort
	s.addressScope = addressScope
	s.publicAddr = publicAddr
	return s.err
}

func (s *fakeHeartbeatStore) MarkProvisioned(context.Context, string, string, time.Time, string, string) error {
	return s.err
}

func (s *fakeHeartbeatStore) InsertProvisionedRelay(context.Context, string, string, []string, []string, string, time.Time, string, string) error {
	return s.err
}

func TestHeartbeatRecordsAuthenticatedRelay(t *testing.T) {
	store := &fakeHeartbeatStore{}
	service := NewService(nil, store, time.Hour)
	notAfter := time.Now().UTC().Add(time.Hour)
	ctx := relayHeartbeatContext(t, appmeta.SPIFFERoleRelay, testRelayID, testRelayID, notAfter)

	response, err := service.Heartbeat(ctx, &relaypb.HeartbeatRequest{
		Version:  "1.2.3",
		Hostname: "relay-a",
	})
	if err != nil {
		t.Fatalf("Heartbeat rejected authenticated Relay: %v", err)
	}
	if response.NextHeartbeatSeconds == 0 || response.ServerTimeUnix == 0 {
		t.Fatalf("unexpected Heartbeat response: %+v", response)
	}
	if store.id != testRelayID || store.certSerial != "2a" ||
		store.version != "1.2.3" || store.hostname != "relay-a" ||
		store.observedIP != "192.168.1.71" || store.observedPort != 54321 ||
		store.addressScope != "private" || store.publicAddr != "" ||
		!store.certNotAfter.Equal(notAfter) {
		t.Fatalf("unexpected heartbeat persistence: %+v", store)
	}
}

func TestObserveRelayPeerAddressClassifiesPublicAddress(t *testing.T) {
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("8.8.8.8"), Port: 54321},
	})

	got := observeRelayPeerAddress(ctx)
	if got.ObservedIP != "8.8.8.8" ||
		got.ObservedPort != 54321 ||
		got.Scope != "public" ||
		got.PublicAddr != "8.8.8.8:9093" {
		t.Fatalf("unexpected observation: %+v", got)
	}
}

func TestHeartbeatRejectsWrongRoleAndCertificateIdentity(t *testing.T) {
	service := NewService(nil, &fakeHeartbeatStore{}, time.Hour)
	for _, ctx := range []context.Context{
		relayHeartbeatContext(t, appmeta.SPIFFERoleConnector, testRelayID, testRelayID, time.Now()),
		relayHeartbeatContext(t, appmeta.SPIFFERoleRelay, testRelayID, "9b2d5cae-5820-4702-adf4-231680852b11", time.Now()),
	} {
		if _, err := service.Heartbeat(ctx, &relaypb.HeartbeatRequest{}); err == nil {
			t.Fatal("Heartbeat accepted invalid authenticated identity")
		} else if status.Code(err).String() == "OK" {
			t.Fatalf("unexpected status: %v", err)
		}
	}
}

func relayHeartbeatContext(t *testing.T, role, contextRelayID, certificateRelayID string, notAfter time.Time) context.Context {
	t.Helper()
	spiffeURI, err := url.Parse(appmeta.RelaySPIFFEID(certificateRelayID))
	if err != nil {
		t.Fatalf("parse SPIFFE URI: %v", err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		NotAfter:     notAfter,
		URIs:         []*url.URL{spiffeURI},
	}
	ctx := spiffe.WithIdentity(
		context.Background(),
		appmeta.RelaySPIFFEID(contextRelayID),
		role,
		contextRelayID,
		appmeta.SPIFFEGlobalTrustDomain,
	)
	return peer.NewContext(ctx, &peer.Peer{
		Addr: &net.TCPAddr{IP: net.ParseIP("192.168.1.71"), Port: 54321},
		AuthInfo: credentials.TLSInfo{
			State: tls.ConnectionState{
				PeerCertificates: []*x509.Certificate{leaf},
			},
		},
	})
}
