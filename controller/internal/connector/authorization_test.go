package connector

// Phase A authorization-contract tests (connector-revocation design §4.3–§4.6,
// §14). DB-backed tests use the throwaway-database pattern of
// setupRevocationTestDB (ENROLLMENT_TEST_DATABASE_URL).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/posture"
	"github.com/yourorg/ztna/controller/internal/spiffe"
)

// ── scripted Control stream ─────────────────────────────────────────────────

// scriptedStream is a Control server stream driven by the test: Recv returns
// injected messages (EOF once closed), Send records, SetTrailer records.
type scriptedStream struct {
	ctx context.Context
	in  chan *pb.ConnectorControlMessage

	mu      sync.Mutex
	sent    []*pb.ConnectorControlMessage
	trailer metadata.MD
	sentCh  chan struct{} // signalled (non-blocking) on every Send

	closeOnce sync.Once
	closed    chan struct{}
}

func newScriptedStream(ctx context.Context) *scriptedStream {
	return &scriptedStream{
		ctx:    ctx,
		in:     make(chan *pb.ConnectorControlMessage, 16),
		sentCh: make(chan struct{}, 1),
		closed: make(chan struct{}),
	}
}

func (s *scriptedStream) SetHeader(metadata.MD) error  { return nil }
func (s *scriptedStream) SendHeader(metadata.MD) error { return nil }
func (s *scriptedStream) SetTrailer(md metadata.MD) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.trailer = metadata.Join(s.trailer, md)
}
func (s *scriptedStream) Context() context.Context { return s.ctx }
func (s *scriptedStream) SendMsg(any) error        { return nil }
func (s *scriptedStream) RecvMsg(any) error        { return io.EOF }
func (s *scriptedStream) Send(m *pb.ConnectorControlMessage) error {
	s.mu.Lock()
	s.sent = append(s.sent, m)
	s.mu.Unlock()
	select {
	case s.sentCh <- struct{}{}:
	default:
	}
	return nil
}
func (s *scriptedStream) Recv() (*pb.ConnectorControlMessage, error) {
	select {
	case m := <-s.in:
		return m, nil
	case <-s.closed:
		return nil, io.EOF
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}
func (s *scriptedStream) close() { s.closeOnce.Do(func() { close(s.closed) }) }

func (s *scriptedStream) snapshot() []*pb.ConnectorControlMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*pb.ConnectorControlMessage(nil), s.sent...)
}

func (s *scriptedStream) marker() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.trailer.Get(ConnectorStateMarkerKey)
}

// waitFor polls until cond holds over the sent messages, or fails.
func (s *scriptedStream) waitFor(t *testing.T, what string, cond func([]*pb.ConnectorControlMessage) bool) []*pb.ConnectorControlMessage {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := s.snapshot()
		if cond(got) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s; sent=%v", what, kinds(got))
		}
		select {
		case <-s.sentCh:
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func kinds(msgs []*pb.ConnectorControlMessage) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, fmt.Sprintf("%T", m.GetBody()))
	}
	return out
}

func ackSeqs(msgs []*pb.ConnectorControlMessage) []uint64 {
	var out []uint64
	for _, m := range msgs {
		if a := m.GetAuthorizationAck(); a != nil {
			out = append(out, a.GetHeartbeatSeq())
		}
	}
	return out
}

func indexOf(msgs []*pb.ConnectorControlMessage, pred func(*pb.ConnectorControlMessage) bool) int {
	for i, m := range msgs {
		if pred(m) {
			return i
		}
	}
	return -1
}

func isRevokedEvent(m *pb.ConnectorControlMessage) bool { return m.GetConnectorRevoked() != nil }
func isAck(m *pb.ConnectorControlMessage) bool          { return m.GetAuthorizationAck() != nil }
func isACL(m *pb.ConnectorControlMessage) bool          { return m.GetAclSnapshot() != nil }

func health(seq uint64) *pb.ConnectorControlMessage {
	return &pb.ConnectorControlMessage{Body: &pb.ConnectorControlMessage_ConnectorHealth{
		ConnectorHealth: &pb.ConnectorHealthReport{Version: "t", Hostname: "h", HeartbeatSeq: seq},
	}}
}

// ── fixtures ────────────────────────────────────────────────────────────────

type authzFixture struct {
	t      *testing.T
	pool   *pgxpool.Pool
	ctx    context.Context
	h      *EnrollmentHandler
	wsID   string
	td     string
	rnID   string
	connID string
}

