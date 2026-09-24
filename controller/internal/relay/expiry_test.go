package relay

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fakeExpiryStore is a hand-rolled fake satisfying the expiryStore interface.
// EvictRelays evicts every requested ID (the fake has no concurrent heartbeat);
// the guarded-UPDATE race is covered by store_evict_integration_test.go.
type fakeExpiryStore struct {
	candidates        []string                       // returned by ListEvictionCandidates
	connectorsByRelay map[string]map[string][]string // relayID → (workspaceID → connectorIDs)
	candidatesErr     error
	evictErr          error
	listErr           error
	capturedBefore    time.Time            // threshold passed to ListEvictionCandidates
	evictBefore       time.Time            // threshold passed to EvictRelays
	evictRequested    []string             // IDs passed to EvictRelays
	refreshed         map[string]time.Time // relayID → seenAt passed to RefreshLastHeartbeat
}

func (s *fakeExpiryStore) ListEvictionCandidates(_ context.Context, before time.Time) ([]string, error) {
	s.capturedBefore = before
	return s.candidates, s.candidatesErr
}

func (s *fakeExpiryStore) RefreshLastHeartbeat(_ context.Context, relayID string, seenAt time.Time) error {
	if s.refreshed == nil {
		s.refreshed = make(map[string]time.Time)
	}
	s.refreshed[relayID] = seenAt
	return nil
}

func (s *fakeExpiryStore) EvictRelays(_ context.Context, relayIDs []string, before time.Time) ([]string, error) {
	s.evictBefore = before
	s.evictRequested = append([]string(nil), relayIDs...)
	if s.evictErr != nil {
		return nil, s.evictErr
	}
	return append([]string(nil), relayIDs...), nil
}

// fakeLiveness is a hand-rolled livenessSource (the Valkey liveness key).
type fakeLiveness struct {
	lastSeen map[string]time.Time // present = key exists
	err      error                // returned by LastHeartbeat for every relay
	cleared  []string             // relay IDs passed to ClearHeartbeatThrottle
}

func (l *fakeLiveness) LastHeartbeat(_ context.Context, relayID string) (time.Time, bool, error) {
	if l.err != nil {
		return time.Time{}, false, l.err
	}
	t, ok := l.lastSeen[relayID]
	return t, ok, nil
}

func (l *fakeLiveness) ClearHeartbeatThrottle(_ context.Context, relayID string) error {
	l.cleared = append(l.cleared, relayID)
	return nil
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
		candidates: []string{"relay-one", "relay-two"},
		connectorsByRelay: map[string]map[string][]string{
			"relay-one": {"ws-a": {"c1"}, "ws-b": {"c2"}},
			"relay-two": {"ws-c": {"c3"}},
		},
	}
	notifier := &fakeNotifier{}

	runEviction(context.Background(), store, nil, notifier, 90*time.Second, nil)

	wantNotified := map[string]bool{"ws-a": true, "ws-b": true, "ws-c": true}
	if len(notifier.called) != 3 {
		t.Fatalf("expected 3 NotifyTopologyChange calls, got %d: %v", len(notifier.called), notifier.called)
	}
	for _, id := range notifier.called {
		if !wantNotified[id] {
			t.Fatalf("unexpected workspace notification %q", id)
		}
	}

	expectedConnectors := map[string][]string{
		"ws-a": {"c1"},
		"ws-b": {"c2"},
		"ws-c": {"c3"},
	}

	for workspaceID, expected := range expectedConnectors {
		actual := notifier.connectorIDs[workspaceID]

		if len(actual) != len(expected) {
			t.Fatalf(
				"workspace %q connector count = %d, want %d: %v",
				workspaceID,
				len(actual),
				len(expected),
				actual,
			)
		}

		for index := range expected {
			if actual[index] != expected[index] {
				t.Fatalf(
					"workspace %q connectors = %v, want %v",
					workspaceID,
					actual,
					expected,
				)
			}
		}
	}
}

// TestRunEviction_EmptyEvictionIsNoop — no expired relays → no notifications fired.
func TestRunEviction_EmptyEvictionIsNoop(t *testing.T) {
	store := &fakeExpiryStore{candidates: []string{}}
	notifier := &fakeNotifier{}
	runEviction(context.Background(), store, nil, notifier, 90*time.Second, nil)
	if len(notifier.called) != 0 {
		t.Fatalf("expected no notifications for empty eviction, got %v", notifier.called)
	}
}

// TestRunEviction_StoreEvictErrorNoNotifications — EvictRelays DB failure
// must not propagate as a panic and must not fire any notifications.
func TestRunEviction_StoreEvictErrorNoNotifications(t *testing.T) {
	store := &fakeExpiryStore{candidates: []string{"relay-one"}, evictErr: errors.New("db failure")}
	notifier := &fakeNotifier{}
	runEviction(context.Background(), store, nil, notifier, 90*time.Second, nil)
	if len(notifier.called) != 0 {
		t.Fatalf("expected no notifications on store error, got %v", notifier.called)
	}
}

// TestRunEviction_ThresholdEqualsNowMinusExpiry — the threshold passed to
// ListEvictionCandidates must be approximately (now - expiry), verifying the
// 3× heartbeat interval calculation in runEviction.
func TestRunEviction_ThresholdEqualsNowMinusExpiry(t *testing.T) {
	const expiry = 90 * time.Second
	store := &fakeExpiryStore{candidates: []string{}}
	notifier := &fakeNotifier{}
	before := time.Now().UTC()
	runEviction(context.Background(), store, nil, notifier, expiry, nil)
	after := time.Now().UTC()

	lo := before.Add(-expiry - time.Second)
	hi := after.Add(-expiry + time.Second)
	if store.capturedBefore.Before(lo) || store.capturedBefore.After(hi) {
		t.Fatalf("evict threshold %v is not within ±1s of (now - %s) [%v, %v]",
			store.capturedBefore, expiry, lo, hi)
	}
}

