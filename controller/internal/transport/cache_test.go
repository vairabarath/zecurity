package transport

import (
	"testing"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
)

// SnapshotCache mirrors policy.SnapshotCache (epoch-CAS, version guard). These
// are pure unit tests — no DB.
func TestSnapshotCache_GetOrCompile_CachesAndReuses(t *testing.T) {
	c := NewSnapshotCache()
	calls := 0
	compile := func() (*clientv1.TransportSnapshot, error) {
		calls++
		return &clientv1.TransportSnapshot{Version: 1}, nil
	}

	first, err := c.GetOrCompile("w1", compile)
	if err != nil || first.Version != 1 {
		t.Fatalf("first compile: snap=%v err=%v", first, err)
	}
	second, err := c.GetOrCompile("w1", compile)
	if err != nil || second.Version != 1 {
		t.Fatalf("second get: snap=%v err=%v", second, err)
	}
	if calls != 1 {
		t.Fatalf("compileFn should run once (cache hit on second), ran %d times", calls)
	}
}

func TestSnapshotCache_InvalidateForcesRecompile(t *testing.T) {
	c := NewSnapshotCache()
	if _, err := c.GetOrCompile("w1", func() (*clientv1.TransportSnapshot, error) {
		return &clientv1.TransportSnapshot{Version: 1}, nil
	}); err != nil {
		t.Fatal(err)
	}
	c.Invalidate("w1")
	if _, ok := c.Get("w1"); ok {
		t.Fatal("Invalidate must drop the cached entry")
	}
	snap, err := c.GetOrCompile("w1", func() (*clientv1.TransportSnapshot, error) {
		return &clientv1.TransportSnapshot{Version: 2}, nil
	})
	if err != nil || snap.Version != 2 {
		t.Fatalf("recompile after invalidate: snap=%v err=%v", snap, err)
	}
}

func TestSnapshotCache_SetIfEpoch_DropsRacedCompile(t *testing.T) {
	c := NewSnapshotCache()
	observed := c.Epoch("w1")
	c.Invalidate("w1") // an invalidation races the in-flight compile
	stored := c.SetIfEpoch("w1", &clientv1.TransportSnapshot{Version: 1}, observed)
	if stored {
		t.Fatal("SetIfEpoch must reject a snapshot whose epoch was superseded")
	}
	if _, ok := c.Get("w1"); ok {
		t.Fatal("raced snapshot must not be cached")
	}
}

func TestSnapshotCache_VersionGuard_NoRegression(t *testing.T) {
	c := NewSnapshotCache()
	e := c.Epoch("w1")
	c.SetIfEpoch("w1", &clientv1.TransportSnapshot{Version: 5}, e)
	// A lower-version snapshot at the same epoch must not overwrite.
	c.SetIfEpoch("w1", &clientv1.TransportSnapshot{Version: 3}, c.Epoch("w1"))
	got, ok := c.Get("w1")
	if !ok || got.Version != 5 {
		t.Fatalf("version guard failed: want cached version 5, got %v (ok=%v)", got, ok)
	}
}
