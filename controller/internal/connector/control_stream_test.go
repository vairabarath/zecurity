package connector

import (
	"context"
	"sync"
	"testing"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/policy"
)

// TestPushACLSnapshot_DeliversCachedAndRespectsGate verifies the heartbeat path
// after the GetOrCompile conversion (ADR-013): on a cache hit it delivers the
// cached snapshot when the connector lags, and skips the push (version gate)
// when the connector is already current. DB-free — the cache is pre-seeded so
// GetOrCompile's fast-path Get hits and no compile runs. The epoch mechanism
// itself is covered by the policy-package cache tests.
func TestPushACLSnapshot_DeliversCachedAndRespectsGate(t *testing.T) {
	const ws = "ws-A"
	cache := policy.NewSnapshotCache()
	// Seed via the public epoch-aware store (Set is unexported after the ADR-013 seal).
	cache.SetIfEpoch(ws, &clientv1.ACLSnapshot{WorkspaceId: ws, Version: 2}, cache.Epoch(ws))

	h := &EnrollmentHandler{
		PolicyStore:    policy.NewStore(nil), // non-nil for the guard; never used on a cache hit
		PolicyCache:    cache,
		PolicyNotifier: policy.NewNotifier(cache),
	}

	// Connector behind (v1) -> receives the cached v2.
	lagging := testClient("c1", ws)
	if err := h.pushACLSnapshot(context.Background(), lagging, 1); err != nil {
		t.Fatalf("pushACLSnapshot (lagging): %v", err)
	}
	if len(lagging.outbound) != 1 {
		t.Fatalf("lagging connector got %d messages, want 1", len(lagging.outbound))
	}
	if v := (<-lagging.outbound).GetAclSnapshot().GetVersion(); v != 2 {
		t.Fatalf("delivered version %d, want 2", v)
	}

	// Connector current (v2) -> version gate skips the push.
	current := testClient("c2", ws)
	if err := h.pushACLSnapshot(context.Background(), current, 2); err != nil {
		t.Fatalf("pushACLSnapshot (current): %v", err)
	}
	if len(current.outbound) != 0 {
		t.Fatalf("current connector got %d messages, want 0 (gate should skip)", len(current.outbound))
	}
}

func testClient(connectorID, tenantID string) *connectorStreamClient {
	return &connectorStreamClient{
		outbound:    make(chan *pb.ConnectorControlMessage, connectorSendQueueSize),
		connectorID: connectorID,
		tenantID:    tenantID,
	}
}

// TestClientsForWorkspace_FiltersByTenant: only clients whose tenantID matches
// are returned.
func TestClientsForWorkspace_FiltersByTenant(t *testing.T) {
	r := NewConnectorRegistry()
	r.add("c1", testClient("c1", "ws-A"))
	r.add("c2", testClient("c2", "ws-A"))
	r.add("c3", testClient("c3", "ws-B"))

	got := r.ClientsForWorkspace("ws-A")
	if len(got) != 2 {
		t.Fatalf("want 2 clients for ws-A, got %d", len(got))
	}
	for _, c := range got {
		if c.tenantID != "ws-A" {
			t.Fatalf("returned client from wrong workspace: %s", c.tenantID)
		}
	}
}

// TestClientsForWorkspace_EmptyWhenNone: an unknown workspace yields an empty
// slice (and no nil dereference when ranged over).
func TestClientsForWorkspace_EmptyWhenNone(t *testing.T) {
	r := NewConnectorRegistry()
	r.add("c1", testClient("c1", "ws-A"))
	got := r.ClientsForWorkspace("ws-unknown")
	if len(got) != 0 {
		t.Fatalf("want 0 clients, got %d", len(got))
	}
	for range got { // must not panic
	}
}

// TestClientsForWorkspace_ReturnsCopy: mutating the registry after the call does
// not change the already-returned slice (callers send outside the lock).
func TestClientsForWorkspace_ReturnsCopy(t *testing.T) {
	r := NewConnectorRegistry()
	r.add("c1", testClient("c1", "ws-A"))
	got := r.ClientsForWorkspace("ws-A")
	r.remove("c1")
	if len(got) != 1 {
		t.Fatalf("returned slice changed after remove: got %d", len(got))
	}
}

// TestRegistry_ConcurrentAddRemoveScan hammers add/remove/get/ClientsForWorkspace
// concurrently under -race to prove the RWMutex protects all access paths.
func TestRegistry_ConcurrentAddRemoveScan(t *testing.T) {
	r := NewConnectorRegistry()
	const workers = 8
	const iters = 300
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			id := "c" + string(rune('a'+w))
			ws := "ws-" + string(rune('a'+(w%3)))
			for i := 0; i < iters; i++ {
				r.add(id, testClient(id, ws))
				_ = r.get(id)
				_ = r.ClientsForWorkspace(ws)
				r.remove(id)
			}
		}(w)
	}
	wg.Wait()
}

// TestConnectorSendFailsFastWhenQueueFull asserts the F14 liveness guarantee:
// send() enqueues into the outbound mailbox and, when the mailbox is full (a
// connector that has stopped draining its stream), returns an error immediately
// instead of blocking the caller. The writer goroutine — not send — is the only
// thing that may block on stream.Send.
func TestConnectorSendFailsFastWhenQueueFull(t *testing.T) {
	c := &connectorStreamClient{
		outbound:    make(chan *pb.ConnectorControlMessage, 1),
		connectorID: "c1",
	}

	if err := c.send(&pb.ConnectorControlMessage{}); err != nil {
		t.Fatalf("first send should enqueue into an empty mailbox: %v", err)
	}
	if err := c.send(&pb.ConnectorControlMessage{}); err == nil {
		t.Fatal("send into a full mailbox must fail fast, not block")
	}

	// Draining one slot frees capacity for another non-blocking send.
	<-c.outbound
	if err := c.send(&pb.ConnectorControlMessage{}); err != nil {
		t.Fatalf("send should enqueue again after the mailbox drains: %v", err)
	}
}
