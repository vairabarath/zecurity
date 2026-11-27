package relay

import (
	"context"
	"errors"
	"strings"
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
	}, nil)
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
	}, nil)
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
			tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{wsA: {ToConnector: 100, ToClient: 1000}}, nil)
			tr.Record("r1", t0.Add(60*time.Second), tc.uptime, map[string]WorkspaceTrafficSample{wsA: tc.after}, nil)
			r := onlyRelay(t, tr).Workspaces[wsA]
			if r.ToClientBytesPerSec != nil || r.ToConnectorBytesPerSec != nil {
				t.Fatalf("rate after a restart: %+v", r)
			}
			// The next window has a rate again, from the restarted counters.
			tr.Record("r1", t0.Add(120*time.Second), tc.uptime+60, map[string]WorkspaceTrafficSample{
				wsA: {ToConnector: tc.after.ToConnector + 60, ToClient: tc.after.ToClient + 600},
			}, nil)
			r = onlyRelay(t, tr).Workspaces[wsA]
			wantRate(t, "to client", r.ToClientBytesPerSec, 10)
			wantRate(t, "to connector", r.ToConnectorBytesPerSec, 1)
		})
	}
}

// Combined restart check (agreed 2026-11-24): restart if
// U_now < U_prev + gap − 5 s. Delivery jitter within the 5 s tolerance is
// not a restart.
func TestWorkspaceTraffic_JitterWithinToleranceStillRates(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{wsA: {ToClient: 0}}, nil)
	// 60 s gap, uptime grew 56 s: this heartbeat arrived 4 s later than the last.
	tr.Record("r1", t0.Add(60*time.Second), 1056, map[string]WorkspaceTrafficSample{wsA: {ToClient: 600}}, nil)
	wantRate(t, "to client", onlyRelay(t, tr).Workspaces[wsA].ToClientBytesPerSec, 10)
}

// A relay that restarts just after a reading reports an uptime close to the
// gap. The old check (uptime + 5 >= gap) passed it; the combined check uses
// the previous uptime and catches it, for a young relay too.
func TestWorkspaceTraffic_RestartJustAfterAReadingIsDetected(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prevUptime uint64
	}{
		{"long-running relay", 1000},
		{"young relay (previous uptime below the gap)", 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewWorkspaceTrafficTracker()
			tr.Record("r1", t0, tc.prevUptime, map[string]WorkspaceTrafficSample{wsA: {ToClient: 100}}, nil)
			// Restarted 4 s after that reading: uptime 56 s at a 60 s gap.
			tr.Record("r1", t0.Add(60*time.Second), 56, map[string]WorkspaceTrafficSample{wsA: {ToClient: 5000}}, nil)
			v := onlyRelay(t, tr)
			if v.WindowSeconds != 0 || v.Workspaces[wsA].ToClientBytesPerSec != nil {
				t.Fatalf("restart not detected: %+v", v)
			}
		})
	}
}

// F23 R1-1 (live run 2026-11-23): a workspace absent from the previous
// reading of a continuous window counts from zero. The relay reports every
// entry that ever carried bytes (relay/src/traffic.rs snapshot), so absence
// means a zero counter: never seen, or pruned and re-created.
func TestWorkspaceTraffic_NewWorkspaceInContinuousWindowRatesFromZero(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{wsA: {ToClient: 10}}, nil)
	tr.Record("r1", t0.Add(60*time.Second), 1060, map[string]WorkspaceTrafficSample{
		wsA: {ToClient: 610}, wsB: {ToClient: 600, ToConnector: 60, ActiveStreams: 1},
	}, nil)
	v := onlyRelay(t, tr)
	wantRate(t, "A", v.Workspaces[wsA].ToClientBytesPerSec, 10)
	wantRate(t, "B to client (from zero)", v.Workspaces[wsB].ToClientBytesPerSec, 10)
	wantRate(t, "B to connector (from zero)", v.Workspaces[wsB].ToConnectorBytesPerSec, 1)
	if v.Workspaces[wsB].ActiveStreams != 1 {
		t.Fatalf("B streams %+v", v.Workspaces[wsB])
	}
}