func newAuthzFixture(t *testing.T, status string) *authzFixture {
	t.Helper()
	pool, ctx := setupRevocationTestDB(t)
	wsID, td, rnID := seedRevocationWorkspace(t, pool, ctx, fmt.Sprintf("authz-%d", time.Now().UnixNano()))
	connID := seedConnector(t, pool, ctx, wsID, rnID, td, status, nil)
	return &authzFixture{
		t: t, pool: pool, ctx: ctx, wsID: wsID, td: td, rnID: rnID, connID: connID,
		h: &EnrollmentHandler{Pool: pool, Registry: NewConnectorRegistry()},
	}
}

// withACL wires the real policy/posture stores, cache and notifier, so
// pushACLSnapshot really compiles and enqueues an ACLSnapshot.
func (f *authzFixture) withACL() *authzFixture {
	cache := policy.NewSnapshotCache()
	f.h.PolicyStore = policy.NewStore(f.pool)
	f.h.PolicyCache = cache
	f.h.PolicyNotifier = policy.NewNotifier(cache)
	f.h.PostureStore = posture.NewStore(f.pool)
	return f
}

func (f *authzFixture) client() *connectorStreamClient {
	c := testClient(f.connID, f.wsID)
	c.trustDomain = f.td
	return c
}

func (f *authzFixture) streamCtx(connID string) context.Context {
	return spiffe.WithIdentity(f.ctx, fmt.Sprintf("spiffe://%s/connector/%s", f.td, connID), "connector", connID, f.td)
}

type controlRun struct {
	stream *scriptedStream
	done   chan error
	cancel context.CancelFunc
}

// openControl starts the real Control handler for connID on a scripted
// stream. It does not wait for registration.
func (f *authzFixture) openControl(connID string) *controlRun {
	sctx, cancel := context.WithCancel(f.streamCtx(connID))
	r := &controlRun{stream: newScriptedStream(sctx), done: make(chan error, 1), cancel: cancel}
	go func() { r.done <- f.h.Control(r.stream) }()
	f.t.Cleanup(func() { r.stream.close(); cancel() })
	return r
}

func (r *controlRun) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Control did not return")
		return nil
	}
}

// waitLive waits until n streams for connID are live in the registry.
func (f *authzFixture) waitLive(connID string, n int) {
	f.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.h.Registry.mu.RLock()
		got := len(f.h.Registry.live[connID])
		f.h.Registry.mu.RUnlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("live streams for %s = %d, want %d", connID, got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func assertStatus(t *testing.T, err error, code codes.Code, what string) {
	t.Helper()
	st, ok := status.FromError(err)
	if !ok || st.Code() != code {
		t.Fatalf("%s: err=%v, want code %s", what, err, code)
	}
}

func assertMarker(t *testing.T, s *scriptedStream, want string, what string) {
	t.Helper()
	got := s.marker()
	if want == "" {
		if len(got) != 0 {
			t.Fatalf("%s: marker=%v, want none", what, got)
		}
		return
	}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("%s: marker=%v, want [%q]", what, got, want)
	}
}

func (f *authzFixture) status(connID string) string {
	f.t.Helper()
	var s string
	if err := f.pool.QueryRow(f.ctx, `SELECT status FROM connectors WHERE id = $1`, connID).Scan(&s); err != nil {
		f.t.Fatalf("read status: %v", err)
	}
	return s
}

func (f *authzFixture) revokeInDB(connID string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `UPDATE connectors SET status='revoked', revoked_at=NOW() WHERE id=$1`, connID); err != nil {
		f.t.Fatalf("revoke: %v", err)
	}
}

// ── T1 / T7: authorized heartbeat, ack after the ACL push ───────────────────

