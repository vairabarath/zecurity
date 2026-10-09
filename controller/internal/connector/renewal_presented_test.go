package connector

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/spiffe"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// Sprint 21 B2: RenewCert stores the new cert_not_after before the connector
// has installed the certificate, so the renewal trigger must also consider the
// certificate the stream actually presented.

// ctxPresenting is a stream context whose TLS peer presented a leaf expiring
// at notAfter (what StreamSPIFFEInterceptor verified).
func ctxPresenting(ctx context.Context, notAfter time.Time) context.Context {
	return peer.NewContext(ctx, &peer.Peer{AuthInfo: credentials.TLSInfo{
		State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{{NotAfter: notAfter}}},
	}})
}

func presenting(notAfter time.Time) *time.Time { return &notAfter }

func TestPresentedCertNotAfter(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	if got := presentedCertNotAfter(ctxPresenting(ctx, at)); got == nil || !got.Equal(at) {
		t.Fatalf("presented leaf: got %v, want %v", got, at)
	}
	unavailable := map[string]context.Context{
		"no peer":          ctx,
		"peer without TLS": peer.NewContext(ctx, &peer.Peer{}),
		"TLS without a client certificate": peer.NewContext(ctx, &peer.Peer{
			AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{}},
		}),
		"leaf without NotAfter": ctxPresenting(ctx, time.Time{}),
	}
	for name, c := range unavailable {
		if got := presentedCertNotAfter(c); got != nil {
			t.Errorf("%s: got %v, want nil (unknown)", name, got)
		}
	}
}