// Rule 2: a relay restart blanks every workspace on that relay, including
// one whose counter is already above its last value, and a new one.
func TestWorkspaceTraffic_RelayRestartBlanksAllWorkspaces(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{wsA: {ToClient: 100, ToConnector: 10}}, nil)
	tr.Record("r1", t0.Add(60*time.Second), 20, map[string]WorkspaceTrafficSample{
		wsA: {ToClient: 5000, ToConnector: 500}, // higher than before, but a new process
		wsB: {ToClient: 600},                    // new
	}, nil)
	v := onlyRelay(t, tr)
	if v.WindowSeconds != 0 {
		t.Fatalf("window after a restart: %v", v.WindowSeconds)
	}
	for _, ws := range []string{wsA, wsB} {
		if r := v.Workspaces[ws]; r.ToClientBytesPerSec != nil || r.ToConnectorBytesPerSec != nil {
			t.Fatalf("%s rated across a relay restart: %+v", ws, r)
		}
	}
}

// After a controller restart the tracker is empty: the first reading of each
// relay has no rate, so a workspace's lifetime counter is never one window.
func TestWorkspaceTraffic_ControllerFirstSampleHasNoRate(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	tr.Record("r1", t0, 999_999, map[string]WorkspaceTrafficSample{wsA: {ToClient: 1 << 40, ActiveStreams: 3}}, nil)
	r := onlyRelay(t, tr).Workspaces[wsA]
	if r.ToClientBytesPerSec != nil || r.ToConnectorBytesPerSec != nil || r.ActiveStreams != 3 {
		t.Fatalf("first reading: streams known, no rate: %+v", r)
	}
}

// Rule 4: a trust domain that was unmatched in the previous reading may hide
// a long-lived counter, so its workspace gets no zero baseline. Only that
// workspace: another new one on the same relay still rates from zero.
func TestWorkspaceTraffic_UnmappedDomainOnlyBlocksItsOwnWorkspace(t *testing.T) {
	const wsC = "cccccccc-0000-0000-0000-00000000000c"
	tr := NewWorkspaceTrafficTracker()
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{wsA: {ToClient: 10}},
		map[string]string{"ws-a.zecurity.in": wsA, "ws-b.zecurity.in": ""})
	tr.Record("r1", t0.Add(60*time.Second), 1060, map[string]WorkspaceTrafficSample{
		wsA: {ToClient: 610}, wsB: {ToClient: 1 << 30}, wsC: {ToClient: 600},
	}, map[string]string{"ws-a.zecurity.in": wsA, "ws-b.zecurity.in": wsB, "ws-c.zecurity.in": wsC})
	v := onlyRelay(t, tr)
	wantRate(t, "A", v.Workspaces[wsA].ToClientBytesPerSec, 10)
	if v.Workspaces[wsB].ToClientBytesPerSec != nil {
		t.Fatalf("B was unmatched last reading: no zero baseline, got %+v", v.Workspaces[wsB])
	}
	wantRate(t, "C (new, never unmatched)", v.Workspaces[wsC].ToClientBytesPerSec, 10)
}

// Rule 4: the unmatched set is the previous reading's only. A domain
// unmatched for several readings blanks the first matched one; the next rates.
func TestWorkspaceTraffic_UnmatchedForSeveralReadingsThenMatched(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	unmatched := map[string]string{"ws-b.zecurity.in": ""}
	for i := 0; i < 3; i++ {
		tr.Record("r1", t0.Add(time.Duration(i)*time.Minute), 1000+uint64(i)*60, map[string]WorkspaceTrafficSample{}, unmatched)
	}
	matched := map[string]string{"ws-b.zecurity.in": wsB}
	tr.Record("r1", t0.Add(3*time.Minute), 1180, map[string]WorkspaceTrafficSample{wsB: {ToClient: 50_000}}, matched)
	if r := onlyRelay(t, tr).Workspaces[wsB]; r.ToClientBytesPerSec != nil {
		t.Fatalf("first matched reading after unmatched ones: blank, got %+v", r)
	}
	tr.Record("r1", t0.Add(4*time.Minute), 1240, map[string]WorkspaceTrafficSample{wsB: {ToClient: 50_600}}, matched)
	wantRate(t, "B next reading", onlyRelay(t, tr).Workspaces[wsB].ToClientBytesPerSec, 10)
}

