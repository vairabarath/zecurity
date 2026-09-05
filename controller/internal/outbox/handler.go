package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var ErrNoHandler = errors.New("outbox: no handler registered")

// EventHandler processes a claimed outbox event.
//
// Handlers must return nil when the event has been successfully applied,
// including when the side effect was already applied (idempotent success).
// A non-nil error indicates that processing should be treated as a failure.
type EventHandler interface {
	Handle(ctx context.Context, evt OutboxEvent) error
}

// HandlerRegistry maps event types to their owning handlers.
//
// Registration is normally performed during startup. The mutex also makes
// registration and dispatch safe if they happen concurrently.
type HandlerRegistry struct {
	mu       sync.RWMutex
	handlers map[string]EventHandler
}

// NewHandlerRegistry creates an empty handler registry.
func NewHandlerRegistry() *HandlerRegistry {
	return &HandlerRegistry{
		handlers: make(map[string]EventHandler),
	}
}

// RegisterHandler registers handler for eventType.
//
// Event types must be non-empty and handlers must not be nil.
func (r *HandlerRegistry) RegisterHandler(
	eventType string,
	handler EventHandler,
) error {
	if eventType == "" {
		return errors.New("outbox: event type is empty")
	}

	if handler == nil {
		return fmt.Errorf("outbox: handler for event type %q is nil", eventType)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.handlers[eventType] = handler

	return nil
}

// lookup returns the handler registered for eventType.
func (r *HandlerRegistry) lookup(
	eventType string,
) (EventHandler, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	handler, ok := r.handlers[eventType]
	return handler, ok
}

// Dispatch resolves the event's handler and invokes it.
//
// Unknown event types are returned as ErrNoHandler. The processor is
// responsible for converting this error into a terminal failed event:
//
//	status = failed
//	retry_count = max_retries
//	next_attempt_at = NULL
//	last_error = "no handler registered for event_type X"
func (r *HandlerRegistry) Dispatch(
	ctx context.Context,
	evt OutboxEvent,
) error {
	handler, ok := r.lookup(evt.EventType)
	if !ok {
		return fmt.Errorf(
			"%w: no handler registered for event_type %s",
			ErrNoHandler,
			evt.EventType,
		)
	}

	return handler.Handle(ctx, evt)
}
