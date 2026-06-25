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
	deleteConnectorID string
	bumpConnectorID   string
	upsertChanged     bool
	deleteChanged     bool
	upsertErr         error
	deleteErr         error
	bumpErr           error
}

func (f *fakeRelayStore) UpsertPlacement(_ context.Context, connectorID, relayID string, _ time.Time) (bool, error) {
	f.upsertConnectorID = connectorID
	f.upsertRelayID = relayID
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

// fakePolicyNotifier records calls to NotifyPolicyChange.
type fakePolicyNotifier struct {
	lastWorkspaceID string
}

func (f *fakePolicyNotifier) NotifyPolicyChange(_ context.Context, workspaceID string) error {
	f.lastWorkspaceID = workspaceID
	return nil
}

func TestHandleConnectorRelayState_Connected(t *testing.T) {
	store := &fakeRelayStore{upsertChanged: true}
	notifier := &fakePolicyNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:     store,
		PolicyNotifier: notifier,
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
	if notifier.lastWorkspaceID != "ws-123" {
		t.Fatalf("expected policy notification for ws-123, got %q", notifier.lastWorkspaceID)
	}
}

func TestHandleConnectorRelayState_Connected_NoChange(t *testing.T) {
	store := &fakeRelayStore{upsertChanged: false}
	notifier := &fakePolicyNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:     store,
		PolicyNotifier: notifier,
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
		t.Fatalf("policy notification triggered on no-op upsert")
	}
}

func TestHandleConnectorRelayState_Connected_EmptyRelayID(t *testing.T) {
	store := &fakeRelayStore{}
	notifier := &fakePolicyNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:     store,
		PolicyNotifier: notifier,
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
	notifier := &fakePolicyNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:     store,
		PolicyNotifier: notifier,
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
		t.Fatalf("expected policy notification for ws-123, got %q", notifier.lastWorkspaceID)
	}
}

func TestHandleConnectorRelayState_Disconnected_NoChange(t *testing.T) {
	store := &fakeRelayStore{deleteChanged: false}
	notifier := &fakePolicyNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:     store,
		PolicyNotifier: notifier,
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
		t.Fatal("policy notification triggered on no-op delete")
	}
}

func TestHandleConnectorRelayState_ConnectorIDMismatch(t *testing.T) {
	store := &fakeRelayStore{}
	notifier := &fakePolicyNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:     store,
		PolicyNotifier: notifier,
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
	notifier := &fakePolicyNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:     store,
		PolicyNotifier: notifier,
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
	notifier := &fakePolicyNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:     store,
		PolicyNotifier: notifier,
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
		t.Fatal("expected policy notification on switch")
	}
}

func TestHandleConnectorRelayState_EmptyConnectorIDInBody(t *testing.T) {
	store := &fakeRelayStore{upsertChanged: true}
	notifier := &fakePolicyNotifier{}
	handler := &EnrollmentHandler{
		RelayStore:     store,
		PolicyNotifier: notifier,
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
