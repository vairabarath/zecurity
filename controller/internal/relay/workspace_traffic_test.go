package relay

import (
	"context"
	"errors"
	"testing"
	"time"

	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// Sprint 24 R1 (D-54): per-workspace relay traffic, same window and restart
// rules as the relay totals (B2).

const (
	wsA = "aaaaaaaa-0000-0000-0000-00000000000a"
	wsB = "bbbbbbbb-0000-0000-0000-00000000000b"
)

var t0 = time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)

func onlyRelay(t *testing.T, tr *WorkspaceTrafficTracker) RelayWorkspaceTraffic {
	t.Helper()
	snap := tr.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot has %d relays, want 1", len(snap))
	}
	return snap[0]
}

func wantRate(t *testing.T, label string, got *float64, want float64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

func TestWorkspaceTraffic_RateFromTwoSamplesPerWorkspace(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{
		wsA: {ToConnector: 100, ToClient: 1000, ActiveStreams: 1},
		wsB: {ToConnector: 0, ToClient: 0, ActiveStreams: 2},
	})
	first := onlyRelay(t, tr)
	if first.WindowSeconds != 0 || first.Workspaces[wsA].ToClientBytesPerSec != nil {
		t.Fatalf("a first sample has no rate: %+v", first)
	}
	if first.Workspaces[wsB].ActiveStreams != 2 {
		t.Fatalf("streams are known from the first sample: %+v", first.Workspaces[wsB])
	}

	tr.Record("r1", t0.Add(60*time.Second), 1060, map[string]WorkspaceTrafficSample{
		wsA: {ToConnector: 700, ToClient: 61_000, ActiveStreams: 3},
		wsB: {ToConnector: 60, ToClient: 6_000, ActiveStreams: 0},
	})
	v := onlyRelay(t, tr)
	if v.WindowSeconds != 60 || !v.MeasuredAt.Equal(t0.Add(60*time.Second)) {
		t.Fatalf("window %v at %v", v.WindowSeconds, v.MeasuredAt)
	}
	wantRate(t, "A to client", v.Workspaces[wsA].ToClientBytesPerSec, 1000)
	wantRate(t, "A to connector", v.Workspaces[wsA].ToConnectorBytesPerSec, 10)
	wantRate(t, "B to client", v.Workspaces[wsB].ToClientBytesPerSec, 100)
	wantRate(t, "B to connector", v.Workspaces[wsB].ToConnectorBytesPerSec, 1)
	if v.Workspaces[wsA].ActiveStreams != 3 || v.Workspaces[wsB].ActiveStreams != 0 {
		t.Fatalf("streams %+v", v.Workspaces)
	}
}

func TestWorkspaceTraffic_RestartGivesNoRate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		uptime uint64
		after  WorkspaceTrafficSample
	}{
		// Uptime shorter than the gap (beyond the 5 s slack): restarted.
		{"short uptime", 30, WorkspaceTrafficSample{ToConnector: 500, ToClient: 5000}},
		// A counter went down: restarted, or the relay dropped and re-created
		// the workspace's entry.
		{"counter decrease", 1060, WorkspaceTrafficSample{ToConnector: 50, ToClient: 5000}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewWorkspaceTrafficTracker()
			tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{wsA: {ToConnector: 100, ToClient: 1000}})
			tr.Record("r1", t0.Add(60*time.Second), tc.uptime, map[string]WorkspaceTrafficSample{wsA: tc.after})
			r := onlyRelay(t, tr).Workspaces[wsA]
			if r.ToClientBytesPerSec != nil || r.ToConnectorBytesPerSec != nil {
				t.Fatalf("rate after a restart: %+v", r)
			}
			// The next window has a rate again, from the restarted counters.
			tr.Record("r1", t0.Add(120*time.Second), tc.uptime+60, map[string]WorkspaceTrafficSample{
				wsA: {ToConnector: tc.after.ToConnector + 60, ToClient: tc.after.ToClient + 600},
			})
			r = onlyRelay(t, tr).Workspaces[wsA]
			wantRate(t, "to client", r.ToClientBytesPerSec, 10)
			wantRate(t, "to connector", r.ToConnectorBytesPerSec, 1)
		})
	}
}

func TestWorkspaceTraffic_UptimeWithinSlackStillRates(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{wsA: {ToClient: 0}})
	// 60 s gap, uptime 56 s: within the 5 s delivery slack, same as B2.
	tr.Record("r1", t0.Add(60*time.Second), 56, map[string]WorkspaceTrafficSample{wsA: {ToClient: 600}})
	wantRate(t, "to client", onlyRelay(t, tr).Workspaces[wsA].ToClientBytesPerSec, 10)
}

func TestWorkspaceTraffic_NewWorkspaceHasNoRateUntilItsSecondSample(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{wsA: {ToClient: 10}})
	tr.Record("r1", t0.Add(60*time.Second), 1060, map[string]WorkspaceTrafficSample{
		wsA: {ToClient: 610}, wsB: {ToClient: 999, ActiveStreams: 1},
	})
	v := onlyRelay(t, tr)
	wantRate(t, "A", v.Workspaces[wsA].ToClientBytesPerSec, 10)
	if v.Workspaces[wsB].ToClientBytesPerSec != nil || v.Workspaces[wsB].ActiveStreams != 1 {
		t.Fatalf("B appeared this window: streams known, no rate: %+v", v.Workspaces[wsB])
	}
}

