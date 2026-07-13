package connector

import (
	"context"
	"log"
	"testing"
	"time"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
)

// fakeRelayStore implements the relay placement interface used by
// handleConnectorRelayState and handleConnectorHealth.
type fakeRelayStore struct {
	upsertConnectorID string
	upsertRelayID     string
	upsertSource      string
	deleteConnectorID string
	bumpConnectorID   string
	upsertChanged     bool
	deleteChanged     bool
	upsertErr         error
	deleteErr         error
	bumpErr           error
}

func (f *fakeRelayStore) UpsertPlacement(_ context.Context, connectorID, relayID string, _ time.Time, source string) (bool, error) {
	f.upsertConnectorID = connectorID
	f.upsertRelayID = relayID
	f.upsertSource = source
	return f.upsertChanged, f.upsertErr
}

func (f *fakeRelayStore) DeletePlacement(_ context.Context, connectorID string) (bool, error) {
	f.deleteConnectorID = connectorID
	return f.deleteChanged, f.deleteErr
}

func (f *fakeRelayStore) BumpLastConfirmed(_ context.Context, connectorID string) error {
	f.bumpConnectorID = connectorID
	return f.bumpErr
}

// fakeTransportNotifier records calls to NotifyTopologyChange. Relay-placement
// changes drive the transport plane (Track B), not the policy plane.
type fakeTransportNotifier struct {
	lastWorkspaceID  string
	lastConnectorIDs []string
}

func (f *fakeTransportNotifier) NotifyTopologyChange(_ context.Context, workspaceID string, connectorIDs []string) error {
	f.lastWorkspaceID = workspaceID
	f.lastConnectorIDs = connectorIDs
	return nil
}

func TestHandleConnectorRelayState_Connected(t *testing.T) {
	store := &fakeRelayStore{upsertChanged: true}
	notifier := &fakeTransportNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:        store,
		TransportNotifier: notifier,
	}
	client := &connectorStreamClient{
		connectorID: "conn-abc",
		tenantID:    "ws-123",
	}

	handler.handleConnectorRelayState(context.Background(), client, &pb.ConnectorRelayState{
		ConnectorId:    "conn-abc",
		RelayId:        "relay-xyz",
		RelaySpiffeId:  "spiffe://zecurity.in/relay/relay-xyz",
		ObservedAtUnix: 1000000,
		Reason:         "connected",
	})

	if store.upsertConnectorID != "conn-abc" {
		t.Fatalf("expected upsert connector conn-abc, got %q", store.upsertConnectorID)
	}
	if store.upsertRelayID != "relay-xyz" {
		t.Fatalf("expected upsert relay relay-xyz, got %q", store.upsertRelayID)
	}
	if store.upsertSource != "event" {
		t.Fatalf("expected source 'event', got %q", store.upsertSource)
	}
	if notifier.lastWorkspaceID != "ws-123" {
		t.Fatalf("expected topology notification for ws-123, got %q", notifier.lastWorkspaceID)
	}
}

func TestHandleConnectorRelayState_Connected_NoChange(t *testing.T) {
	store := &fakeRelayStore{upsertChanged: false}
	notifier := &fakeTransportNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:        store,
		TransportNotifier: notifier,
	}
	client := &connectorStreamClient{
		connectorID: "conn-abc",
		tenantID:    "ws-123",
	}

	handler.handleConnectorRelayState(context.Background(), client, &pb.ConnectorRelayState{
		ConnectorId: "conn-abc",
		RelayId:     "relay-xyz",
		Reason:      "connected",
	})

	if notifier.lastWorkspaceID != "" {
		t.Fatalf("topology notification triggered on no-op upsert")
	}
}

func TestHandleConnectorRelayState_Connected_EmptyRelayID(t *testing.T) {
	store := &fakeRelayStore{}
	notifier := &fakeTransportNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:        store,
		TransportNotifier: notifier,
	}
	client := &connectorStreamClient{
		connectorID: "conn-abc",
	}

	handler.handleConnectorRelayState(context.Background(), client, &pb.ConnectorRelayState{
		ConnectorId: "conn-abc",
		RelayId:     "",
		Reason:      "connected",
	})

	if store.upsertConnectorID != "" {
		t.Fatal("expected no upsert for empty relay_id with reason=connected")
	}
}