// TestRunEviction_FreshLivenessRefreshesInsteadOfEvicting — a relay whose
// Postgres heartbeat is stale only because DB writes are throttled must NOT be
// evicted when its Valkey liveness key is fresh: the persisted timestamp is
// refreshed instead, with no topology notification and no relay-list broadcast.
// A genuinely dead relay in the same sweep is still evicted.
func TestRunEviction_FreshLivenessRefreshesInsteadOfEvicting(t *testing.T) {
	seenAt := time.Now().UTC().Add(-10 * time.Second).Truncate(time.Second)
	store := &fakeExpiryStore{
		candidates: []string{"relay-alive", "relay-dead"},
		connectorsByRelay: map[string]map[string][]string{
			"relay-alive": {"ws-a": {"c1"}},
			"relay-dead":  {"ws-b": {"c2"}},
		},
	}
	liveness := &fakeLiveness{lastSeen: map[string]time.Time{"relay-alive": seenAt}}
	notifier := &fakeNotifier{}
	poolChanges := 0

	runEviction(context.Background(), store, liveness, notifier, 90*time.Second,
		func(context.Context) { poolChanges++ })

	if got, ok := store.refreshed["relay-alive"]; !ok || !got.Equal(seenAt) {
		t.Fatalf("relay-alive refreshed = %v (ok=%v), want %v", got, ok, seenAt)
	}
	if len(store.evictRequested) != 1 || store.evictRequested[0] != "relay-dead" {
		t.Fatalf("EvictRelays requested %v, want [relay-dead]", store.evictRequested)
	}
	if len(notifier.called) != 1 || notifier.called[0] != "ws-b" {
		t.Fatalf("topology notifications = %v, want only ws-b (the dead relay)", notifier.called)
	}
	if poolChanges != 1 {
		t.Fatalf("onPoolChange called %d times, want 1", poolChanges)
	}
	if len(liveness.cleared) != 1 || liveness.cleared[0] != "relay-dead" {
		t.Fatalf("throttle cleared for %v, want only [relay-dead]", liveness.cleared)
	}
}

// TestRunEviction_StaleEverywhereEvictsAndClearsThrottle — with no fresh
// liveness signal (key absent, or older than the threshold) the relay is
// evicted, its workspaces are notified on the transport plane, the relay list
// is re-broadcast once, and the write-throttling markers are cleared so its
// first heartbeat after returning persists to Postgres.
func TestRunEviction_StaleEverywhereEvictsAndClearsThrottle(t *testing.T) {
	store := &fakeExpiryStore{
		candidates: []string{"relay-absent", "relay-stale"},
		connectorsByRelay: map[string]map[string][]string{
			"relay-absent": {"ws-a": {"c1"}},
			"relay-stale":  {"ws-b": {"c2"}},
		},
	}
	liveness := &fakeLiveness{lastSeen: map[string]time.Time{
		"relay-stale": time.Now().UTC().Add(-5 * time.Minute),
	}}
	notifier := &fakeNotifier{}
	poolChanges := 0

	runEviction(context.Background(), store, liveness, notifier, 90*time.Second,
		func(context.Context) { poolChanges++ })

	if len(store.refreshed) != 0 {
		t.Fatalf("no relay should be refreshed, got %v", store.refreshed)
	}
	if len(store.evictRequested) != 2 {
		t.Fatalf("EvictRelays requested %v, want both relays", store.evictRequested)
	}
	if !store.evictBefore.Equal(store.capturedBefore) {
		t.Fatalf("EvictRelays threshold %v != candidate threshold %v", store.evictBefore, store.capturedBefore)
	}
	if len(notifier.called) != 2 {
		t.Fatalf("topology notifications = %v, want ws-a and ws-b", notifier.called)
	}
	if poolChanges != 1 {
		t.Fatalf("onPoolChange called %d times, want 1", poolChanges)
	}
	if len(liveness.cleared) != 2 {
		t.Fatalf("throttle cleared for %v, want both evicted relays", liveness.cleared)
	}
}

// TestRunEviction_LivenessErrorFallsBackToPostgres — when Valkey cannot be
// read the sweep falls back to Postgres-only eviction (the pre-Phase-A
// behaviour). This is safe: on a cache error Heartbeat forces a DB write on
// every beat, so the persisted timestamp is fresh for live relays.
func TestRunEviction_LivenessErrorFallsBackToPostgres(t *testing.T) {
	store := &fakeExpiryStore{
		candidates:        []string{"relay-one"},
		connectorsByRelay: map[string]map[string][]string{"relay-one": {"ws-a": {"c1"}}},
	}
	liveness := &fakeLiveness{err: errors.New("valkey unavailable")}
	notifier := &fakeNotifier{}

	runEviction(context.Background(), store, liveness, notifier, 90*time.Second, nil)

	if len(store.refreshed) != 0 {
		t.Fatalf("no relay should be refreshed on a liveness error, got %v", store.refreshed)
	}
	if len(store.evictRequested) != 1 || store.evictRequested[0] != "relay-one" {
		t.Fatalf("EvictRelays requested %v, want [relay-one]", store.evictRequested)
	}
	if len(notifier.called) != 1 || notifier.called[0] != "ws-a" {
		t.Fatalf("topology notifications = %v, want [ws-a]", notifier.called)
	}
}
