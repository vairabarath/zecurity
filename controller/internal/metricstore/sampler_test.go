package metricstore

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/yourorg/ztna/controller/internal/providerquery"
	"github.com/yourorg/ztna/controller/internal/relay"
)

// Sprint 24 M1 (D-49): which values become samples. An unknown value is
// never a sample (a gap, never 0).

func f(v float64) *float64 { return &v }

type key struct{ series, subject string }

func index(samples []Sample) map[key]float64 {
	m := map[key]float64{}
	for _, s := range samples {
		m[key{s.Series, s.SubjectID}] = s.Value
	}
	return m
}

func has(m map[key]float64, series, subject string) bool {
	_, ok := m[key{series, subject}]
	return ok
}

const fleet = providerquery.FleetSubjectID

func allLive(string) bool { return true }

// trs runs tenantRelaySamples with the given relays active and live, and no
// recent connector placements.
func trs(tenants []tenantCount, usable map[string]bool, views []relay.RelayWorkspaceTraffic, maxAge time.Duration) []Sample {
	relays := map[string]relayState{}
	for id, ok := range usable {
		if ok {
			relays[id] = relayState{active: true, live: true}
		}
	}
	return tenantRelaySamples(tenantRelayInput{tenants: tenants, relays: relays, views: views, placed: map[string]map[string]bool{}, now: now, maxAge: maxAge})
}

func TestRelaySamples_OnlyActiveRelaysAndKnownRates(t *testing.T) {
	relays := []providerquery.RelaySummary{
		{ID: "r-active", Status: "active", ConnectionCount: 4, Bandwidth: &providerquery.RelayBandwidth{ToClientBytesPerSec: f(1000), ToConnectorBytesPerSec: f(10)}},
		{ID: "r-measuring", Status: "active", ConnectionCount: 2, Bandwidth: &providerquery.RelayBandwidth{}}, // rates unknown yet
		{ID: "r-no-bw", Status: "active", ConnectionCount: 1},                                                 // doesn't report bytes
		{ID: "r-offline", Status: "inactive", ConnectionCount: 9, Bandwidth: &providerquery.RelayBandwidth{ToClientBytesPerSec: f(5)}},
	}
	active := map[string]bool{}
	m := index(relaySamples(relays, false, allLive, active))

	if m[key{providerquery.SeriesRelayConnections, "r-active"}] != 4 || m[key{providerquery.SeriesRelayBytesToClients, "r-active"}] != 1000 {
		t.Fatalf("active relay samples: %v", m)
	}
	for _, s := range []string{providerquery.SeriesRelayConnections, providerquery.SeriesRelayBytesToClients} {
		if has(m, s, "r-offline") {
			t.Fatalf("an offline relay must be a gap, got %s", s)
		}
	}
	if has(m, providerquery.SeriesRelayBytesToClients, "r-measuring") || has(m, providerquery.SeriesRelayBytesToClients, "r-no-bw") {
		t.Fatalf("an unknown rate must be a gap, not 0: %v", m)
	}
	if !has(m, providerquery.SeriesRelayConnections, "r-measuring") {
		t.Fatal("connections are known even while the rate is measuring")
	}
	// Fleet: sums over the relays that report (offline excluded).
	if m[key{providerquery.SeriesFleetConnections, fleet}] != 7 || m[key{providerquery.SeriesFleetBytesToClients, fleet}] != 1000 || m[key{providerquery.SeriesFleetBytesToConnectors, fleet}] != 10 {
		t.Fatalf("fleet totals: %v", m)
	}
	if len(active) != 3 || active["r-offline"] {
		t.Fatalf("active set %v", active)
	}
}

func TestRelaySamples_FleetGapWhenNothingReportsOrListTruncated(t *testing.T) {
	m := index(relaySamples([]providerquery.RelaySummary{{ID: "r", Status: "inactive"}}, false, allLive, map[string]bool{}))
	if has(m, providerquery.SeriesFleetConnections, fleet) {
		t.Fatal("no reporting relay → fleet gap, not 0")
	}
	m = index(relaySamples([]providerquery.RelaySummary{{ID: "r", Status: "active", ConnectionCount: 3}}, true, allLive, map[string]bool{}))
	if has(m, providerquery.SeriesFleetConnections, fleet) {
		t.Fatal("a truncated relay list can't give a fleet total")
	}
	if !has(m, providerquery.SeriesRelayConnections, "r") {
		t.Fatal("the relays that were read are still sampled")
	}
}

