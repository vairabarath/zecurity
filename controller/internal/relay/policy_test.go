package relay

import (
	"errors"
	"testing"
	"time"

	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// Relay pools PR 3 (D-40): every heartbeat response, throttled or not, carries
// the workspace policy of the relay's pool with the lease, and the relay's
// report is recorded. A policy lookup failure sends no policy (version 0), so
// a controller error never tells a residency relay that nothing is enforced.

func TestHeartbeat_SendsWorkspacePolicyOnBothPaths(t *testing.T) {
	rdb := newProvisionTestValkey(t)
	store := &countingHeartbeatStore{fakeHeartbeatStore: &fakeHeartbeatStore{
		policy: WorkspacePolicy{Enforced: true, Version: 42, Workspace: []string{"ws-a", "ws-b"}},
	}}
	service := NewService(nil, store, time.Hour).WithHeartbeatCache(rdb, 5*time.Minute).WithPolicyLease(7 * time.Minute)
	hbCtx := relayHeartbeatContext(t, appmeta.SPIFFERoleRelay, testRelayID, testRelayID, time.Now().UTC().Add(time.Hour))
	req := &relaypb.HeartbeatRequest{Version: "1.0.0", Hostname: "relay-a",
		WorkspacePolicySupported: true, WorkspacePolicyVersionApplied: 41}

	full, err := service.Heartbeat(hbCtx, req)
	if err != nil {
		t.Fatal(err)
	}
	throttled, err := service.Heartbeat(hbCtx, req)
	if err != nil {
		t.Fatal(err)
	}
	if store.recordCalls != 1 {
		t.Fatalf("expected one full and one throttled beat, recordCalls=%d", store.recordCalls)
	}
	for name, resp := range map[string]*relaypb.HeartbeatResponse{"full": full, "throttled": throttled} {
		if !resp.WorkspacePolicyEnforced || resp.WorkspacePolicyVersion != 42 ||
			len(resp.AllowedWorkspaceIds) != 2 || resp.WorkspacePolicyLeaseSeconds != 420 {
			t.Errorf("%s path: policy enforced=%v version=%d allowed=%v lease=%d",
				name, resp.WorkspacePolicyEnforced, resp.WorkspacePolicyVersion, resp.AllowedWorkspaceIds, resp.WorkspacePolicyLeaseSeconds)
		}
	}
	if len(store.acks) != 2 || store.acks[0] != (policyAck{true, 41}) {
		t.Fatalf("acks %v, want the relay's report on each beat", store.acks)
	}
}

func TestHeartbeat_PolicyLookupFailureSendsNoPolicy(t *testing.T) {
	store := &fakeHeartbeatStore{policyErr: errors.New("db down")}
	service := NewService(nil, store, time.Hour)
	hbCtx := relayHeartbeatContext(t, appmeta.SPIFFERoleRelay, testRelayID, testRelayID, time.Now().UTC().Add(time.Hour))
	resp, err := service.Heartbeat(hbCtx, &relaypb.HeartbeatRequest{Version: "1.0.0"})
	if err != nil {
		t.Fatalf("a policy lookup failure must not fail the heartbeat: %v", err)
	}
	if resp.WorkspacePolicyVersion != 0 || resp.WorkspacePolicyEnforced || len(resp.AllowedWorkspaceIds) != 0 || resp.WorkspacePolicyLeaseSeconds != 0 {
		t.Fatalf("lookup failure must send no policy (version 0, nothing else), got %+v", resp)
	}
}

func TestPolicyLeaseMinimumCoversFourHeartbeats(t *testing.T) {
	if MinPolicyLease != 4*defaultHeartbeatInterval || DefaultPolicyLease < MinPolicyLease {
		t.Fatalf("lease bounds: min %s default %s", MinPolicyLease, DefaultPolicyLease)
	}
}
