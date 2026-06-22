package policy

import (
	"sync"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
)

// SnapshotCache is a process-local per-workspace ACL snapshot cache.
// Cache misses must compile from DB and store the result via Set.
// Policy mutations must call Invalidate after a successful DB commit.
type SnapshotCache struct {
	mu      sync.RWMutex
	entries map[string]*clientv1.ACLSnapshot
}

// NewSnapshotCache creates an empty SnapshotCache.
func NewSnapshotCache() *SnapshotCache {
	return &SnapshotCache{entries: make(map[string]*clientv1.ACLSnapshot)}
}

// Get returns the cached snapshot for workspaceID, or (nil, false) on a miss.
func (c *SnapshotCache) Get(workspaceID string) (*clientv1.ACLSnapshot, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.entries[workspaceID]
	return s, ok
}

// Set stores a compiled snapshot for workspaceID, but never regresses the
// cached version: it stores only when no entry exists or the incoming
// snapshot.Version >= the cached Version. Within a process run the notifier
// counter is monotonic per workspace (it only increments on NotifyPolicyChange
// and resets to 0 only on restart, when this cache is also empty), so this
// prevents an out-of-order compile from caching a stale snapshot — the race
// where a v42 compile finishes after a v43 compile and clobbers it (Finding 1).
//
// The comparison is >= (not >) on purpose: an equal-version Set must still
// overwrite, because connector_tunnel_addr can refresh on a connector heartbeat
// without a policy version bump, and we want the latest routing hint to win.
func (c *SnapshotCache) Set(workspaceID string, snapshot *clientv1.ACLSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[workspaceID]; ok && snapshot.Version < existing.Version {
		return
	}
	c.entries[workspaceID] = snapshot
}

// Invalidate removes the cached snapshot for workspaceID so the next Get
// triggers a recompile.
func (c *SnapshotCache) Invalidate(workspaceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, workspaceID)
}
