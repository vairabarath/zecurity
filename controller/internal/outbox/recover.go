package outbox

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/yourorg/ztna/controller/internal/audit"
)

func (o *Outbox) Recover(
	ctx context.Context,
	eventID uuid.UUID,
	operatorID uuid.UUID,
	reason string,
	resetRetryBudget bool,
) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("outbox recovery reason is required")
	}

	if operatorID == uuid.Nil {
		return errors.New("outbox recovery operator is required")
	}

	tx, err := o.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin outbox recovery: %w", err)
	}
	defer tx.Rollback(ctx)

	var (
		status      string
		retryCount  int
		workspaceID uuid.UUID
	)

	err = tx.QueryRow(
		ctx,
		`SELECT status, retry_count, workspace_id
		   FROM outbox_events
		  WHERE id = $1
		  FOR UPDATE`,
		eventID,
	).Scan(
		&status,
		&retryCount,
		&workspaceID,
	)
	if err != nil {
		return fmt.Errorf("load outbox event for recovery: %w", err)
	}

	if status != "failed" {
		return fmt.Errorf(
			"outbox event %s is not terminal: status=%s",
			eventID,
			status,
		)
	}

	if retryCount < o.retryPolicy.MaxRetries {
		return fmt.Errorf(
			"outbox event %s is failed but not terminal: retry_count=%d max_retries=%d",
			eventID,
			retryCount,
			o.retryPolicy.MaxRetries,
		)
	}

	var actorEmail string

	err = tx.QueryRow(
		ctx,
		`SELECT email
		   FROM users
		  WHERE id = $1`,
		operatorID,
	).Scan(&actorEmail)
	if err != nil {
		return fmt.Errorf("load recovery operator: %w", err)
	}

	newRetryCount := retryCount
	if resetRetryBudget {
		newRetryCount = 0
	}

	tag, err := tx.Exec(
		ctx,
		`UPDATE outbox_events
		    SET status          = 'failed',
		        retry_count     = $2,
		        next_attempt_at = NOW(),
		        lease_id        = NULL,
		        claimed_at      = NULL,
		        updated_at      = NOW()
		  WHERE id = $1
		    AND status = 'failed'
		    AND retry_count >= $3`,
		eventID,
		newRetryCount,
		o.retryPolicy.MaxRetries,
	)
	if err != nil {
		return fmt.Errorf("recover outbox event: %w", err)
	}

	if tag.RowsAffected() != 1 {
		return ErrStaleLease
	}

	if err := audit.RecordTx(ctx, tx, audit.Entry{
		TenantID:    workspaceID.String(),
		ActorUserID: operatorID.String(),
		ActorEmail:  actorEmail,
		Action:      "outbox.recover",
		TargetType:  "outbox_event",
		TargetID:    eventID.String(),
		Details: map[string]any{
			"reason":               reason,
			"reset_retry_budget":   resetRetryBudget,
			"previous_retry_count": retryCount,
			"new_retry_count":      newRetryCount,
		},
	}); err != nil {
		return fmt.Errorf("audit outbox recovery: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit outbox recovery: %w", err)
	}

	return nil
}
