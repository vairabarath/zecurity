package connector

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// errorPolicyNotifier is a fake PolicyChangeNotifier that returns an error.
type errorPolicyNotifier struct {
	calls int
}

func (e *errorPolicyNotifier) NotifyPolicyChange(ctx context.Context, workspaceID string) error {
	e.calls++
	return errors.New("policy notifier boom")
}

// errorTransportNotifier is a fake TransportChangeNotifier that returns an error.
type errorTransportNotifier struct {
	calls []string
}

func (e *errorTransportNotifier) NotifyTopologyChange(ctx context.Context, workspaceID string, connectorIDs []string) error {
	e.calls = append(e.calls, connectorIDs...)
	return errors.New("transport notifier boom")
}

// syncRecordingPolicyNotifier is thread-safe for watcher testing.
type syncRecordingPolicyNotifier struct {
	mu    sync.Mutex
	calls map[string]int
}

func newSyncRecordingPolicyNotifier() *syncRecordingPolicyNotifier {
	return &syncRecordingPolicyNotifier{calls: make(map[string]int)}
}

func (s *syncRecordingPolicyNotifier) NotifyPolicyChange(ctx context.Context, workspaceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[workspaceID]++
	return nil
}

func (s *syncRecordingPolicyNotifier) countFor(ws string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[ws]
}

func (s *syncRecordingPolicyNotifier) totalCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, n := range s.calls {
		total += n
	}
	return total
}

// syncRecordingTransportNotifier is thread-safe for watcher testing.
type syncRecordingTransportNotifier struct {
	mu    sync.Mutex
	calls map[string][]string
}

func newSyncRecordingTransportNotifier() *syncRecordingTransportNotifier {
	return &syncRecordingTransportNotifier{calls: make(map[string][]string)}
}

func (s *syncRecordingTransportNotifier) NotifyTopologyChange(ctx context.Context, workspaceID string, connectorIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls[workspaceID] = append(s.calls[workspaceID], connectorIDs...)
	return nil
}

func (s *syncRecordingTransportNotifier) forWorkspace(ws string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := make([]string, len(s.calls[ws]))
	copy(res, s.calls[ws])
	return res
}

func (s *syncRecordingTransportNotifier) totalCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, ids := range s.calls {
		total += len(ids)
	}
	return total
}

func setConnectorHeartbeatStale(t *testing.T, pool *pgxpool.Pool, ctx context.Context, connID string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE connectors SET last_heartbeat_at = NOW() - interval '3 minutes' WHERE id = $1`, connID); err != nil {
		t.Fatalf("set connector stale: %v", err)
	}
}

func setConnectorHeartbeatFresh(t *testing.T, pool *pgxpool.Pool, ctx context.Context, connID string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE connectors SET last_heartbeat_at = NOW() WHERE id = $1`, connID); err != nil {
		t.Fatalf("set connector fresh: %v", err)
	}
}

func runWatcherBriefly(ctx context.Context, pool *pgxpool.Pool, cfg Config, policy PolicyChangeNotifier, topology TransportChangeNotifier) {
	watcherCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunDisconnectWatcher(watcherCtx, pool, cfg, policy, topology)
	}()

	time.Sleep(350 * time.Millisecond)
	cancel()
	<-done
}

