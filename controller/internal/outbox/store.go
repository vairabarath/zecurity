package outbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNilTx = errors.New("outbox enqueue: transaction is nil")

// Outbox persists durable events using caller-owned transactions.
type Outbox struct {
	pool *pgxpool.Pool
}

// NewOutbox creates an outbox store.
func NewOutbox(pool *pgxpool.Pool) *Outbox {
	return &Outbox{pool: pool}
}

// Enqueue inserts an event into the caller's transaction.
//
// The transaction is owned by the caller. Enqueue never starts, commits,
// rolls back, or replaces the supplied transaction with the pool.
func (o *Outbox) Enqueue(
	ctx context.Context,
	tx pgx.Tx,
	evt Event,
) error {
	if tx == nil {
		return ErrNilTx
	}

	_, err := tx.Exec(
		ctx,
		`INSERT INTO outbox_events (
			event_type,
			workspace_id,
			user_id,
			correlation_id,
			payload,
			status,
			retry_count,
			next_attempt_at
		)
		VALUES ($1, $2, $3, $4, $5, 'pending', 0, NOW())`,
		evt.EventType,
		evt.WorkspaceID,
		evt.UserID,
		evt.CorrelationID,
		evt.Payload,
	)
	if err != nil {
		return fmt.Errorf("enqueue outbox event: %w", err)
	}

	return nil
}
