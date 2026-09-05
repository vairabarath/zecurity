package outbox

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Event is a provider-independent durable outbox event.
//
// Payload is opaque to the outbox. The originating service is responsible
// for constructing the event payload and correlation ID.
type Event struct {
	EventType     string
	WorkspaceID   uuid.UUID
	UserID        *uuid.UUID
	CorrelationID uuid.UUID
	Payload       json.RawMessage
}

// OutboxEvent represents a persisted outbox event returned by ClaimEvents.
type OutboxEvent struct {
	ID            uuid.UUID
	EventType     string
	WorkspaceID   uuid.UUID
	UserID        *uuid.UUID
	CorrelationID uuid.UUID
	Payload       json.RawMessage
	Status        string
	RetryCount    int
	CreatedAt     time.Time
	UpdatedAt     time.Time
	NextAttemptAt *time.Time
	LeaseID       *uuid.UUID
	ClaimedAt     *time.Time
	LastError     *string
}
