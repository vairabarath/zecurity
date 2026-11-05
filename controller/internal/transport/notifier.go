package transport

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/yourorg/ztna/controller/internal/metrics"
)

// Notifier tracks a monotonic transport version per workspace and invalidates
// the SnapshotCache when the topology changes. It is the transport-plane
// counterpart to policy.Notifier, deliberately independent so a transport
// change never bumps the ACL version and vice versa (the Track B invariant).
//
// There is no proactive push: the client is the sole consumer of the transport
// snapshot and fetches it via GetTransportSnapshot (poll + relay-failure
// re-poll). A topology change bumps the version and invalidates the cache;
// the client's next poll observes the new version and recompiles. Since FU-8
// a change listener (the client wake-up hint hub) also tells connected
// clients that the version moved, so they poll at once; it carries no
// snapshot and the poll stays authoritative. The
// affectedConnectorIDs argument is retained for a future push channel and for
// observability, but is not acted on today.
//
// LIMITATION — single-controller (finding #3): versions and the SnapshotCache
// are process-local and in-memory, exactly like policy.Notifier / policy
// SnapshotCache. Consequences with more than one controller replica, or across
// a restart:
//   - Restart resets versions to 0. A client's known_version (say N) then no
//     longer matches, so its next poll gets a full snapshot once (harmless — a
//     redundant fetch, never a stale route).
//   - Multiple replicas keep independent version counters, so a client that
//     hops between replicas may re-fetch on each hop (churn, still correct).
//
// This is a system-wide single-controller assumption shared with the ACL plane,
// not specific to transport. Making it multi-replica-safe (persist the version
// epoch, or distribute invalidations via Valkey) is Controller-HA scope
// (PENDING-12) and must cover both notifiers together. Safe as-is for a single
// controller.
type Notifier struct {
	cache    *SnapshotCache
	mu       sync.Mutex
	versions map[string]*atomic.Uint64

	// listeners are non-blocking consumers fired after each change (FU-8).
	// Registered at startup before serving, read-only afterwards.
	listeners []func(workspaceID string)
}

// NewNotifier creates a Notifier backed by the given cache.
func NewNotifier(cache *SnapshotCache) *Notifier {
	return &Notifier{
		cache:    cache,
		versions: make(map[string]*atomic.Uint64),
	}
}

// NotifyTopologyChange increments the workspace transport version and
// invalidates its cached snapshot, then fires the push hook targeting only
// affectedConnectorIDs (the connectors touched by this topology event — e.g.
// those placed on the changed/evicted relay, or the single connector that
// re-homed). Clients pick up the new version on their next GetTransportSnapshot.
// It never touches the ACL plane — that is the Track B decoupling invariant.
func (n *Notifier) NotifyTopologyChange(_ context.Context, workspaceID string, affectedConnectorIDs []string) error {
	if workspaceID == "" {
		return fmt.Errorf("notify topology change: workspaceID is required")
	}
	n.mu.Lock()
	v, ok := n.versions[workspaceID]
	if !ok {
		v = &atomic.Uint64{}
		n.versions[workspaceID] = v
	}
	n.mu.Unlock()

	v.Add(1)
	n.cache.Invalidate(workspaceID)
	metrics.RecordPolicyNotification("transport")
	for _, listen := range n.listeners {
		listen(workspaceID)
	}
	return nil
}

// AddChangeListener registers a non-blocking consumer of topology changes,
// fired after the version bump and cache invalidation (FU-8 client wake-up
// hints). Call it during startup wiring, before serving; fn must return
// promptly.
func (n *Notifier) AddChangeListener(fn func(workspaceID string)) {
	n.listeners = append(n.listeners, fn)
}

// Version returns the current transport version for workspaceID (0 if never
// changed).
func (n *Notifier) Version(workspaceID string) uint64 {
	n.mu.Lock()
	v, ok := n.versions[workspaceID]
	n.mu.Unlock()
	if !ok {
		return 0
	}
	return v.Load()
}
