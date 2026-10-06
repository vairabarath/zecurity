package connector

// Control-stream ownership tests for make-before-break certificate renewal
// (Sprint 20 Fix: Connector Control-Stream Certificate Renewal).
//
// Invariant under test: when a newer stream B for a connector has become
// authoritative, the older stream A closing must NOT remove B from the
// registry, must NOT mark the connector disconnected and must NOT notify.

import (
	"context"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/spiffe"
)

// countingNotifier is a goroutine-safe policy + transport notifier.
type countingNotifier struct {
	mu        sync.Mutex
	policy    int
	transport int
}

func (n *countingNotifier) NotifyPolicyChange(context.Context, string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.policy++
	return nil
}

func (n *countingNotifier) NotifyTopologyChange(context.Context, string, []string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.transport++
	return nil
}

func (n *countingNotifier) counts() (int, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.policy, n.transport
}

// heldControlServer is a Control stream whose Recv blocks until close() and
// then reports EOF (the connector half-closing / dropping the stream).
type heldControlServer struct {
	ctx    context.Context
	closed chan struct{}
	once   sync.Once
}

func newHeldStream(ctx context.Context) *heldControlServer {
	return &heldControlServer{ctx: ctx, closed: make(chan struct{})}
}

func (s *heldControlServer) close() { s.once.Do(func() { close(s.closed) }) }

func (s *heldControlServer) SetHeader(metadata.MD) error  { return nil }
func (s *heldControlServer) SendHeader(metadata.MD) error { return nil }
func (s *heldControlServer) SetTrailer(metadata.MD)       {}
func (s *heldControlServer) Context() context.Context     { return s.ctx }
func (s *heldControlServer) SendMsg(any) error            { return nil }
func (s *heldControlServer) RecvMsg(any) error            { <-s.closed; return io.EOF }
func (s *heldControlServer) Send(*pb.ConnectorControlMessage) error {
	return nil
}
func (s *heldControlServer) Recv() (*pb.ConnectorControlMessage, error) {
	<-s.closed
	return nil, io.EOF
}

var _ pb.ConnectorService_ControlServer = (*heldControlServer)(nil)

type lifecycleFixture struct {
	t      *testing.T
	h      *EnrollmentHandler
	n      *countingNotifier
	connID string
	wsID   string
	ctx    context.Context
	td     string
}

func newLifecycleFixture(t *testing.T, initialStatus string) *lifecycleFixture {
	t.Helper()
	pool, ctx := setupRevocationTestDB(t)
	slug := fmt.Sprintf("cs-life-%d", time.Now().UnixNano())
	wsID, td, rnID := seedRevocationWorkspace(t, pool, ctx, slug)
	connID := seedConnector(t, pool, ctx, wsID, rnID, td, initialStatus, nil)
	n := &countingNotifier{}
	return &lifecycleFixture{
		t: t,
		h: &EnrollmentHandler{
			Pool:              pool,
			Registry:          NewConnectorRegistry(),
			PolicyNotifier:    n,
			TransportNotifier: n,
		},
		n: n, connID: connID, wsID: wsID, ctx: ctx, td: td,
	}
}

// running is one Control handler invocation in flight.
type running struct {
	stream *heldControlServer
	done   chan error
	cancel context.CancelFunc
}

// open starts h.Control on a new held stream and waits until that stream is
// the registered (authoritative) one.
func (f *lifecycleFixture) open() *running {
	f.t.Helper()
	sctx, cancel := context.WithCancel(spiffe.WithIdentity(f.ctx,
		fmt.Sprintf("spiffe://%s/connector/%s", f.td, f.connID), "connector", f.connID, f.td))
	r := &running{stream: newHeldStream(sctx), done: make(chan error, 1), cancel: cancel}
	go func() { r.done <- f.h.Control(r.stream) }()
	f.waitRegistered(r)
	return r
}

