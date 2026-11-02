package client

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/changehint"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/transport"
)

// FU-8 WatchChanges: unit tests with real notifiers and hub, a fake token
// check and device gate, and an in-memory stream.

const hintWS = "11111111-1111-1111-1111-111111111111"

type hintAuth struct {
	postureTestAuth
}

func (a *hintAuth) VerifyAccessToken(token string) (*auth.AccessTokenClaims, error) {
	if token != "good" {
		return nil, errors.New("bad signature")
	}
	return a.claims, nil
}

type hintStream struct {
	grpc.ServerStream
	ctx   context.Context
	hints chan *clientv1.ChangeHint
	sends atomic.Int32
	// onSend runs inside Send before the hint is delivered (n = 1 for the first).
	onSend func(n int32)
}

func (s *hintStream) Context() context.Context { return s.ctx }

func (s *hintStream) Send(h *clientv1.ChangeHint) error {
	n := s.sends.Add(1)
	if s.onSend != nil {
		s.onSend(n)
	}
	s.hints <- h
	return nil
}

type hintRig struct {
	t         *testing.T
	svc       *Service
	hub       *changehint.Hub
	acl       *policy.Notifier
	transport *transport.Notifier
	shutdown  chan struct{}
	directive atomic.Int32
	gateErr   atomic.Bool
	claims    *auth.AccessTokenClaims
}

func newHintRig(t *testing.T) *hintRig {
	t.Helper()
	r := &hintRig{
		t:         t,
		hub:       changehint.New(),
		acl:       policy.NewNotifier(policy.NewSnapshotCache()),
		transport: transport.NewNotifier(transport.NewSnapshotCache()),
		shutdown:  make(chan struct{}),
		claims:    &auth.AccessTokenClaims{UserID: "u1", TenantID: hintWS, ExpiresAt: time.Now().Add(time.Hour)},
	}
	r.acl.AddChangeListener(r.hub.Notify)
	r.transport.AddChangeListener(r.hub.Notify)
	r.svc = &Service{authSvc: &hintAuth{postureTestAuth{claims: r.claims}}, policyNotifier: r.acl}
	r.svc.EnableChangeHints(r.hub, r.transport, r.shutdown)
	r.svc.hints.gate = func(context.Context, string, *auth.AccessTokenClaims) (clientv1.DeviceDirective, string, error) {
		if r.gateErr.Load() {
			return 0, "", errors.New("device not found")
		}
		return clientv1.DeviceDirective(r.directive.Load()), "", nil
	}
	return r
}

// open starts WatchChanges for device and returns its stream and result.
func (r *hintRig) open(ctx context.Context, device string, onSend func(int32)) (*hintStream, <-chan error) {
	s := &hintStream{ctx: ctx, hints: make(chan *clientv1.ChangeHint, 64), onSend: onSend}
	done := make(chan error, 1)
	go func() {
		done <- r.svc.WatchChanges(&clientv1.WatchChangesRequest{AccessToken: "good", DeviceId: device}, s)
	}()
	return s, done
}

func (r *hintRig) next(s *hintStream) *clientv1.ChangeHint {
	r.t.Helper()
	select {
	case h := <-s.hints:
		return h
	case <-time.After(5 * time.Second):
		r.t.Fatal("no hint within 5 s")
		return nil
	}
}

func (r *hintRig) none(s *hintStream) {
	r.t.Helper()
	select {
	case h := <-s.hints:
		r.t.Fatalf("unexpected hint %v", h)
	case <-time.After(150 * time.Millisecond):
	}
}

func (r *hintRig) ended(done <-chan error, want codes.Code) {
	r.t.Helper()
	select {
	case err := <-done:
		if status.Code(err) != want {
			r.t.Fatalf("stream ended with %v (%v), want %v", status.Code(err), err, want)
		}
	case <-time.After(5 * time.Second):
		r.t.Fatalf("stream did not end (want %v)", want)
	}
	if n := r.hub.Len(); n != 0 {
		r.t.Fatalf("%d subscriptions left after the stream ended", n)
	}
}

func (r *hintRig) policyChange() {
	if err := r.acl.NotifyPolicyChange(context.Background(), hintWS); err != nil {
		r.t.Fatal(err)
	}
}

