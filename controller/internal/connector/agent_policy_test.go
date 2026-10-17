package connector

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/spiffe"
)

// Sprint 21 K2: AgentPolicy carries the shield renewal window to connectors.

// Sprint 20 connector-revocation Phase A (design §4.6): AgentPolicy is ALWAYS
// produced and always asserts the authorization-contract capability; an unset
// shield renewal window no longer suppresses the whole message (that would
// silently drop the capability).
func TestAgentPolicyMessage(t *testing.T) {
	cases := []struct {
		window   time.Duration
		wantSecs uint64 // 0 = window "not set" (connector keeps its default)
	}{
		{48 * time.Hour, 172800},
		{24 * time.Minute, 1440},
		{1500 * time.Millisecond, 1}, // whole seconds only
		{500 * time.Millisecond, 0},  // rounds to 0: window not set
		{0, 0},
		{-time.Hour, 0},
	}
	for _, tc := range cases {
		h := &EnrollmentHandler{Cfg: Config{ShieldRenewalWindow: tc.window}}
		msg := h.agentPolicyMessage()
		body, ok := msg.GetBody().(*pb.ConnectorControlMessage_AgentPolicy)
		if !ok {
			t.Fatalf("window %s: got %v, want an AgentPolicy", tc.window, msg)
		}
		if got := body.AgentPolicy.GetShieldRenewalWindowSecs(); got != tc.wantSecs {
			t.Errorf("window %s: shield_renewal_window_secs=%d, want %d", tc.window, got, tc.wantSecs)
		}
		if !body.AgentPolicy.GetAuthorizationAcks() {
			t.Errorf("window %s: authorization_acks=false, want true (capability must not depend on the window)", tc.window)
		}
	}
}

func TestSendAgentPolicy_EnqueuesOnTheStream(t *testing.T) {
	h := &EnrollmentHandler{Cfg: Config{ShieldRenewalWindow: 24 * time.Minute}}
	client := testClient("c1", "ws")
	h.sendAgentPolicy(client)
	select {
	case msg := <-client.outbound:
		if got := msg.GetAgentPolicy().GetShieldRenewalWindowSecs(); got != 1440 {
			t.Fatalf("shield_renewal_window_secs = %d, want 1440", got)
		}
		if !msg.GetAgentPolicy().GetAuthorizationAcks() {
			t.Fatal("authorization_acks = false, want true")
		}
	default:
		t.Fatal("no AgentPolicy enqueued")
	}

	// T8: no shield window configured — the capability is still sent.
	none := testClient("c2", "ws")
	(&EnrollmentHandler{}).sendAgentPolicy(none)
	select {
	case msg := <-none.outbound:
		p := msg.GetAgentPolicy()
		if p == nil || !p.GetAuthorizationAcks() || p.GetShieldRenewalWindowSecs() != 0 {
			t.Fatalf("no-window AgentPolicy = %v, want authorization_acks=true, window 0", msg)
		}
	default:
		t.Fatal("AgentPolicy (capability) omitted when no shield window is configured")
	}
}

// recordingControlServer is a held Control stream that records what the
// controller sends.
type recordingControlServer struct {
	*heldControlServer
	mu   sync.Mutex
	sent []*pb.ConnectorControlMessage
}

func (s *recordingControlServer) Send(msg *pb.ConnectorControlMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, msg)
	return nil
}

func (s *recordingControlServer) agentPolicies() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []uint64
	for _, m := range s.sent {
		if p := m.GetAgentPolicy(); p != nil {
			out = append(out, p.GetShieldRenewalWindowSecs())
		}
	}
	return out
}

// The real Control handler sends exactly one AgentPolicy when a stream opens.
func TestControl_SendsAgentPolicyOnStreamOpen(t *testing.T) {
	f := newLifecycleFixture(t, "pending")
	f.h.Cfg.ShieldRenewalWindow = 24 * time.Minute

	sctx, cancel := context.WithCancel(spiffe.WithIdentity(f.ctx,
		fmt.Sprintf("spiffe://%s/connector/%s", f.td, f.connID), "connector", f.connID, f.td))
	defer cancel()
	stream := &recordingControlServer{heldControlServer: newHeldStream(sctx)}
	done := make(chan error, 1)
	go func() { done <- f.h.Control(stream) }()

	deadline := time.Now().Add(5 * time.Second)
	for len(stream.agentPolicies()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := stream.agentPolicies(); len(got) != 1 || got[0] != 1440 {
		t.Fatalf("AgentPolicy sent on stream open = %v, want exactly [1440]", got)
	}

	stream.close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Control did not return after EOF")
	}
}
