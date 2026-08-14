package outbox

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNilTx      = errors.New("outbox enqueue: transaction is nil")
	ErrStaleLease = errors.New("outbox: stale or lost lease")
)

// DefaultMaxRetries is the default retry limit defined by PENDING-15.
const DefaultMaxRetries = 100

// Outbox persists durable events using caller-owned transactions.
type Outbox struct {
	pool        *pgxpool.Pool
	retryPolicy RetryPolicy
	jitter      JitterSource
	clock       func() time.Time
}

func (o *Outbox) MarkFailedTerminal(
	ctx context.Context,
	eventID uuid.UUID,
	leaseID uuid.UUID,
	handlerErr error,
) error {
	lastError := truncateError(handlerErr)

	tag, err := o.pool.Exec(
		ctx,
		`UPDATE outbox_events
		    SET status          = 'failed',
		        retry_count     = $3,
		        next_attempt_at = NULL,
		        lease_id        = NULL,
		        claimed_at      = NULL,
		        last_error      = $4,
		        updated_at      = NOW()
		  WHERE id = $1
		    AND lease_id = $2
		    AND status = 'processing'`,
		eventID,
		leaseID,
		o.retryPolicy.MaxRetries,
		lastError,
	)
	if err != nil {
		return fmt.Errorf("mark terminal outbox event failed: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return ErrStaleLease
	}

	return nil
}

// NewOutbox creates an outbox store.
func NewOutbox(pool *pgxpool.Pool) *Outbox {
	policy := DefaultRetryPolicy()
	return &Outbox{
		pool:        pool,
		retryPolicy: policy,
		jitter:      NoJitter{},
		clock:       time.Now,
	}
}

// NewOutboxWithMaxRetries creates an outbox store with an explicit retry limit.
//
// Configuration validation belongs to the processor/configuration phase.
// This constructor rejects invalid values so the store itself cannot operate
// with a nonsensical retry limit.
func NewOutboxWithMaxRetries(pool *pgxpool.Pool, maxRetries int) (*Outbox, error) {
	if maxRetries < 1 || maxRetries > 1000 {
		return nil, fmt.Errorf("outbox max retries must be between 1 and 1000: %d", maxRetries)
	}
	policy := DefaultRetryPolicy()
	policy.MaxRetries = maxRetries

	return &Outbox{
		pool:        pool,
		retryPolicy: policy,
		jitter:      NoJitter{},
		clock:       time.Now,
	}, nil
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

// ClaimEvents atomically transitions eligible events to processing and returns
// the rows claimed by this worker.
//
// FOR UPDATE SKIP LOCKED is deliberately kept inside the same SQL statement
// as the UPDATE so concurrent workers cannot claim the same eligible event.
func (o *Outbox) ClaimEvents(
	ctx context.Context,
	limit int,
) ([]OutboxEvent, error) {
	if limit <= 0 {
		return []OutboxEvent{}, nil
	}

	rows, err := o.pool.Query(
		ctx,
		`WITH candidates AS (
			SELECT id
			  FROM outbox_events
			 WHERE (status = 'pending'
			        OR (status = 'failed' AND retry_count < $2))
			   AND next_attempt_at <= NOW()
			 ORDER BY next_attempt_at, created_at
			 LIMIT $1
			 FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox_events o
		   SET status       = 'processing',
		       lease_id     = gen_random_uuid(),
		       claimed_at   = NOW(),
		       updated_at   = NOW()
		  FROM candidates c
		 WHERE o.id = c.id
		RETURNING
			o.id,
			o.event_type,
			o.workspace_id,
			o.user_id,
			o.correlation_id,
			o.payload,
			o.status,
			o.retry_count,
			o.created_at,
			o.updated_at,
			o.next_attempt_at,
			o.lease_id,
			o.claimed_at,
			o.last_error`,
		limit,
		o.retryPolicy.MaxRetries,
	)
	if err != nil {
		return nil, fmt.Errorf("claim outbox events: %w", err)
	}
	defer rows.Close()

	events := make([]OutboxEvent, 0)

	for rows.Next() {
		var evt OutboxEvent

		if err := rows.Scan(
			&evt.ID,
			&evt.EventType,
			&evt.WorkspaceID,
			&evt.UserID,
			&evt.CorrelationID,
			&evt.Payload,
			&evt.Status,
			&evt.RetryCount,
			&evt.CreatedAt,
			&evt.UpdatedAt,
			&evt.NextAttemptAt,
			&evt.LeaseID,
			&evt.ClaimedAt,
			&evt.LastError,
		); err != nil {
			return nil, fmt.Errorf("scan claimed outbox event: %w", err)
		}

		events = append(events, evt)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate claimed outbox events: %w", err)
	}

	if len(events) > 0 {
		eventIDs := make([]string, 0, len(events))

		for _, evt := range events {
			eventIDs = append(
				eventIDs,
				fmt.Sprintf(
					"%s(event_type=%s correlation_id=%s)",
					evt.ID,
					evt.EventType,
					evt.CorrelationID,
				),
			)
		}

		log.Printf(
			"outbox: claimed %d event(s): %v",
			len(events),
			eventIDs,
		)
	}

	return events, nil
}

// MarkDone marks an event successfully processed.
//
// The event can only be completed by the worker holding its current lease.
// A zero-row update means the lease was lost, replaced, or the event was
// already transitioned by another worker.
func (o *Outbox) MarkDone(
	ctx context.Context,
	eventID uuid.UUID,
	leaseID uuid.UUID,
) error {
	tag, err := o.pool.Exec(
		ctx,
		`UPDATE outbox_events
		    SET status     = 'done',
		        lease_id   = NULL,
		        claimed_at = NULL,
		        updated_at = NOW()
		  WHERE id = $1
		    AND lease_id = $2
		    AND status = 'processing'`,
		eventID,
		leaseID,
	)
	if err != nil {
		return fmt.Errorf("mark outbox event done: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return ErrStaleLease
	}

	return nil
}

// MarkFailed marks an event as failed while retaining ownership checks.
//
// Retry scheduling/backoff is intentionally not implemented here yet;
// Phase 4 owns the processing/retry scheduling behavior. For Phase 3,
// the event becomes immediately eligible for the next processing pass.
func (o *Outbox) MarkFailed(
	ctx context.Context,
	eventID uuid.UUID,
	leaseID uuid.UUID,
	handlerErr error,
) error {
	var retryCount int

	err := o.pool.QueryRow(
		ctx,
		`SELECT retry_count
       FROM outbox_events
      WHERE id = $1
        AND lease_id = $2
        AND status = 'processing'`,
		eventID,
		leaseID,
	).Scan(&retryCount)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrStaleLease
		}

		return fmt.Errorf("get outbox retry count: %w", err)
	}

	retryCount++
	nextAttemptAt := o.nextAttemptAt(retryCount)
	lastError := truncateError(handlerErr)

	tag, err := o.pool.Exec(
		ctx,
		`UPDATE outbox_events
		    SET status           = 'failed',
		        retry_count      = retry_count + 1,
		        next_attempt_at  = $3,
		        lease_id         = NULL,
		        claimed_at       = NULL,
		        last_error       = $4,
		        updated_at       = NOW()
		  WHERE id = $1
		    AND lease_id = $2
		    AND status = 'processing'`,
		eventID,
		leaseID,
		nextAttemptAt,
		lastError,
	)
	if err != nil {
		return fmt.Errorf("mark outbox event failed: %w", err)
	}

	if tag.RowsAffected() == 0 {
		return ErrStaleLease
	}

	return nil
}

// truncateError bounds persisted errors to the PENDING-15 limit.
func truncateError(err error) *string {
	if err == nil {
		return nil
	}

	const maxErrorBytes = 4096

	value := err.Error()
	data := []byte(value)

	if len(data) > maxErrorBytes {
		data = data[:maxErrorBytes]
		value = string(data)
	}

	return &value
}

// Keep json.RawMessage visible to this package's public event contract.

func (o *Outbox) nextAttemptAt(retryCount int) *time.Time {
	if retryCount >= o.retryPolicy.MaxRetries {
		return nil
	}

	delay := o.retryPolicy.Backoff(retryCount)
	delay = o.jitter.Apply(delay, o.retryPolicy.Jitter)

	next := o.clock().Add(delay)
	return &next
}

// ReapAbandoned resets processing events whose lease has expired.
//
// The lease ID captured during the expiry scan is re-checked in the UPDATE.
// This prevents the reaper from clearing a newer lease acquired after the
// scan but before the update.
func (o *Outbox) ReapAbandoned(
	ctx context.Context,
	lockWindow time.Duration,
) (int, error) {
	if lockWindow <= 0 {
		return 0, fmt.Errorf("outbox reaper lock window must be positive")
	}
	cutoff := o.clock().Add(-lockWindow)

	rows, err := o.pool.Query(
		ctx,
		`SELECT id, lease_id, retry_count
		   FROM outbox_events
		  WHERE status = 'processing'
		    AND claimed_at <= $1`,
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("find abandoned outbox events: %w", err)
	}
	defer rows.Close()

	type abandonedEvent struct {
		id         uuid.UUID
		leaseID    uuid.UUID
		retryCount int
	}

	var events []abandonedEvent

	for rows.Next() {
		var event abandonedEvent

		if err := rows.Scan(
			&event.id,
			&event.leaseID,
			&event.retryCount,
		); err != nil {
			return 0, fmt.Errorf("scan abandoned outbox event: %w", err)
		}

		events = append(events, event)
	}

	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterate abandoned outbox events: %w", err)
	}

	reaped := 0

	for _, event := range events {
		retryCount := event.retryCount + 1
		nextAttemptAt := o.nextAttemptAt(retryCount)

		tag, err := o.pool.Exec(
			ctx,
			`UPDATE outbox_events
			    SET status          = 'failed',
			        retry_count     = retry_count + 1,
			        next_attempt_at = $3,
			        lease_id        = NULL,
			        claimed_at      = NULL,
			        updated_at      = NOW()
			  WHERE id = $1
			    AND lease_id = $2
			    AND status = 'processing'`,
			event.id,
			event.leaseID,
			nextAttemptAt,
		)
		if err != nil {
			return reaped, fmt.Errorf(
				"reap abandoned outbox event %s: %w",
				event.id,
				err,
			)
		}

		if tag.RowsAffected() == 1 {
			reaped++
		}
	}

	return reaped, nil
}
