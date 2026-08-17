package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
)

type testEventHandler struct {
	called bool
	event  OutboxEvent
	err    error
}

func (h *testEventHandler) Handle(
	_ context.Context,
	evt OutboxEvent,
) error {
	h.called = true
	h.event = evt
	return h.err
}

func testOutboxEvent(eventType string) OutboxEvent {
	return OutboxEvent{
		ID:            uuid.New(),
		EventType:     eventType,
		WorkspaceID:   uuid.New(),
		CorrelationID: uuid.New(),
	}
}

func TestHandlerRegistryRegisterAndDispatch(t *testing.T) {
	registry := NewHandlerRegistry()

	handler := &testEventHandler{}
	event := testOutboxEvent("test.event")

	if err := registry.RegisterHandler(event.EventType, handler); err != nil {
		t.Fatalf("register handler: %v", err)
	}

	if err := registry.Dispatch(context.Background(), event); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if !handler.called {
		t.Fatal("handler was not called")
	}

	if handler.event.ID != event.ID {
		t.Fatalf("handler received event ID %s, want %s", handler.event.ID, event.ID)
	}

	if handler.event.EventType != event.EventType {
		t.Fatalf(
			"handler received event type %q, want %q",
			handler.event.EventType,
			event.EventType,
		)
	}
}

func TestHandlerRegistryHandlerError(t *testing.T) {
	registry := NewHandlerRegistry()

	handlerErr := errors.New("handler failed")
	handler := &testEventHandler{
		err: handlerErr,
	}

	event := testOutboxEvent("test.error")

	if err := registry.RegisterHandler(event.EventType, handler); err != nil {
		t.Fatalf("register handler: %v", err)
	}

	err := registry.Dispatch(context.Background(), event)
	if !errors.Is(err, handlerErr) {
		t.Fatalf("dispatch error = %v, want %v", err, handlerErr)
	}

	if !handler.called {
		t.Fatal("handler was not called")
	}
}

func TestHandlerRegistryUnknownEvent(t *testing.T) {
	registry := NewHandlerRegistry()

	event := testOutboxEvent("test.unknown")

	err := registry.Dispatch(context.Background(), event)
	if err == nil {
		t.Fatal("dispatch unknown event returned nil")
	}

	if !errors.Is(err, ErrNoHandler) {
		t.Fatalf("dispatch error = %v, want ErrNoHandler", err)
	}

	want := "no handler registered for event_type test.unknown"
	if err.Error() != "outbox: no handler registered: "+want {
		t.Fatalf(
			"dispatch error = %q, want %q",
			err.Error(),
			"outbox: no handler registered: "+want,
		)
	}
}

func TestHandlerRegistryInvalidRegistration(t *testing.T) {
	registry := NewHandlerRegistry()
	handler := &testEventHandler{}

	t.Run("empty event type", func(t *testing.T) {
		if err := registry.RegisterHandler("", handler); err == nil {
			t.Fatal("register empty event type returned nil")
		}
	})

	t.Run("nil handler", func(t *testing.T) {
		if err := registry.RegisterHandler("test.event", nil); err == nil {
			t.Fatal("register nil handler returned nil")
		}
	})
}

func TestHandlerRegistryDuplicateRegistration(t *testing.T) {
	registry := NewHandlerRegistry()

	first := &testEventHandler{}
	second := &testEventHandler{}

	if err := registry.RegisterHandler("test.event", first); err != nil {
		t.Fatalf("register first handler: %v", err)
	}

	if err := registry.RegisterHandler("test.event", second); err != nil {
		t.Fatalf("register second handler: %v", err)
	}

	event := testOutboxEvent("test.event")

	if err := registry.Dispatch(context.Background(), event); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if first.called {
		t.Fatal("first handler was called after replacement")
	}

	if !second.called {
		t.Fatal("second handler was not called")
	}
}

func TestHandlerRegistryConcurrentAccess(t *testing.T) {
	registry := NewHandlerRegistry()

	const workers = 20

	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()

			eventType := fmt.Sprintf("test.event.%d", i)
			handler := &testEventHandler{}

			if err := registry.RegisterHandler(eventType, handler); err != nil {
				t.Errorf("register handler: %v", err)
				return
			}

			event := testOutboxEvent(eventType)

			if err := registry.Dispatch(context.Background(), event); err != nil {
				t.Errorf("dispatch handler: %v", err)
				return
			}
		}(i)
	}

	wg.Wait()
}

type idempotentTestHandler struct{}

func (idempotentTestHandler) Handle(
	ctx context.Context,
	evt OutboxEvent,
) error {
	// Simulate an already-applied side effect.
	// Idempotent handlers treat redelivery as success.
	return nil
}

func TestHandlerIdempotency(t *testing.T) {
	registry := NewHandlerRegistry()

	if err := registry.RegisterHandler(
		"test.idempotent",
		idempotentTestHandler{},
	); err != nil {
		t.Fatalf("register handler: %v", err)
	}

	evt := OutboxEvent{
		ID:            uuid.New(),
		EventType:     "test.idempotent",
		CorrelationID: uuid.New(),
	}

	if err := registry.Dispatch(context.Background(), evt); err != nil {
		t.Fatalf("idempotent redelivery returned error: %v", err)
	}
}