func (r *hintRig) topologyChange() {
	if err := r.transport.NotifyTopologyChange(context.Background(), hintWS, []string{"c1"}); err != nil {
		r.t.Fatal(err)
	}
}

func want(t *testing.T, h *clientv1.ChangeHint, acl, tr uint64) {
	t.Helper()
	if h.GetAclVersion() != acl || h.GetTransportVersion() != tr {
		t.Fatalf("hint = acl %d transport %d, want acl %d transport %d",
			h.GetAclVersion(), h.GetTransportVersion(), acl, tr)
	}
}

func TestWatchChanges_RefusesBadCallers(t *testing.T) {
	call := func(svc *Service, req *clientv1.WatchChangesRequest) error {
		return svc.WatchChanges(req, &hintStream{ctx: context.Background(), hints: make(chan *clientv1.ChangeHint, 1)})
	}
	good := &clientv1.WatchChangesRequest{AccessToken: "good", DeviceId: "dev"}

	if err := call(&Service{}, good); status.Code(err) != codes.Unimplemented {
		t.Fatalf("hints off: %v, want Unimplemented", err)
	}
	r := newHintRig(t)
	cases := []struct {
		name  string
		req   *clientv1.WatchChangesRequest
		setup func()
		code  codes.Code
	}{
		{"no token", &clientv1.WatchChangesRequest{DeviceId: "dev"}, nil, codes.InvalidArgument},
		{"no device", &clientv1.WatchChangesRequest{AccessToken: "good"}, nil, codes.InvalidArgument},
		{"bad token", &clientv1.WatchChangesRequest{AccessToken: "forged", DeviceId: "dev"}, nil, codes.Unauthenticated},
		{"unknown or foreign device", good, func() { r.gateErr.Store(true) }, codes.PermissionDenied},
		{"revoked device", good, func() { r.directive.Store(int32(clientv1.DeviceDirective_DIRECTIVE_REVOKED)) }, codes.FailedPrecondition},
		{"re-enroll required", good, func() { r.directive.Store(int32(clientv1.DeviceDirective_DIRECTIVE_RE_ENROLL_REQUIRED)) }, codes.FailedPrecondition},
		{"token already expired", good, func() { r.claims.ExpiresAt = time.Now().Add(-time.Second) }, codes.Unauthenticated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r.gateErr.Store(false)
			r.directive.Store(0)
			r.claims.ExpiresAt = time.Now().Add(time.Hour)
			if c.setup != nil {
				c.setup()
			}
			if err := call(r.svc, c.req); status.Code(err) != c.code {
				t.Fatalf("got %v (%v), want %v", status.Code(err), err, c.code)
			}
			if r.hub.Len() != 0 {
				t.Fatal("a refused stream left a subscription")
			}
		})
	}
}

// The first hint carries the versions current at open (changes made before
// the stream existed are not lost), then each change sends the new versions,
// each plane in its own field.
func TestWatchChanges_SendsCurrentVersionsThenEachChange(t *testing.T) {
	r := newHintRig(t)
	r.policyChange()
	r.policyChange()
	r.topologyChange()
	ctx, cancel := context.WithCancel(context.Background())
	s, done := r.open(ctx, "dev", nil)

	want(t, r.next(s), 2, 1)
	r.policyChange()
	want(t, r.next(s), 3, 1)
	r.topologyChange()
	want(t, r.next(s), 3, 2)
	// A wake with no version change sends nothing (no duplicate hint).
	r.hub.Notify(hintWS)
	r.none(s)

	cancel()
	r.ended(done, codes.Canceled)
}

// The subscription exists before the versions are read: a change landing
// right after the first read (here: while the first hint is being sent) still
// produces a hint with the new version.
func TestWatchChanges_ChangeRightAfterTheFirstReadIsNotMissed(t *testing.T) {
	r := newHintRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, _ := r.open(ctx, "dev", func(n int32) {
		if n == 1 {
			r.policyChange()
		}
	})
	want(t, r.next(s), 0, 0)
	want(t, r.next(s), 1, 0)
}

