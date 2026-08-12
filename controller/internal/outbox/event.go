package outbox

import (
	"encoding/json"

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
