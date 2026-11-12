package relay

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// Relay drain and resume (Sprint 22 Phase D, Decision Record D-32).
//
// A draining relay has drain_started_at set. eligible.Where leaves it out of
// the relay list published to connectors, so no new placements reach it and
// connectors migrate away through their existing path (no connector, relay or
// proto change). Draining the last eligible relay of a pool in use is refused
// (per pool since D-41); resume puts the relay back. Drain is an availability tool, not a security control: revoke
// is the control.

var (
	errInvalidState      = errors.New("relay is not enrolled")
	errLastEligibleRelay = errors.New("relay is the last eligible relay")
)

// DrainRelay sets drain_started_at, refusing to drain the last eligible relay
// of a pool in use (D-32, per pool since D-41: a pool that is a workspace's
// primary pool, or the default pool). One transaction locks the relay's pool
// row, then the target and every eligible relay of that pool in id order, so
// two drains, or a drain and a move, of a pool's last two eligible relays
// serialise and can't both pass; a drain waiting on another re-checks
// eligible.Where against the committed rows (READ COMMITTED). Draining is
// refused even when the pool's tenants have a failover pool: failover is for
// emergencies (D-41). inTx (the audit row) runs in the same transaction.
// changed=false means the relay was already draining: nothing is written and
// inTx doesn't run.
func (s *Store) DrainRelay(ctx context.Context, relayID string,
	inTx func(ctx context.Context, tx pgx.Tx, drainStartedAt time.Time) error) (changed bool, err error) {
	err = s.withPoolRetry(ctx, "drain relay", func(tx pgx.Tx) error {
		changed = false
		poolID, err := relayPoolOf(ctx, tx, relayID)
		if err != nil {
			return err
		}
		if _, err := lockPools(ctx, tx, poolID); err != nil {
			return err
		}
		l, err := lockRelayAndPoolPeers(ctx, tx, relayID, poolID)
		if err != nil {
			return err
		}
		switch {
		case !l.found:
			return ErrRelayNotFound
		case l.poolID != poolID:
			return errPoolChangedConcurrently
		case l.status == "deleted":
			return errAlreadyDeleted
		case l.status == "revoked":
			return errAlreadyRevoked
		case l.status == "pending":
			return errInvalidState
		case l.draining:
			return nil
		}
		if l.eligible && l.eligibleInPool == 1 {
			used, err := poolInUse(ctx, tx, poolID)
			if err != nil {
				return fmt.Errorf("check pool use: %w", err)
			}
			if used {
				return errLastEligibleRelay
			}
		}

		// FU-7: the controller's clock, read after the row locks are held. The
		// drain window is measured against connection_count_at, which is also
		// the controller's clock (the heartbeat's receive time); the database's
		// NOW() is the transaction start, before any lock wait, and may be
		// skewed.
		var started time.Time
		if err := tx.QueryRow(ctx,
			`UPDATE relays SET drain_started_at = $2, updated_at = NOW() WHERE id = $1 RETURNING drain_started_at`,
			relayID, time.Now().UTC()).Scan(&started); err != nil {
			return fmt.Errorf("mark relay draining: %w", err)
		}
		if inTx != nil {
			if err := inTx(ctx, tx, started); err != nil {
				return err
			}
		}
		changed = true
		return nil
	})
	return changed, err
}

// ResumeRelay clears drain_started_at under the row lock. inTx (the audit row)
// gets the time the drain started and runs in the same transaction.
// changed=false means the relay wasn't draining: nothing is written.
func (s *Store) ResumeRelay(ctx context.Context, relayID string,
	inTx func(ctx context.Context, tx pgx.Tx, drainedSince time.Time) error) (changed bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin resume relay: %w", err)
	}
	defer tx.Rollback(ctx)

	var status string
	var since *time.Time
	if err := tx.QueryRow(ctx,
		`SELECT status, drain_started_at FROM relays WHERE id = $1 FOR UPDATE`, relayID,
	).Scan(&status, &since); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrRelayNotFound
		}
		return false, fmt.Errorf("lock relay for resume: %w", err)
	}
	switch {
	case status == "deleted":
		return false, errAlreadyDeleted
	case status == "revoked":
		return false, errAlreadyRevoked
	case since == nil:
		return false, nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE relays SET drain_started_at = NULL, updated_at = NOW() WHERE id = $1`, relayID); err != nil {
		return false, fmt.Errorf("clear relay drain: %w", err)
	}
	if inTx != nil {
		if err := inTx(ctx, tx, *since); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit resume relay: %w", err)
	}
	return true, nil
}

// ListDrainingRelayIDs returns the draining relays that may still heartbeat
// (active or inactive): the source of truth for the heartbeat draining flags.
func (s *Store) ListDrainingRelayIDs(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text FROM relays
		 WHERE drain_started_at IS NOT NULL
		   AND status IN ('active', 'inactive')`)
	if err != nil {
		return nil, fmt.Errorf("list draining relays: %w", err)
	}
	defer rows.Close()
	return scanRelayIDs(rows)
}