func TestTenantSamples_CountsAndFleetTotals(t *testing.T) {
	m := index(tenantSamples([]tenantCount{{"t1", 3, 2, 1}, {"t2", 0, 1, 0}}))
	if m[key{providerquery.SeriesTenantActiveSessions, "t1"}] != 3 || m[key{providerquery.SeriesTenantActiveSessions, "t2"}] != 0 {
		t.Fatalf("tenant sessions: %v", m)
	}
	if m[key{providerquery.SeriesFleetActiveSessions, fleet}] != 3 || m[key{providerquery.SeriesFleetConnectorsOnline, fleet}] != 3 || m[key{providerquery.SeriesFleetShieldsOnline, fleet}] != 1 {
		t.Fatalf("fleet totals: %v", m)
	}
}

var now = time.Date(2026, 11, 21, 12, 0, 30, 0, time.UTC)

func view(id string, age time.Duration, ws map[string]relay.WorkspaceRate) relay.RelayWorkspaceTraffic {
	return relay.RelayWorkspaceTraffic{RelayID: id, MeasuredAt: now.Add(-age), WindowSeconds: 300, Workspaces: ws}
}

func TestTenantRelaySamples_SumOverFreshActiveRelays(t *testing.T) {
	tenants := []tenantCount{{id: "t1"}, {id: "t2"}, {id: "t3"}}
	active := map[string]bool{"r1": true, "r2": true}
	views := []relay.RelayWorkspaceTraffic{
		view("r1", time.Minute, map[string]relay.WorkspaceRate{
			"t1": {ToClientBytesPerSec: f(100), ToConnectorBytesPerSec: f(1), ActiveStreams: 2},
			"t2": {ToClientBytesPerSec: nil, ToConnectorBytesPerSec: nil, ActiveStreams: 1}, // restarted: unknown
		}),
		view("r2", time.Minute, map[string]relay.WorkspaceRate{
			"t1": {ToClientBytesPerSec: f(50), ToConnectorBytesPerSec: f(2), ActiveStreams: 1},
		}),
		view("r-stale", time.Hour, map[string]relay.WorkspaceRate{"t1": {ToClientBytesPerSec: f(9999), ToConnectorBytesPerSec: f(9999)}}),
		// Offline for long: its last reading is past the residual cutoff (F23
		// rule 6a), so it no longer blanks t1. A recent one does (below).
		view("r-offline", 20*time.Minute, map[string]relay.WorkspaceRate{"t1": {ToClientBytesPerSec: f(7777), ToConnectorBytesPerSec: f(7777)}}),
	}
	m := index(trs(tenants, active, views, 11*time.Minute))

	if m[key{providerquery.SeriesTenantRelayBytesToClient, "t1"}] != 150 || m[key{providerquery.SeriesTenantRelayBytesToConn, "t1"}] != 3 || m[key{providerquery.SeriesTenantRelayStreams, "t1"}] != 3 {
		t.Fatalf("t1 over r1+r2 (stale and offline relays excluded): %v", m)
	}
	if has(m, providerquery.SeriesTenantRelayBytesToClient, "t2") {
		t.Fatal("t2's rate is unknown on a reporting relay: a gap, not 0")
	}
	if m[key{providerquery.SeriesTenantRelayStreams, "t2"}] != 1 {
		t.Fatal("t2's streams are still known")
	}
	if m[key{providerquery.SeriesTenantRelayBytesToClient, "t3"}] != 0 || !has(m, providerquery.SeriesTenantRelayBytesToClient, "t3") {
		t.Fatal("t3 had no traffic on any reporting relay: 0 (D-54 refined)")
	}
}

func TestTenantRelaySamples_NotReportedWhenNoFreshRelay(t *testing.T) {
	tenants := []tenantCount{{id: "t1"}}
	views := []relay.RelayWorkspaceTraffic{view("r1", time.Hour, map[string]relay.WorkspaceRate{})}
	if got := trs(tenants, map[string]bool{"r1": true}, views, 11*time.Minute); len(got) != 0 {
		t.Fatalf("no fresh relay view → no tenant relay sample at all, got %v", got)
	}
	if got := trs(tenants, map[string]bool{}, nil, 11*time.Minute); len(got) != 0 {
		t.Fatalf("no relay → nothing, got %v", got)
	}
}