// 1. TestDisconnectWatcher_MarksStaleAndNotifiesBothPlanes
func TestDisconnectWatcher_MarksStaleAndNotifiesBothPlanes(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)

	// Part A: Direct call to markDisconnected
	slugW1 := fmt.Sprintf("dw-w1-%d", time.Now().UnixNano())
	w1ID, w1TD, w1RN := seedRevocationWorkspace(t, pool, ctx, slugW1)

	slugW2 := fmt.Sprintf("dw-w2-%d", time.Now().UnixNano())
	w2ID, w2TD, w2RN := seedRevocationWorkspace(t, pool, ctx, slugW2)

	c1 := seedConnector(t, pool, ctx, w1ID, w1RN, w1TD, "active", nil)
	c2 := seedConnector(t, pool, ctx, w1ID, w1RN, w1TD, "active", nil)
	c3 := seedConnector(t, pool, ctx, w1ID, w1RN, w1TD, "active", nil)
	c4 := seedConnector(t, pool, ctx, w2ID, w2RN, w2TD, "active", nil)

	setConnectorHeartbeatStale(t, pool, ctx, c1)
	setConnectorHeartbeatStale(t, pool, ctx, c2)
	setConnectorHeartbeatFresh(t, pool, ctx, c3)
	setConnectorHeartbeatStale(t, pool, ctx, c4)

	transitions, err := markDisconnected(ctx, pool, 90*time.Second)
	if err != nil {
		t.Fatalf("markDisconnected: %v", err)
	}

	w1Conns := transitions[w1ID]
	sort.Strings(w1Conns)
	expectedW1 := []string{c1, c2}
	sort.Strings(expectedW1)
	if len(w1Conns) != 2 || w1Conns[0] != expectedW1[0] || w1Conns[1] != expectedW1[1] {
		t.Errorf("w1 transitions got %v, want %v", w1Conns, expectedW1)
	}

	w2Conns := transitions[w2ID]
	if len(w2Conns) != 1 || w2Conns[0] != c4 {
		t.Errorf("w2 transitions got %v, want [%s]", w2Conns, c4)
	}

	if len(transitions) != 2 {
		t.Errorf("expected 2 workspaces in transitions, got %d", len(transitions))
	}

	// Verify statuses in DB
	checkStatus := func(id, want string) {
		var st string
		if err := pool.QueryRow(ctx, `SELECT status FROM connectors WHERE id = $1`, id).Scan(&st); err != nil {
			t.Fatalf("query status %s: %v", id, err)
		}
		if st != want {
			t.Errorf("connector %s status = %q, want %q", id, st, want)
		}
	}
	checkStatus(c1, "disconnected")
	checkStatus(c2, "disconnected")
	checkStatus(c3, "active")
	checkStatus(c4, "disconnected")

	// Part B: Full RunDisconnectWatcher orchestration in a second workspace pair
	slugW3 := fmt.Sprintf("dw-w3-%d", time.Now().UnixNano())
	w3ID, w3TD, w3RN := seedRevocationWorkspace(t, pool, ctx, slugW3)

	slugW4 := fmt.Sprintf("dw-w4-%d", time.Now().UnixNano())
	w4ID, w4TD, w4RN := seedRevocationWorkspace(t, pool, ctx, slugW4)

	c5 := seedConnector(t, pool, ctx, w3ID, w3RN, w3TD, "active", nil)
	c6 := seedConnector(t, pool, ctx, w3ID, w3RN, w3TD, "active", nil)
	c7 := seedConnector(t, pool, ctx, w4ID, w4RN, w4TD, "active", nil)

	setConnectorHeartbeatStale(t, pool, ctx, c5)
	setConnectorHeartbeatStale(t, pool, ctx, c6)
	setConnectorHeartbeatStale(t, pool, ctx, c7)

	policyFake := newSyncRecordingPolicyNotifier()
	topologyFake := newSyncRecordingTransportNotifier()

	cfg := Config{
		HeartbeatInterval:   50 * time.Millisecond,
		DisconnectThreshold: 90 * time.Second,
	}

	runWatcherBriefly(ctx, pool, cfg, policyFake, topologyFake)

	if policyFake.countFor(w3ID) != 1 {
		t.Errorf("policy fake got %d calls for w3, want 1", policyFake.countFor(w3ID))
	}
	if policyFake.countFor(w4ID) != 1 {
		t.Errorf("policy fake got %d calls for w4, want 1", policyFake.countFor(w4ID))
	}

	topW3 := topologyFake.forWorkspace(w3ID)
	sort.Strings(topW3)
	expectedW3 := []string{c5, c6}
	sort.Strings(expectedW3)
	if len(topW3) != 2 || topW3[0] != expectedW3[0] || topW3[1] != expectedW3[1] {
		t.Errorf("topology fake got %v for w3, want %v", topW3, expectedW3)
	}

	topW4 := topologyFake.forWorkspace(w4ID)
	if len(topW4) != 1 || topW4[0] != c7 {
		t.Errorf("topology fake got %v for w4, want [%s]", topW4, c7)
	}
}