// T1+T7: an authorized connector's health report gets AuthorizationAck with
// the report's seq, enqueued AFTER the ACL snapshot the report required.
func TestAuthz_AuthorizedHealthAckFollowsACLPush(t *testing.T) {
	f := newAuthzFixture(t, "active").withACL()
	// Bump the workspace ACL version to ≥ 1, so "already current" is
	// reportable (the version gate treats 0 as "no ACL loaded").
	if err := f.h.PolicyNotifier.NotifyPolicyChange(f.ctx, f.wsID); err != nil {
		t.Fatal(err)
	}
	c := f.client()

	if rej := f.h.handleConnectorHealth(f.ctx, c, health(7).GetConnectorHealth()); rej != nil {
		t.Fatalf("authorized health refused: %s", describeRejection(rej))
	}
	got := drain(c)
	aclAt, ackAt := indexOf(got, isACL), indexOf(got, isAck)
	if aclAt < 0 || ackAt < 0 {
		t.Fatalf("want ACLSnapshot and AuthorizationAck, got %v", kinds(got))
	}
	if ackAt < aclAt {
		t.Fatalf("AuthorizationAck (idx %d) precedes the required ACL push (idx %d): %v", ackAt, aclAt, kinds(got))
	}
	if ackAt != len(got)-1 {
		t.Fatalf("AuthorizationAck is not the last message for the report: %v", kinds(got))
	}
	if seqs := ackSeqs(got); len(seqs) != 1 || seqs[0] != 7 {
		t.Fatalf("ack seqs = %v, want [7]", seqs)
	}

	// Report with the now-current ACL version: no push is required, ack only.
	ver := got[aclAt].GetAclSnapshot().GetVersion()
	r := health(8).GetConnectorHealth()
	r.AclVersion = ver
	if ver == 0 {
		t.Fatal("compiled ACL version is 0 after a policy bump")
	}
	if rej := f.h.handleConnectorHealth(f.ctx, c, r); rej != nil {
		t.Fatalf("second health refused: %s", describeRejection(rej))
	}
	got = drain(c)
	if indexOf(got, isACL) >= 0 {
		t.Fatalf("ACL re-pushed although current: %v", kinds(got))
	}
	if seqs := ackSeqs(got); len(seqs) != 1 || seqs[0] != 8 {
		t.Fatalf("ack seqs = %v, want [8]", seqs)
	}
}

// T7: a required ACL push that fails withholds the acknowledgement.
func TestAuthz_ACLPushFailureWithholdsAck(t *testing.T) {
	f := newAuthzFixture(t, "active").withACL()
	c := f.client()
	c.tenantID = "not-a-uuid" // ACL compile fails deterministically

	if rej := f.h.handleConnectorHealth(f.ctx, c, health(3).GetConnectorHealth()); rej != nil {
		t.Fatalf("health refused: %s (an ACL failure must not end the stream)", describeRejection(rej))
	}
	if got := drain(c); indexOf(got, isAck) >= 0 {
		t.Fatalf("AuthorizationAck sent although the required ACL push failed: %v", kinds(got))
	}
}

// T7: a full mailbox (ACL push not enqueued) withholds the acknowledgement.
func TestAuthz_ACLPushQueueFullWithholdsAck(t *testing.T) {
	f := newAuthzFixture(t, "active").withACL()
	c := f.client()
	for len(c.outbound) < cap(c.outbound) {
		c.outbound <- &pb.ConnectorControlMessage{}
	}
	if rej := f.h.handleConnectorHealth(f.ctx, c, health(4).GetConnectorHealth()); rej != nil {
		t.Fatalf("health refused: %s", describeRejection(rej))
	}
	if got := drain(c); indexOf(got, isAck) >= 0 {
		t.Fatalf("AuthorizationAck enqueued although the ACL push could not be: %v", kinds(got))
	}
}

func drain(c *connectorStreamClient) []*pb.ConnectorControlMessage {
	var out []*pb.ConnectorControlMessage
	for {
		select {
		case m := <-c.outbound:
			out = append(out, m)
		default:
			return out
		}
	}
}

// ── T2 / T3 / T20: revoked vs unknown, at open, mid-stream, at activation ───

func TestAuthz_OpenRevokedVsUnknownMarkers(t *testing.T) {
	f := newAuthzFixture(t, "revoked")
	f.revokeInDB(f.connID)

	rev := f.openControl(f.connID)
	err := rev.wait(t)
	assertStatus(t, err, codes.PermissionDenied, "open revoked")
	assertMarker(t, rev.stream, ConnectorStateRevoked, "open revoked")
	if n := len(ackSeqs(rev.stream.snapshot())); n != 0 {
		t.Fatalf("revoked connector got %d ack(s)", n)
	}

	unk := f.openControl("00000000-0000-0000-0000-000000000abc")
	err = unk.wait(t)
	assertStatus(t, err, codes.NotFound, "open unknown")
	assertMarker(t, unk.stream, ConnectorStateUnknown, "open unknown")
	if len(unk.stream.snapshot()) != 0 {
		t.Fatalf("unknown connector received messages: %v", kinds(unk.stream.snapshot()))
	}
}

