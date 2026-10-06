package transport

import (
	"testing"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
)

// Pure (no DB) tests for assembleTransportSnapshot — the empty-network
// representation that lets a client tell "no reachable connector" apart from
// "not covered by the transport plane"
// (Fix05-Empty-Network-Transport-Delivery).

func row(rn, conn string) *WorkspaceConnectorRow {
	return &WorkspaceConnectorRow{
		RemoteNetworkID: rn,
		ConnectorID:     conn,
		LanAddr:         "10.0.0.1:9091",
		TrustDomain:     "td.test",
	}
}

func rnByID(t *testing.T, snap *clientv1.TransportSnapshot) map[string]*clientv1.TransportRemoteNetwork {
	t.Helper()
	out := make(map[string]*clientv1.TransportRemoteNetwork, len(snap.RemoteNetworks))
	for _, rn := range snap.RemoteNetworks {
		if _, dup := out[rn.RemoteNetworkId]; dup {
			t.Fatalf("remote network %q emitted twice", rn.RemoteNetworkId)
		}
		out[rn.RemoteNetworkId] = rn
	}
	return out
}

func TestAssemble_RNWithConnectorsUnchanged(t *testing.T) {
	rows := []*WorkspaceConnectorRow{row("rn-a", "c1"), row("rn-a", "c2")}
	snap := assembleTransportSnapshot(rows, []string{"rn-a"}, 7)

	if snap.Version != 7 {
		t.Fatalf("version: want 7 got %d", snap.Version)
	}
	if len(snap.RemoteNetworks) != 1 {
		t.Fatalf("want 1 remote network, got %d", len(snap.RemoteNetworks))
	}
	rn := snap.RemoteNetworks[0]
	if rn.RemoteNetworkId != "rn-a" || len(rn.Connectors) != 2 {
		t.Fatalf("unexpected rn: %+v", rn)
	}
	// Row order (freshest heartbeat first) is preserved.
	if rn.Connectors[0].ConnectorId != "c1" || rn.Connectors[1].ConnectorId != "c2" {
		t.Fatalf("connector order changed: %q, %q", rn.Connectors[0].ConnectorId, rn.Connectors[1].ConnectorId)
	}
	if rn.Connectors[0].ConnectorTunnelAddr != "10.0.0.1:9092" {
		t.Fatalf("tunnel addr: got %q", rn.Connectors[0].ConnectorTunnelAddr)
	}
}

func TestAssemble_EmptyActiveRNEmitted(t *testing.T) {
	// The last connector left: no rows, but the network is still active.
	snap := assembleTransportSnapshot(nil, []string{"rn-a"}, 3)

	if len(snap.RemoteNetworks) != 1 {
		t.Fatalf("active RN with zero connectors must be present, got %d RNs", len(snap.RemoteNetworks))
	}
	rn := snap.RemoteNetworks[0]
	if rn.RemoteNetworkId != "rn-a" {
		t.Fatalf("remote_network_id: got %q", rn.RemoteNetworkId)
	}
	if rn.Connectors == nil || len(rn.Connectors) != 0 {
		t.Fatalf("want explicit empty connector list, got %+v", rn.Connectors)
	}
}

func TestAssemble_MultipleRNsIndependent(t *testing.T) {
	rows := []*WorkspaceConnectorRow{row("rn-b", "cb1")}
	snap := assembleTransportSnapshot(rows, []string{"rn-a", "rn-b", "rn-c"}, 1)

	by := rnByID(t, snap)
	if len(by) != 3 {
		t.Fatalf("want 3 RNs, got %d", len(by))
	}
	if len(by["rn-a"].Connectors) != 0 || len(by["rn-c"].Connectors) != 0 {
		t.Fatalf("rn-a/rn-c must be empty: a=%d c=%d", len(by["rn-a"].Connectors), len(by["rn-c"].Connectors))
	}
	if len(by["rn-b"].Connectors) != 1 || by["rn-b"].Connectors[0].ConnectorId != "cb1" {
		t.Fatalf("rn-b must keep its connector: %+v", by["rn-b"].Connectors)
	}
	// Deterministic order: connector-bearing RNs first (row order), then the
	// empty ones in activeRNIDs order.
	got := []string{snap.RemoteNetworks[0].RemoteNetworkId, snap.RemoteNetworks[1].RemoteNetworkId, snap.RemoteNetworks[2].RemoteNetworkId}
	want := []string{"rn-b", "rn-a", "rn-c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order: want %v got %v", want, got)
		}
	}
}

func TestAssemble_LastConnectorLeavesThenReturns(t *testing.T) {
	active := []string{"rn-a", "rn-b"}
	before := rnByID(t, assembleTransportSnapshot(
		[]*WorkspaceConnectorRow{row("rn-a", "ca1"), row("rn-b", "cb1")}, active, 1))
	gone := rnByID(t, assembleTransportSnapshot(
		[]*WorkspaceConnectorRow{row("rn-b", "cb1")}, active, 2))
	back := rnByID(t, assembleTransportSnapshot(
		[]*WorkspaceConnectorRow{row("rn-a", "ca1"), row("rn-b", "cb1")}, active, 3))

	if len(before["rn-a"].Connectors) != 1 {
		t.Fatalf("before: rn-a should have 1 connector")
	}
	if rn, ok := gone["rn-a"]; !ok || len(rn.Connectors) != 0 {
		t.Fatalf("after last connector left: rn-a must be present and empty, got %+v", rn)
	}
	if len(gone["rn-b"].Connectors) != 1 {
		t.Fatalf("unrelated rn-b must be untouched")
	}
	if len(back["rn-a"].Connectors) != 1 || back["rn-a"].Connectors[0].ConnectorId != "ca1" {
		t.Fatalf("connector return must repopulate rn-a, got %+v", back["rn-a"].Connectors)
	}
}

func TestAssemble_InactiveRNNotAdded(t *testing.T) {
	// A deleted RN (not in activeRNIDs) with no connectors keeps today's
	// shape: absent.
	snap := assembleTransportSnapshot(nil, nil, 1)
	if len(snap.RemoteNetworks) != 0 {
		t.Fatalf("want no RNs, got %+v", snap.RemoteNetworks)
	}
	// A connector row whose RN is not in the active list is still emitted from
	// its row, exactly as before this change.
	snap = assembleTransportSnapshot([]*WorkspaceConnectorRow{row("rn-x", "cx")}, nil, 1)
	if len(snap.RemoteNetworks) != 1 || len(snap.RemoteNetworks[0].Connectors) != 1 {
		t.Fatalf("connector rows must be emitted unchanged: %+v", snap.RemoteNetworks)
	}
}
