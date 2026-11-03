package policy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/yourorg/ztna/controller/internal/outbox"
)

// EventPolicyInvalidationRequested asks the controller to invalidate a
// workspace's compiled policy (certificate audit bugs 4/5). It is enqueued in
// the same transaction as a lifecycle change that affects who may connect —
// an identity-connection disable or delete — so the invalidation can never be
// lost after the change commits.
const EventPolicyInvalidationRequested = "policy.invalidation.requested"

type invalidationPayload struct {
	Reason string `json:"reason"`
}

// NewInvalidationEvent builds the outbox event for workspaceID; reason is
// recorded for diagnostics only.
func NewInvalidationEvent(workspaceID uuid.UUID, reason string) outbox.Event {
	payload, _ := json.Marshal(invalidationPayload{Reason: reason})
	return outbox.Event{
		EventType:     EventPolicyInvalidationRequested,
		WorkspaceID:   workspaceID,
		CorrelationID: uuid.New(),
		Payload:       payload,
	}
}

// changeNotifier is the subset of *Notifier the handler needs.
type changeNotifier interface {
	NotifyPolicyChange(ctx context.Context, workspaceID string) error
}

// InvalidationHandler consumes EventPolicyInvalidationRequested: it
// invalidates the event's workspace policy. Invalidating twice is harmless, so
// duplicate delivery is safe; a failure is returned so the outbox retries.
type InvalidationHandler struct {
	notifier changeNotifier
}

// NewInvalidationHandler builds the handler.
func NewInvalidationHandler(notifier changeNotifier) *InvalidationHandler {
	return &InvalidationHandler{notifier: notifier}
}

// Handle implements outbox.EventHandler.
func (h *InvalidationHandler) Handle(ctx context.Context, evt outbox.OutboxEvent) error {
	if err := h.notifier.NotifyPolicyChange(ctx, evt.WorkspaceID.String()); err != nil {
		return fmt.Errorf("%s: %w", EventPolicyInvalidationRequested, err)
	}
	return nil
}
