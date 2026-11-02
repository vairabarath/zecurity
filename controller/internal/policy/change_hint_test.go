package policy

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/yourorg/ztna/controller/internal/changehint"
	"github.com/yourorg/ztna/controller/internal/posture"
)

// FU-8: a change listener runs after the connector push hook, sees the new
// version (so a client woken by it fetches that version), and never replaces
// the hook.
func TestNotifier_ChangeListenerRunsAfterPushHookWithNewVersion(t *testing.T) {
	n := NewNotifier(NewSnapshotCache())
	var order []string
	var seen uint64
	n.RegisterPushHook(func(ws string) { order = append(order, "connector-push") })
	n.AddChangeListener(func(ws string) {
		order = append(order, "client-hint")
		seen = n.Version(ws)
	})

	if err := n.NotifyPolicyChange(context.Background(), "ws-1"); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != "connector-push" || order[1] != "client-hint" {
		t.Fatalf("order = %v, want connector push then client hint", order)
	}
	if seen != 1 || n.Version("ws-1") != 1 {
		t.Fatalf("listener saw version %d (notifier %d), want 1", seen, n.Version("ws-1"))
	}
}

// FU-8: client hints can never hold up the connector push. A hub whose only
// stream never reads takes every notification without blocking, and the
// connector hook still runs every time.
func TestNotifier_StuckHintStreamDoesNotBlockConnectorPush(t *testing.T) {
	n := NewNotifier(NewSnapshotCache())
	hub := changehint.New()
	pushes := 0
	n.RegisterPushHook(func(string) { pushes++ })
	n.AddChangeListener(hub.Notify)
	stuck := hub.Subscribe("ws-1", "device-never-reads")
	defer stuck.Close()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			_ = n.NotifyPolicyChange(context.Background(), "ws-1")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("NotifyPolicyChange blocked on a stream that never reads")
	}
	if pushes != 1000 {
		t.Fatalf("connector push ran %d times, want 1000", pushes)
	}
	if got := len(stuck.C); got != 1 {
		t.Fatalf("pending wakes = %d, want 1 (coalesced)", got)
	}
}

// FU-8 / ADR-030: approving a device whose access depends on an approval
// profile bumps the ACL version through EvaluateDevice, wakes the device's
// hint stream, and the authoritative compile then authorizes the device. The
// version the stream would send equals the snapshot version the client gets.
func TestDeviceApproval_WakesHintStreamAndAuthorizesThroughSnapshot(t *testing.T) {
	pool, ctx := lifecycleTestPool(t)
	f := newLifecycleFixture(t, pool, ctx, "approvalhint")
	rnID, _ := mustInsertRNWithConnector(t, ctx, pool, f.wsID, "rn-approval", "td-approval", "10.41.0.1")
	rID := mustInsertResource(t, ctx, pool, f.wsID, rnID, "res-approval", "10.41.0.10", 443)
	mustAssignResourceToGroup(t, ctx, pool, f.wsID, rID, f.grpID)
	userID := mustInsertUser(t, ctx, pool, f.wsID)
	mustAddGroupMember(t, ctx, pool, f.grpID, userID)
	spiffe := "spiffe://td/client/" + userID
	deviceID := mustInsertClientDevice(t, ctx, pool, f.wsID, userID, spiffe)

	ws, dev, res := uuid.MustParse(f.wsID), uuid.MustParse(deviceID), uuid.MustParse(rID)
	pstore := posture.NewStore(pool)
	profile, err := pstore.CreateProfile(ctx, ws, "Company devices", false)
	if err != nil {
		t.Fatalf("create profile: %v", err)
	}
	if err := pstore.AddRequirement(ctx, ws, profile.ID, posture.Requirement{CheckID: posture.CheckDeviceApproved}); err != nil {
		t.Fatalf("add requirement: %v", err)
	}
	if _, err := pstore.UpdateProfileMode(ctx, ws, profile.ID, posture.ModeEnforce); err != nil {
		t.Fatalf("enforce profile: %v", err)
	}
	if _, err := pstore.CreateResourceBinding(ctx, ws, res, profile.ID); err != nil {
		t.Fatalf("bind profile: %v", err)
	}
	if err := pstore.InsertReport(ctx, ws, dev, posture.Report{
		ReportID:      uuid.NewString(),
		ClientVersion: "test-client",
		OSInfo:        json.RawMessage(`{"name":"linux","version":"test"}`),
		ReportedAt:    time.Now().UTC(),
		Observations:  []posture.Observation{{CheckID: posture.CheckFirewall, Status: posture.ObservationStatusPass}},
	}); err != nil {
		t.Fatalf("insert report: %v", err)
	}

	cache := NewSnapshotCache()
	notifier := NewNotifier(cache)
	hub := changehint.New()
	notifier.AddChangeListener(hub.Notify)
	evaluator := posture.NewEvaluator(pstore, notifier)
	store := NewStore(pool)
	// What GetACLSnapshot serves: the cached compile.
	authorized := func() (bool, uint64) {
		t.Helper()
		snap, err := cache.GetOrCompile(f.wsID, func() (*CompiledACL, error) {
			return CompileACLSnapshot(ctx, store, pstore, notifier, f.wsID)
		})
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		for _, e := range snap.Entries {
			if contains(e.AllowedSpiffeIds, spiffe) {
				return true, snap.Version
			}
		}
		return false, snap.Version
	}

	if _, err := evaluator.EvaluateDevice(ctx, ws, dev); err != nil {
		t.Fatalf("first evaluation: %v", err)
	}
	if ok, _ := authorized(); ok {
		t.Fatal("an unapproved device is authorized by an approval-gated resource")
	}

	sub := hub.Subscribe(f.wsID, deviceID)
	defer sub.Close()
	before := notifier.Version(f.wsID)
	mustExec(t, ctx, pool, `UPDATE client_devices SET approved_at = NOW() WHERE id = $1`, deviceID)
	if _, err := evaluator.EvaluateDevice(ctx, ws, dev); err != nil {
		t.Fatalf("evaluation after approval: %v", err)
	}

	select {
	case <-sub.C:
	default:
		t.Fatal("approval did not wake the device's hint stream")
	}
	hinted := notifier.Version(f.wsID) // what WatchChanges sends as acl_version
	if hinted <= before {
		t.Fatalf("approval did not bump the ACL version (%d -> %d)", before, hinted)
	}
	ok, version := authorized()
	if !ok {
		t.Fatal("the approved device is not authorized by the snapshot")
	}
	if version != hinted {
		t.Fatalf("snapshot version %d != hinted version %d: the client would sync forever", version, hinted)
	}

	// Re-evaluating with nothing changed must not wake the stream again.
	if _, err := evaluator.EvaluateDevice(ctx, ws, dev); err != nil {
		t.Fatalf("re-evaluation: %v", err)
	}
	select {
	case <-sub.C:
		t.Fatal("an evaluation without a transition woke the stream")
	default:
	}
}
