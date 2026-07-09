package policy

import (
	"context"
	"errors"
	"sync"
	"testing"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
)

var errBoom = errors.New("boom")

func snap(ws string, version uint64) *clientv1.ACLSnapshot {
	return &clientv1.ACLSnapshot{WorkspaceId: ws, Version: version}
}

// TestSet_StoresWhenAbsent: an empty cache accepts the first snapshot.
func TestSet_StoresWhenAbsent(t *testing.T) {
	c := NewSnapshotCache()
	c.set("ws", snap("ws", 1))
	got, ok := c.Get("ws")
	if !ok || got.Version != 1 {
		t.Fatalf("want version 1 present, got ok=%v snap=%v", ok, got)
	}
}

// TestSet_AcceptsNewer: a newer version overwrites an older one.
func TestSet_AcceptsNewer(t *testing.T) {
	c := NewSnapshotCache()
	c.set("ws", snap("ws", 42))
	c.set("ws", snap("ws", 43))
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
	c.set("ws", snap("ws", 43)) // newer compile finishes first
	c.set("ws", snap("ws", 42)) // older compile finishes later — must be rejected
	got, _ := c.Get("ws")
	if got.Version != 43 {
		t.Fatalf("version regressed: want 43 retained, got %d", got.Version)
	}
}

// TestSet_EqualVersionOverwrites: equal versions must overwrite, because the
// relay routing metadata (relay_addr) can refresh on a heartbeat without a policy
// version bump. The guard uses >=, not >.
func TestSet_EqualVersionOverwrites(t *testing.T) {
	c := NewSnapshotCache()
	first := snap("ws", 7)
	first.RelayAddr = "relay1.example.com:9093"
	second := snap("ws", 7)
	second.RelayAddr = "relay2.example.com:9093"
	c.set("ws", first)
	c.set("ws", second)
	got, _ := c.Get("ws")
	if got.RelayAddr != "relay2.example.com:9093" {
		t.Fatalf("equal-version refresh dropped: want relay2.example.com:9093, got %q", got.RelayAddr)
	}
}

// TestSet_StoresAfterInvalidate: Invalidate clears the entry so the next Set
// stores unconditionally (no stale high-version entry to compare against).
func TestSet_StoresAfterInvalidate(t *testing.T) {
	c := NewSnapshotCache()
	c.set("ws", snap("ws", 43))
	c.Invalidate("ws")
	c.set("ws", snap("ws", 44))
	got, ok := c.Get("ws")
	if !ok || got.Version != 44 {
		t.Fatalf("want version 44 after invalidate, got ok=%v snap=%v", ok, got)
	}
}

