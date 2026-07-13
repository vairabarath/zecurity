package relay

import (
	"context"
	"log"
	"time"
)

type expiryStore interface {
	EvictExpiredRelays(ctx context.Context, before time.Time) ([]string, error)
	ListConnectorsForRelay(ctx context.Context, relayID string) (map[string][]string, error)
}

// RunExpiryLoop periodically marks relays inactive when their heartbeat has
// not been seen within expiry duration, then fires a transport-plane topology
// change for the connectors on each evicted relay so their transport snapshot
// drops the dead relay — without recompiling the ACL (Track B invariant).
//
// interval     — how often to run the sweep (default: 60s)
// expiry       — how long since last heartbeat before a relay is evicted (default: 90s = 3× heartbeat interval)
// onPoolChange — optional ADR-016 callback fired once per sweep that evicted
//                at least one relay, so connectors receive a fresh
//                LabelledRelayList without the dead relay. Nil-safe.
func RunExpiryLoop(ctx context.Context, store expiryStore, notifier topologyChangeNotifier, interval, expiry time.Duration, onPoolChange func(ctx context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runEviction(ctx, store, notifier, expiry, onPoolChange)
		}
	}
}

func runEviction(ctx context.Context, store expiryStore, notifier topologyChangeNotifier, expiry time.Duration, onPoolChange func(ctx context.Context)) {
	threshold := time.Now().UTC().Add(-expiry)
	relayIDs, err := store.EvictExpiredRelays(ctx, threshold)
	if err != nil {
		log.Printf("relay expiry: evict: %v", err)
		return
	}
	for _, relayID := range relayIDs {
		log.Printf("relay expiry: evicted relay %s", relayID)
		byWorkspace, err := store.ListConnectorsForRelay(ctx, relayID)
		if err != nil {
			log.Printf("relay expiry: list connectors for relay %s: %v", relayID, err)
			continue
		}
		// Transport-plane change only — the evicted relay drops from the
		// connectors' transport snapshot; the ACL is untouched.
		for wsID, connectorIDs := range byWorkspace {
			if err := notifier.NotifyTopologyChange(ctx, wsID, connectorIDs); err != nil {
				log.Printf("relay expiry: notify topology workspace %s: %v", wsID, err)
			}
		}
	}
	if len(relayIDs) > 0 && onPoolChange != nil {
		onPoolChange(ctx)
	}
}