func TestHandleConnectorRelayState_Disconnected(t *testing.T) {
	store := &fakeRelayStore{deleteChanged: true}
	notifier := &fakeTransportNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:        store,
		TransportNotifier: notifier,
	}
	client := &connectorStreamClient{
		connectorID: "conn-abc",
		tenantID:    "ws-123",
	}

	handler.handleConnectorRelayState(context.Background(), client, &pb.ConnectorRelayState{
		ConnectorId: "conn-abc",
		RelayId:     "",
		Reason:      "disconnected",
	})

	if store.deleteConnectorID != "conn-abc" {
		t.Fatalf("expected delete for conn-abc, got %q", store.deleteConnectorID)
	}
	if notifier.lastWorkspaceID != "ws-123" {
		t.Fatalf("expected topology notification for ws-123, got %q", notifier.lastWorkspaceID)
	}
}

func TestHandleConnectorRelayState_Disconnected_NoChange(t *testing.T) {
	store := &fakeRelayStore{deleteChanged: false}
	notifier := &fakeTransportNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:        store,
		TransportNotifier: notifier,
	}
	client := &connectorStreamClient{
		connectorID: "conn-abc",
		tenantID:    "ws-123",
	}

	handler.handleConnectorRelayState(context.Background(), client, &pb.ConnectorRelayState{
		ConnectorId: "conn-abc",
		Reason:      "disconnected",
	})

	if notifier.lastWorkspaceID != "" {
		t.Fatal("topology notification triggered on no-op delete")
	}
}

func TestHandleConnectorRelayState_ConnectorIDMismatch(t *testing.T) {
	store := &fakeRelayStore{}
	notifier := &fakeTransportNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:        store,
		TransportNotifier: notifier,
	}
	client := &connectorStreamClient{
		connectorID: "conn-abc",
	}

	// Body claims a different connector_id — must be ignored.
	handler.handleConnectorRelayState(context.Background(), client, &pb.ConnectorRelayState{
		ConnectorId: "conn-evil",
		RelayId:     "relay-xyz",
		Reason:      "connected",
	})

	if store.upsertConnectorID != "" {
		t.Fatal("expected no upsert on connector_id mismatch")
	}
}

func TestHandleConnectorRelayState_UnknownReason(t *testing.T) {
	store := &fakeRelayStore{}
	notifier := &fakeTransportNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:        store,
		TransportNotifier: notifier,
	}
	client := &connectorStreamClient{
		connectorID: "conn-abc",
	}

	handler.handleConnectorRelayState(context.Background(), client, &pb.ConnectorRelayState{
		ConnectorId: "conn-abc",
		RelayId:     "relay-xyz",
		Reason:      "alien_abduction",
	})

	if store.upsertConnectorID != "" && store.deleteConnectorID != "" {
		t.Fatal("expected no action on unknown reason")
	}
}

func TestHandleConnectorRelayState_NoRelayStore(t *testing.T) {
	handler := &EnrollmentHandler{}
	client := &connectorStreamClient{
		connectorID: "conn-abc",
	}

	// Should not panic when RelayStore is nil.
	handler.handleConnectorRelayState(context.Background(), client, &pb.ConnectorRelayState{
		ConnectorId: "conn-abc",
		RelayId:     "relay-xyz",
		Reason:      "connected",
	})
}

func TestHandleConnectorRelayState_Switched(t *testing.T) {
	store := &fakeRelayStore{upsertChanged: true}
	notifier := &fakeTransportNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:        store,
		TransportNotifier: notifier,
	}
	client := &connectorStreamClient{
		connectorID: "conn-abc",
		tenantID:    "ws-123",
	}

	handler.handleConnectorRelayState(context.Background(), client, &pb.ConnectorRelayState{
		ConnectorId: "conn-abc",
		RelayId:     "relay-new",
		Reason:      "switched",
	})

	if store.upsertRelayID != "relay-new" {
		t.Fatalf("expected upsert for switched relay-new, got %q", store.upsertRelayID)
	}
	if notifier.lastWorkspaceID != "ws-123" {
		t.Fatal("expected topology notification on switch")
	}
}

func TestHandleConnectorRelayState_EmptyConnectorIDInBody(t *testing.T) {
	store := &fakeRelayStore{upsertChanged: true}
	notifier := &fakeTransportNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:        store,
		TransportNotifier: notifier,
	}
	client := &connectorStreamClient{
		connectorID: "conn-abc",
		tenantID:    "ws-123",
	}

	// Empty connector_id in body should be allowed (defense-in-depth for old connectors).
	handler.handleConnectorRelayState(context.Background(), client, &pb.ConnectorRelayState{
		ConnectorId: "",
		RelayId:     "relay-xyz",
		Reason:      "connected",
	})

	if store.upsertConnectorID != "conn-abc" {
		t.Fatalf("expected upsert even with empty connector_id in body")
	}
}

// Silences unused import warning.
var _ = log.Printf
