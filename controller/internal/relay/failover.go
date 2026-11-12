package relay

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yourorg/ztna/controller/internal/relay/eligible"
)

// Controller-driven relay pool failover (Decision Record D-39; relay-pools
// PR 2).
//
// A primary pool is exhausted as soon as it has no eligible relay; its
// tenants that have a failover pool are then sent the failover pool's list.
// It recovers only after it has had an eligible relay continuously for the
// hold-down (RELAY_POOL_FAILBACK_HOLDDOWN, default 5 minutes); any loss of
// eligibility during the hold-down restarts it. Failover is not debounced
// further: eligibility already needs 90 s without a heartbeat to drop
// (eviction).
//
// The state (relay_pools.exhausted_at, eligible_since) is kept only for pools
// that matter, those some workspace uses as primary with a failover pool, so a
// new or unused pool is never "recovering". It lives in Postgres: a restart
// resumes mid-failover. ReconcilePools decides each pool in its own
// transaction under the pool row lock, so concurrent runs (the periodic loop
// and a publish) serialise and repeated runs at the same time change nothing.

const (
	DefaultFailbackHoldDown = 5 * time.Minute
	MinFailbackHoldDown     = time.Minute

	// Audit actions and actor for the system-made switches.
	ActionPoolFailover = "relay_pool.failover"
	ActionPoolFailback = "relay_pool.failback"
	PoolFailoverActor  = "system:relay-pool-failover"
)

// PoolTransition is one switch, passed to the audit hook.
type PoolTransition struct {
	PoolID   string
	PoolName string
	Action   string // ActionPoolFailover or ActionPoolFailback
	At       time.Time
	// Tenants is how many workspaces use the pool as primary with a failover
	// pool, so the switch changed their relay list.
	Tenants int
	// ExhaustedAt is when the pool became exhausted (failback only).
	ExhaustedAt *time.Time
}

// ReconcilePools applies the failover rules to every pool at now. audit runs
// in the transaction of each switch (the switch and its audit row commit
// together). changed reports whether any workspace's effective pool changed,
// so the caller publishes the relay lists.
func (s *Store) ReconcilePools(ctx context.Context, now time.Time, holdDown time.Duration,
	audit func(ctx context.Context, tx pgx.Tx, t PoolTransition) error) (changed bool, err error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM relay_pools ORDER BY id`)
	if err != nil {
		return false, fmt.Errorf("list relay pools: %w", err)
	}
	ids, err := scanRelayIDs(rows)
	rows.Close()
	if err != nil {
		return false, fmt.Errorf("list relay pools: %w", err)
	}
	for _, id := range ids {
		c, err := s.reconcilePool(ctx, id, now, holdDown, audit)
		if err != nil {
			return changed, fmt.Errorf("reconcile pool %s: %w", id, err)
		}
		changed = changed || c
	}
	return changed, nil
}

func (s *Store) reconcilePool(ctx context.Context, poolID string, now time.Time, holdDown time.Duration,
	audit func(ctx context.Context, tx pgx.Tx, t PoolTransition) error) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	var name string
	var exhaustedAt, eligibleSince *time.Time
	err = tx.QueryRow(ctx,
		`SELECT name, exhausted_at, eligible_since FROM relay_pools WHERE id = $1 FOR UPDATE`, poolID).
		Scan(&name, &exhaustedAt, &eligibleSince)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // deleted meanwhile
	}
	if err != nil {
		return false, err
	}
	var tenants, eligibleCount int
	if err := tx.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM workspaces WHERE relay_pool_id = $1 AND relay_failover_pool_id IS NOT NULL),
		       (SELECT count(*) FROM relays WHERE pool_id = $1 AND `+eligible.Where+`)`, poolID).
		Scan(&tenants, &eligibleCount); err != nil {
		return false, err
	}

	set := func(ex, since *time.Time) error {
		_, err := tx.Exec(ctx,
			`UPDATE relay_pools SET exhausted_at = $2, eligible_since = $3 WHERE id = $1`, poolID, ex, since)
		return err
	}
	commit := func() error { return tx.Commit(ctx) }
	switched := func(t PoolTransition) (bool, error) {
		if audit != nil {
			if err := audit(ctx, tx, t); err != nil {
				return false, err
			}
		}
		if err := commit(); err != nil {
			return false, err
		}
		return true, nil
	}

	switch {
	case tenants == 0:
		// No tenant fails over from this pool: no state to keep.
		if exhaustedAt != nil || eligibleSince != nil {
			if err := set(nil, nil); err != nil {
				return false, err
			}
			return false, commit()
		}
		return false, nil

	case eligibleCount == 0:
		if exhaustedAt == nil {
			at := now.UTC()
			if err := set(&at, nil); err != nil {
				return false, err
			}
			return switched(PoolTransition{PoolID: poolID, PoolName: name, Action: ActionPoolFailover, At: at, Tenants: tenants})
		}
		if eligibleSince != nil { // recovery interrupted: the hold-down restarts
			if err := set(exhaustedAt, nil); err != nil {
				return false, err
			}
			return false, commit()
		}
		return false, nil

	default: // eligible relays present
		switch {
		case exhaustedAt == nil:
			return false, nil
		case eligibleSince == nil:
			at := now.UTC()
			if err := set(exhaustedAt, &at); err != nil {
				return false, err
			}
			return false, commit()
		case now.Sub(*eligibleSince) >= holdDown:
			if err := set(nil, nil); err != nil {
				return false, err
			}
			return switched(PoolTransition{PoolID: poolID, PoolName: name, Action: ActionPoolFailback,
				At: now.UTC(), Tenants: tenants, ExhaustedAt: exhaustedAt})
		}
		return false, nil
	}
}

// RunPoolReconcileLoop runs ReconcilePools every interval until ctx ends, and
// publishes the relay lists when an effective pool changed. It completes a
// hold-down even when nothing else happens (no eviction, no operator action).
func RunPoolReconcileLoop(ctx context.Context, store *Store, interval, holdDown time.Duration,
	audit func(ctx context.Context, tx pgx.Tx, t PoolTransition) error, publish func(ctx context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			changed, err := store.ReconcilePools(ctx, time.Now(), holdDown, audit)
			if err != nil {
				log.Printf("relay pool failover: reconcile: %v", err)
			}
			if changed {
				if err := publish(ctx); err != nil {
					log.Printf("relay pool failover: publish relay lists: %v", err)
				}
			}
		}
	}
}