// TestSet_PerWorkspaceIndependent: the guard is scoped per workspace; a low
// version in one workspace does not affect another.
func TestSet_PerWorkspaceIndependent(t *testing.T) {
	c := NewSnapshotCache()
	c.set("a", snap("a", 100))
	c.set("b", snap("b", 1))
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
				c.set("ws", snap("ws", uint64(i)))
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

// TestEpoch_ZeroWhenAbsent: a never-invalidated workspace reports epoch 0.
func TestEpoch_ZeroWhenAbsent(t *testing.T) {
	c := NewSnapshotCache()
	if e := c.Epoch("ws"); e != 0 {
		t.Fatalf("epoch of unseen workspace = %d, want 0", e)
	}
}

// TestEpoch_BumpsOnInvalidate: each Invalidate advances the epoch.
func TestEpoch_BumpsOnInvalidate(t *testing.T) {
	c := NewSnapshotCache()
	c.Invalidate("ws")
	c.Invalidate("ws")
	if e := c.Epoch("ws"); e != 2 {
		t.Fatalf("epoch after 2 invalidations = %d, want 2", e)
	}
}

// TestSetIfEpoch_AcceptsOnMatch: stores when the epoch is unchanged.
func TestSetIfEpoch_AcceptsOnMatch(t *testing.T) {
	c := NewSnapshotCache()
	ep := c.Epoch("ws")
	if !c.SetIfEpoch("ws", snap("ws", 1), ep) {
		t.Fatal("SetIfEpoch should store when epoch matches")
	}
	if got, _ := c.Get("ws"); got == nil || got.Version != 1 {
		t.Fatalf("want v1 stored, got %v", got)
	}
}

// TestSetIfEpoch_RejectsOnAdvance: a snapshot whose compile was raced by an
// invalidation is dropped, even into an empty slot — this is the stale-insert fix.
func TestSetIfEpoch_RejectsOnAdvance(t *testing.T) {
	c := NewSnapshotCache()
	ep := c.Epoch("ws") // captured "before compile"
	c.Invalidate("ws")  // a change races the compile
	if c.SetIfEpoch("ws", snap("ws", 1), ep) {
		t.Fatal("SetIfEpoch must reject a snapshot whose epoch was superseded")
	}
	if _, ok := c.Get("ws"); ok {
		t.Fatal("rejected snapshot must not poison the empty slot")
	}
}

// TestSetIfEpoch_KeepsVersionGuard: even on an epoch match, a strictly-older
// version does not overwrite a newer cached one.
func TestSetIfEpoch_KeepsVersionGuard(t *testing.T) {
	c := NewSnapshotCache()
	ep := c.Epoch("ws")
	c.SetIfEpoch("ws", snap("ws", 43), ep)
	c.SetIfEpoch("ws", snap("ws", 42), ep) // same epoch, older version
	if got, _ := c.Get("ws"); got.Version != 43 {
		t.Fatalf("version regressed to %d, want 43", got.Version)
	}
}

// TestGetOrCompile_HitReturnsCached: a cache hit short-circuits compilation.
func TestGetOrCompile_HitReturnsCached(t *testing.T) {
	c := NewSnapshotCache()
	c.set("ws", snap("ws", 5))
	calls := 0
	got, err := c.GetOrCompile("ws", func() (*clientv1.ACLSnapshot, error) {
		calls++
		return snap("ws", 99), nil
	})
	if err != nil || got.Version != 5 || calls != 0 {
		t.Fatalf("hit should return cached v5 without compiling; got v%d err=%v calls=%d", got.GetVersion(), err, calls)
	}
}

// TestGetOrCompile_MissCompilesAndStores: a miss compiles once and caches.
func TestGetOrCompile_MissCompilesAndStores(t *testing.T) {
	c := NewSnapshotCache()
	calls := 0
	got, err := c.GetOrCompile("ws", func() (*clientv1.ACLSnapshot, error) {
		calls++
		return snap("ws", 7), nil
	})
	if err != nil || got.Version != 7 || calls != 1 {
		t.Fatalf("miss should compile once -> v7; got v%d err=%v calls=%d", got.GetVersion(), err, calls)
	}
	if cached, ok := c.Get("ws"); !ok || cached.Version != 7 {
		t.Fatalf("compiled snapshot should be cached, got ok=%v %v", ok, cached)
	}
}

// TestGetOrCompile_MidCompileInvalidationRecompiles is the unit-level analogue of
// the proven proactive-push bug: an invalidation during compile #1 forces a
// recompile, and the latest version (not the stale one) is returned and cached.
func TestGetOrCompile_MidCompileInvalidationRecompiles(t *testing.T) {
	c := NewSnapshotCache()
	calls := 0
	got, err := c.GetOrCompile("ws", func() (*clientv1.ACLSnapshot, error) {
		calls++
		if calls == 1 {
			c.Invalidate("ws") // a change races compile #1
			return snap("ws", 1), nil
		}
		return snap("ws", 2), nil // recompile sees the latest
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("returned stale v%d, want recompiled v2", got.Version)
	}
	if calls != 2 {
		t.Fatalf("want exactly 2 compiles (initial + 1 recompile), got %d", calls)
	}
	if cached, _ := c.Get("ws"); cached == nil || cached.Version != 2 {
		t.Fatalf("cache should hold v2, got %v", cached)
	}
}

// TestGetOrCompile_CompileErrorNotCached: a compile error returns the error and
// caches nothing (default-deny).
func TestGetOrCompile_CompileErrorNotCached(t *testing.T) {
	c := NewSnapshotCache()
	_, err := c.GetOrCompile("ws", func() (*clientv1.ACLSnapshot, error) {
		return nil, errBoom
	})
	if err == nil {
		t.Fatal("want error propagated")
	}
	if _, ok := c.Get("ws"); ok {
		t.Fatal("nothing should be cached on compile error")
	}
}

// TestGetOrCompile_BoundedUnderChurn: continuous invalidation never blocks; after
// the retry cap a non-nil snapshot is returned and nothing stale is cached.
func TestGetOrCompile_BoundedUnderChurn(t *testing.T) {
	c := NewSnapshotCache()
	calls := 0
	got, err := c.GetOrCompile("ws", func() (*clientv1.ACLSnapshot, error) {
		calls++
		c.Invalidate("ws") // every compile is immediately superseded
		return snap("ws", uint64(calls)), nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("must return a best-effort snapshot, not nil")
	}
	if calls != maxCompileRetries {
		t.Fatalf("want exactly %d compile attempts, got %d", maxCompileRetries, calls)
	}
	if _, ok := c.Get("ws"); ok {
		t.Fatal("exhausted retry must not cache (return-last-uncached)")
	}
}

// TestGetOrCompile_CrossPathConvergesToLatest models all three compile paths
// (heartbeat / proactive / client-pull) hammering one workspace concurrently
// with a real NotifyPolicyChange mutator (bump + invalidate). Under -race it
// proves no data race, no served version ahead of the notifier, and — once churn
// settles — convergence to exactly the latest version (no stale poison survives).
func TestGetOrCompile_CrossPathConvergesToLatest(t *testing.T) {
	cache := NewSnapshotCache()
	n := NewNotifier(cache)
	const ws = "ws"
	const iters = 400
	var wg sync.WaitGroup

	compileLatest := func() (*clientv1.ACLSnapshot, error) {
		return &clientv1.ACLSnapshot{WorkspaceId: ws, Version: n.Version(ws)}, nil
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = n.NotifyPolicyChange(context.Background(), ws)
		}
	}()

	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				snap, err := cache.GetOrCompile(ws, compileLatest)
				if err == nil && snap != nil && snap.Version > n.Version(ws) {
					t.Errorf("served version %d exceeds notifier version %d", snap.Version, n.Version(ws))
				}
			}
		}()
	}
	wg.Wait()

	// After churn settles, a fresh compile must deliver exactly the latest version.
	cache.Invalidate(ws)
	final, err := cache.GetOrCompile(ws, compileLatest)
	if err != nil {
		t.Fatalf("final compile: %v", err)
	}
	if final.Version != n.Version(ws) {
		t.Fatalf("post-churn served v%d, want latest v%d", final.Version, n.Version(ws))
	}
}

// TestCache_ConcurrentGetOrCompileAndInvalidate exercises the epoch path under
// -race across workspaces.
func TestCache_ConcurrentGetOrCompileAndInvalidate(t *testing.T) {
	c := NewSnapshotCache()
	const workers = 8
	const iters = 300
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ws := "ws-" + string(rune('a'+(w%3)))
			for i := 1; i <= iters; i++ {
				_, _ = c.GetOrCompile(ws, func() (*clientv1.ACLSnapshot, error) {
					return snap(ws, uint64(i)), nil
				})
				if i%25 == 0 {
					c.Invalidate(ws)
				}
			}
		}(w)
	}
	wg.Wait()
}