// F23 R1-2 (live run 2026-11-23): a fresh relay view without a window (the
// relay restarted, or its first reading) can't tell which tenants it carried,
// so no tenant gets byte samples that minute: a gap, never a partial sum.
// Stream counts are still known. The next continuous window recovers.
func TestTenantRelaySamples_NonContinuousViewWritesNoBytes(t *testing.T) {
	tenants := []tenantCount{{id: "t1"}, {id: "t2"}}
	active := map[string]bool{"r1": true, "r2": true}
	restarted := view("r2", time.Minute, map[string]relay.WorkspaceRate{"t2": {ActiveStreams: 1}})
	restarted.WindowSeconds = 0
	views := []relay.RelayWorkspaceTraffic{
		view("r1", time.Minute, map[string]relay.WorkspaceRate{
			"t1": {ToClientBytesPerSec: f(100), ToConnectorBytesPerSec: f(1), ActiveStreams: 2},
		}),
		restarted,
	}
	m := index(trs(tenants, active, views, 11*time.Minute))
	for _, id := range []string{"t1", "t2"} {
		if has(m, providerquery.SeriesTenantRelayBytesToClient, id) || has(m, providerquery.SeriesTenantRelayBytesToConn, id) {
			t.Fatalf("%s got bytes while r2 has no window (t1 is on an unaffected relay; still a gap): %v", id, m)
		}
	}
	if m[key{providerquery.SeriesTenantRelayStreams, "t1"}] != 2 || m[key{providerquery.SeriesTenantRelayStreams, "t2"}] != 1 {
		t.Fatalf("streams are still known: %v", m)
	}

	// Next minute r2 has a continuous window again: bytes are back.
	recovered := view("r2", time.Minute, map[string]relay.WorkspaceRate{"t2": {ToClientBytesPerSec: f(7), ToConnectorBytesPerSec: f(0), ActiveStreams: 1}})
	m = index(trs(tenants, active, []relay.RelayWorkspaceTraffic{views[0], recovered}, 11*time.Minute))
	if m[key{providerquery.SeriesTenantRelayBytesToClient, "t1"}] != 100 || m[key{providerquery.SeriesTenantRelayBytesToClient, "t2"}] != 7 {
		t.Fatalf("recovered window: %v", m)
	}
}

// F23 R1-3 B (2026-11-24): a relay's byte rates are averages over its last
// DB-write window. Once its heartbeats stop they would be repeated each
// minute until eviction (up to 150 s), so they are used only while the relay
// is live (last heartbeat within RelayLiveMaxAge). Connections still follow
// the relay status.
func TestRelaySamples_SilentRelayHasNoByteSamples(t *testing.T) {
	relays := []providerquery.RelaySummary{
		{ID: "r-live", Status: "active", ConnectionCount: 1, Bandwidth: &providerquery.RelayBandwidth{ToClientBytesPerSec: f(100), ToConnectorBytesPerSec: f(1)}},
		{ID: "r-silent", Status: "active", ConnectionCount: 2, Bandwidth: &providerquery.RelayBandwidth{ToClientBytesPerSec: f(9000), ToConnectorBytesPerSec: f(90)}},
	}
	live := func(id string) bool { return id == "r-live" }
	m := index(relaySamples(relays, false, live, map[string]bool{}))
	if has(m, providerquery.SeriesRelayBytesToClients, "r-silent") || has(m, providerquery.SeriesRelayBytesToConnectors, "r-silent") {
		t.Fatalf("a silent relay's last rate must not be repeated: %v", m)
	}
	if m[key{providerquery.SeriesFleetBytesToClients, fleet}] != 100 {
		t.Fatalf("fleet bytes over live relays only: %v", m)
	}
	if !has(m, providerquery.SeriesRelayConnections, "r-silent") || m[key{providerquery.SeriesFleetConnections, fleet}] != 3 {
		t.Fatalf("connections still follow the relay status: %v", m)
	}
}

type fakeHeartbeats struct {
	at  map[string]time.Time
	err error
}

func (f fakeHeartbeats) LastHeartbeat(_ context.Context, id string) (time.Time, bool, error) {
	if f.err != nil {
		return time.Time{}, false, f.err
	}
	t, ok := f.at[id]
	return t, ok, nil
}

