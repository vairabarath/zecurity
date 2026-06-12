package policy

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// Notifier tracks a monotonic policy version per workspace and invalidates
// the SnapshotCache when policy changes. All policy mutations must call
// NotifyPolicyChange after a successful DB commit.
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

// NotifyPolicyChange increments the version counter for workspaceID and
// invalidates its cached snapshot so Connectors receive the latest on their
// next heartbeat and Clients get a fresh compile on the next GetACLSnapshot.
func (n *Notifier) NotifyPolicyChange(_ context.Context, workspaceID string) error {
	if workspaceID == "" {
		return fmt.Errorf("notify policy change: workspaceID is required")
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

// Version returns the current policy version for workspaceID (0 if never changed).
func (n *Notifier) Version(workspaceID string) uint64 {
	n.mu.Lock()
	v, ok := n.versions[workspaceID]
	n.mu.Unlock()
	if !ok {
		return 0
	}
	return v.Load()
}
