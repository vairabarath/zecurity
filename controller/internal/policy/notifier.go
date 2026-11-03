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

	// pushHook, if set, is invoked after each successful NotifyPolicyChange
	// (post version-bump, post cache-invalidate) to drive proactive ACL
	// propagation. It is registered once at startup via RegisterPushHook,
	// before serving, and only read afterward — so no lock guards it. The hook
	// MUST be non-blocking: NotifyPolicyChange runs on the mutation request
	// path, so the hook must schedule its own async work and return promptly.
	// It takes only the workspaceID (no ctx) by design, so the async worker is
	// forced to mint its own background context rather than capture a
	// request-scoped one that is cancelled when the mutation returns.
	pushHook func(workspaceID string)

	// listeners are extra non-blocking consumers fired after pushHook (FU-8:
	// the client wake-up hint hub). Same contract as pushHook: registered at
	// startup before serving, read-only afterwards, must return promptly. They
	// are separate from pushHook so the connector push never depends on them.
	listeners []func(workspaceID string)
}

var (
    expiryNotifyOnce sync.Once
    expiryNotifier   func(string)
)
// NewNotifier creates a Notifier backed by the given cache.
func NewNotifier(cache *SnapshotCache) *Notifier {
	return &Notifier{
		cache:    cache,
		versions: make(map[string]*atomic.Uint64),
	}
}

func RegisterExpiryNotifier(fn func(string)) {
    expiryNotifyOnce.Do(func() {
        expiryNotifier = fn
    })
}

func notifyExpiryEvent(workspaceID string) {
    if expiryNotifier != nil {
        expiryNotifier(workspaceID)
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

	// Fire the proactive-push hook after the version is bumped and the cache is
	// invalidated, so a hook that recompiles observes the new version and a cold
	// cache. The hook is non-blocking by contract.
	if n.pushHook != nil {
		n.pushHook(workspaceID)
	}
	for _, listen := range n.listeners {
		listen(workspaceID)
	}
	return nil
}

// AddChangeListener registers an extra non-blocking consumer of policy
// changes, fired after the push hook (FU-8 client wake-up hints). Call it
// during startup wiring, before serving; fn must return promptly.
func (n *Notifier) AddChangeListener(fn func(workspaceID string)) {
	n.listeners = append(n.listeners, fn)
}

// RegisterPushHook installs the proactive-push callback fired after every
// successful NotifyPolicyChange. Call it once during startup wiring, before the
// server begins handling mutations. The callback must return quickly and
// schedule its own async work (see the pushHook field doc).
func (n *Notifier) RegisterPushHook(fn func(workspaceID string)) {
	n.pushHook = fn
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
