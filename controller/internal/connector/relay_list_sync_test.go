package connector

import (
	"context"
	"testing"
	"time"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/relay/relaylist"
	"github.com/yourorg/ztna/controller/internal/relay/relaypool"
)

// FU-6: a relay list change that a broadcast failed to deliver reaches the
// connector at the next sync; connectors already on the current list get
// nothing (each push costs the connector a probe sweep).

func relayList(version uint64) *pb.LabelledRelayList {
	return &pb.LabelledRelayList{Version: version}
}

// emptyListVersion is the version tests give the empty list.
const emptyListVersion = 1000

// oneSet is a set where workspace "ws" uses pool "p", whose list is version.
func oneSet(version uint64) *relaylist.Set {
	return relaylist.NewSet(map[string]*pb.LabelledRelayList{"p": relayList(version)},
		map[string]string{"ws": "p"}, relayList(emptyListVersion))
}

// drainRelayLists empties c's mailbox and returns the relay list versions in it.
func drainRelayLists(c *connectorStreamClient) []uint64 {
	var got []uint64
	for {
		select {
		case m := <-c.outbound:
			if l := m.GetRelayList(); l != nil {
				got = append(got, l.GetVersion())
			}
		default:
			return got
		}
	}
}

func equalVersions(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestBroadcastRelayLists_SendsOnlyToStreamsBehind(t *testing.T) {
	r := NewConnectorRegistry()
	current, missed, fresh := testClient("c1", "ws"), testClient("c2", "ws"), testClient("c3", "ws")
	r.add("c1", current)
	r.add("c2", missed)

	r.BroadcastRelayLists(oneSet(1))
	drainRelayLists(current)
	drainRelayLists(missed)

	// c2's queue is full when v2 is broadcast: it misses it.
	for len(missed.outbound) < cap(missed.outbound) {
		missed.outbound <- &pb.ConnectorControlMessage{}
	}
	r.BroadcastRelayLists(oneSet(2))
	if got := drainRelayLists(current); !equalVersions(got, []uint64{2}) {
		t.Fatalf("current stream got %v, want [2]", got)
	}
	drainRelayLists(missed)

	// A stream that never got a list (registered after the broadcast).
	r.add("c3", fresh)

	if n := r.BroadcastRelayLists(oneSet(2)); n != 2 {
		t.Fatalf("sync sent to %d streams, want 2", n)
	}
	if got := drainRelayLists(current); len(got) != 0 {
		t.Fatalf("stream already on v2 got %v, want nothing", got)
	}
	if got := drainRelayLists(missed); !equalVersions(got, []uint64{2}) {
		t.Fatalf("stream that missed v2 got %v, want [2]", got)
	}
	if got := drainRelayLists(fresh); !equalVersions(got, []uint64{2}) {
		t.Fatalf("stream without a list got %v, want [2]", got)
	}

	// Everyone is current: a repeat sync sends nothing.
	if n := r.BroadcastRelayLists(oneSet(2)); n != 0 {
		t.Fatalf("repeat sync sent to %d streams, want 0", n)
	}
}

// Two broadcasts delivered out of order (the older list last): the stream
// records the list it received last, so the sync corrects it.
func TestBroadcastRelayLists_CorrectsOutOfOrderBroadcast(t *testing.T) {
	r := NewConnectorRegistry()
	c := testClient("c1", "ws")
	r.add("c1", c)
	r.BroadcastRelayLists(oneSet(7)) // newer
	r.BroadcastRelayLists(oneSet(6)) // older, sent last
	drainRelayLists(c)

	if n := r.BroadcastRelayLists(oneSet(7)); n != 1 {
		t.Fatalf("sync sent to %d streams, want 1", n)
	}
	if got := drainRelayLists(c); !equalVersions(got, []uint64{7}) {
		t.Fatalf("got %v, want [7]", got)
	}
}

// A send that fails (queue full) records nothing: the next sync retries.
func TestBroadcastRelayLists_FailedSendRetriedNextSync(t *testing.T) {
	r := NewConnectorRegistry()
	c := &connectorStreamClient{outbound: make(chan *pb.ConnectorControlMessage, 1), connectorID: "c1", tenantID: "ws"}
	r.add("c1", c)
	c.outbound <- &pb.ConnectorControlMessage{}
	if n := r.BroadcastRelayLists(oneSet(3)); n != 0 {
		t.Fatalf("sync into a full queue reported %d sent, want 0", n)
	}
	<-c.outbound
	if n := r.BroadcastRelayLists(oneSet(3)); n != 1 {
		t.Fatalf("retry sync sent to %d streams, want 1", n)
	}
	if got := drainRelayLists(c); !equalVersions(got, []uint64{3}) {
		t.Fatalf("got %v, want [3]", got)
	}
}

type stubRelayListSource struct{ version uint64 }

func (s stubRelayListSource) BuildRelayLists(context.Context) (*relaylist.Set, error) {
	return oneSet(s.version), nil
}

func (s stubRelayListSource) BuildLabelledRelayListForWorkspace(context.Context, string) (*pb.LabelledRelayList, error) {
	return relayList(s.version), nil
}

func TestRunRelayListSync_DeliversMissedList(t *testing.T) {
	r := NewConnectorRegistry()
	c := testClient("c1", "ws")
	r.add("c1", c)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.RunRelayListSync(ctx, stubRelayListSource{version: 9}, 10*time.Millisecond)
		close(done)
	}()

	select {
	case m := <-c.outbound:
		if v := m.GetRelayList().GetVersion(); v != 9 {
			t.Fatalf("got list version %d, want 9", v)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sync loop never delivered the list")
	}
	// Later ticks send nothing more: the stream is current.
	time.Sleep(50 * time.Millisecond)
	if got := drainRelayLists(c); len(got) != 0 {
		t.Fatalf("current stream got %v after the first sync, want nothing", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sync loop did not stop on cancel")
	}
}

// D-38: each connector gets its workspace's pool's list, and a change to one
// pool reaches only the connectors of the workspaces using it.
func TestBroadcastRelayLists_PerPoolIsolation(t *testing.T) {
	r := NewConnectorRegistry()
	a1, a2, b1 := testClient("a1", "ws-a"), testClient("a2", "ws-a"), testClient("b1", "ws-b")
	r.add("a1", a1)
	r.add("a2", a2)
	r.add("b1", b1)
	pools := map[string]string{"ws-a": "pool-a", "ws-b": "pool-b"}
	set := func(va, vb uint64) *relaylist.Set {
		return relaylist.NewSet(map[string]*pb.LabelledRelayList{"pool-a": relayList(va), "pool-b": relayList(vb)},
			pools, relayList(emptyListVersion))
	}

	if n := r.BroadcastRelayLists(set(1, 2)); n != 3 {
		t.Fatalf("first broadcast sent to %d streams, want 3", n)
	}
	for c, want := range map[*connectorStreamClient]uint64{a1: 1, a2: 1, b1: 2} {
		if got := drainRelayLists(c); !equalVersions(got, []uint64{want}) {
			t.Fatalf("connector %s got %v, want [%d]", c.connectorID, got, want)
		}
	}

	// Pool A changes; pool B doesn't: only workspace A's connectors are sent a list.
	if n := r.BroadcastRelayLists(set(3, 2)); n != 2 {
		t.Fatalf("broadcast after a pool-A change sent to %d streams, want 2", n)
	}
	if got := drainRelayLists(b1); len(got) != 0 {
		t.Fatalf("pool-B connector got %v after a pool-A change, want nothing", got)
	}
	for _, c := range []*connectorStreamClient{a1, a2} {
		if got := drainRelayLists(c); !equalVersions(got, []uint64{3}) {
			t.Fatalf("pool-A connector %s got %v, want [3]", c.connectorID, got)
		}
	}
}

// A pool without eligible relays sends the empty list (fail closed, never
// another pool's relays); a workspace the set doesn't know is in the default
// pool.
func TestBroadcastRelayLists_EmptyPoolAndUnknownWorkspace(t *testing.T) {
	r := NewConnectorRegistry()
	empty, unknown := testClient("e1", "ws-empty"), testClient("u1", "ws-new")
	r.add("e1", empty)
	r.add("u1", unknown)
	set := relaylist.NewSet(
		map[string]*pb.LabelledRelayList{relaypool.DefaultPoolID: relayList(5), "pool-other": relayList(6)},
		map[string]string{"ws-empty": "pool-without-relays"}, relayList(emptyListVersion))

	r.BroadcastRelayLists(set)
	if got := drainRelayLists(empty); !equalVersions(got, []uint64{emptyListVersion}) {
		t.Fatalf("connector of an empty pool got %v, want the empty list [%d]", got, emptyListVersion)
	}
	if got := drainRelayLists(unknown); !equalVersions(got, []uint64{5}) {
		t.Fatalf("connector of an unknown workspace got %v, want the default pool's list [5]", got)
	}
}