func TestWorkspaceTraffic_LateSampleIsIgnored(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	tr.Record("r1", t0.Add(60*time.Second), 1060, map[string]WorkspaceTrafficSample{wsA: {ToClient: 600}})
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{wsA: {ToClient: 0, ActiveStreams: 9}})
	if v := onlyRelay(t, tr); !v.MeasuredAt.Equal(t0.Add(60*time.Second)) || v.Workspaces[wsA].ActiveStreams != 0 {
		t.Fatalf("a late heartbeat replaced the newer sample: %+v", v)
	}
}

func TestWorkspaceTraffic_IdleRelayIsKnownAndEmpty(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{})
	v := onlyRelay(t, tr)
	if v.RelayID != "r1" || len(v.Workspaces) != 0 || !v.MeasuredAt.Equal(t0) {
		t.Fatalf("an idle relay is a known, empty view (pre-production rule): %+v", v)
	}
}

func TestWorkspaceTraffic_SnapshotIsACopy(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{wsA: {ToClient: 0}})
	tr.Record("r1", t0.Add(time.Minute), 1060, map[string]WorkspaceTrafficSample{wsA: {ToClient: 600}})
	snap := tr.Snapshot()
	*snap[0].Workspaces[wsA].ToClientBytesPerSec = 1
	delete(snap[0].Workspaces, wsA)
	wantRate(t, "after mutating a snapshot", onlyRelay(t, tr).Workspaces[wsA].ToClientBytesPerSec, 10)
}

// fakeWorkspaceLookup answers trust domain → workspace from a fixed map.
type fakeWorkspaceLookup struct {
	known map[string]string
	err   error
	asked [][]string
}

func (f *fakeWorkspaceLookup) ActiveWorkspacesByTrustDomain(_ context.Context, domains []string) (map[string]string, error) {
	f.asked = append(f.asked, domains)
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]string{}
	for _, d := range domains {
		if id, ok := f.known[d]; ok {
			out[d] = id
		}
	}
	return out, nil
}

func heartbeatWithTraffic(t *testing.T, svc *Service, uptime uint64, traffic ...*relaypb.WorkspaceTraffic) {
	t.Helper()
	ctx := relayHeartbeatContext(t, appmeta.SPIFFERoleRelay, testRelayID, testRelayID, time.Now().UTC().Add(time.Hour))
	if _, err := svc.Heartbeat(ctx, &relaypb.HeartbeatRequest{Version: "1", UptimeSeconds: uptime, WorkspaceTraffic: traffic}); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
}

func TestHeartbeat_RecordsWorkspaceTrafficAndIgnoresUnknownTrustDomains(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	lookup := &fakeWorkspaceLookup{known: map[string]string{"ws-a.zecurity.in": wsA, "ws-b.zecurity.in": wsB}}
	svc := NewService(nil, &fakeHeartbeatStore{}, time.Hour).WithWorkspaceTraffic(tr, lookup)

	heartbeatWithTraffic(t, svc, 100,
		&relaypb.WorkspaceTraffic{TrustDomain: "ws-a.zecurity.in", BytesClientToConnector: 7, BytesConnectorToClient: 70, ActiveStreams: 2},
		&relaypb.WorkspaceTraffic{TrustDomain: "ws-b.zecurity.in", ActiveStreams: 1},
		&relaypb.WorkspaceTraffic{TrustDomain: "ws-gone.zecurity.in", BytesConnectorToClient: 999, ActiveStreams: 4},
	)
	v := onlyRelay(t, tr)
	if v.RelayID != testRelayID {
		t.Fatalf("relay %q", v.RelayID)
	}
	if len(v.Workspaces) != 2 {
		t.Fatalf("an unknown trust domain was stored: %+v", v.Workspaces)
	}
	if v.Workspaces[wsA].ActiveStreams != 2 || v.Workspaces[wsB].ActiveStreams != 1 {
		t.Fatalf("workspaces %+v", v.Workspaces)
	}
	if got := lookup.asked[0]; len(got) != 3 {
		t.Fatalf("looked up %v", got)
	}
}

func TestHeartbeat_EmptyFieldThirteenIsAnIdleRelay(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	svc := NewService(nil, &fakeHeartbeatStore{}, time.Hour).WithWorkspaceTraffic(tr, &fakeWorkspaceLookup{})
	heartbeatWithTraffic(t, svc, 100)
	if v := onlyRelay(t, tr); len(v.Workspaces) != 0 {
		t.Fatalf("idle relay view %+v", v)
	}
}

func TestHeartbeat_WorkspaceLookupFailureRecordsNothing(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	lookup := &fakeWorkspaceLookup{err: errors.New("db down")}
	svc := NewService(nil, &fakeHeartbeatStore{}, time.Hour).WithWorkspaceTraffic(tr, lookup)
	heartbeatWithTraffic(t, svc, 100, &relaypb.WorkspaceTraffic{TrustDomain: "ws-a.zecurity.in", ActiveStreams: 1})
	if snap := tr.Snapshot(); len(snap) != 0 {
		t.Fatalf("recorded despite a failed lookup: %+v", snap)
	}
}

func TestHeartbeat_WorkspaceTrafficOffByDefault(t *testing.T) {
	svc := NewService(nil, &fakeHeartbeatStore{}, time.Hour)
	heartbeatWithTraffic(t, svc, 100, &relaypb.WorkspaceTraffic{TrustDomain: "ws-a.zecurity.in", ActiveStreams: 1})
}
