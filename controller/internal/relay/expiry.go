package relay

import (
	"context"
	"log"
	"time"
)

type expiryStore interface {
	EvictExpiredRelays(ctx context.Context, before time.Time) ([]string, error)
	ListWorkspacesForRelay(ctx context.Context, relayID string) ([]string, error)
}

// RunExpiryLoop periodically marks relays inactive when their heartbeat has
// not been seen within expiry duration, then notifies the affected workspaces
// so their ACL snapshots are recompiled without the dead relay.
//
// interval     — how often to run the sweep (default: 60s)
// expiry       — how long since last heartbeat before a relay is evicted (default: 90s = 3× heartbeat interval)
// onPoolChange — optional ADR-016 callback fired once per sweep that evicted
//                at least one relay, so connectors receive a fresh
//                LabelledRelayList without the dead relay. Nil-safe.
func RunExpiryLoop(ctx context.Context, store expiryStore, notifier policyChangeNotifier, interval, expiry time.Duration, onPoolChange func(ctx context.Context)) {
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

func runEviction(ctx context.Context, store expiryStore, notifier policyChangeNotifier, expiry time.Duration, onPoolChange func(ctx context.Context)) {
	threshold := time.Now().UTC().Add(-expiry)
	relayIDs, err := store.EvictExpiredRelays(ctx, threshold)
	if err != nil {
		log.Printf("relay expiry: evict: %v", err)
		return
	}
	for _, relayID := range relayIDs {
		log.Printf("relay expiry: evicted relay %s", relayID)
		workspaceIDs, err := store.ListWorkspacesForRelay(ctx, relayID)
		if err != nil {
			log.Printf("relay expiry: list workspaces for relay %s: %v", relayID, err)
			continue
		}
		for _, wsID := range workspaceIDs {
			if err := notifier.NotifyPolicyChange(ctx, wsID); err != nil {
				log.Printf("relay expiry: notify workspace %s: %v", wsID, err)
			}
		}
	}
	if len(relayIDs) > 0 && onPoolChange != nil {
		onPoolChange(ctx)
	}
}