// An entry pruned and re-created between readings: absent, then back with a
// lower counter. Absent → zero baseline; a lower counter later → blank.
func TestWorkspaceTraffic_DisappearAndReappear(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{wsA: {ToClient: 9000}}, nil)
	tr.Record("r1", t0.Add(60*time.Second), 1060, map[string]WorkspaceTrafficSample{}, nil)
	tr.Record("r1", t0.Add(120*time.Second), 1120, map[string]WorkspaceTrafficSample{wsA: {ToClient: 300}}, nil)
	wantRate(t, "A back after absence (from zero)", onlyRelay(t, tr).Workspaces[wsA].ToClientBytesPerSec, 5)

	// Pruned and re-created within one window, counter now lower: blank.
	tr.Record("r1", t0.Add(180*time.Second), 1180, map[string]WorkspaceTrafficSample{wsA: {ToClient: 60}}, nil)
	if r := onlyRelay(t, tr).Workspaces[wsA]; r.ToClientBytesPerSec != nil {
		t.Fatalf("counter went down: blank, got %+v", r)
	}
}

// KNOWN LIMITATION (option D, accepted 2026-11-23; the exact fix, an entry
// epoch in field 13, is deferred). An entry pruned and re-created within one
// window whose new counter is above the old one reads as normal growth: the
// window is undercounted. This test pins the behaviour so a change is
// deliberate; it does not assert it is desirable.
func TestWorkspaceTraffic_RecreatedEntryLargerThanBefore_KnownUndercount(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{wsA: {ToClient: 1000}}, nil)
	// Really: pruned, re-created at 0, then carried 1200 bytes.
	tr.Record("r1", t0.Add(60*time.Second), 1060, map[string]WorkspaceTrafficSample{wsA: {ToClient: 1200}}, nil)
	wantRate(t, "undercount (200 of 1200 bytes)", onlyRelay(t, tr).Workspaces[wsA].ToClientBytesPerSec, 200.0/60)
}

func TestWorkspaceTraffic_IndependentRelays(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{wsA: {ToClient: 100}}, nil)
	tr.Record("r2", t0, 50, map[string]WorkspaceTrafficSample{}, nil)
	tr.Record("r2", t0.Add(60*time.Second), 10, map[string]WorkspaceTrafficSample{wsA: {ToClient: 600}}, nil) // r2 restarted
	tr.Record("r1", t0.Add(60*time.Second), 1060, map[string]WorkspaceTrafficSample{wsA: {ToClient: 700}}, nil)
	snap := tr.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("relays %d", len(snap))
	}
	wantRate(t, "r1 A", snap[0].Workspaces[wsA].ToClientBytesPerSec, 10)
	if snap[1].Workspaces[wsA].ToClientBytesPerSec != nil {
		t.Fatalf("r2 restarted: blank, got %+v", snap[1].Workspaces[wsA])
	}
}

func TestWorkspaceTraffic_LateSampleIsIgnored(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	tr.Record("r1", t0.Add(60*time.Second), 1060, map[string]WorkspaceTrafficSample{wsA: {ToClient: 600}}, nil)
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{wsA: {ToClient: 0, ActiveStreams: 9}}, nil)
	if v := onlyRelay(t, tr); !v.MeasuredAt.Equal(t0.Add(60*time.Second)) || v.Workspaces[wsA].ActiveStreams != 0 {
		t.Fatalf("a late heartbeat replaced the newer sample: %+v", v)
	}
}

func TestWorkspaceTraffic_IdleRelayIsKnownAndEmpty(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{}, nil)
	v := onlyRelay(t, tr)
	if v.RelayID != "r1" || len(v.Workspaces) != 0 || !v.MeasuredAt.Equal(t0) {
		t.Fatalf("an idle relay is a known, empty view (pre-production rule): %+v", v)
	}
}

func TestWorkspaceTraffic_SnapshotIsACopy(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{wsA: {ToClient: 0}}, nil)
	tr.Record("r1", t0.Add(time.Minute), 1060, map[string]WorkspaceTrafficSample{wsA: {ToClient: 600}}, nil)
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