// 2. TestDisconnectWatcher_RevokedUntouchedNoNotify
func TestDisconnectWatcher_RevokedUntouchedNoNotify(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)

	slug := fmt.Sprintf("dw-rev-%d", time.Now().UnixNano())
	wsID, wsTD, wsRN := seedRevocationWorkspace(t, pool, ctx, slug)

	cActive := seedConnector(t, pool, ctx, wsID, wsRN, wsTD, "active", nil)
	now := time.Now()
	cRevoked := seedConnector(t, pool, ctx, wsID, wsRN, wsTD, "revoked", &now)

	setConnectorHeartbeatStale(t, pool, ctx, cActive)
	setConnectorHeartbeatStale(t, pool, ctx, cRevoked)

	policyFake := newSyncRecordingPolicyNotifier()
	topologyFake := newSyncRecordingTransportNotifier()

	cfg := Config{
		HeartbeatInterval:   50 * time.Millisecond,
		DisconnectThreshold: 90 * time.Second,
	}

	runWatcherBriefly(ctx, pool, cfg, policyFake, topologyFake)

	// Revoked connector is never notified
	topConns := topologyFake.forWorkspace(wsID)
	for _, id := range topConns {
		if id == cRevoked {
			t.Errorf("topology notifier was invoked for revoked connector %s", cRevoked)
		}
	}

	// Active connector was notified
	foundActive := false
	for _, id := range topConns {
		if id == cActive {
			foundActive = true
		}
	}
	if !foundActive {
		t.Errorf("topology notifier did not receive active connector %s (got %v)", cActive, topConns)
	}

	// Revoked connector status and revoked_at are untouched
	var revStatus string
	var revAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, revoked_at FROM connectors WHERE id = $1`, cRevoked).Scan(&revStatus, &revAt); err != nil {
		t.Fatalf("query revoked status: %v", err)
	}
	if revStatus != "revoked" {
		t.Errorf("revoked connector status became %q, want 'revoked'", revStatus)
	}
	if revAt == nil {
		t.Error("revoked connector revoked_at became nil")
	}

	// Active connector transitioned to disconnected
	var actStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM connectors WHERE id = $1`, cActive).Scan(&actStatus); err != nil {
		t.Fatalf("query active status: %v", err)
	}
	if actStatus != "disconnected" {
		t.Errorf("active connector status became %q, want 'disconnected'", actStatus)
	}
}

// 3. TestDisconnectWatcher_NonActiveWorkspaceIgnored
func TestDisconnectWatcher_NonActiveWorkspaceIgnored(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)

	slug := fmt.Sprintf("dw-susp-%d", time.Now().UnixNano())
	trustDomain := fmt.Sprintf("ws-%s.zecurity.in", slug)

	var wsID string
	err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, trust_domain, status)
		 VALUES ($1, $1, $2, 'suspended')
		 RETURNING id`,
		slug, trustDomain,
	).Scan(&wsID)
	if err != nil {
		t.Fatalf("seed suspended workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM workspaces WHERE id = $1`, wsID)
	})

	var rnID string
	err = pool.QueryRow(ctx,
		`INSERT INTO remote_networks (tenant_id, name, location)
		 VALUES ($1, 'rn', 'office')
		 RETURNING id`,
		wsID,
	).Scan(&rnID)
	if err != nil {
		t.Fatalf("seed remote_network: %v", err)
	}

	cSusp := seedConnector(t, pool, ctx, wsID, rnID, trustDomain, "active", nil)
	setConnectorHeartbeatStale(t, pool, ctx, cSusp)

	// markDisconnected must not include it
	transitions, err := markDisconnected(ctx, pool, 90*time.Second)
	if err != nil {
		t.Fatalf("markDisconnected: %v", err)
	}
	if _, ok := transitions[wsID]; ok {
		t.Fatalf("markDisconnected included suspended workspace %s", wsID)
	}

	// Watcher must not notify
	policyFake := newSyncRecordingPolicyNotifier()
	topologyFake := newSyncRecordingTransportNotifier()

	cfg := Config{
		HeartbeatInterval:   50 * time.Millisecond,
		DisconnectThreshold: 90 * time.Second,
	}

	runWatcherBriefly(ctx, pool, cfg, policyFake, topologyFake)

	if policyFake.countFor(wsID) != 0 {
		t.Errorf("policy fake received notification for suspended workspace, count=%d", policyFake.countFor(wsID))
	}
	if len(topologyFake.forWorkspace(wsID)) != 0 {
		t.Errorf("topology fake received notification for suspended workspace, got %v", topologyFake.forWorkspace(wsID))
	}

	// Connector remains active
	var st string
	if err := pool.QueryRow(ctx, `SELECT status FROM connectors WHERE id = $1`, cSusp).Scan(&st); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if st != "active" {
		t.Errorf("connector status became %q, want 'active'", st)
	}
}

