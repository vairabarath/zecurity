package transport

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// Notifier tracks a monotonic transport version per workspace and invalidates
// the SnapshotCache when the topology changes. It is the transport-plane
// counterpart to policy.Notifier, deliberately independent so a transport
// change never bumps the ACL version and vice versa (the Track B invariant).
//
// There is no proactive push: the client is the sole consumer of the transport
// snapshot and fetches it via GetTransportSnapshot (poll + relay-failure
// re-poll). A topology change just bumps the version and invalidates the cache;
// the client's next poll observes the new version and recompiles. The
// affectedConnectorIDs argument is retained for a future push channel and for
// observability, but is not acted on today.
type Notifier struct {
	cache    *SnapshotCache
	mu       sync.Mutex
	versions map[string]*atomic.Uint64
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
	return nil
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