func TestRelayLive_ValkeyThenPersistedHeartbeat(t *testing.T) {
	persistedOld := now.Add(-4 * time.Minute)
	persistedNew := now.Add(-10 * time.Second)
	for _, tc := range []struct {
		name      string
		hb        providerquery.HeartbeatSource
		persisted *time.Time
		want      bool
	}{
		{"valkey fresh", fakeHeartbeats{at: map[string]time.Time{"r": now.Add(-40 * time.Second)}}, &persistedOld, true},
		{"valkey older than 75 s", fakeHeartbeats{at: map[string]time.Time{"r": now.Add(-80 * time.Second)}}, &persistedOld, false},
		{"valkey down: persisted (written every heartbeat then)", fakeHeartbeats{err: errors.New("down")}, &persistedNew, true},
		{"valkey down, persisted old", fakeHeartbeats{err: errors.New("down")}, &persistedOld, false},
		{"no cache configured", nil, &persistedNew, true},
		{"no heartbeat at all", fakeHeartbeats{}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := relayLive(context.Background(), tc.hb, "r", tc.persisted, now, RelayLiveMaxAge); got != tc.want {
				t.Fatalf("live = %v, want %v", got, tc.want)
			}
		})
	}
}

// F23 rule 6 (2026-11-24): a relay left out of the sum (not active, not
// live, or no fresh reading) may still have carried a tenant's traffic.
func rule6Input(r2 relayState, r2View *relay.RelayWorkspaceTraffic, placed map[string]map[string]bool) tenantRelayInput {
	views := []relay.RelayWorkspaceTraffic{
		view("r1", time.Minute, map[string]relay.WorkspaceRate{
			"t1": {ToClientBytesPerSec: f(100), ToConnectorBytesPerSec: f(1), ActiveStreams: 1},
			"t2": {ToClientBytesPerSec: f(50), ToConnectorBytesPerSec: f(2), ActiveStreams: 1},
		}),
	}
	if r2View != nil {
		views = append(views, *r2View)
	}
	return tenantRelayInput{
		tenants: []tenantCount{{id: "t1"}, {id: "t2"}},
		relays:  map[string]relayState{"r1": {active: true, live: true}, "r2": r2},
		views:   views, placed: placed, now: now, maxAge: 11 * time.Minute,
	}
}

func bytesOf(m map[key]float64, id string) (float64, bool) {
	v, ok := m[key{providerquery.SeriesTenantRelayBytesToClient, id}]
	return v, ok
}

// 6a(i): the tenant was carrying traffic in the left-out relay's recent
// reading → its total is a gap. An idle tenant there, or one not there, is
// unaffected.
func TestTenantRelaySamples_LeftOutRelayWithRecentReadingBlanksItsTenants(t *testing.T) {
	for _, tc := range []struct {
		name string
		r2   relayState
	}{
		{"evicted (inactive)", relayState{}},
		{"active but silent (not live)", relayState{active: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := view("r2", 2*time.Minute, map[string]relay.WorkspaceRate{
				"t1": {ToClientBytesPerSec: f(900), ToConnectorBytesPerSec: f(9), ActiveStreams: 2},
				"t2": {ToClientBytesPerSec: f(0), ToConnectorBytesPerSec: f(0)}, // idle there
			})
			m := index(tenantRelaySamples(rule6Input(tc.r2, &v, map[string]map[string]bool{})))
			if _, ok := bytesOf(m, "t1"); ok {
				t.Fatalf("t1 was carrying traffic on the left-out relay: gap, got %v", m)
			}
			if got, ok := bytesOf(m, "t2"); !ok || got != 50 {
				t.Fatalf("t2 was idle there: r1 only, got %v (present %v)", got, ok)
			}
			if m[key{providerquery.SeriesTenantRelayStreams, "t1"}] != 1 {
				t.Fatalf("streams from the used relays: %v", m)
			}
		})
	}
}

