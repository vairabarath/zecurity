package transport

import (
	"context"
	"testing"

	clientv1 "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
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

func TestNotifier_PushHook_ReceivesAffectedConnectors(t *testing.T) {
	n := NewNotifier(NewSnapshotCache())
	var gotWS string
	var gotConns []string
	n.RegisterPushHook(func(ws string, connIDs []string) {
		gotWS = ws
		gotConns = connIDs
	})

	if err := n.NotifyTopologyChange(context.Background(), "w1", []string{"c1", "c2"}); err != nil {
		t.Fatalf("notify: %v", err)
	}
	if gotWS != "w1" {
		t.Fatalf("hook workspace: want w1 got %q", gotWS)
	}
	if len(gotConns) != 2 || gotConns[0] != "c1" || gotConns[1] != "c2" {
		t.Fatalf("hook must receive the affected connector IDs, got %v", gotConns)
	}
}

func TestNotifier_EmptyWorkspaceRejected(t *testing.T) {
	n := NewNotifier(NewSnapshotCache())
	if err := n.NotifyTopologyChange(context.Background(), "", []string{"c1"}); err == nil {
		t.Fatal("empty workspaceID must return an error")
	}
}
