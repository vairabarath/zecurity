package relay

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go/valkeycompat"

	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// Sprint 22 Phase D (P4): while a relay drains, every heartbeat persists its
// connection count, so the console can tell when the relay is idle. The
// Valkey flag only speeds persistence up; Postgres says what is draining.

// measuringHeartbeatStore records each persisted heartbeat's measured time.
type measuringHeartbeatStore struct {
	*fakeHeartbeatStore
	measured []time.Time
}

func (s *measuringHeartbeatStore) RecordHeartbeat(ctx context.Context, id, certSerial string, certNotAfter time.Time, version, hostname, observedIP string, observedPort int, addressScope, publicAddr string, connectionCount, maxConnections uint32, measuredAt time.Time) error {
	s.measured = append(s.measured, measuredAt)
	return s.fakeHeartbeatStore.RecordHeartbeat(ctx, id, certSerial, certNotAfter, version, hostname, observedIP, observedPort, addressScope, publicAddr, connectionCount, maxConnections, measuredAt)
}

func newDrainFlagService(t *testing.T, rdb valkeycompat.Cmdable) (*Service, *measuringHeartbeatStore, func()) {
	t.Helper()
	store := &measuringHeartbeatStore{fakeHeartbeatStore: &fakeHeartbeatStore{}}
	svc := NewService(nil, store, time.Hour).WithHeartbeatCache(rdb, 5*time.Minute)
	hbCtx := relayHeartbeatContext(t, appmeta.SPIFFERoleRelay, testRelayID, testRelayID, time.Now().UTC().Add(time.Hour))
	beat := func() {
		t.Helper()
		if _, err := svc.Heartbeat(hbCtx, &relaypb.HeartbeatRequest{Version: "1.0.0", Hostname: "relay-a", ConnectionCount: 3}); err != nil {
			t.Fatalf("Heartbeat: %v", err)
		}
	}
	return svc, store, beat
}

func TestHeartbeat_DrainingFlagForcesDBWrite(t *testing.T) {
	ctx := context.Background()
	rdb := newProvisionTestValkey(t)
	svc, store, beat := newDrainFlagService(t, rdb)

	beat() // first: persisted
	beat() // throttled
	if len(store.measured) != 1 {
		t.Fatalf("writes before drain: %d, want 1 (second beat throttled)", len(store.measured))
	}
	if err := svc.MarkDraining(ctx, testRelayID); err != nil {
		t.Fatal(err)
	}
	ttl, err := rdb.TTL(ctx, relayHeartbeatDrainingPrefix+testRelayID).Result()
	if err != nil || ttl <= 0 || ttl > drainingFlagTTL {
		t.Fatalf("draining flag TTL %v (err %v), want (0, %v]", ttl, err, drainingFlagTTL)
	}
	start := time.Now().UTC()
	beat()
	beat()
	end := time.Now().UTC()
	if len(store.measured) != 3 {
		t.Fatalf("writes while draining: %d, want every beat persisted (3)", len(store.measured))
	}
	for _, m := range store.measured[1:] {
		if m.Before(start.Add(-time.Second)) || m.After(end) {
			t.Fatalf("measured time %v outside the heartbeat call [%v, %v]", m, start, end)
		}
	}
	if err := svc.ClearDraining(ctx, testRelayID); err != nil {
		t.Fatal(err)
	}
	beat()
	if len(store.measured) != 3 {
		t.Fatalf("writes after resume: %d, want the throttle back (3)", len(store.measured))
	}
}

// A Valkey error reading the flag counts as "write due": the count can only
// get fresher, never staler, because of a cache fault.
func TestHeartbeat_DrainingFlagReadErrorForcesWrite(t *testing.T) {
	rdb := newProvisionTestValkey(t)
	_, store, beat := newDrainFlagService(t, failingExists{rdb})
	beat()
	beat()
	if len(store.measured) != 2 {
		t.Fatalf("writes with a failing flag read: %d, want every beat persisted (2)", len(store.measured))
	}
}

type failingExists struct{ valkeycompat.Cmdable }