// 6a(i) ends at max(TrafficMaxAge, ResidualWindow): by then connectors have
// moved off an evicted relay (eviction ≤150 s + drain 120 s + slack).
func TestTenantRelaySamples_ResidualReadingPastCutoffCountsZero(t *testing.T) {
	v := view("r2", 12*time.Minute, map[string]relay.WorkspaceRate{"t1": {ToClientBytesPerSec: f(900), ToConnectorBytesPerSec: f(9), ActiveStreams: 2}})
	m := index(tenantRelaySamples(rule6Input(relayState{}, &v, map[string]map[string]bool{})))
	if got, ok := bytesOf(m, "t1"); !ok || got != 100 {
		t.Fatalf("past the cutoff the relay counts 0: got %v (present %v)", got, ok)
	}
	// With the lab's 1-minute DB writes TrafficMaxAge is 3 min; the cutoff is
	// still ResidualWindow (5.5 min), so a 4-minute-old reading blanks.
	in := rule6Input(relayState{}, nil, map[string]map[string]bool{})
	in.maxAge = 3 * time.Minute
	in.views = append(in.views, view("r2", 4*time.Minute, map[string]relay.WorkspaceRate{"t1": {ActiveStreams: 1}}))
	in.views[0].MeasuredAt = now.Add(-time.Minute)
	if _, ok := bytesOf(index(tenantRelaySamples(in)), "t1"); ok {
		t.Fatal("a reading inside ResidualWindow still blanks, whatever TrafficMaxAge is")
	}
}

// 6a(ii): a connector of the tenant still confirms its placement on the
// left-out relay (partition: relay cut off from the controller, connector
// still attached and serving) → gap, for as long as that lasts.
func TestTenantRelaySamples_PlacementOnLeftOutRelayBlanksTenant(t *testing.T) {
	placed := map[string]map[string]bool{"t1": {"r2": true}, "t2": {"r1": true}}
	m := index(tenantRelaySamples(rule6Input(relayState{}, nil, placed)))
	if _, ok := bytesOf(m, "t1"); ok {
		t.Fatalf("t1's connector is attached to the left-out relay: gap, got %v", m)
	}
	if got, ok := bytesOf(m, "t2"); !ok || got != 50 {
		t.Fatalf("t2's connector is on a used relay: %v (present %v)", got, ok)
	}
}

// 6b: a live, active relay without a fresh reading can't say which tenants
// it carries → no tenant relay sample at all this minute.
func TestTenantRelaySamples_LiveRelayWithoutFreshReadingWritesNothing(t *testing.T) {
	stale := view("r2", 12*time.Minute, map[string]relay.WorkspaceRate{})
	for _, v := range []*relay.RelayWorkspaceTraffic{nil, &stale} {
		if got := tenantRelaySamples(rule6Input(relayState{active: true, live: true}, v, map[string]map[string]bool{})); len(got) != 0 {
			t.Fatalf("live relay without a fresh reading: nothing, got %v", got)
		}
	}
}

// Placements unread (query failed): which tenants sit on a left-out relay is
// unknown → no tenant bytes; streams still.
func TestTenantRelaySamples_PlacementsUnknownWritesNoBytes(t *testing.T) {
	m := index(tenantRelaySamples(rule6Input(relayState{}, nil, nil)))
	if _, ok := bytesOf(m, "t2"); ok {
		t.Fatalf("placements unknown: no bytes, got %v", m)
	}
	if !has(m, providerquery.SeriesTenantRelayStreams, "t2") {
		t.Fatal("streams still known")
	}
}

func TestEverySampledSeriesIsRegisteredWithItsSubject(t *testing.T) {
	var all []Sample
	all = append(all, relaySamples([]providerquery.RelaySummary{{ID: "r", Status: "active", Bandwidth: &providerquery.RelayBandwidth{ToClientBytesPerSec: f(1), ToConnectorBytesPerSec: f(1)}}}, false, allLive, map[string]bool{})...)
	all = append(all, tenantSamples([]tenantCount{{id: "t"}})...)
	all = append(all, trs([]tenantCount{{id: "t"}}, map[string]bool{"r": true},
		[]relay.RelayWorkspaceTraffic{view("r", 0, map[string]relay.WorkspaceRate{"t": {ToClientBytesPerSec: f(1), ToConnectorBytesPerSec: f(1)}})}, time.Minute)...)
	seen := map[string]bool{}
	for _, s := range all {
		def, ok := providerquery.LookupMetricSeries(s.Series)
		if !ok || def.Subject != s.SubjectType {
			t.Fatalf("sample %+v: registered %v as %+v", s, ok, def)
		}
		seen[s.Series] = true
	}
	var missing []string
	for _, n := range providerquery.MetricSeriesNames() {
		if !seen[n] {
			missing = append(missing, n)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("registered series the sampler never writes: %v", missing)
	}
}
