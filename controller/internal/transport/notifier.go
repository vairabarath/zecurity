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
// Phase A wiring: the version is served by CompileTransportSnapshot and the
// GetTransportSnapshot handler. Phase B (ADR-017) rewires the relay
// heartbeat/expiry/registration triggers to call NotifyTopologyChange and
// refines the push hook to target only the affected connectors.
type Notifier struct {
	cache    *SnapshotCache
	mu       sync.Mutex
	versions map[string]*atomic.Uint64

	// pushHook, if set, runs after each NotifyTopologyChange (post version-bump,
	// post cache-invalidate) to drive proactive, topology-scoped transport
	// propagation — it receives the affected connector IDs so the pusher targets
	// only those streams, not the whole workspace. Registered once at startup
	// before serving; must be non-blocking (it runs on the mutation path and must
	// schedule its own async work).
	pushHook func(workspaceID string, affectedConnectorIDs []string)
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

	if n.pushHook != nil {
		n.pushHook(workspaceID, affectedConnectorIDs)
	}
	return nil
}

// RegisterPushHook installs the proactive-push callback fired after every
// NotifyTopologyChange. Call once during startup wiring, before serving.
func (n *Notifier) RegisterPushHook(fn func(workspaceID string, affectedConnectorIDs []string)) {
	n.pushHook = fn
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
