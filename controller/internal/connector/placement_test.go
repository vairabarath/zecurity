package connector

import (
	"context"
	"errors"
	"testing"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
)

// D-40: a residency tenant's connector reporting a relay outside its
// authorized pools is not recorded; an existing placement is removed (so the
// transport snapshot stops publishing the relay path); the refusal is
// recorded once per connector and relay, not per heartbeat.

type rejectSpy struct{ calls []string }

func (r *rejectSpy) hook(_ context.Context, workspaceID, connectorID, relayID string) {
	r.calls = append(r.calls, workspaceID+"/"+connectorID+"/"+relayID)
}

func relayEvent(relayID string) *pb.ConnectorRelayState {
	return &pb.ConnectorRelayState{ConnectorId: "conn-1", RelayId: relayID, ObservedAtUnix: 1, Reason: "connected"}
}

func TestPlacement_RefusedIsNotRecordedAndRemovesExisting(t *testing.T) {
	store := &fakeRelayStore{refuseRelays: map[string]bool{"relay-out": true}, deleteChanged: true}
	notifier := &fakeTransportNotifier{}
	spy := &rejectSpy{}
	h := &EnrollmentHandler{RelayStore: store, TransportNotifier: notifier, OnPlacementRejected: spy.hook}
	client := &connectorStreamClient{connectorID: "conn-1", tenantID: "ws-res"}

	h.handleConnectorRelayState(context.Background(), client, relayEvent("relay-out"))

	if store.upserts != 0 {
		t.Fatalf("refused placement was upserted %d time(s)", store.upserts)
	}
	if store.deleteConnectorID != "conn-1" {
		t.Fatalf("existing placement not removed (delete connector %q)", store.deleteConnectorID)
	}
	if notifier.lastWorkspaceID != "ws-res" || len(notifier.lastConnectorIDs) != 1 || notifier.lastConnectorIDs[0] != "conn-1" {
		t.Fatalf("topology not notified for the removed placement: ws=%q connectors=%v", notifier.lastWorkspaceID, notifier.lastConnectorIDs)
	}
	if len(spy.calls) != 1 || spy.calls[0] != "ws-res/conn-1/relay-out" {
		t.Fatalf("rejection hook calls %v, want one for ws-res/conn-1/relay-out", spy.calls)
	}
}

func TestPlacement_RefusalRecordedOncePerTransition(t *testing.T) {
	store := &fakeRelayStore{refuseRelays: map[string]bool{"relay-out": true}}
	spy := &rejectSpy{}
	h := &EnrollmentHandler{RelayStore: store, OnPlacementRejected: spy.hook}
	client := &connectorStreamClient{connectorID: "conn-1", tenantID: "ws-res"}

	for i := 0; i < 3; i++ { // repeated by heartbeats and events
		h.handleConnectorRelayState(context.Background(), client, relayEvent("relay-out"))
	}
	if len(spy.calls) != 1 {
		t.Fatalf("repeated refusal recorded %d times, want 1", len(spy.calls))
	}
	// An allowed placement in between resets it: the next refusal is new.
	h.handleConnectorRelayState(context.Background(), client, relayEvent("relay-in"))
	h.handleConnectorRelayState(context.Background(), client, relayEvent("relay-out"))
	if len(spy.calls) != 2 {
		t.Fatalf("refusal after an allowed placement recorded %d times in total, want 2", len(spy.calls))
	}
	if store.upserts != 1 || store.upsertRelayID != "relay-in" {
		t.Fatalf("upserts=%d last=%q, want exactly the allowed relay-in", store.upserts, store.upsertRelayID)
	}
}

func TestPlacement_AllowedIsRecorded(t *testing.T) {
	store := &fakeRelayStore{upsertChanged: true}
	notifier := &fakeTransportNotifier{}
	spy := &rejectSpy{}
	h := &EnrollmentHandler{RelayStore: store, TransportNotifier: notifier, OnPlacementRejected: spy.hook}
	client := &connectorStreamClient{connectorID: "conn-1", tenantID: "ws"}

	h.handleConnectorRelayState(context.Background(), client, relayEvent("relay-in"))
	if store.upserts != 1 || store.upsertSource != "event" || notifier.lastWorkspaceID != "ws" {
		t.Fatalf("allowed placement: upserts=%d source=%q notified=%q", store.upserts, store.upsertSource, notifier.lastWorkspaceID)
	}
	if len(spy.calls) != 0 {
		t.Fatalf("allowed placement reported as refused: %v", spy.calls)
	}
}

// A failed check is fail-closed: nothing is recorded or removed.
func TestPlacement_CheckErrorRecordsNothing(t *testing.T) {
	store := &fakeRelayStore{checkErr: errors.New("db down")}
	h := &EnrollmentHandler{RelayStore: store}
	client := &connectorStreamClient{connectorID: "conn-1", tenantID: "ws"}

	h.handleConnectorRelayState(context.Background(), client, relayEvent("relay-in"))
	if store.upserts != 0 || store.deleteConnectorID != "" {
		t.Fatalf("check error: upserts=%d delete=%q, want nothing recorded", store.upserts, store.deleteConnectorID)
	}
}
