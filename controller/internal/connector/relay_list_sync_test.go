package connector

import (
	"context"
	"testing"
	"time"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
)

// FU-6: a relay list change that a broadcast failed to deliver reaches the
// connector at the next sync; connectors already on the current list get
// nothing (each push costs the connector a probe sweep).

func relayList(version uint64) *pb.LabelledRelayList {
	return &pb.LabelledRelayList{Version: version}
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

func TestSyncRelayList_SendsOnlyToStreamsBehind(t *testing.T) {
	r := NewConnectorRegistry()
	current, missed, fresh := testClient("c1", "ws"), testClient("c2", "ws"), testClient("c3", "ws")
	r.add("c1", current)
	r.add("c2", missed)

	r.BroadcastRelayList(relayList(1))
	drainRelayLists(current)
	drainRelayLists(missed)

	// c2's queue is full when v2 is broadcast: it misses it.
	for len(missed.outbound) < cap(missed.outbound) {
		missed.outbound <- &pb.ConnectorControlMessage{}
	}
	r.BroadcastRelayList(relayList(2))
	if got := drainRelayLists(current); !equalVersions(got, []uint64{2}) {
		t.Fatalf("current stream got %v, want [2]", got)
	}
	drainRelayLists(missed)

	// A stream that never got a list (registered after the broadcast).
	r.add("c3", fresh)

	if n := r.SyncRelayList(relayList(2)); n != 2 {
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
	if n := r.SyncRelayList(relayList(2)); n != 0 {
		t.Fatalf("repeat sync sent to %d streams, want 0", n)
	}
}

// Two broadcasts delivered out of order (the older list last): the stream
// records the list it received last, so the sync corrects it.
func TestSyncRelayList_CorrectsOutOfOrderBroadcast(t *testing.T) {
	r := NewConnectorRegistry()
	c := testClient("c1", "ws")
	r.add("c1", c)
	r.BroadcastRelayList(relayList(7)) // newer
	r.BroadcastRelayList(relayList(6)) // older, sent last
	drainRelayLists(c)

	if n := r.SyncRelayList(relayList(7)); n != 1 {
		t.Fatalf("sync sent to %d streams, want 1", n)
	}
	if got := drainRelayLists(c); !equalVersions(got, []uint64{7}) {
		t.Fatalf("got %v, want [7]", got)
	}
}

// A send that fails (queue full) records nothing: the next sync retries.
func TestSyncRelayList_FailedSendRetriedNextSync(t *testing.T) {
	r := NewConnectorRegistry()
	c := &connectorStreamClient{outbound: make(chan *pb.ConnectorControlMessage, 1), connectorID: "c1"}
	r.add("c1", c)
	c.outbound <- &pb.ConnectorControlMessage{}
	if n := r.SyncRelayList(relayList(3)); n != 0 {
		t.Fatalf("sync into a full queue reported %d sent, want 0", n)
	}
	<-c.outbound
	if n := r.SyncRelayList(relayList(3)); n != 1 {
		t.Fatalf("retry sync sent to %d streams, want 1", n)
	}
	if got := drainRelayLists(c); !equalVersions(got, []uint64{3}) {
		t.Fatalf("got %v, want [3]", got)
	}
}

type stubRelayListSource struct{ version uint64 }

func (s stubRelayListSource) BuildLabelledRelayList(context.Context) (*pb.LabelledRelayList, error) {
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
