package relay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Relay workspace policy (Decision Record D-40; relay-pools PR 3).
//
// Every heartbeat response carries the policy of the relay's pool: whether it
// enforces residency, the workspaces allowed on it, a version and a lease. A
// workspace is allowed on a pool while the pool is its primary, its failover
// or the previous pool of an open graceful migration window; the window is
// closed (and the pool's version bumped) by ExpireMigrationWindows, so the
// allowed set only changes together with the version.
//
// Versions come from relay_policy_version_seq, shared by every pool, so they
// only increase for a relay, also across a move between pools.

// DefaultPolicyLease is how long a relay may keep using a policy without a
// renewed one (D-40). It must cover several heartbeats: a missed heartbeat
// must not stop a region.
const (
	DefaultPolicyLease = 10 * time.Minute
	MinPolicyLease     = 4 * defaultHeartbeatInterval
)

// bumpPolicyVersions gives each pool a new policy version, in the caller's
// transaction. Empty and duplicate ids are ignored.
func bumpPolicyVersions(ctx context.Context, tx pgx.Tx, poolIDs ...string) error {
	seen := map[string]bool{}
	ids := make([]string, 0, len(poolIDs))
	for _, id := range poolIDs {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx,
		`UPDATE relay_pools SET policy_version = nextval('relay_policy_version_seq')
		  WHERE id = ANY($1::uuid[])`, ids); err != nil {
		return fmt.Errorf("bump relay policy versions: %w", err)
	}
	return nil
}

// assignmentPools lists every pool an assignment authorizes.
func assignmentPools(a PoolAssignment) []string {
	out := []string{a.PoolID}
	if a.FailoverPoolID != nil {
		out = append(out, *a.FailoverPoolID)
	}
	if a.PreviousPoolID != nil {
		out = append(out, *a.PreviousPoolID)
	}
	return out
}

// WorkspacePolicy is the policy a relay is sent.
type WorkspacePolicy struct {
	Enforced  bool
	Version   uint64
	Workspace []string
}

// RelayWorkspacePolicy reads the policy of a relay's pool. The workspace list
// is filled only for an enforcing (residency) pool.
func (s *Store) RelayWorkspacePolicy(ctx context.Context, relayID string) (WorkspacePolicy, error) {
	var p WorkspacePolicy
	var poolID string
	var version int64
	err := s.pool.QueryRow(ctx, `
		SELECT p.id::text, p.residency, p.policy_version
		  FROM relays r JOIN relay_pools p ON p.id = r.pool_id
		 WHERE r.id = $1`, relayID).Scan(&poolID, &p.Enforced, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return WorkspacePolicy{}, ErrRelayNotFound
	}
	if err != nil {
		return WorkspacePolicy{}, fmt.Errorf("read relay policy: %w", err)
	}
	p.Version = uint64(version)
	if !p.Enforced {
		return p, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id::text FROM workspaces
		 WHERE relay_pool_id = $1 OR relay_failover_pool_id = $1 OR relay_pool_previous_id = $1
		 ORDER BY id`, poolID)
	if err != nil {
		return WorkspacePolicy{}, fmt.Errorf("read relay policy workspaces: %w", err)
	}
	ids, err := scanRelayIDs(rows)
	rows.Close()
	if err != nil {
		return WorkspacePolicy{}, fmt.Errorf("read relay policy workspaces: %w", err)
	}
	p.Workspace = ids
	return p, nil
}

// RecordPolicyAck stores what a relay reported about its policy. It writes
// only when something changed.
func (s *Store) RecordPolicyAck(ctx context.Context, relayID string, supported bool, applied uint64) error {
	var acked *int64
	if applied > 0 {
		v := int64(applied)
		acked = &v
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE relays
		   SET policy_supported = $2, policy_version_acked = $3, policy_acked_at = NOW()
		 WHERE id = $1
		   AND (policy_supported IS DISTINCT FROM $2 OR policy_version_acked IS DISTINCT FROM $3)`,
		relayID, supported, acked)
	if err != nil {
		return fmt.Errorf("record relay policy ack: %w", err)
	}
	return nil
}

// ExpireMigrationWindows closes the graceful migration windows that ended by
// now: the previous pool loses the workspace's authorization and gets a new
// policy version, so its relays drop the workspace at their next heartbeat
// (D-40: then it is a revocation). Returns how many windows closed.
func (s *Store) ExpireMigrationWindows(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `
		WITH expired AS (
			SELECT id, relay_pool_previous_id AS prev
			  FROM workspaces
			 WHERE relay_pool_previous_until IS NOT NULL AND relay_pool_previous_until <= $1
			   FOR UPDATE
		)
		UPDATE workspaces w
		   SET relay_pool_previous_id = NULL, relay_pool_previous_until = NULL
		  FROM expired
		 WHERE w.id = expired.id
		RETURNING expired.prev::text`, now)
	if err != nil {
		return 0, fmt.Errorf("expire migration windows: %w", err)
	}
	pools, err := scanRelayIDs(rows)
	rows.Close()
	if err != nil {
		return 0, fmt.Errorf("expire migration windows: %w", err)
	}
	if err := bumpPolicyVersions(ctx, tx, pools...); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit expire migration windows: %w", err)
	}
	return len(pools), nil
}
