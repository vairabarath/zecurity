package provider_test

import (
	"encoding/json"
	"math"
	"testing"
)

// F5 headroom (Sprint 23 B2, D-42 refined 2026-11-18): relay reads return
// the configured capacity and headroom = capacity − (to clients + to
// connectors); no capacity → link_capacity_bps and headroom null.
func TestRelayReads_LinkCapacityAndHeadroom(t *testing.T) {
	e := newReadEnv(t)
	type headroom struct {
		Used         float64 `json:"used_bytes_per_sec"`
		Free         float64 `json:"headroom_bytes_per_sec"`
		Utilization  float64 `json:"utilization"`
		OverCapacity bool    `json:"over_capacity"`
	}
	read := func() (*int64, *headroom) {
		t.Helper()
		r := e.get(t, "/provider/relays/"+e.relayID, e.opsTok)
		var d struct {
			LinkCapacityBps *int64    `json:"link_capacity_bps"`
			Headroom        *headroom `json:"headroom"`
		}
		if err := json.Unmarshal([]byte(r.body), &d); err != nil || r.code != 200 {
			t.Fatalf("relay detail %d %s: %v", r.code, r.body, err)
		}
		return d.LinkCapacityBps, d.Headroom
	}
	// Measured traffic: 60 MB/s to clients, 50 MB/s to connectors.
	exec(t, e.pool, `UPDATE relays SET status = 'active', bytes_sampled_at = now(), bytes_rate_window_seconds = 300,
	                  bytes_to_client_per_sec = 60000000, bytes_to_connector_per_sec = 50000000 WHERE id = $1`, e.relayID)
	if c, h := read(); c != nil || h != nil {
		t.Fatalf("no capacity configured: capacity %v headroom %+v, want both null", c, h)
	}
	// A 1 Gbit/s link: both flows share it.
	exec(t, e.pool, `UPDATE relays SET link_capacity_bps = 125000000 WHERE id = $1`, e.relayID)
	c, h := read()
	if c == nil || *c != 125_000_000 || h == nil || h.Used != 110_000_000 || h.Free != 15_000_000 ||
		h.OverCapacity || math.Abs(h.Utilization-0.88) > 1e-9 {
		t.Fatalf("capacity %v headroom %+v, want used 110 MB/s, 15 MB/s free, 88 %%", c, h)
	}
	// The list returns the same.
	r := e.get(t, "/provider/relays", e.opsTok)
	var list struct {
		Items []struct {
			ID       string    `json:"id"`
			Headroom *headroom `json:"headroom"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(r.body), &list); err != nil {
		t.Fatal(err)
	}
	for _, it := range list.Items {
		if it.ID == e.relayID && (it.Headroom == nil || it.Headroom.Free != 15_000_000) {
			t.Fatalf("list headroom %+v", it.Headroom)
		}
	}
}