func TestAuthz_MidStreamRevokedVsDeleted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(f *authzFixture)
		code   codes.Code
		marker string
	}{
		{"revoked", func(f *authzFixture) { f.revokeInDB(f.connID) }, codes.PermissionDenied, ConnectorStateRevoked},
		{"deleted", func(f *authzFixture) {
			if _, err := f.pool.Exec(f.ctx, `DELETE FROM connectors WHERE id=$1`, f.connID); err != nil {
				f.t.Fatalf("delete: %v", err)
			}
		}, codes.NotFound, ConnectorStateUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAuthzFixture(t, "active")
			r := f.openControl(f.connID)
			r.stream.in <- health(1)
			r.stream.waitFor(t, "ack 1", func(m []*pb.ConnectorControlMessage) bool { return len(ackSeqs(m)) == 1 })

			tc.mutate(f) // DB change only — no SignalRevoked: the heartbeat path must classify it
			r.stream.in <- health(2)
			err := r.wait(t)
			assertStatus(t, err, tc.code, tc.name)
			assertMarker(t, r.stream, tc.marker, tc.name)
			if seqs := ackSeqs(r.stream.snapshot()); len(seqs) != 1 || seqs[0] != 1 {
				t.Fatalf("acks = %v, want only [1] (no ack for the refused report)", seqs)
			}
		})
	}
}

func TestAuthz_ActivationRevokedVsDeleted(t *testing.T) {
	f := newAuthzFixture(t, "active")
	c := f.client()
	f.revokeInDB(f.connID)
	if _, rej := f.h.activateStream(f.ctx, c); rej == nil || rej.code != codes.PermissionDenied || rej.marker != ConnectorStateRevoked {
		t.Fatalf("activation of revoked: %s, want PermissionDenied+revoked", describeRejection(rej))
	}

	g := newAuthzFixture(t, "active")
	d := g.client()
	if _, err := g.pool.Exec(g.ctx, `DELETE FROM connectors WHERE id=$1`, g.connID); err != nil {
		t.Fatal(err)
	}
	if _, rej := g.h.activateStream(g.ctx, d); rej == nil || rej.code != codes.NotFound || rej.marker != ConnectorStateUnknown {
		t.Fatalf("activation of deleted: %s, want NotFound+unknown (previously mislabelled revoked)", describeRejection(rej))
	}
	if g.h.Registry.get(g.connID) != nil {
		t.Fatal("deleted connector registered")
	}
}

// ── Fix 13 (F40): a deleted revoked connector stays revoked (041 tombstone) ──

func (f *authzFixture) deleteRow(connID string) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.ctx, `DELETE FROM connectors WHERE id=$1`, connID); err != nil {
		f.t.Fatalf("delete: %v", err)
	}
}

func TestAuthz_DeletedRevokedConnectorIsStillRevoked(t *testing.T) {
	t.Run("open", func(t *testing.T) {
		f := newAuthzFixture(t, "active")
		f.revokeInDB(f.connID)
		f.deleteRow(f.connID)
		r := f.openControl(f.connID)
		err := r.wait(t)
		assertStatus(t, err, codes.PermissionDenied, "open deleted revoked")
		assertMarker(t, r.stream, ConnectorStateRevoked, "open deleted revoked")
	})
	t.Run("mid-stream", func(t *testing.T) {
		f := newAuthzFixture(t, "active")
		r := f.openControl(f.connID)
		r.stream.in <- health(1)
		r.stream.waitFor(t, "ack 1", func(m []*pb.ConnectorControlMessage) bool { return len(ackSeqs(m)) == 1 })
		f.revokeInDB(f.connID)
		f.deleteRow(f.connID)
		r.stream.in <- health(2)
		err := r.wait(t)
		assertStatus(t, err, codes.PermissionDenied, "mid-stream deleted revoked")
		assertMarker(t, r.stream, ConnectorStateRevoked, "mid-stream deleted revoked")
	})
	t.Run("activation", func(t *testing.T) {
		f := newAuthzFixture(t, "active")
		c := f.client()
		f.revokeInDB(f.connID)
		f.deleteRow(f.connID)
		if _, rej := f.h.activateStream(f.ctx, c); rej == nil || rej.code != codes.PermissionDenied || rej.marker != ConnectorStateRevoked {
			t.Fatalf("activation of deleted revoked: %s, want PermissionDenied+revoked", describeRejection(rej))
		}
	})
}