// relayHeartbeatDrainingPrefix marks a draining relay in Valkey so each of its
// heartbeats persists the connection count (normally written at most every
// RELAY_HEARTBEAT_DB_WRITE_INTERVAL). Postgres stays authoritative: the flag
// expires, and RunDrainFlagLoop re-sets it from Postgres. Any lost or failed
// flag only makes stored counts older, never an early "drained".
const (
	relayHeartbeatDrainingPrefix = "relay:heartbeat:draining:"
	drainingFlagTTL              = 10 * time.Minute
)

// MarkDraining sets (or refreshes) a relay's draining flag. No-op without a cache.
func (s *Service) MarkDraining(ctx context.Context, relayID string) error {
	if s.redis == nil {
		return nil
	}
	if err := s.redis.Set(ctx, relayHeartbeatDrainingPrefix+relayID, "1", drainingFlagTTL).Err(); err != nil {
		return fmt.Errorf("set draining flag: %w", err)
	}
	return nil
}

// ClearDraining deletes a relay's draining flag. No-op without a cache.
func (s *Service) ClearDraining(ctx context.Context, relayID string) error {
	if s.redis == nil {
		return nil
	}
	if err := s.redis.Del(ctx, relayHeartbeatDrainingPrefix+relayID).Err(); err != nil {
		return fmt.Errorf("clear draining flag: %w", err)
	}
	return nil
}

// drainingFlagSet reports whether the relay's draining flag is present.
func (s *Service) drainingFlagSet(ctx context.Context, relayID string) (bool, error) {
	n, err := s.redis.Exists(ctx, relayHeartbeatDrainingPrefix+relayID).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

type drainingRelayLister interface {
	ListDrainingRelayIDs(ctx context.Context) ([]string, error)
}

// ReconcileDrainFlags sets the draining flag, with a fresh TTL, for every
// draining relay in Postgres: a flag lost to a failed set, a controller restart
// or an expiry comes back within one pass. A resumed relay's flag is not
// re-set and expires on its own.
func ReconcileDrainFlags(ctx context.Context, store drainingRelayLister, svc *Service) {
	ids, err := store.ListDrainingRelayIDs(ctx)
	if err != nil {
		log.Printf("relay drain flags: list draining relays: %v", err)
		return
	}
	for _, id := range ids {
		if err := svc.MarkDraining(ctx, id); err != nil {
			log.Printf("relay drain flags: relay %s: %v", id, err)
		}
	}
}

// RunDrainFlagLoop runs ReconcileDrainFlags every interval until ctx ends.
func RunDrainFlagLoop(ctx context.Context, store drainingRelayLister, svc *Service, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ReconcileDrainFlags(ctx, store, svc)
		}
	}
}

type drainFlagger interface {
	MarkDraining(ctx context.Context, relayID string) error
	ClearDraining(ctx context.Context, relayID string) error
}

// DrainFanout is the post-commit hook for a drain or resume that changed
// state: set or clear the heartbeat draining flag, then publish the relay
// list so connectors migrate (or may return). Not the ACL, and no transport
// invalidation: placements notify as connectors move (ADR-017). Failures are
// logged with the relay id and never undo the committed change: a lost flag is
// restored by RunDrainFlagLoop, and connectors that miss the list get the
// current one at their next broadcast or reconnect.
func DrainFanout(flags drainFlagger, publish func(ctx context.Context) error) func(ctx context.Context, relayID string, draining bool) {
	return func(ctx context.Context, relayID string, draining bool) {
		var err error
		if draining {
			err = flags.MarkDraining(ctx, relayID)
		} else {
			err = flags.ClearDraining(ctx, relayID)
		}
		if err != nil {
			log.Printf("relay drain fan-out: relay %s draining=%v: draining flag: %v (restored by the reconcile loop)", relayID, draining, err)
		}
		if err := publish(ctx); err != nil {
			log.Printf("relay drain fan-out: relay %s draining=%v: publish relay list: %v (connectors keep their list until the next broadcast or reconnect)", relayID, draining, err)
		}
	}
}
