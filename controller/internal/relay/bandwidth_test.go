package relay

import (
	"context"
	"math"
	"testing"
	"time"

	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// F5: relay bandwidth from the heartbeat's cumulative byte counters.

func u64(v uint64) *uint64 { return &v }

func TestBandwidthSample_FromHeartbeat(t *testing.T) {
	cases := []struct {
		name string
		req  *relaypb.HeartbeatRequest
		want *BandwidthSample
	}{
		{"older relay: not reported", &relaypb.HeartbeatRequest{UptimeSeconds: 10}, nil},
		{"one counter only: not reported", &relaypb.HeartbeatRequest{BytesClientToConnector: u64(5)}, nil},
		{"zero traffic is reported", &relaypb.HeartbeatRequest{BytesClientToConnector: u64(0), BytesConnectorToClient: u64(0), UptimeSeconds: 7},
			&BandwidthSample{UptimeSeconds: 7}},
		{"both counters", &relaypb.HeartbeatRequest{BytesClientToConnector: u64(10), BytesConnectorToClient: u64(20), UptimeSeconds: 30},
			&BandwidthSample{ToConnector: 10, ToClient: 20, UptimeSeconds: 30}},
		{"overflowing int64: not reported", &relaypb.HeartbeatRequest{BytesClientToConnector: u64(math.MaxUint64), BytesConnectorToClient: u64(1)}, nil},
	}
	for _, c := range cases {
		got := bandwidthSample(c.req)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Fatalf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestHeartbeat_PassesBandwidthToTheStore(t *testing.T) {
	store := &fakeHeartbeatStore{}
	service := NewService(nil, store, time.Hour)
	ctx := relayHeartbeatContext(t, appmeta.SPIFFERoleRelay, testRelayID, testRelayID, time.Now().UTC().Add(time.Hour))
	if _, err := service.Heartbeat(ctx, &relaypb.HeartbeatRequest{
		Version: "1.2.3", UptimeSeconds: 90,
		BytesClientToConnector: u64(1000), BytesConnectorToClient: u64(4000),
	}); err != nil {
		t.Fatal(err)
	}
	if store.bandwidth == nil || *store.bandwidth != (BandwidthSample{ToConnector: 1000, ToClient: 4000, UptimeSeconds: 90}) {
		t.Fatalf("bandwidth passed to the store: %+v", store.bandwidth)
	}
}

type bwRow struct {
	toConnTotal, toClientTotal *int64
	sampledAt                  *time.Time
	toConnRate, toClientRate   *float64
	window                     *float64
}

func (e *adminEnv) bandwidthRow(t *testing.T, id string) bwRow {
	t.Helper()
	var r bwRow
	if err := e.pool.QueryRow(context.Background(), `
		SELECT bytes_to_connector_total, bytes_to_client_total, bytes_sampled_at,
		       bytes_to_connector_per_sec, bytes_to_client_per_sec, bytes_rate_window_seconds
		  FROM relays WHERE id = $1`, id).Scan(&r.toConnTotal, &r.toClientTotal, &r.sampledAt, &r.toConnRate, &r.toClientRate, &r.window); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRecordHeartbeat_BandwidthRate(t *testing.T) {
	e := newAdminEnv(t, false)
	id := e.relay(t, "relay-a")
	t0 := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	beat := func(at time.Time, bw *BandwidthSample) {
		t.Helper()
		if err := e.store.RecordHeartbeat(context.Background(), id, "s", at.Add(time.Hour), "1.0", "h",
			"", 0, "", "relay-a.relay.test:9093", 1, 100, at, bw); err != nil {
			t.Fatal(err)
		}
	}
	sample := func(conn, client, uptime int64) *BandwidthSample {
		return &BandwidthSample{ToConnector: conn, ToClient: client, UptimeSeconds: uptime}
	}
	wantTotals := func(step string, conn, client int64, at time.Time) bwRow {
		t.Helper()
		r := e.bandwidthRow(t, id)
		if r.toConnTotal == nil || *r.toConnTotal != conn || r.toClientTotal == nil || *r.toClientTotal != client ||
			r.sampledAt == nil || !r.sampledAt.Equal(at) {
			t.Fatalf("%s: totals %v/%v at %v, want %d/%d at %v", step, r.toConnTotal, r.toClientTotal, r.sampledAt, conn, client, at)
		}
		return r
	}
	wantNoRate := func(step string, r bwRow) {
		t.Helper()
		if r.toConnRate != nil || r.toClientRate != nil || r.window != nil {
			t.Fatalf("%s: want no rate, got %v/%v over %v", step, r.toConnRate, r.toClientRate, r.window)
		}
	}
	wantRate := func(step string, r bwRow, conn, client, window float64) {
		t.Helper()
		if r.toConnRate == nil || r.toClientRate == nil || r.window == nil ||
			math.Abs(*r.toConnRate-conn) > 1e-9 || math.Abs(*r.toClientRate-client) > 1e-9 || math.Abs(*r.window-window) > 1e-9 {
			t.Fatalf("%s: rate %v/%v over %v, want %v/%v over %v", step, r.toConnRate, r.toClientRate, r.window, conn, client, window)
		}
	}

	// Not reported (older relay): nothing stored.
	beat(t0, nil)
	if r := e.bandwidthRow(t, id); r.toConnTotal != nil || r.sampledAt != nil {
		t.Fatalf("not reported stored a sample: %+v", r)
	}
	wantNoRate("not reported", e.bandwidthRow(t, id))

	// First sample: stored, no rate yet.
	t0b := t0.Add(30 * time.Second)
	beat(t0b, sample(1000, 2000, 100))
	wantNoRate("first sample", wantTotals("first sample", 1000, 2000, t0b))

	// Five minutes later: growth / gap.
	t1 := t0b.Add(300 * time.Second)
	beat(t1, sample(31000, 62000, 400))
	wantRate("second sample", wantTotals("second sample", 31000, 62000, t1), 100, 200, 300)

	// A late, older heartbeat changes nothing.
	beat(t0b.Add(200*time.Second), sample(99999, 99999, 300))
	wantRate("late heartbeat", wantTotals("late heartbeat", 31000, 62000, t1), 100, 200, 300)

	// Same instant: not newer, nothing changes.
	beat(t1, sample(50000, 50000, 400))
	wantRate("same instant", wantTotals("same instant", 31000, 62000, t1), 100, 200, 300)

	// Restart: a counter went down. Sample kept, no rate.
	t2 := t1.Add(300 * time.Second)
	beat(t2, sample(500, 70000, 60))
	wantNoRate("counter went down", wantTotals("counter went down", 500, 70000, t2))

	// Restart without a decrease: uptime shorter than the gap.
	t3 := t2.Add(300 * time.Second)
	beat(t3, sample(100500, 170000, 200))
	wantNoRate("uptime shorter than gap", wantTotals("uptime shorter than gap", 100500, 170000, t3))

	// Uptime within the 5 s jitter slack of the gap still counts.
	t4 := t3.Add(300 * time.Second)
	beat(t4, sample(130500, 230000, 296))
	wantRate("within slack", wantTotals("within slack", 130500, 230000, t4), 100, 200, 300)

	// Zero traffic is a rate of zero, not "no rate".
	t5 := t4.Add(30 * time.Second)
	beat(t5, sample(130500, 230000, 326))
	wantRate("idle", wantTotals("idle", 130500, 230000, t5), 0, 0, 30)

	// A relay that stops reporting (downgraded): the sample is cleared.
	beat(t5.Add(30*time.Second), nil)
	if r := e.bandwidthRow(t, id); r.toConnTotal != nil || r.toClientTotal != nil || r.sampledAt != nil {
		t.Fatalf("stopped reporting kept a sample: %+v", r)
	}
	wantNoRate("stopped reporting", e.bandwidthRow(t, id))
}
