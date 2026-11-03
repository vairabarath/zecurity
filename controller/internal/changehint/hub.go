// Package changehint fans workspace change signals out to the clients' open
// WatchChanges streams (FU-8, ADR-018 amendment 2026-11-02).
//
// A signal carries nothing: it only wakes the stream, which then reads the
// workspace's current ACL and transport versions and sends those. So signals
// coalesce freely (a 1-slot channel per stream), a slow or stuck stream never
// blocks the notifier that fired, and nothing queues up per subscriber.
// Process-local, like the notifiers it listens to (D-02, single controller).
package changehint

import "sync"

// Hub holds the open subscriptions, at most one per device.
type Hub struct {
	mu   sync.Mutex
	subs map[string]map[string]*Subscription // workspace ID -> device ID -> subscription
}

// New returns an empty Hub.
func New() *Hub {
	return &Hub{subs: make(map[string]map[string]*Subscription)}
}

// Subscription is one open stream's registration.
type Subscription struct {
	// C receives a value when the workspace changed since the last receive.
	// Several changes before a receive collapse into one.
	C <-chan struct{}
	// Replaced is closed when a newer stream of the same device subscribed;
	// the older stream should end.
	Replaced <-chan struct{}

	c         chan struct{}
	replaced  chan struct{}
	hub       *Hub
	workspace string
	device    string
}

// Subscribe registers a stream for workspaceID. A previous subscription of the
// same device is replaced (its Replaced channel closes), which bounds the
// registrations to one per device and drops a half-open stream on reconnect.
// Subscribe before reading the versions to send, so a change made after the
// read always wakes the stream.
func (h *Hub) Subscribe(workspaceID, deviceID string) *Subscription {
	c := make(chan struct{}, 1)
	replaced := make(chan struct{})
	sub := &Subscription{
		C: c, Replaced: replaced,
		c: c, replaced: replaced,
		hub: h, workspace: workspaceID, device: deviceID,
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	devices := h.subs[workspaceID]
	if devices == nil {
		devices = make(map[string]*Subscription)
		h.subs[workspaceID] = devices
	}
	if old := devices[deviceID]; old != nil {
		close(old.replaced)
	}
	devices[deviceID] = sub
	return sub
}

// Close removes the subscription if it is still the device's current one.
// Idempotent; safe after Replaced closed.
func (s *Subscription) Close() {
	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	devices := h.subs[s.workspace]
	if devices[s.device] != s {
		return
	}
	delete(devices, s.device)
	if len(devices) == 0 {
		delete(h.subs, s.workspace)
	}
}

// Notify wakes every stream of workspaceID. It never blocks: a stream that
// already has a pending wake keeps that one.
func (h *Hub) Notify(workspaceID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, sub := range h.subs[workspaceID] {
		select {
		case sub.c <- struct{}{}:
		default:
		}
	}
}

// Len returns the number of open subscriptions (all workspaces).
func (h *Hub) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, devices := range h.subs {
		n += len(devices)
	}
	return n
}