func (f failingExists) Exists(ctx context.Context, keys ...string) *valkeycompat.IntCmd {
	if len(keys) == 1 && strings.HasPrefix(keys[0], relayHeartbeatDrainingPrefix) {
		cmd := &valkeycompat.IntCmd{}
		cmd.SetErr(errors.New("valkey unavailable"))
		return cmd
	}
	return f.Cmdable.Exists(ctx, keys...)
}

type fakeDrainingLister struct {
	ids []string
	err error
}

func (f fakeDrainingLister) ListDrainingRelayIDs(context.Context) ([]string, error) {
	return f.ids, f.err
}

// The reconcile pass re-sets the flag from Postgres: a flag lost to a failed
// set, a restart or an expiry comes back within one pass, with a fresh TTL.
func TestReconcileDrainFlags(t *testing.T) {
	ctx := context.Background()
	rdb := newProvisionTestValkey(t)
	svc := NewService(nil, &fakeHeartbeatStore{}, time.Hour).WithHeartbeatCache(rdb, 5*time.Minute)
	const a, b, other = "relay-a", "relay-b", "relay-other"

	ReconcileDrainFlags(ctx, fakeDrainingLister{ids: []string{a, b}}, svc)
	for _, id := range []string{a, b} {
		ttl, err := rdb.TTL(ctx, relayHeartbeatDrainingPrefix+id).Result()
		if err != nil || ttl <= 0 {
			t.Fatalf("flag for draining relay %s: ttl %v err %v", id, ttl, err)
		}
	}
	if n, _ := rdb.Exists(ctx, relayHeartbeatDrainingPrefix+other).Result(); n != 0 {
		t.Fatal("reconcile flagged a relay that is not draining")
	}

	// A lister error changes nothing.
	if err := svc.ClearDraining(ctx, a); err != nil {
		t.Fatal(err)
	}
	ReconcileDrainFlags(ctx, fakeDrainingLister{err: errors.New("db down")}, svc)
	if n, _ := rdb.Exists(ctx, relayHeartbeatDrainingPrefix+a).Result(); n != 0 {
		t.Fatal("reconcile with a failing lister changed flags")
	}
}

// Without a cache every heartbeat already writes Postgres; the flag helpers
// are no-ops.
func TestDrainFlags_NoCache(t *testing.T) {
	svc := NewService(nil, &fakeHeartbeatStore{}, time.Hour)
	if err := svc.MarkDraining(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ClearDraining(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
}

type recordingFlags struct {
	marked, cleared []string
	err             error
}

func (f *recordingFlags) MarkDraining(_ context.Context, id string) error {
	f.marked = append(f.marked, id)
	return f.err
}

func (f *recordingFlags) ClearDraining(_ context.Context, id string) error {
	f.cleared = append(f.cleared, id)
	return f.err
}

// captureLog redirects the standard logger for one test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() {
		log.SetOutput(prev)
		log.SetFlags(flags)
	})
	return &buf
}

// DrainFanout: a flag failure doesn't stop the broadcast; every failure is
// logged with the relay id; resume clears the flag.
func TestDrainFanout(t *testing.T) {
	ctx := context.Background()
	logs := captureLog(t)
	flags := &recordingFlags{err: errors.New("valkey down")}
	published := 0
	fanout := DrainFanout(flags, func(context.Context) error { published++; return nil })

	fanout(ctx, "relay-a", true)
	if published != 1 || len(flags.marked) != 1 {
		t.Fatalf("drain fan-out: published %d, marked %v; a flag failure must not stop the broadcast", published, flags.marked)
	}
	if !strings.Contains(logs.String(), "relay relay-a draining=true: draining flag: valkey down") {
		t.Fatalf("flag failure log: %q", logs.String())
	}

	flags.err = nil
	logs.Reset()
	fanout(ctx, "relay-a", false)
	if published != 2 || !sameIDs(flags.cleared, []string{"relay-a"}) || logs.Len() != 0 {
		t.Fatalf("resume fan-out: published %d, cleared %v, log %q", published, flags.cleared, logs.String())
	}

	failing := DrainFanout(flags, func(context.Context) error { return errors.New("build list: db down") })
	failing(ctx, "relay-b", true)
	if !strings.Contains(logs.String(), "relay relay-b draining=true: publish relay list: build list: db down") {
		t.Fatalf("publish failure log: %q", logs.String())
	}
}
