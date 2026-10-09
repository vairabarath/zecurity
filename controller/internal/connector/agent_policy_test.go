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

func TestAgentPolicyMessage(t *testing.T) {
	cases := []struct {
		window   time.Duration
		wantSecs uint64 // 0 = no message
	}{
		{48 * time.Hour, 172800},
		{24 * time.Minute, 1440},
		{1500 * time.Millisecond, 1}, // whole seconds only
		{500 * time.Millisecond, 0},  // rounds to 0: not sent
		{0, 0},
		{-time.Hour, 0},
	}
	for _, tc := range cases {
		h := &EnrollmentHandler{Cfg: Config{ShieldRenewalWindow: tc.window}}
		msg := h.agentPolicyMessage()
		if tc.wantSecs == 0 {
			if msg != nil {
				t.Errorf("window %s: got %v, want no AgentPolicy", tc.window, msg)
			}
			continue
		}
		body, ok := msg.GetBody().(*pb.ConnectorControlMessage_AgentPolicy)
		if !ok || body.AgentPolicy.GetShieldRenewalWindowSecs() != tc.wantSecs {
			t.Errorf("window %s: got %v, want shield_renewal_window_secs=%d", tc.window, msg, tc.wantSecs)
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
	default:
		t.Fatal("no AgentPolicy enqueued")
	}

	none := testClient("c2", "ws")
	(&EnrollmentHandler{}).sendAgentPolicy(none)
	if len(none.outbound) != 0 {
		t.Fatal("AgentPolicy enqueued without a configured window")
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
