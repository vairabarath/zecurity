package relay

import (
	"context"
	"log"
	"time"
)

type expiryStore interface {
	ListEvictionCandidates(ctx context.Context, before time.Time) ([]string, error)
	RefreshLastHeartbeat(ctx context.Context, relayID string, seenAt time.Time) error
	EvictRelays(ctx context.Context, relayIDs []string, before time.Time) ([]string, error)
	ListConnectorsForRelay(ctx context.Context, relayID string) (map[string][]string, error)
}

// livenessSource exposes the Valkey heartbeat state (implemented by *Service).
// Postgres heartbeat writes are throttled, so the persisted timestamp alone
// cannot tell a live relay from a dead one — the liveness key can.
type livenessSource interface {
	LastHeartbeat(ctx context.Context, relayID string) (time.Time, bool, error)
	ClearHeartbeatThrottle(ctx context.Context, relayID string) error
}

// RunExpiryLoop periodically marks relays inactive when their heartbeat has
// not been seen within expiry duration, then fires a transport-plane topology
// change for the connectors on each evicted relay so their transport snapshot
// drops the dead relay — without recompiling the ACL (Track B invariant).
//
// interval     — how often to run the sweep (default: 60s)
// liveness     — Valkey liveness source; nil = Postgres-only eviction
// expiry       — how long since last heartbeat before a relay is evicted (default: 90s = 3× heartbeat interval)
// onPoolChange — optional ADR-016 callback fired once per sweep that evicted
//
//	at least one relay, so connectors receive a fresh
//	LabelledRelayList without the dead relay. Nil-safe.
func RunExpiryLoop(ctx context.Context, store expiryStore, liveness livenessSource, notifier topologyChangeNotifier, interval, expiry time.Duration, onPoolChange func(ctx context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runEviction(ctx, store, liveness, notifier, expiry, onPoolChange)
		}
	}
}

func runEviction(ctx context.Context, store expiryStore, liveness livenessSource, notifier topologyChangeNotifier, expiry time.Duration, onPoolChange func(ctx context.Context)) {
	threshold := time.Now().UTC().Add(-expiry)
	candidates, err := store.ListEvictionCandidates(ctx, threshold)
	if err != nil {
		log.Printf("relay expiry: list candidates: %v", err)
		return
	}

	// A Postgres-stale relay whose Valkey liveness key is fresh is alive — its
	// DB write was merely throttled. Refresh the persisted timestamp instead of
	// evicting. Without a liveness signal (key absent, Valkey error, no cache)
	// fall back to Postgres: while Valkey is failing, Heartbeat forces a DB
	// write on every beat, so the persisted timestamp is authoritative.
	toEvict := make([]string, 0, len(candidates))
	for _, relayID := range candidates {
		if liveness != nil {
			seenAt, ok, err := liveness.LastHeartbeat(ctx, relayID)
			if err != nil {
				log.Printf("relay expiry: read liveness for relay %s (falling back to Postgres): %v", relayID, err)
			} else if ok && !seenAt.Before(threshold) {
				if err := store.RefreshLastHeartbeat(ctx, relayID, seenAt); err != nil {
					log.Printf("relay expiry: refresh heartbeat for relay %s: %v", relayID, err)
				}
				continue
			}
		}
		toEvict = append(toEvict, relayID)
	}

	relayIDs, err := store.EvictRelays(ctx, toEvict, threshold)
	if err != nil {
		log.Printf("relay expiry: evict: %v", err)
		return
	}
	for _, relayID := range relayIDs {
		log.Printf("relay expiry: evicted relay %s", relayID)
		// Clear the write-throttling markers so the relay's first heartbeat
		// after it returns persists to Postgres and re-activates it at once.
		if liveness != nil {
			if err := liveness.ClearHeartbeatThrottle(ctx, relayID); err != nil {
				log.Printf("relay expiry: clear heartbeat throttle for relay %s: %v", relayID, err)
			}
		}
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
