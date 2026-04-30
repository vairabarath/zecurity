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

// Set stores a compiled snapshot for workspaceID.
func (c *SnapshotCache) Set(workspaceID string, snapshot *clientv1.ACLSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[workspaceID] = snapshot
}

// Invalidate removes the cached snapshot for workspaceID so the next Get
// triggers a recompile.
func (c *SnapshotCache) Invalidate(workspaceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, workspaceID)
}
