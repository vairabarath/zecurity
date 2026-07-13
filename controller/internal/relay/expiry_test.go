package relay

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeExpiryStore is a hand-rolled fake satisfying the expiryStore interface.
type fakeExpiryStore struct {
	evictedIDs        []string                       // returned by EvictExpiredRelays
	connectorsByRelay map[string]map[string][]string // relayID → (workspaceID → connectorIDs)
	evictErr          error
	listErr           error
	capturedBefore    time.Time // threshold passed to EvictExpiredRelays
}

func (s *fakeExpiryStore) EvictExpiredRelays(_ context.Context, before time.Time) ([]string, error) {
	s.capturedBefore = before
	return s.evictedIDs, s.evictErr
}

func (s *fakeExpiryStore) ListConnectorsForRelay(_ context.Context, relayID string) (map[string][]string, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.connectorsByRelay[relayID], nil
}

// TestRunEviction_EvictsAndNotifiesWorkspaces — Gap 3 regression (Track B).
// When relays are evicted, every workspace that had connectors on those relays
// must receive a NotifyTopologyChange call so the transport snapshot drops the
// dead relay — without recompiling the ACL.
func TestRunEviction_EvictsAndNotifiesWorkspaces(t *testing.T) {
	store := &fakeExpiryStore{
		evictedIDs: []string{"relay-one", "relay-two"},
		connectorsByRelay: map[string]map[string][]string{
			"relay-one": {"ws-a": {"c1"}, "ws-b": {"c2"}},
			"relay-two": {"ws-c": {"c3"}},
		},
	}
	notifier := &fakeNotifier{}

	runEviction(context.Background(), store, notifier, 90*time.Second, nil)

	wantNotified := map[string]bool{"ws-a": true, "ws-b": true, "ws-c": true}
	if len(notifier.called) != 3 {
		t.Fatalf("expected 3 NotifyTopologyChange calls, got %d: %v", len(notifier.called), notifier.called)
	}
	for _, id := range notifier.called {
		if !wantNotified[id] {
			t.Fatalf("unexpected workspace notification %q", id)
		}
	}
}

// TestRunEviction_EmptyEvictionIsNoop — no expired relays → no notifications fired.
func TestRunEviction_EmptyEvictionIsNoop(t *testing.T) {
	store := &fakeExpiryStore{evictedIDs: []string{}}
	notifier := &fakeNotifier{}
	runEviction(context.Background(), store, notifier, 90*time.Second, nil)
	if len(notifier.called) != 0 {
		t.Fatalf("expected no notifications for empty eviction, got %v", notifier.called)
	}
}

// TestRunEviction_StoreEvictErrorNoNotifications — EvictExpiredRelays DB failure
// must not propagate as a panic and must not fire any notifications.
func TestRunEviction_StoreEvictErrorNoNotifications(t *testing.T) {
	store := &fakeExpiryStore{evictErr: errors.New("db failure")}
	notifier := &fakeNotifier{}
	runEviction(context.Background(), store, notifier, 90*time.Second, nil)
	if len(notifier.called) != 0 {
		t.Fatalf("expected no notifications on store error, got %v", notifier.called)
	}
}

// TestRunEviction_ThresholdEqualsNowMinusExpiry — the threshold passed to
// EvictExpiredRelays must be approximately (now - expiry), verifying the
// 3× heartbeat interval calculation in runEviction.
func TestRunEviction_ThresholdEqualsNowMinusExpiry(t *testing.T) {
	const expiry = 90 * time.Second
	store := &fakeExpiryStore{evictedIDs: []string{}}
	notifier := &fakeNotifier{}
	before := time.Now().UTC()
	runEviction(context.Background(), store, notifier, expiry, nil)
	after := time.Now().UTC()

	lo := before.Add(-expiry - time.Second)
	hi := after.Add(-expiry + time.Second)
	if store.capturedBefore.Before(lo) || store.capturedBefore.After(hi) {
		t.Fatalf("evict threshold %v is not within ±1s of (now - %s) [%v, %v]",
			store.capturedBefore, expiry, lo, hi)
	}
}
