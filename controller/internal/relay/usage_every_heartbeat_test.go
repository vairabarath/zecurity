package relay

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
)

// recordingUsageSink keeps every RecordRelayCounters call.
type recordingUsageSink struct {
	mu    sync.Mutex
	calls []map[string]WorkspaceTrafficSample
	ats   []time.Time
}

func (r *recordingUsageSink) RecordRelayCounters(_ context.Context, _ string, at time.Time, _ uint64, c map[string]WorkspaceTrafficSample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
	r.ats = append(r.ats, at)
}

// A DB-write heartbeat feeds both the R1 tracker and usage. The tracker keeps
// the monotonic arrival time (F23 fix); usage gets the same instant as UTC,
// the timestamp it gets on throttled heartbeats too.
func TestHeartbeat_DBWriteFeedsTrackerAndUsageWithUTCTime(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	sink := &recordingUsageSink{}
	lookup := &fakeWorkspaceLookup{known: map[string]string{"ws-a.zecurity.in": wsA}}
	svc := NewService(nil, &fakeHeartbeatStore{}, time.Hour).
		WithWorkspaceTraffic(tr, lookup).
		WithUsageSink(sink)

	heartbeatWithTraffic(t, svc, 100,
		&relaypb.WorkspaceTraffic{TrustDomain: "ws-a.zecurity.in", BytesConnectorToClient: 100, ActiveStreams: 1})

	if len(sink.ats) != 1 {
		t.Fatalf("usage saw %d heartbeats, want 1", len(sink.ats))
	}
	usageAt := sink.ats[0]
	if usageAt.Location() != time.UTC {
		t.Fatalf("usage time is in %v, want UTC", usageAt.Location())
	}
	if strings.Contains(usageAt.String(), "m=") {
		t.Fatalf("usage time carries a monotonic reading: %s", usageAt)
	}
	v := onlyRelay(t, tr)
	if !strings.Contains(v.MeasuredAt.String(), "m=") {
		t.Fatalf("tracker time has no monotonic reading: %s", v.MeasuredAt)
	}
	if !v.MeasuredAt.Equal(usageAt) {
		t.Fatalf("tracker %s and usage %s are different instants", v.MeasuredAt, usageAt)
	}
	if _, ok := v.Workspaces[wsA]; !ok {
		t.Fatalf("tracker has no entry for %s", wsA)
	}
}

// Sprint 25 live finding F32-1: the usage collector must see every heartbeat,
// not only the ones written to the DB (the B2 cadence), so a relay restart
// loses at most the bytes since its last heartbeat (D-59). R1's rate tracker
// keeps the DB-write cadence (its window is unchanged).
func TestHeartbeat_UsageSeesEveryHeartbeatRatesKeepDBCadence(t *testing.T) {
	tr := NewWorkspaceTrafficTracker()
	sink := &recordingUsageSink{}
	lookup := &fakeWorkspaceLookup{known: map[string]string{"ws-a.zecurity.in": wsA}}
	svc := NewService(nil, &fakeHeartbeatStore{}, time.Hour).
		WithHeartbeatCache(newProvisionTestValkey(t), time.Hour).
		WithWorkspaceTraffic(tr, lookup).
		WithUsageSink(sink)

	for i, toClient := range []uint64{100, 250, 400} {
		heartbeatWithTraffic(t, svc, uint64(100+30*i),
			&relaypb.WorkspaceTraffic{TrustDomain: "ws-a.zecurity.in", BytesConnectorToClient: toClient, ActiveStreams: 1})
	}

	if len(sink.calls) != 3 {
		t.Fatalf("usage saw %d of 3 heartbeats", len(sink.calls))
	}
	for i, want := range []uint64{100, 250, 400} {
		if got := sink.calls[i][wsA].ToClient; got != want {
			t.Fatalf("heartbeat %d: usage got %d bytes, want %d", i, got, want)
		}
	}
	// Only the first heartbeat was a DB write: the tracker has one sample,
	// so no rate window yet (a second throttled sample would have made one).
	if v := onlyRelay(t, tr); v.WindowSeconds != 0 {
		t.Fatalf("rate tracker recorded a throttled heartbeat (window %v)", v.WindowSeconds)
	}
}

// A failed workspace lookup on a throttled heartbeat records nothing; the
// next heartbeat counts the span (counters are cumulative).
func TestHeartbeat_ThrottledLookupFailureRecordsNoUsage(t *testing.T) {
	sink := &recordingUsageSink{}
	lookup := &fakeWorkspaceLookup{known: map[string]string{"ws-a.zecurity.in": wsA}}
	svc := NewService(nil, &fakeHeartbeatStore{}, time.Hour).
		WithHeartbeatCache(newProvisionTestValkey(t), time.Hour).
		WithWorkspaceTraffic(NewWorkspaceTrafficTracker(), lookup).
		WithUsageSink(sink)
	heartbeatWithTraffic(t, svc, 100, &relaypb.WorkspaceTraffic{TrustDomain: "ws-a.zecurity.in", BytesConnectorToClient: 1})
	lookup.err = context.DeadlineExceeded
	heartbeatWithTraffic(t, svc, 130, &relaypb.WorkspaceTraffic{TrustDomain: "ws-a.zecurity.in", BytesConnectorToClient: 2})
	if len(sink.calls) != 1 {
		t.Fatalf("usage recorded %d heartbeats, want 1 (the failed lookup records nothing)", len(sink.calls))
	}
}
