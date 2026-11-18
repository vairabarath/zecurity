package providerquery

import (
	"math"
	"testing"
)

// F5 headroom (D-42, refined 2026-11-18): both flows share the relay's link,
// so headroom = capacity − (to clients + to connectors); unknown inputs give
// no headroom (never zero or unlimited).
func TestRelayHeadroom(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	i := func(v int64) *int64 { return &v }
	bw := func(toClient, toConnector *float64) *RelayBandwidth {
		return &RelayBandwidth{ToClientBytesPerSec: toClient, ToConnectorBytesPerSec: toConnector}
	}

	// 1 Gbit/s = 125,000,000 B/s; 60 + 50 MB/s used → 15 MB/s left, 88 %.
	h := relayHeadroom(i(125_000_000), bw(f(60_000_000), f(50_000_000)))
	if h == nil || h.UsedBytesPerSec != 110_000_000 || h.HeadroomBytesPerSec != 15_000_000 || h.OverCapacity ||
		math.Abs(h.Utilization-0.88) > 1e-9 {
		t.Fatalf("summed flows: %+v, want used 110 MB/s, headroom 15 MB/s, 88 %%", h)
	}

	// Over capacity: headroom clamps at 0, flagged, utilization above 1.
	h = relayHeadroom(i(100), bw(f(80), f(40)))
	if h == nil || h.HeadroomBytesPerSec != 0 || !h.OverCapacity || h.UsedBytesPerSec != 120 || math.Abs(h.Utilization-1.2) > 1e-9 {
		t.Fatalf("over capacity: %+v", h)
	}

	// Exactly full is not over capacity.
	if h = relayHeadroom(i(100), bw(f(60), f(40))); h == nil || h.OverCapacity || h.HeadroomBytesPerSec != 0 {
		t.Fatalf("exactly full: %+v", h)
	}

	// Idle relay with a capacity: all headroom.
	if h = relayHeadroom(i(100), bw(f(0), f(0))); h == nil || h.HeadroomBytesPerSec != 100 || h.Utilization != 0 {
		t.Fatalf("idle: %+v", h)
	}

	// Unknown → nil: no capacity, no bandwidth, or either rate measuring.
	for name, got := range map[string]*RelayHeadroom{
		"no capacity":            relayHeadroom(nil, bw(f(1), f(1))),
		"zero capacity":          relayHeadroom(i(0), bw(f(1), f(1))),
		"no bandwidth":           relayHeadroom(i(100), nil),
		"to clients measuring":   relayHeadroom(i(100), bw(nil, f(1))),
		"to connector measuring": relayHeadroom(i(100), bw(f(1), nil)),
	} {
		if got != nil {
			t.Errorf("%s: headroom %+v, want nil", name, got)
		}
	}
}
