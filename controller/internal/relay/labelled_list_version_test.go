package relay

import (
	"testing"

	connectorpb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
)

func relayInfo(id, addr string, label connectorpb.RelayCapacityLabel) *connectorpb.LabelledRelayInfo {
	return &connectorpb.LabelledRelayInfo{RelayId: id, RelayAddr: addr, Label: label}
}

// F11: the connector compares LabelledRelayList.version by equality
// (relay_ranking.rs version_matches) to decide whether to re-probe. The version
// must therefore be a stable, content-addressed fingerprint: identical for
// identical content regardless of order, and different when any relay's
// identity/address/label or the set membership changes.
func TestRelayListVersion(t *testing.T) {
	high := connectorpb.RelayCapacityLabel_RELAY_CAPACITY_HIGH
	medium := connectorpb.RelayCapacityLabel_RELAY_CAPACITY_MEDIUM

	base := []*connectorpb.LabelledRelayInfo{
		relayInfo("a", "10.0.0.1:9093", high),
		relayInfo("b", "10.0.0.2:9093", medium),
	}

	t.Run("order independent", func(t *testing.T) {
		reordered := []*connectorpb.LabelledRelayInfo{
			relayInfo("b", "10.0.0.2:9093", medium),
			relayInfo("a", "10.0.0.1:9093", high),
		}
		if relayListVersion(base) != relayListVersion(reordered) {
			t.Fatal("version must be identical for the same content in a different order")
		}
	})

	t.Run("deterministic", func(t *testing.T) {
		if relayListVersion(base) != relayListVersion(base) {
			t.Fatal("version must be deterministic for identical input")
		}
	})

	t.Run("address change alters version", func(t *testing.T) {
		changed := []*connectorpb.LabelledRelayInfo{
			relayInfo("a", "10.0.0.1:9093", high),
			relayInfo("b", "10.9.9.9:9093", medium), // address changed
		}
		if relayListVersion(base) == relayListVersion(changed) {
			t.Fatal("an address change (label unchanged) must change the version")
		}
	})

	t.Run("label change alters version", func(t *testing.T) {
		changed := []*connectorpb.LabelledRelayInfo{
			relayInfo("a", "10.0.0.1:9093", medium), // label changed
			relayInfo("b", "10.0.0.2:9093", medium),
		}
		if relayListVersion(base) == relayListVersion(changed) {
			t.Fatal("a label change must change the version")
		}
	})

	t.Run("membership change alters version", func(t *testing.T) {
		added := append([]*connectorpb.LabelledRelayInfo{}, base...)
		added = append(added, relayInfo("c", "10.0.0.3:9093", high))
		if relayListVersion(base) == relayListVersion(added) {
			t.Fatal("adding a relay must change the version")
		}
	})

	t.Run("empty is stable", func(t *testing.T) {
		if relayListVersion(nil) != relayListVersion([]*connectorpb.LabelledRelayInfo{}) {
			t.Fatal("empty lists must share a stable version")
		}
	})
}
