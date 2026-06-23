package policy

import (
	"log"
	"sync"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
)

// maxCompileRetries bounds GetOrCompile's recompile loop when invalidations keep
// racing the compile. On exhaustion the freshest compiled snapshot is returned
// WITHOUT being cached (best-effort delivery; the next access recompiles
// cleanly), so sustained churn can never spin forever or poison the cache.
const maxCompileRetries = 3

// SnapshotCache is a process-local per-workspace ACL snapshot cache.
// Misses compile from DB via GetOrCompile, which stores the result only if no
// invalidation raced the compile (epoch CAS). Policy mutations must call
// Invalidate after a successful DB commit.
type SnapshotCache struct {
	mu      sync.RWMutex
	entries map[string]*clientv1.ACLSnapshot
	// epoch is the per-workspace invalidation counter. Invalidate bumps it; a
	// compile captures it before reading any state and SetIfEpoch stores only if
	// it is unchanged, so a snapshot built from a now-superseded view is dropped
	// instead of poisoning the slot (ADR-013).
	epoch map[string]uint64
}

// NewSnapshotCache creates an empty SnapshotCache.
func NewSnapshotCache() *SnapshotCache {
	return &SnapshotCache{
		entries: make(map[string]*clientv1.ACLSnapshot),
		epoch:   make(map[string]uint64),
	}
}

// Get returns the cached snapshot for workspaceID, or (nil, false) on a miss.
func (c *SnapshotCache) Get(workspaceID string) (*clientv1.ACLSnapshot, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.entries[workspaceID]
	return s, ok
}

// Epoch returns the current invalidation epoch for workspaceID (0 if never
// invalidated). Callers capture this BEFORE compiling and pass it to SetIfEpoch.
func (c *SnapshotCache) Epoch(workspaceID string) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.epoch[workspaceID]
}

// set stores a compiled snapshot for workspaceID, but never regresses the cached
// version: it stores only when no entry exists or the incoming snapshot.Version
// >= the cached Version. The comparison is >= (not >) so an equal-version set
// still overwrites — connector_tunnel_addr can refresh on a connector heartbeat
// without a policy version bump, and the latest routing hint should win.
//
// set is epoch-unaware and unexported (ADR-013 seal): the only store paths
// available to callers are GetOrCompile (epoch CAS) and SetIfEpoch, so no caller
// can plant a stale snapshot that bypasses the epoch check.
func (c *SnapshotCache) set(workspaceID string, snapshot *clientv1.ACLSnapshot) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setLocked(workspaceID, snapshot)
}

// setLocked applies the version guard and stores. Callers must hold c.mu.
func (c *SnapshotCache) setLocked(workspaceID string, snapshot *clientv1.ACLSnapshot) {
	if existing, ok := c.entries[workspaceID]; ok && snapshot.Version < existing.Version {
		return
	}
	c.entries[workspaceID] = snapshot
}

// SetIfEpoch stores snapshot only if no invalidation raced the compile that
// produced it — i.e. the workspace's epoch is still observedEpoch. Returns true
// if stored. The version guard from Set still applies as defense-in-depth.
func (c *SnapshotCache) SetIfEpoch(workspaceID string, snapshot *clientv1.ACLSnapshot, observedEpoch uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.epoch[workspaceID] != observedEpoch {
		return false // an Invalidate happened during the compile; drop the stale result
	}
	c.setLocked(workspaceID, snapshot)
	return true
}

// GetOrCompile returns the cached snapshot or compiles one, never caching a
// snapshot that an invalidation raced. It captures the epoch BEFORE compiling
// and stores via SetIfEpoch; on a CAS loss it returns a fresher cached entry if
// one appeared, otherwise it recompiles at the new epoch. After maxCompileRetries
// it returns the last compiled snapshot uncached and logs a warning.
func (c *SnapshotCache) GetOrCompile(workspaceID string, compileFn func() (*clientv1.ACLSnapshot, error)) (*clientv1.ACLSnapshot, error) {
	if snap, ok := c.Get(workspaceID); ok {
		return snap, nil
	}
	var last *clientv1.ACLSnapshot
	for attempt := 0; attempt < maxCompileRetries; attempt++ {
		observed := c.Epoch(workspaceID) // capture before compiling
		snap, err := compileFn()
		if err != nil {
			return nil, err
		}
		last = snap
		if c.SetIfEpoch(workspaceID, snap, observed) {
			return snap, nil
		}
		// An invalidation raced the compile. Prefer a fresher entry a concurrent
		// compiler may have stored; otherwise recompile at the new epoch.
		if fresh, ok := c.Get(workspaceID); ok {
			return fresh, nil
		}
	}
	log.Printf("acl cache: workspace %s lost epoch CAS %d times; returning uncached snapshot", workspaceID, maxCompileRetries)
	return last, nil
}

// Invalidate removes the cached snapshot for workspaceID and bumps its epoch so
// any compile already in flight is dropped by SetIfEpoch rather than poisoning
// the freshly-emptied slot.
func (c *SnapshotCache) Invalidate(workspaceID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, workspaceID)
	c.epoch[workspaceID]++
}
