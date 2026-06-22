package policy

import (
	"sync"
	"testing"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
)

func snap(ws string, version uint64) *clientv1.ACLSnapshot {
	return &clientv1.ACLSnapshot{WorkspaceId: ws, Version: version}
}

// TestSet_StoresWhenAbsent: an empty cache accepts the first snapshot.
func TestSet_StoresWhenAbsent(t *testing.T) {
	c := NewSnapshotCache()
	c.Set("ws", snap("ws", 1))
	got, ok := c.Get("ws")
	if !ok || got.Version != 1 {
		t.Fatalf("want version 1 present, got ok=%v snap=%v", ok, got)
	}
}

// TestSet_AcceptsNewer: a newer version overwrites an older one.
func TestSet_AcceptsNewer(t *testing.T) {
	c := NewSnapshotCache()
	c.Set("ws", snap("ws", 42))
	c.Set("ws", snap("ws", 43))
	got, _ := c.Get("ws")
	if got.Version != 43 {
		t.Fatalf("want version 43, got %d", got.Version)
	}
}

// TestSet_RejectsOlder is the headline version-regression guard. A late-arriving
// older compile must NOT overwrite a newer cached snapshot (review Finding 1).
// This test FAILS against the current unconditional Set and PASSES once the
// version guard lands.
func TestSet_RejectsOlder(t *testing.T) {
	c := NewSnapshotCache()
	c.Set("ws", snap("ws", 43)) // newer compile finishes first
	c.Set("ws", snap("ws", 42)) // older compile finishes later — must be rejected
	got, _ := c.Get("ws")
	if got.Version != 43 {
		t.Fatalf("version regressed: want 43 retained, got %d", got.Version)
	}
}

// TestSet_EqualVersionOverwrites: equal versions must overwrite, because the
// connector_tunnel_addr can refresh on a connector heartbeat without a policy
// version bump. The guard uses >=, not >.
func TestSet_EqualVersionOverwrites(t *testing.T) {
	c := NewSnapshotCache()
	first := snap("ws", 7)
	first.ConnectorTunnelAddr = "10.0.0.1:9092"
	second := snap("ws", 7)
	second.ConnectorTunnelAddr = "10.0.0.2:9092"
	c.Set("ws", first)
	c.Set("ws", second)
	got, _ := c.Get("ws")
	if got.ConnectorTunnelAddr != "10.0.0.2:9092" {
		t.Fatalf("equal-version refresh dropped: want 10.0.0.2:9092, got %q", got.ConnectorTunnelAddr)
	}
}

// TestSet_StoresAfterInvalidate: Invalidate clears the entry so the next Set
// stores unconditionally (no stale high-version entry to compare against).
func TestSet_StoresAfterInvalidate(t *testing.T) {
	c := NewSnapshotCache()
	c.Set("ws", snap("ws", 43))
	c.Invalidate("ws")
	c.Set("ws", snap("ws", 44))
	got, ok := c.Get("ws")
	if !ok || got.Version != 44 {
		t.Fatalf("want version 44 after invalidate, got ok=%v snap=%v", ok, got)
	}
}

// TestSet_PerWorkspaceIndependent: the guard is scoped per workspace; a low
// version in one workspace does not affect another.
func TestSet_PerWorkspaceIndependent(t *testing.T) {
	c := NewSnapshotCache()
	c.Set("a", snap("a", 100))
	c.Set("b", snap("b", 1))
	a, _ := c.Get("a")
	b, _ := c.Get("b")
	if a.Version != 100 || b.Version != 1 {
		t.Fatalf("cross-workspace contamination: a=%d b=%d", a.Version, b.Version)
	}
}

// TestCache_ConcurrentSetGetInvalidate exercises the cache under -race with many
// goroutines hammering Set (increasing versions), Get, and Invalidate. Asserts
// the detector stays clean and the cache never reports a version higher than the
// max ever written (no torn reads / no fabricated versions).
func TestCache_ConcurrentSetGetInvalidate(t *testing.T) {
	c := NewSnapshotCache()
	const workers = 8
	const iters = 500
	var wg sync.WaitGroup

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 1; i <= iters; i++ {
				c.Set("ws", snap("ws", uint64(i)))
				if g, ok := c.Get("ws"); ok && g.Version > uint64(iters) {
					t.Errorf("observed impossible version %d", g.Version)
				}
				if i%50 == 0 {
					c.Invalidate("ws")
				}
			}
		}()
	}
	wg.Wait()
}