func TestRenewalExpiry(t *testing.T) {
	now := time.Now()
	soon, later := now.Add(time.Hour), now.Add(7*24*time.Hour)
	cases := []struct {
		name              string
		stored, presented *time.Time
		want              *time.Time
	}{
		{"lost response: DB fresh, old cert presented", &later, &soon, &soon},
		{"installed: both fresh", &later, &later, &later},
		{"presented newer than DB", &soon, &later, &soon},
		{"presented unknown: stored only", &later, nil, &later},
		{"stored NULL: presented", nil, &soon, &soon},
		{"both unknown", nil, nil, nil},
	}
	for _, tc := range cases {
		got := renewalExpiry(tc.stored, tc.presented)
		if (got == nil) != (tc.want == nil) || (got != nil && !got.Equal(*tc.want)) {
			t.Errorf("%s: renewalExpiry = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// renewalFixture: an active connector whose stored cert_not_after is set.
func renewalFixture(t *testing.T, stored time.Time) (*EnrollmentHandler, context.Context, string, string) {
	t.Helper()
	pool, ctx := setupRevocationTestDB(t)
	wsID, trustDomain, rnID := seedRevocationWorkspace(t, pool, ctx, fmt.Sprintf("b2-%d", time.Now().UnixNano()))
	connID := seedConnector(t, pool, ctx, wsID, rnID, trustDomain, "active", nil)
	h := &EnrollmentHandler{Pool: pool, Cfg: Config{RenewalWindow: testRenewalWindow}}
	setConnectorCertNotAfter(t, h, connID, &stored)
	return h, ctx, connID, wsID
}

func reportHealth(t *testing.T, h *EnrollmentHandler, ctx context.Context, client *connectorStreamClient, times int) int {
	t.Helper()
	sent := 0
	for i := 0; i < times; i++ {
		if !h.handleConnectorHealth(ctx, client, healthReport()) {
			t.Fatal("health report closed the stream")
		}
		sent += drainReEnrolls(client)
	}
	return sent
}

// Lost or rejected RenewCert response (or a crash before the connector saved
// the certificate): the DB holds the new expiry, the connector still presents
// the old near-expiry certificate → it is asked to renew again.
func TestHandleConnectorHealth_OldCertPresentedAfterUnconfirmedRenewal(t *testing.T) {
	h, ctx, connID, wsID := renewalFixture(t, time.Now().Add(7*24*time.Hour))
	client := testClient(connID, wsID)
	client.presentedCertNotAfter = presenting(time.Now().Add(time.Hour))

	if n := reportHealth(t, h, ctx, client, 3); n != 1 {
		t.Fatalf("ReEnroll over 3 reports = %d, want 1 (asked again, then throttled)", n)
	}
}

// Normal renewal / successful recovery: the replacement stream presents the
// renewed certificate and the DB agrees → no ReEnroll until the next window.
func TestHandleConnectorHealth_RenewedCertPresentedNoReEnroll(t *testing.T) {
	fresh := time.Now().Add(7 * 24 * time.Hour)
	h, ctx, connID, wsID := renewalFixture(t, fresh)
	client := testClient(connID, wsID)
	client.presentedCertNotAfter = presenting(fresh)

	if n := reportHealth(t, h, ctx, client, 3); n != 0 {
		t.Fatalf("ReEnroll after a successful renewal = %d, want 0", n)
	}
}

// Old certificate presented and the DB agrees (no renewal yet): unchanged.
func TestHandleConnectorHealth_OldCertPresentedAndStored(t *testing.T) {
	soon := time.Now().Add(time.Hour)
	h, ctx, connID, wsID := renewalFixture(t, soon)
	client := testClient(connID, wsID)
	client.presentedCertNotAfter = presenting(soon)

	if n := reportHealth(t, h, ctx, client, 2); n != 1 {
		t.Fatalf("ReEnroll = %d, want 1", n)
	}
}

// Controller restart: nothing in memory survives; a new handler reading the
// same DB still asks a connector that presents its old certificate to renew.
func TestHandleConnectorHealth_RecoveryAfterControllerRestart(t *testing.T) {
	h, ctx, connID, wsID := renewalFixture(t, time.Now().Add(7*24*time.Hour))
	restarted := &EnrollmentHandler{Pool: h.Pool, Cfg: h.Cfg}
	client := testClient(connID, wsID) // the connector's new stream after the restart
	client.presentedCertNotAfter = presenting(time.Now().Add(time.Hour))

	if n := reportHealth(t, restarted, ctx, client, 1); n != 1 {
		t.Fatalf("ReEnroll after controller restart = %d, want 1", n)
	}
}

// Unknown presented certificate: the decision uses the stored expiry alone —
// it is not treated as proof that a renewal was installed.
func TestHandleConnectorHealth_UnknownPresentedUsesStoredExpiry(t *testing.T) {
	h, ctx, connID, wsID := renewalFixture(t, time.Now().Add(time.Hour))
	due := testClient(connID, wsID) // presentedCertNotAfter nil
	if n := reportHealth(t, h, ctx, due, 1); n != 1 {
		t.Fatalf("stored expiry inside window, presented unknown: ReEnroll = %d, want 1", n)
	}

	fresh := time.Now().Add(7 * 24 * time.Hour)
	setConnectorCertNotAfter(t, h, connID, &fresh)
	notDue := testClient(connID, wsID)
	if n := reportHealth(t, h, ctx, notDue, 1); n != 0 {
		t.Fatalf("stored expiry outside window, presented unknown: ReEnroll = %d, want 0", n)
	}
}

// No storm: with the old certificate still presented, a report every 15 s
// gets one ReEnroll per 10-minute throttle interval.
func TestMaybeSendReEnroll_OldCertPresentedIsThrottled(t *testing.T) {
	h := renewalHandler()
	client := testClient("c1", "ws")
	t0 := time.Now()
	stored := t0.Add(7 * 24 * time.Hour)
	client.presentedCertNotAfter = presenting(t0.Add(time.Hour))

	sent := 0
	for at := t0; at.Before(t0.Add(25 * time.Minute)); at = at.Add(15 * time.Second) {
		if h.maybeSendReEnroll(client, renewalExpiry(&stored, client.presentedCertNotAfter), at) {
			sent++
		}
	}
	if sent != 3 { // at 0, 10 and 20 minutes
		t.Fatalf("ReEnroll over 25 min of reports = %d, want 3", sent)
	}
}

// scriptedControlServer delivers the given messages, then blocks until closed.
type scriptedControlServer struct {
	*heldControlServer
	mu    sync.Mutex
	queue []*pb.ConnectorControlMessage
	sent  []*pb.ConnectorControlMessage
}

func (s *scriptedControlServer) Recv() (*pb.ConnectorControlMessage, error) {
	s.mu.Lock()
	if len(s.queue) > 0 {
		msg := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()
		return msg, nil
	}
	s.mu.Unlock()
	<-s.closed
	return nil, io.EOF
}

func (s *scriptedControlServer) Send(msg *pb.ConnectorControlMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, msg)
	return nil
}

func (s *scriptedControlServer) reEnrolls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, m := range s.sent {
		if m.GetReEnroll() != nil {
			n++
		}
	}
	return n
}

// End to end through Control: the presented certificate is captured from the
// stream's TLS peer when the stream opens and drives the trigger. The DB
// already holds the renewed expiry; the stream presents the old certificate.
// (The renewed-certificate case is TestHandleConnectorHealth_RenewedCertPresentedNoReEnroll.)
func TestControl_PresentedOldCertTriggersReEnroll(t *testing.T) {
	f := newLifecycleFixture(t, "active")
	f.h.Cfg.RenewalWindow = testRenewalWindow
	stored := time.Now().Add(7 * 24 * time.Hour) // RenewCert already stored the new expiry
	setConnectorCertNotAfter(t, f.h, f.connID, &stored)

	sctx, cancel := context.WithCancel(spiffe.WithIdentity(ctxPresenting(f.ctx, time.Now().Add(time.Hour)),
		fmt.Sprintf("spiffe://%s/connector/%s", f.td, f.connID), "connector", f.connID, f.td))
	defer cancel()
	stream := &scriptedControlServer{
		heldControlServer: newHeldStream(sctx),
		queue: []*pb.ConnectorControlMessage{{
			Body: &pb.ConnectorControlMessage_ConnectorHealth{ConnectorHealth: healthReport()},
		}},
	}
	done := make(chan error, 1)
	go func() { done <- f.h.Control(stream) }()

	deadline := time.Now().Add(5 * time.Second)
	for stream.reEnrolls() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no ReEnroll: the presented old certificate was not taken into account")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := stream.reEnrolls(); got != 1 {
		t.Fatalf("ReEnroll sent = %d, want 1 (one health report)", got)
	}

	stream.close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Control did not return after EOF")
	}
}