// Changes made while a send is stuck collapse into one hint with the latest
// versions; nothing queues per change.
func TestWatchChanges_BurstCollapsesToTheLatestVersion(t *testing.T) {
	r := newHintRig(t)
	release := make(chan struct{})
	var once sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, _ := r.open(ctx, "dev", func(n int32) {
		if n == 2 {
			once.Do(func() {
				for i := 0; i < 9; i++ {
					r.policyChange()
				}
				<-release
			})
		}
	})
	want(t, r.next(s), 0, 0)
	r.policyChange() // wakes send #2 (version 1), which then sees 9 more changes
	close(release)
	want(t, r.next(s), 1, 0)
	want(t, r.next(s), 10, 0)
	r.none(s)
	if got := s.sends.Load(); got != 3 {
		t.Fatalf("sends = %d, want 3 (initial, first change, one for the burst)", got)
	}
}

func TestWatchChanges_HeartbeatAndRevokeWhileOpen(t *testing.T) {
	r := newHintRig(t)
	r.svc.hints.recheck = 30 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s, done := r.open(ctx, "dev", nil)
	want(t, r.next(s), 0, 0)
	want(t, r.next(s), 0, 0) // heartbeat repeats the current versions

	r.directive.Store(int32(clientv1.DeviceDirective_DIRECTIVE_REVOKED))
	r.ended(done, codes.FailedPrecondition)
}

func TestWatchChanges_EndsAtTokenExpiry(t *testing.T) {
	r := newHintRig(t)
	r.claims.ExpiresAt = time.Now().Add(150 * time.Millisecond)
	s, done := r.open(context.Background(), "dev", nil)
	want(t, r.next(s), 0, 0)
	r.ended(done, codes.Unauthenticated)
}

func TestWatchChanges_EndsOnControllerShutdown(t *testing.T) {
	r := newHintRig(t)
	s, done := r.open(context.Background(), "dev", nil)
	want(t, r.next(s), 0, 0)
	close(r.shutdown)
	r.ended(done, codes.Unavailable)
}

func TestWatchChanges_NewStreamOfTheSameDeviceReplacesTheOld(t *testing.T) {
	r := newHintRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old, oldDone := r.open(ctx, "dev", nil)
	want(t, r.next(old), 0, 0)
	cur, curDone := r.open(ctx, "dev", nil)
	want(t, r.next(cur), 0, 0)
	select {
	case err := <-oldDone:
		if status.Code(err) != codes.Aborted {
			t.Fatalf("old stream ended with %v, want Aborted", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("old stream not ended by the reconnect")
	}
	r.policyChange()
	want(t, r.next(cur), 1, 0)
	if r.hub.Len() != 1 {
		t.Fatalf("subscriptions = %d, want 1", r.hub.Len())
	}
	cancel()
	r.ended(curDone, codes.Canceled)
}

// RENEW_SOON is routine (every certificate passes through it while renewing):
// the stream opens and stays open, and keeps delivering hints.
func TestWatchChanges_RenewSoonKeepsTheStream(t *testing.T) {
	r := newHintRig(t)
	r.svc.hints.recheck = 30 * time.Millisecond
	r.directive.Store(int32(clientv1.DeviceDirective_DIRECTIVE_RENEW_SOON))
	ctx, cancel := context.WithCancel(context.Background())
	s, done := r.open(ctx, "dev", nil)
	want(t, r.next(s), 0, 0)
	want(t, r.next(s), 0, 0) // survived a re-check
	r.policyChange()
	for h := r.next(s); h.GetAclVersion() != 1; h = r.next(s) {
	}
	select {
	case err := <-done:
		t.Fatalf("stream ended during RENEW_SOON: %v", err)
	default:
	}
	cancel()
	r.ended(done, codes.Canceled)
}

// A revoke notifies the policy plane; the wake re-checks the device and ends
// the stream at once, without waiting for the periodic re-check, and sends
// nothing more to the revoked device.
func TestWatchChanges_RevokeEndsTheStreamOnTheWake(t *testing.T) {
	r := newHintRig(t) // recheck stays at 60 s: only the wake can end it in time
	s, done := r.open(context.Background(), "dev", nil)
	want(t, r.next(s), 0, 0)
	r.directive.Store(int32(clientv1.DeviceDirective_DIRECTIVE_REVOKED))
	r.policyChange()
	r.ended(done, codes.FailedPrecondition)
	r.none(s)
}