func TestAuthz_TombstoneLookup(t *testing.T) {
	f := newAuthzFixture(t, "active")
	f.revokeInDB(f.connID)
	f.deleteRow(f.connID)
	_, otherTD, _ := seedRevocationWorkspace(t, f.pool, f.ctx, fmt.Sprintf("authz-other-%d", time.Now().UnixNano()))

	for _, tc := range []struct {
		name, id, td string
		want         connectorAuthState
	}{
		{"tombstoned, same trust domain", f.connID, f.td, connectorStateRevoked},
		{"tombstoned in another trust domain", f.connID, otherTD, connectorStateAbsent},
		{"never existed", "00000000-0000-0000-0000-000000000abc", f.td, connectorStateAbsent},
	} {
		state, _, err := f.h.lookupConnector(f.ctx, tc.id, tc.td)
		if err != nil || state != tc.want {
			t.Fatalf("%s: state=%v err=%v, want %v", tc.name, state, err, tc.want)
		}
	}

	// The tombstone query failing is undetermined, never "unknown".
	if _, err := f.pool.Exec(f.ctx, `ALTER TABLE connector_revocations RENAME TO connector_revocations_gone`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.h.lookupConnector(f.ctx, f.connID, f.td); err == nil {
		t.Fatal("tombstone query failed but the lookup reported a state")
	}
}

// ── T4 / T21 / T22: DB failure → Unavailable, never NotFound, never marked ──

func TestAuthz_OpenDBFailureIsUnavailableUnmarked(t *testing.T) {
	f := newAuthzFixture(t, "active")
	f.pool.Close() // every query now fails: state undetermined
	r := f.openControl(f.connID)
	err := r.wait(t)
	assertStatus(t, err, codes.Unavailable, "open with DB down")
	assertMarker(t, r.stream, "", "open with DB down")
	if st, _ := status.FromError(err); st.Code() == codes.NotFound {
		t.Fatal("DB failure reported as NotFound")
	}
}

func TestAuthz_FollowUpLookupFailureIsUnavailableUnmarked(t *testing.T) {
	lookupErr := errors.New("simulated pool failure")
	fail := func(context.Context, string, string) (connectorAuthState, connectorRecord, error) {
		return 0, connectorRecord{}, lookupErr
	}

	t.Run("health", func(t *testing.T) {
		f := newAuthzFixture(t, "active")
		f.revokeInDB(f.connID) // guarded UPDATE → ErrNoRows → follow-up lookup
		f.h.lookupOverride = fail
		c := f.client()
		rej := f.h.handleConnectorHealth(f.ctx, c, health(5).GetConnectorHealth())
		if rej == nil || rej.code != codes.Unavailable || rej.marker != "" {
			t.Fatalf("health follow-up failure: %s, want Unavailable, no marker", describeRejection(rej))
		}
		if got := drain(c); len(got) != 0 {
			t.Fatalf("messages enqueued: %v", kinds(got))
		}
	})
	t.Run("activation", func(t *testing.T) {
		f := newAuthzFixture(t, "active")
		f.revokeInDB(f.connID)
		f.h.lookupOverride = fail
		_, rej := f.h.activateStream(f.ctx, f.client())
		if rej == nil || rej.code != codes.Unavailable || rej.marker != "" {
			t.Fatalf("activation follow-up failure: %s, want Unavailable, no marker", describeRejection(rej))
		}
	})
	t.Run("follow-up finds an authorized row (inconsistent)", func(t *testing.T) {
		if rej := rejectionFor(connectorStateAuthorized, nil); rej.code != codes.Unavailable || rej.marker != "" {
			t.Fatalf("inconsistent follow-up: %s, want Unavailable, no marker", describeRejection(rej))
		}
	})
}

// A DB error on the guarded health UPDATE itself (not ErrNoRows): the stream
// stays open, nothing is pushed or acknowledged, the next report retries.
// Checked both with a WARM ACL cache (an ACL push would succeed without the
// database) and with no ACL infrastructure (push is a no-op), so only the
// DB-error branch itself can be what withholds the push and the ack.
func TestAuthz_HealthUpdateDBErrorNoACLNoAck(t *testing.T) {
	t.Run("warm ACL cache", func(t *testing.T) {
		f := newAuthzFixture(t, "active").withACL()
		c := f.client()
		if rej := f.h.handleConnectorHealth(f.ctx, c, health(8).GetConnectorHealth()); rej != nil {
			t.Fatalf("warm-up health refused: %s", describeRejection(rej))
		}
		if got := drain(c); indexOf(got, isACL) < 0 || indexOf(got, isAck) < 0 {
			t.Fatalf("warm-up did not push+ack: %v", kinds(got))
		}
		f.pool.Close()
		if rej := f.h.handleConnectorHealth(f.ctx, c, health(9).GetConnectorHealth()); rej != nil {
			t.Fatalf("DB error closed the stream: %s (must stay open and retry)", describeRejection(rej))
		}
		if got := drain(c); len(got) != 0 {
			t.Fatalf("enqueued %v on a DB error, want nothing (no ACL push, no ack)", kinds(got))
		}
	})
	t.Run("no ACL infrastructure", func(t *testing.T) {
		f := newAuthzFixture(t, "active")
		c := f.client()
		f.pool.Close()
		if rej := f.h.handleConnectorHealth(f.ctx, c, health(9).GetConnectorHealth()); rej != nil {
			t.Fatalf("DB error closed the stream: %s", describeRejection(rej))
		}
		if got := drain(c); len(got) != 0 {
			t.Fatalf("enqueued %v on a DB error, want nothing", kinds(got))
		}
	})
}

// SignalRevoked is atomic with stream activation: while an activation holds
// lifecycleMu (guarded UPDATE done, registration pending), SignalRevoked
// waits, then reaches the newly registered stream. Without the lock it would
// snapshot the live set before registration and miss the stream.
func TestAuthz_SignalRevokedWaitsForInFlightActivation(t *testing.T) {
	r := NewConnectorRegistry()
	c := &connectorStreamClient{
		outbound:     make(chan *pb.ConnectorControlMessage, 1),
		connectorID:  "c1",
		terminal:     make(chan *pb.ConnectorControlMessage, 1),
		terminalSent: make(chan struct{}),
	}
	r.lifecycleMu.Lock() // activation in flight: UPDATE succeeded, not yet registered
	done := make(chan int, 1)
	go func() { done <- r.SignalRevoked("c1") }()
	select {
	case n := <-done:
		r.lifecycleMu.Unlock()
		t.Fatalf("SignalRevoked returned (%d) during an in-flight activation", n)
	case <-time.After(100 * time.Millisecond):
	}
	r.add("c1", c)
	r.trackLive("c1", c) // activation completes registration
	r.lifecycleMu.Unlock()
	select {
	case n := <-done:
		if n != 1 {
			t.Fatalf("SignalRevoked queued on %d streams, want 1 (the just-activated one)", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SignalRevoked never returned")
	}
	if len(c.terminal) != 1 || !c.revoked {
		t.Fatal("the just-activated stream did not receive ConnectorRevoked")
	}
}

// Markers are only ever produced by a successful lookup.
func TestAuthz_MarkerOnlyAfterSuccessfulLookup(t *testing.T) {
	if rej := rejectionFor(connectorStateRevoked, errors.New("x")); rej.marker != "" || rej.code != codes.Unavailable {
		t.Fatalf("errored lookup produced %s", describeRejection(rej))
	}
	if rej := rejectionFor(connectorStateAbsent, errors.New("x")); rej.marker != "" || rej.code != codes.Unavailable {
		t.Fatalf("errored lookup produced %s", describeRejection(rej))
	}
	if rej := rejectUnavailable(); rej.marker != "" {
		t.Fatal("Unavailable carries a marker")
	}
	if rej := rejectMissingSeq(); rej.marker != "" || rej.code != codes.FailedPrecondition {
		t.Fatalf("missing seq: %s", describeRejection(rej))
	}
}

// ── T6 (controller side) / T9: seq = 0 ──────────────────────────────────────

func TestAuthz_ZeroSeqFailedPreconditionNoAckNoWrite(t *testing.T) {
	f := newAuthzFixture(t, "disconnected")
	r := f.openControl(f.connID)
	f.waitLive(f.connID, 1)
	before := f.status(f.connID) // 'active' after activation
	var hbBefore time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT last_heartbeat_at FROM connectors WHERE id=$1`, f.connID).Scan(&hbBefore); err != nil {
		t.Fatal(err)
	}
	r.stream.in <- health(0)
	err := r.wait(t)
	assertStatus(t, err, codes.FailedPrecondition, "seq=0")
	assertMarker(t, r.stream, "", "seq=0")
	if n := len(ackSeqs(r.stream.snapshot())); n != 0 {
		t.Fatalf("seq=0 acknowledged (%d acks)", n)
	}
	var hbAfter time.Time
	if err := f.pool.QueryRow(f.ctx, `SELECT last_heartbeat_at FROM connectors WHERE id=$1`, f.connID).Scan(&hbAfter); err != nil {
		t.Fatal(err)
	}
	if !hbAfter.Equal(hbBefore) || before != "active" {
		t.Fatalf("seq=0 report touched the row: heartbeat %v → %v (status before %q)", hbBefore, hbAfter, before)
	}
}

// ── T8: capability is the first message on every stream ─────────────────────

func TestAuthz_AgentPolicyCapabilityFirstOnStream(t *testing.T) {
	f := newAuthzFixture(t, "active") // no shield renewal window configured
	r := f.openControl(f.connID)
	r.stream.in <- health(1)
	got := r.stream.waitFor(t, "ack", func(m []*pb.ConnectorControlMessage) bool { return len(ackSeqs(m)) == 1 })
	if p := got[0].GetAgentPolicy(); p == nil || !p.GetAuthorizationAcks() {
		t.Fatalf("first message = %v, want AgentPolicy{authorization_acks:true}", kinds(got))
	}
	if indexOf(got, isAck) <= 0 {
		t.Fatal("ack not after the capability")
	}
}

// ── T5: explicit live revocation ────────────────────────────────────────────

func TestAuthz_SignalRevokedDeliversOnLiveStreamAndCloses(t *testing.T) {
	f := newAuthzFixture(t, "active")
	r := f.openControl(f.connID)
	r.stream.in <- health(1)
	r.stream.waitFor(t, "ack 1", func(m []*pb.ConnectorControlMessage) bool { return len(ackSeqs(m)) == 1 })

	f.revokeInDB(f.connID)
	if n := f.h.Registry.SignalRevoked(f.connID); n != 1 {
		t.Fatalf("SignalRevoked queued on %d streams, want 1", n)
	}
	err := r.wait(t)
	assertStatus(t, err, codes.PermissionDenied, "after ConnectorRevoked")
	assertMarker(t, r.stream, ConnectorStateRevoked, "after ConnectorRevoked")
	got := r.stream.snapshot()
	i := indexOf(got, isRevokedEvent)
	if i < 0 {
		t.Fatalf("no ConnectorRevoked on the live stream: %v", kinds(got))
	}
	if i != len(got)-1 {
		t.Fatalf("messages after ConnectorRevoked: %v", kinds(got[i:]))
	}
}

// T5 race: revocation while a make-before-break renewal has two live streams.
// The connector may still be reading the superseded stream A (it switches only
// once B's open completes), so BOTH must receive ConnectorRevoked.
func TestAuthz_RevokeDuringRenewalReachesSupersededAndNewStream(t *testing.T) {
	f := newAuthzFixture(t, "active")
	a := f.openControl(f.connID)
	f.waitLive(f.connID, 1)
	b := f.openControl(f.connID) // replacement registers; A is superseded but live
	f.waitLive(f.connID, 2)
	if f.h.Registry.get(f.connID) == nil {
		t.Fatal("no authoritative stream")
	}

	f.revokeInDB(f.connID)
	if n := f.h.Registry.SignalRevoked(f.connID); n != 2 {
		t.Fatalf("SignalRevoked queued on %d streams, want 2 (old + replacement)", n)
	}
	for name, r := range map[string]*controlRun{"superseded A": a, "replacement B": b} {
		err := r.wait(t)
		assertStatus(t, err, codes.PermissionDenied, name)
		assertMarker(t, r.stream, ConnectorStateRevoked, name)
		if indexOf(r.stream.snapshot(), isRevokedEvent) < 0 {
			t.Fatalf("%s did not receive ConnectorRevoked: %v", name, kinds(r.stream.snapshot()))
		}
	}
	// Any later stream is refused with the revoked marker at open.
	c := f.openControl(f.connID)
	assertStatus(t, c.wait(t), codes.PermissionDenied, "reopen after revoke")
	assertMarker(t, c.stream, ConnectorStateRevoked, "reopen after revoke")
}

// T5 race: revocation racing stream activation. Every stream either receives
// ConnectorRevoked or is refused with the revoked marker — none ends up live
// and unaware.
func TestAuthz_RevokeRacingActivation(t *testing.T) {
	for i := 0; i < 15; i++ {
		f := newAuthzFixture(t, "active")
		old := f.openControl(f.connID)
		f.waitLive(f.connID, 1)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var repl *controlRun
		wg.Add(2)
		go func() { defer wg.Done(); <-start; repl = f.openControl(f.connID) }()
		go func() {
			defer wg.Done()
			<-start
			f.revokeInDB(f.connID)
			f.h.Registry.SignalRevoked(f.connID)
		}()
		close(start)
		wg.Wait()

		for name, r := range map[string]*controlRun{"old": old, "replacement": repl} {
			err := r.wait(t)
			assertStatus(t, err, codes.PermissionDenied, fmt.Sprintf("iter %d %s", i, name))
			assertMarker(t, r.stream, ConnectorStateRevoked, fmt.Sprintf("iter %d %s", i, name))
			got := r.stream.snapshot()
			// Either it was refused at open/activation (never activated: no
			// messages at all) or it was live and received ConnectorRevoked.
			if len(got) != 0 && indexOf(got, isRevokedEvent) < 0 {
				t.Fatalf("iter %d %s: activated stream ended without ConnectorRevoked: %v", i, name, kinds(got))
			}
		}
	}
}

// T5 race: revocation racing heartbeats. No AuthorizationAck ever follows
// ConnectorRevoked on the wire.
func TestAuthz_RevokeRacingHeartbeatsNoAckAfterEvent(t *testing.T) {
	for i := 0; i < 10; i++ {
		f := newAuthzFixture(t, "active")
		r := f.openControl(f.connID)
		f.waitLive(f.connID, 1)
		go func() {
			for s := uint64(1); s <= 12; s++ {
				select {
				case r.stream.in <- health(s):
				case <-r.stream.ctx.Done():
					return
				}
			}
		}()
		time.Sleep(time.Duration(i) * time.Millisecond)
		f.revokeInDB(f.connID)
		f.h.Registry.SignalRevoked(f.connID)
		_ = r.wait(t)
		got := r.stream.snapshot()
		ev := indexOf(got, isRevokedEvent)
		if ev < 0 {
			// The health path refused first (revoked marker) — also correct.
			assertMarker(t, r.stream, ConnectorStateRevoked, "health-path close")
			continue
		}
		for _, m := range got[ev+1:] {
			if isAck(m) {
				t.Fatalf("iter %d: AuthorizationAck after ConnectorRevoked: %v", i, kinds(got))
			}
		}
	}
}

// T5: ConnectorRevoked can't be dropped by a full outbound mailbox and goes
// out before anything still queued; a later ack is refused.
func TestAuthz_RevokedEventBypassesFullMailbox(t *testing.T) {
	s := newScriptedStream(context.Background())
	c := &connectorStreamClient{
		stream:       s,
		outbound:     make(chan *pb.ConnectorControlMessage, 2),
		connectorID:  "c1",
		terminal:     make(chan *pb.ConnectorControlMessage, 1),
		terminalSent: make(chan struct{}),
	}
	c.outbound <- &pb.ConnectorControlMessage{Body: &pb.ConnectorControlMessage_Ping{}}
	c.outbound <- &pb.ConnectorControlMessage{Body: &pb.ConnectorControlMessage_Ping{}}
	if !c.markRevoked() {
		t.Fatal("markRevoked did not queue the event on a full mailbox")
	}
	if c.markRevoked() {
		t.Fatal("second markRevoked queued another event")
	}
	if err := c.sendAuthorizationAck(1); !errors.Is(err, errStreamRevoked) {
		t.Fatalf("ack after revocation: err=%v, want errStreamRevoked", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.runWriter(ctx)
	select {
	case <-c.terminalSent:
	case <-time.After(5 * time.Second):
		t.Fatal("terminal not sent")
	}
	got := s.snapshot()
	if len(got) != 1 || !isRevokedEvent(got[0]) {
		t.Fatalf("writer sent %v, want ConnectorRevoked first and only", kinds(got))
	}
}

// SignalRevoked for a connector with no live stream is a no-op.
func TestAuthz_SignalRevokedNoLiveStream(t *testing.T) {
	if n := NewConnectorRegistry().SignalRevoked("nobody"); n != 0 {
		t.Fatalf("queued on %d streams", n)
	}
}

// T10 (controller side): make-before-break renewal produces no error, no
// marker and acks on both overlapping streams; retiring the superseded stream
// keeps the replacement live.
func TestAuthz_RenewalOverlapAcksBothStreamsNoMarker(t *testing.T) {
	f := newAuthzFixture(t, "active")
	a := f.openControl(f.connID)
	f.waitLive(f.connID, 1)
	b := f.openControl(f.connID)
	f.waitLive(f.connID, 2)

	a.stream.in <- health(10) // the outstanding seq, delivered on the old stream
	b.stream.in <- health(10) // and re-sent on the replacement (same seq)
	a.stream.waitFor(t, "ack on A", func(m []*pb.ConnectorControlMessage) bool { return len(ackSeqs(m)) == 1 })
	b.stream.waitFor(t, "ack on B", func(m []*pb.ConnectorControlMessage) bool { return len(ackSeqs(m)) == 1 })

	a.stream.close() // connector switches and drops A
	if err := a.wait(t); err != nil {
		t.Fatalf("superseded stream ended with %v, want clean EOF", err)
	}
	assertMarker(t, a.stream, "", "superseded close")
	f.waitLive(f.connID, 1)
	if f.status(f.connID) != "active" {
		t.Fatalf("status %q after superseded close, want active", f.status(f.connID))
	}
	b.stream.in <- health(11)
	b.stream.waitFor(t, "ack 11 on B", func(m []*pb.ConnectorControlMessage) bool { return len(ackSeqs(m)) == 2 })
}
