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
	workspaceIDs []string // returned by ListWorkspacesForRelay
}

type fakeNotifier struct {
	called []string // workspace IDs NotifyPolicyChange was called with
	err    error
}

func (n *fakeNotifier) NotifyPolicyChange(_ context.Context, workspaceID string) error {
	n.called = append(n.called, workspaceID)
	return n.err
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

func (s *fakeHeartbeatStore) ListWorkspacesForRelay(_ context.Context, _ string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.workspaceIDs, nil
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

	// listen_port reported by relay — should override the hardcoded default.
	got := observeRelayPeerAddress(ctx, 9093)
	if got.ObservedIP != "8.8.8.8" ||
		got.ObservedPort != 54321 ||
		got.Scope != "public" ||
		got.PublicAddr != "8.8.8.8:9093" {
		t.Fatalf("unexpected observation: %+v", got)
	}

	// Zero listen_port falls back to the hardcoded default.
	gotFallback := observeRelayPeerAddress(ctx, 0)
	if gotFallback.PublicAddr != "8.8.8.8:9093" {
		t.Fatalf("expected fallback to default port, got: %+v", gotFallback)
	}

	// Non-default listen_port is used as-is.
	gotCustom := observeRelayPeerAddress(ctx, 19093)
	if gotCustom.PublicAddr != "8.8.8.8:19093" {
		t.Fatalf("expected custom port in PublicAddr, got: %+v", gotCustom)
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

// TestHeartbeat_NotifiesWorkspacesOnMetadataChange — Gap 2 regression.
// Without Redis the cache layer always sets metadataChanged=true, so the
// heartbeat handler must call NotifyPolicyChange for every workspace that
// has connectors assigned to this relay.
func TestHeartbeat_NotifiesWorkspacesOnMetadataChange(t *testing.T) {
	store := &fakeHeartbeatStore{workspaceIDs: []string{"ws-alpha", "ws-beta"}}
	notifier := &fakeNotifier{}
	service := NewService(nil, store, time.Hour).WithPolicyNotifier(notifier)
	notAfter := time.Now().UTC().Add(time.Hour)
	ctx := relayHeartbeatContext(t, appmeta.SPIFFERoleRelay, testRelayID, testRelayID, notAfter)

	if _, err := service.Heartbeat(ctx, &relaypb.HeartbeatRequest{Version: "1.0.0", Hostname: "relay-a"}); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	if len(notifier.called) != 2 {
		t.Fatalf("expected 2 NotifyPolicyChange calls, got %d: %v", len(notifier.called), notifier.called)
	}
	notified := make(map[string]bool, len(notifier.called))
	for _, id := range notifier.called {
		notified[id] = true
	}
	for _, wantID := range []string{"ws-alpha", "ws-beta"} {
		if !notified[wantID] {
			t.Fatalf("workspace %q was not notified; got calls: %v", wantID, notifier.called)
		}
	}
}

// TestHeartbeat_NoNotifierDoesNotPanic — Gap 2 regression.
// Heartbeat must succeed when no notifier is wired (nil guard in notifyRelayWorkspaces).
func TestHeartbeat_NoNotifierDoesNotPanic(t *testing.T) {
	store := &fakeHeartbeatStore{workspaceIDs: []string{"ws-orphan"}}
	service := NewService(nil, store, time.Hour) // no notifier
	notAfter := time.Now().UTC().Add(time.Hour)
	ctx := relayHeartbeatContext(t, appmeta.SPIFFERoleRelay, testRelayID, testRelayID, notAfter)
	if _, err := service.Heartbeat(ctx, &relaypb.HeartbeatRequest{Version: "1.0.0"}); err != nil {
		t.Fatalf("Heartbeat with nil notifier: %v", err)
	}
}