// 4. TestDisconnectWatcher_NilTopologyNoPanic
func TestDisconnectWatcher_NilTopologyNoPanic(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)

	slug := fmt.Sprintf("dw-niltop-%d", time.Now().UnixNano())
	wsID, wsTD, wsRN := seedRevocationWorkspace(t, pool, ctx, slug)

	c1 := seedConnector(t, pool, ctx, wsID, wsRN, wsTD, "active", nil)
	setConnectorHeartbeatStale(t, pool, ctx, c1)

	policyFake := newSyncRecordingPolicyNotifier()

	cfg := Config{
		HeartbeatInterval:   50 * time.Millisecond,
		DisconnectThreshold: 90 * time.Second,
	}

	// Must not panic when topology is nil
	runWatcherBriefly(ctx, pool, cfg, policyFake, nil)

	if policyFake.countFor(wsID) != 1 {
		t.Errorf("policy fake got %d calls, want 1", policyFake.countFor(wsID))
	}

	var st string
	if err := pool.QueryRow(ctx, `SELECT status FROM connectors WHERE id = $1`, c1).Scan(&st); err != nil {
		t.Fatalf("query status: %v", err)
	}
	if st != "disconnected" {
		t.Errorf("connector status became %q, want 'disconnected'", st)
	}
}

// 5. TestDisconnectWatcher_NotifierErrorIndependence
func TestDisconnectWatcher_NotifierErrorIndependence(t *testing.T) {
	pool, ctx := setupRevocationTestDB(t)

	cfg := Config{
		HeartbeatInterval:   50 * time.Millisecond,
		DisconnectThreshold: 90 * time.Second,
	}

	// Sub-test 1: Error in Policy notifier does NOT prevent Topology notification
	t.Run("PolicyErrorDoesNotSkipTopology", func(t *testing.T) {
		slug := fmt.Sprintf("dw-errpol-%d", time.Now().UnixNano())
		wsID, wsTD, wsRN := seedRevocationWorkspace(t, pool, ctx, slug)

		c1 := seedConnector(t, pool, ctx, wsID, wsRN, wsTD, "active", nil)
		setConnectorHeartbeatStale(t, pool, ctx, c1)

		errPolicy := &errorPolicyNotifier{}
		topFake := newSyncRecordingTransportNotifier()

		runWatcherBriefly(ctx, pool, cfg, errPolicy, topFake)

		if errPolicy.calls < 1 {
			t.Errorf("expected errorPolicy to be called, got %d", errPolicy.calls)
		}
		topConns := topFake.forWorkspace(wsID)
		if len(topConns) != 1 || topConns[0] != c1 {
			t.Errorf("topology fake did not receive connector %s despite policy error, got %v", c1, topConns)
		}
	})

	// Sub-test 2: Error in Topology notifier does NOT prevent Policy notification
	t.Run("TopologyErrorDoesNotSkipPolicy", func(t *testing.T) {
		slug := fmt.Sprintf("dw-errtop-%d", time.Now().UnixNano())
		wsID, wsTD, wsRN := seedRevocationWorkspace(t, pool, ctx, slug)

		c2 := seedConnector(t, pool, ctx, wsID, wsRN, wsTD, "active", nil)
		setConnectorHeartbeatStale(t, pool, ctx, c2)

		polFake := newSyncRecordingPolicyNotifier()
		errTop := &errorTransportNotifier{}

		runWatcherBriefly(ctx, pool, cfg, polFake, errTop)

		if polFake.countFor(wsID) != 1 {
			t.Errorf("expected policy fake to be called once, got %d", polFake.countFor(wsID))
		}
		if len(errTop.calls) != 1 || errTop.calls[0] != c2 {
			t.Errorf("expected errorTop to receive connector %s, got %v", c2, errTop.calls)
		}
	})
}
