package metricstore

import (
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

func TestRelaySamples_OnlyActiveRelaysAndKnownRates(t *testing.T) {
	relays := []providerquery.RelaySummary{
		{ID: "r-active", Status: "active", ConnectionCount: 4, Bandwidth: &providerquery.RelayBandwidth{ToClientBytesPerSec: f(1000), ToConnectorBytesPerSec: f(10)}},
		{ID: "r-measuring", Status: "active", ConnectionCount: 2, Bandwidth: &providerquery.RelayBandwidth{}}, // rates unknown yet
		{ID: "r-no-bw", Status: "active", ConnectionCount: 1},                                                 // doesn't report bytes
		{ID: "r-offline", Status: "inactive", ConnectionCount: 9, Bandwidth: &providerquery.RelayBandwidth{ToClientBytesPerSec: f(5)}},
	}
	active := map[string]bool{}
	m := index(relaySamples(relays, false, active))

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
	m := index(relaySamples([]providerquery.RelaySummary{{ID: "r", Status: "inactive"}}, false, map[string]bool{}))
	if has(m, providerquery.SeriesFleetConnections, fleet) {
		t.Fatal("no reporting relay → fleet gap, not 0")
	}
	m = index(relaySamples([]providerquery.RelaySummary{{ID: "r", Status: "active", ConnectionCount: 3}}, true, map[string]bool{}))
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
		view("r-offline", time.Minute, map[string]relay.WorkspaceRate{"t1": {ToClientBytesPerSec: f(7777), ToConnectorBytesPerSec: f(7777)}}),
	}
	m := index(tenantRelaySamples(tenants, active, views, now, 11*time.Minute))

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
	if got := tenantRelaySamples(tenants, map[string]bool{"r1": true}, views, now, 11*time.Minute); len(got) != 0 {
		t.Fatalf("no fresh relay view → no tenant relay sample at all, got %v", got)
	}
	if got := tenantRelaySamples(tenants, map[string]bool{}, nil, now, 11*time.Minute); len(got) != 0 {
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
	m := index(tenantRelaySamples(tenants, active, views, now, 11*time.Minute))
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
	m = index(tenantRelaySamples(tenants, active, []relay.RelayWorkspaceTraffic{views[0], recovered}, now, 11*time.Minute))
	if m[key{providerquery.SeriesTenantRelayBytesToClient, "t1"}] != 100 || m[key{providerquery.SeriesTenantRelayBytesToClient, "t2"}] != 7 {
		t.Fatalf("recovered window: %v", m)
	}
}

func TestEverySampledSeriesIsRegisteredWithItsSubject(t *testing.T) {
	var all []Sample
	all = append(all, relaySamples([]providerquery.RelaySummary{{ID: "r", Status: "active", Bandwidth: &providerquery.RelayBandwidth{ToClientBytesPerSec: f(1), ToConnectorBytesPerSec: f(1)}}}, false, map[string]bool{})...)
	all = append(all, tenantSamples([]tenantCount{{id: "t"}})...)
	all = append(all, tenantRelaySamples([]tenantCount{{id: "t"}}, map[string]bool{"r": true},
		[]relay.RelayWorkspaceTraffic{view("r", 0, map[string]relay.WorkspaceRate{"t": {ToClientBytesPerSec: f(1), ToConnectorBytesPerSec: f(1)}})}, now, time.Minute)...)
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
