package transport

import (
	"log"
	"sync"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
)

// maxCompileRetries bounds GetOrCompile's recompile loop when invalidations keep
// racing the compile. On exhaustion the freshest compiled snapshot is returned
// WITHOUT being cached (best-effort; the next access recompiles cleanly), so
// sustained churn can never spin forever or poison the cache. Mirrors the
// policy SnapshotCache (ADR-013).
const maxCompileRetries = 3

// SnapshotCache is a process-local per-workspace transport snapshot cache.
// Misses compile from DB via GetOrCompile, which stores the result only if no
// invalidation raced the compile (epoch CAS). Topology mutations must call
// Invalidate after a successful DB commit (via the Notifier).
type SnapshotCache struct {
	mu      sync.RWMutex
	entries map[string]*clientv1.TransportSnapshot
	// epoch is the per-workspace invalidation counter. Invalidate bumps it; a
	// compile captures it before reading state and SetIfEpoch stores only if it
	// is unchanged, so a snapshot built from a superseded view is dropped rather
	// than poisoning the slot (ADR-013).
	epoch map[string]uint64
}

// NewSnapshotCache creates an empty transport SnapshotCache.
func NewSnapshotCache() *SnapshotCache {
	return &SnapshotCache{
		entries: make(map[string]*clientv1.TransportSnapshot),
		epoch:   make(map[string]uint64),
	}
}

// Get returns the cached snapshot for workspaceID, or (nil, false) on a miss.
func (c *SnapshotCache) Get(workspaceID string) (*clientv1.TransportSnapshot, bool) {
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

// setLocked applies the version guard and stores. Callers must hold c.mu. It
// never regresses the cached version: it stores only when no entry exists or
// the incoming snapshot.Version >= the cached Version (>= so an equal-version
// set still overwrites with the freshest routing hint).
func (c *SnapshotCache) setLocked(workspaceID string, snapshot *clientv1.TransportSnapshot) {
	if existing, ok := c.entries[workspaceID]; ok && snapshot.Version < existing.Version {
		return
	}
	c.entries[workspaceID] = snapshot
}

// SetIfEpoch stores snapshot only if no invalidation raced the compile that
// produced it — i.e. the workspace's epoch is still observedEpoch. Returns true
// if stored.
func (c *SnapshotCache) SetIfEpoch(workspaceID string, snapshot *clientv1.TransportSnapshot, observedEpoch uint64) bool {
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
// one appeared, otherwise it recompiles at the new epoch. After
// maxCompileRetries it returns the last compiled snapshot uncached.
func (c *SnapshotCache) GetOrCompile(workspaceID string, compileFn func() (*clientv1.TransportSnapshot, error)) (*clientv1.TransportSnapshot, error) {
	if snap, ok := c.Get(workspaceID); ok {
		return snap, nil
	}
	var last *clientv1.TransportSnapshot
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
		if fresh, ok := c.Get(workspaceID); ok {
			return fresh, nil
		}
	}
	log.Printf("transport cache: workspace %s lost epoch CAS %d times; returning uncached snapshot", workspaceID, maxCompileRetries)
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
