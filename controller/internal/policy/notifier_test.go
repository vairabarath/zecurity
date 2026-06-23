package policy

import (
	"context"
	"sync"
	"testing"
)

// TestNotify_FiresHookWithWorkspace: the hook receives the mutated workspaceID.
func TestNotify_FiresHookWithWorkspace(t *testing.T) {
	n := NewNotifier(NewSnapshotCache())
	got := make(chan string, 1)
	n.RegisterPushHook(func(ws string) { got <- ws })

	if err := n.NotifyPolicyChange(context.Background(), "ws-1"); err != nil {
		t.Fatalf("notify: %v", err)
	}
	select {
	case ws := <-got:
		if ws != "ws-1" {
			t.Fatalf("hook got %q, want ws-1", ws)
		}
	default:
		t.Fatal("hook was not fired")
	}
}

// TestNotify_NilHookSafe: NotifyPolicyChange must not panic when no hook is set.
func TestNotify_NilHookSafe(t *testing.T) {
	n := NewNotifier(NewSnapshotCache())
	if err := n.NotifyPolicyChange(context.Background(), "ws-1"); err != nil {
		t.Fatalf("notify with nil hook: %v", err)
	}
}

// TestNotify_OrderingBumpInvalidateThenHook: inside the hook the version is
// already incremented and the cache entry is already invalidated.
func TestNotify_OrderingBumpInvalidateThenHook(t *testing.T) {
	cache := NewSnapshotCache()
	n := NewNotifier(cache)
	cache.set("ws-1", snap("ws-1", 1)) // pre-seed so we can observe invalidation

	var sawVersion uint64
	var cachePresent bool
	n.RegisterPushHook(func(ws string) {
		sawVersion = n.Version(ws)
		_, cachePresent = cache.Get(ws)
	})

	if err := n.NotifyPolicyChange(context.Background(), "ws-1"); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if sawVersion != 1 {
		t.Fatalf("hook saw version %d, want 1 (bump must precede hook)", sawVersion)
	}
	if cachePresent {
		t.Fatal("hook saw a cached entry; invalidate must precede hook")
	}
}

// TestNotify_EmptyWorkspaceError: empty workspace returns an error and the hook
// must NOT fire.
func TestNotify_EmptyWorkspaceError(t *testing.T) {
	n := NewNotifier(NewSnapshotCache())
	fired := false
	n.RegisterPushHook(func(string) { fired = true })

	if err := n.NotifyPolicyChange(context.Background(), ""); err == nil {
		t.Fatal("want error for empty workspaceID, got nil")
	}
	if fired {
		t.Fatal("hook fired on validation error")
	}
}

// TestNotifier_ConcurrentNotify exercises concurrent NotifyPolicyChange across
// many workspaces with a hook installed, under -race. Asserts the detector is
// clean and the hook fires exactly once per call.
func TestNotifier_ConcurrentNotify(t *testing.T) {
	n := NewNotifier(NewSnapshotCache())
	var count int64
	var mu sync.Mutex
	n.RegisterPushHook(func(string) {
		mu.Lock()
		count++
		mu.Unlock()
	})

	const workers = 8
	const iters = 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ws := "ws-" + string(rune('a'+w))
			for i := 0; i < iters; i++ {
				_ = n.NotifyPolicyChange(context.Background(), ws)
			}
		}(w)
	}
	wg.Wait()

	if count != int64(workers*iters) {
		t.Fatalf("hook fired %d times, want %d", count, workers*iters)
	}
}
