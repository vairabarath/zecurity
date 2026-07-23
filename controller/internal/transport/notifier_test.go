package transport

import (
	"context"
	"testing"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	"github.com/yourorg/ztna/controller/internal/policy"
)

func TestNotifier_NotifyTopologyChange_BumpsVersionAndInvalidates(t *testing.T) {
	cache := NewSnapshotCache()
	n := NewNotifier(cache)
	ctx := context.Background()

	if n.Version("w1") != 0 {
		t.Fatalf("initial version should be 0, got %d", n.Version("w1"))
	}
	// Prime the cache so we can prove NotifyTopologyChange invalidates it.
	cache.SetIfEpoch("w1", &clientv1.TransportSnapshot{Version: 1}, cache.Epoch("w1"))

	if err := n.NotifyTopologyChange(ctx, "w1", []string{"c1"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if n.Version("w1") != 1 {
		t.Fatalf("version should bump to 1, got %d", n.Version("w1"))
	}
	if _, ok := cache.Get("w1"); ok {
		t.Fatal("NotifyTopologyChange must invalidate the cached snapshot")
	}
	// Other workspaces are untouched — versions are per-workspace.
	if n.Version("w2") != 0 {
		t.Fatalf("unrelated workspace version must stay 0, got %d", n.Version("w2"))
	}
}

func TestNotifier_EmptyWorkspaceRejected(t *testing.T) {
	n := NewNotifier(NewSnapshotCache())
	if err := n.NotifyTopologyChange(context.Background(), "", []string{"c1"}); err == nil {
		t.Fatal("empty workspaceID must return an error")
	}
}

func TestNotifier_MultipleChangesIncreaseVersionMonotonically(t *testing.T) {
	n := NewNotifier(NewSnapshotCache())
	ctx := context.Background()

	for want := uint64(1); want <= 3; want++ {
		if err := n.NotifyTopologyChange(ctx, "w1", []string{"c1"}); err != nil {
			t.Fatalf("notify change %d: %v", want, err)
		}
		if got := n.Version("w1"); got != want {
			t.Fatalf("version after change %d = %d, want %d", want, got, want)
		}
	}
}

func TestNotifier_TransportAndPolicyPlanesAreIndependent(t *testing.T) {
	ctx := context.Background()
	const workspaceID = "ws-isolation"

	transportCache := NewSnapshotCache()
	transportNotifier := NewNotifier(transportCache)

	policyCache := policy.NewSnapshotCache()
	policyNotifier := policy.NewNotifier(policyCache)

	transportCache.SetIfEpoch(
		workspaceID,
		&clientv1.TransportSnapshot{Version: 1},
		transportCache.Epoch(workspaceID),
	)
	policyCache.SetIfEpoch(
		workspaceID,
		&clientv1.ACLSnapshot{Version: 1},
		policyCache.Epoch(workspaceID),
	)

	// AT-CORE: a topology change affects only the transport plane.
	if err := transportNotifier.NotifyTopologyChange(
		ctx,
		workspaceID,
		[]string{"connector-1"},
	); err != nil {
		t.Fatalf("notify topology change: %v", err)
	}

	if got := transportNotifier.Version(workspaceID); got != 1 {
		t.Fatalf("transport version = %d, want 1", got)
	}
	if got := policyNotifier.Version(workspaceID); got != 0 {
		t.Fatalf("policy version changed after topology event: got %d, want 0", got)
	}
	if _, ok := transportCache.Get(workspaceID); ok {
		t.Fatal("topology event did not invalidate transport cache")
	}
	if _, ok := policyCache.Get(workspaceID); !ok {
		t.Fatal("topology event incorrectly invalidated policy cache")
	}

	// Re-prime transport cache before testing the reverse direction.
	transportCache.SetIfEpoch(
		workspaceID,
		&clientv1.TransportSnapshot{Version: 2},
		transportCache.Epoch(workspaceID),
	)

	// AT-CORE-R: a policy change affects only the ACL plane.
	if err := policyNotifier.NotifyPolicyChange(ctx, workspaceID); err != nil {
		t.Fatalf("notify policy change: %v", err)
	}

	if got := policyNotifier.Version(workspaceID); got != 1 {
		t.Fatalf("policy version = %d, want 1", got)
	}
	if got := transportNotifier.Version(workspaceID); got != 1 {
		t.Fatalf(
			"transport version changed after policy event: got %d, want 1",
			got,
		)
	}
	if _, ok := policyCache.Get(workspaceID); ok {
		t.Fatal("policy event did not invalidate policy cache")
	}
	if _, ok := transportCache.Get(workspaceID); !ok {
		t.Fatal("policy event incorrectly invalidated transport cache")
	}
}