func (f *lifecycleFixture) waitRegistered(r *running) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if c := f.h.Registry.get(f.connID); c != nil && c.stream == r.stream {
			return
		}
		select {
		case err := <-r.done:
			f.t.Fatalf("Control returned before registering: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			f.t.Fatal("stream was not registered")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// closeAndWait ends the stream (connector EOF) and waits for the handler,
// including its close defer, to finish.
func (f *lifecycleFixture) closeAndWait(r *running) {
	f.t.Helper()
	r.stream.close()
	select {
	case err := <-r.done:
		if err != nil {
			f.t.Fatalf("Control returned error on EOF: %v", err)
		}
	case <-time.After(5 * time.Second):
		f.t.Fatal("Control did not return after EOF")
	}
	r.cancel()
}

func (f *lifecycleFixture) status() string {
	f.t.Helper()
	var s string
	if err := f.h.Pool.QueryRow(f.ctx, `SELECT status FROM connectors WHERE id = $1`, f.connID).Scan(&s); err != nil {
		f.t.Fatalf("query status: %v", err)
	}
	return s
}

func (f *lifecycleFixture) assertCounts(wantPolicy, wantTransport int, when string) {
	f.t.Helper()
	p, tr := f.n.counts()
	if p != wantPolicy || tr != wantTransport {
		f.t.Fatalf("%s: notifies policy=%d transport=%d, want %d/%d", when, p, tr, wantPolicy, wantTransport)
	}
}

// Old stream A closes AFTER the new stream B became authoritative: A must not
// remove B, must not mark the connector disconnected, must not notify.
func TestControlLifecycle_OldClosesAfterNewActive(t *testing.T) {
	f := newLifecycleFixture(t, "disconnected")

	a := f.open()
	f.assertCounts(1, 1, "A activation (disconnected→active)")
	if got := f.status(); got != "active" {
		t.Fatalf("after A: status %q, want active", got)
	}

	b := f.open() // make-before-break: B registers while A is still open
	f.assertCounts(1, 1, "B activation on already-active connector")

	f.closeAndWait(a)
	if c := f.h.Registry.get(f.connID); c == nil || c.stream != b.stream {
		t.Fatal("A's close removed or replaced B's registry entry")
	}
	if got := f.status(); got != "active" {
		t.Fatalf("after A closed: status %q, want active", got)
	}
	f.assertCounts(1, 1, "A (superseded) close")

	// B is the authoritative stream, so its close is a real disconnect.
	f.closeAndWait(b)
	if f.h.Registry.get(f.connID) != nil {
		t.Fatal("B's close left a registry entry")
	}
	if got := f.status(); got != "disconnected" {
		t.Fatalf("after B closed: status %q, want disconnected", got)
	}
	f.assertCounts(2, 2, "B (authoritative) close")
}

// Old stream A closes BEFORE B exists (old stream died first): a real gap,
// reported truthfully — disconnected + notify, then active + notify.
func TestControlLifecycle_OldClosesBeforeNewActive(t *testing.T) {
	f := newLifecycleFixture(t, "disconnected")

	a := f.open()
	f.closeAndWait(a)
	if got := f.status(); got != "disconnected" {
		t.Fatalf("after A closed: status %q, want disconnected", got)
	}
	if f.h.Registry.get(f.connID) != nil {
		t.Fatal("A's close left a registry entry")
	}
	f.assertCounts(2, 2, "A activation + A close")

	b := f.open()
	if got := f.status(); got != "active" {
		t.Fatalf("after B: status %q, want active", got)
	}
	f.assertCounts(3, 3, "B activation (disconnected→active)")
	f.closeAndWait(b)
}

// Deterministic version of the pre-fix race:
//
//	B: activation UPDATE      (already active → no notify)
//	A: ownership check        (B not yet registered → would pass)
//	B: Registry.add(B)
//	A: disconnect UPDATE      → 'disconnected' while B is live
//
// The test plays B's activation by holding lifecycleMu across (UPDATE + add),
// and starts A's retirement in that window. A must block until B is
// registered, then find it is no longer current and change nothing.
func TestControlLifecycle_RetireBlockedDuringActivation(t *testing.T) {
	f := newLifecycleFixture(t, "active")

	a := testClient(f.connID, f.wsID)
	f.h.Registry.add(f.connID, a)
	b := testClient(f.connID, f.wsID)

	f.h.Registry.lifecycleMu.Lock() // B's activation begins
	if _, err := f.h.Pool.Exec(f.ctx,
		`UPDATE connectors SET status = 'active', last_heartbeat_at = NOW() WHERE id = $1`, f.connID); err != nil {
		f.h.Registry.lifecycleMu.Unlock()
		t.Fatalf("B activation UPDATE: %v", err)
	}

	type result struct{ current, disconnected bool }
	aDone := make(chan result, 1)
	go func() {
		cur, disc := f.h.retireStream(context.Background(), a)
		aDone <- result{cur, disc}
	}()

	select {
	case r := <-aDone:
		f.h.Registry.lifecycleMu.Unlock()
		t.Fatalf("A retired while B's activation held lifecycleMu: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	if got := f.status(); got != "active" {
		f.h.Registry.lifecycleMu.Unlock()
		t.Fatalf("status changed to %q while B activating", got)
	}

	f.h.Registry.add(f.connID, b) // B registers, then releases the lock
	f.h.Registry.lifecycleMu.Unlock()

	r := <-aDone
	if r.current || r.disconnected {
		t.Fatalf("A retire after B registered: current=%v disconnected=%v, want false/false", r.current, r.disconnected)
	}
	if f.h.Registry.get(f.connID) != b {
		t.Fatal("A removed B")
	}
	if got := f.status(); got != "active" {
		t.Fatalf("final status %q, want active", got)
	}
}

// Both orders of activateStream/retireStream are correct.
func TestControlLifecycle_ActivateRetireBothOrders(t *testing.T) {
	t.Run("new activates then old retires", func(t *testing.T) {
		f := newLifecycleFixture(t, "active")
		a := testClient(f.connID, f.wsID)
		f.h.Registry.add(f.connID, a)
		b := testClient(f.connID, f.wsID)

		became, err := f.h.activateStream(f.ctx, b)
		if err != nil || became {
			t.Fatalf("activate B: became=%v err=%v, want false/nil", became, err)
		}
		cur, disc := f.h.retireStream(context.Background(), a)
		if cur || disc {
			t.Fatalf("retire A: current=%v disconnected=%v, want false/false", cur, disc)
		}
		if f.h.Registry.get(f.connID) != b || f.status() != "active" {
			t.Fatalf("want registry=B status=active, got status %q", f.status())
		}
	})
	t.Run("old retires then new activates", func(t *testing.T) {
		f := newLifecycleFixture(t, "active")
		a := testClient(f.connID, f.wsID)
		f.h.Registry.add(f.connID, a)
		b := testClient(f.connID, f.wsID)

		cur, disc := f.h.retireStream(context.Background(), a)
		if !cur || !disc {
			t.Fatalf("retire A: current=%v disconnected=%v, want true/true", cur, disc)
		}
		if f.status() != "disconnected" {
			t.Fatalf("after A retire: status %q", f.status())
		}
		became, err := f.h.activateStream(f.ctx, b)
		if err != nil || !became {
			t.Fatalf("activate B: became=%v err=%v, want true/nil", became, err)
		}
		if f.h.Registry.get(f.connID) != b || f.status() != "active" {
			t.Fatalf("want registry=B status=active, got status %q", f.status())
		}
	})
}

// A connector revoked while two streams overlap stays revoked: neither close
// rewrites the status and neither notifies a disconnect.
func TestControlLifecycle_RevokedStaysRevoked(t *testing.T) {
	f := newLifecycleFixture(t, "active")

	a := f.open()
	b := f.open()
	f.assertCounts(0, 0, "activations on already-active connector")

	if _, err := f.h.Pool.Exec(f.ctx,
		`UPDATE connectors SET status = 'revoked', revoked_at = NOW() WHERE id = $1`, f.connID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	f.closeAndWait(a) // superseded
	f.closeAndWait(b) // authoritative
	if got := f.status(); got != "revoked" {
		t.Fatalf("status %q, want revoked", got)
	}
	f.assertCounts(0, 0, "closes of a revoked connector")
	if f.h.Registry.get(f.connID) != nil {
		t.Fatal("registry still holds a stream for the revoked connector")
	}

	// A revoked connector can never be (re)registered by activation.
	c := testClient(f.connID, f.wsID)
	_, err := f.h.activateStream(f.ctx, c)
	if st, ok := status.FromError(err); !ok || st.Code() != codes.PermissionDenied {
		t.Fatalf("activate revoked: err=%v, want PermissionDenied", err)
	}
	if f.h.Registry.get(f.connID) != nil {
		t.Fatal("revoked connector was registered")
	}
	if got := f.status(); got != "revoked" {
		t.Fatalf("status %q after activation attempt, want revoked", got)
	}
}