// R1-1 rule 4 through the heartbeat path: a trust domain unmatched in one
// reading blanks its workspace's first matched reading. A reading skipped by
// a lookup failure records nothing, so the unmatched set survives the skip.
func TestHeartbeat_UnmatchedDomainSurvivesASkippedReading(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	lookup := &fakeWorkspaceLookup{known: map[string]string{"ws-a.zecurity.in": wsA}}
	svc := NewService(nil, &fakeHeartbeatStore{}, time.Hour).WithWorkspaceTraffic(tr, lookup)
	a := func(n uint64) *relaypb.WorkspaceTraffic {
		return &relaypb.WorkspaceTraffic{TrustDomain: "ws-a.zecurity.in", BytesConnectorToClient: n}
	}
	b := &relaypb.WorkspaceTraffic{TrustDomain: "ws-b.zecurity.in", BytesConnectorToClient: 1 << 30, ActiveStreams: 1}

	heartbeatWithTraffic(t, svc, 1000, a(10), b) // ws-b unmatched
	lookup.err = errors.New("db down")
	heartbeatWithTraffic(t, svc, 1000, a(20), b) // skipped
	lookup.err = nil
	lookup.known["ws-b.zecurity.in"] = wsB
	heartbeatWithTraffic(t, svc, 1000, a(30), b) // ws-b matches now

	v := onlyRelay(t, tr)
	if v.WindowSeconds == 0 {
		t.Fatal("the window should be continuous")
	}
	if r := v.Workspaces[wsB]; r.ToClientBytesPerSec != nil || r.ActiveStreams != 1 {
		t.Fatalf("ws-b was unmatched before the skip: streams known, no zero baseline; got %+v", r)
	}
	if v.Workspaces[wsA].ToClientBytesPerSec == nil {
		t.Fatal("ws-a still rates")
	}
}

// Rule 5 (agreed 2026-11-24): a gap longer than the tracker's MaxGap (main.go:
// TrafficMaxAge, two DB-write windows plus a minute) gives no rate: a long
// silence averaged into one minute would misstate it. The rejected reading
// is still the baseline for the next one.
func TestWorkspaceTraffic_GapLongerThanMaxGapHasNoRate(t *testing.T) {
	tr := NewWorkspaceTrafficTracker().WithMaxGap(11 * time.Minute)
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{wsA: {ToClient: 0}}, nil)
	tr.Record("r1", t0.Add(20*time.Minute), 2200, map[string]WorkspaceTrafficSample{wsA: {ToClient: 120_000}}, nil)
	v := onlyRelay(t, tr)
	if v.WindowSeconds != 0 || v.Workspaces[wsA].ToClientBytesPerSec != nil {
		t.Fatalf("20 min gap > 11 min: no rate, got %+v", v)
	}
	tr.Record("r1", t0.Add(21*time.Minute), 2260, map[string]WorkspaceTrafficSample{wsA: {ToClient: 120_600}}, nil)
	wantRate(t, "next reading (baseline = rejected one)", onlyRelay(t, tr).Workspaces[wsA].ToClientBytesPerSec, 10)
}

// A reading skipped (lookup failure) inside MaxGap: the next one rates over
// the longer window.
func TestWorkspaceTraffic_SkippedReadingWithinMaxGapRatesOverLongerWindow(t *testing.T) {
	tr := NewWorkspaceTrafficTracker().WithMaxGap(11 * time.Minute)
	tr.Record("r1", t0, 1000, map[string]WorkspaceTrafficSample{wsA: {ToClient: 0}}, nil)
	tr.Record("r1", t0.Add(10*time.Minute), 1600, map[string]WorkspaceTrafficSample{wsA: {ToClient: 6000}}, nil)
	v := onlyRelay(t, tr)
	if v.WindowSeconds != 600 {
		t.Fatalf("window %v", v.WindowSeconds)
	}
	wantRate(t, "over 10 min", v.Workspaces[wsA].ToClientBytesPerSec, 10)
}

// F23 fix (2026-11-24): the heartbeat hands the tracker a time that keeps
// Go's monotonic clock reading, so the window between two readings can't be
// stretched or shrunk by a wall-clock step on the controller (time.Now().UTC()
// strips the monotonic reading; a 30 s backward step would double a rate).
func TestHeartbeat_TrafficTimeKeepsTheMonotonicClock(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	svc := NewService(nil, &fakeHeartbeatStore{}, time.Hour).WithWorkspaceTraffic(tr, &fakeWorkspaceLookup{})
	heartbeatWithTraffic(t, svc, 100)
	if at := onlyRelay(t, tr).MeasuredAt; !strings.Contains(at.String(), "m=") {
		t.Fatalf("MeasuredAt has no monotonic reading: %s", at)
	}
}
