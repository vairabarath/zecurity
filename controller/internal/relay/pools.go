package relay

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yourorg/ztna/controller/internal/relay/eligible"
	"github.com/yourorg/ztna/controller/internal/relay/relaypool"
)

// Relay pools (Decision Record D-37, D-38, D-40, D-41; relay-pools PR 1).
//
// Every relay is in exactly one pool and every workspace has a primary pool;
// connectors receive only their workspace's pool's eligible relays. Guards
// that keep a pool usable (D-41, the per-pool form of D-32) serialise on the
// pool row: every operation that can take a pool's last eligible relay away or
// start using a pool locks the pool row(s) first, in id order, then any relay
// rows, in id order, in one transaction. Involuntary changes (a relay stops
// heartbeating) take no pool lock: the guard protects against operator
// actions, not outages.

var (
	ErrPoolNotFound            = errors.New("relay pool not found")
	errPoolNameTaken           = errors.New("relay pool name taken")
	errPoolIsDefault           = errors.New("the default relay pool can't be changed or deleted")
	errPoolInUse               = errors.New("relay pool is used by a workspace")
	errPoolNotEmpty            = errors.New("relay pool still has relays or workspaces")
	errPoolHasNoEligibleRelay  = errors.New("relay pool has no eligible relay")
	ErrWorkspaceNotFound       = errors.New("workspace not found")
	errPoolChangedConcurrently = errors.New("relay pool changed concurrently")
)

// Pool is the stored form of a relay pool.
type Pool struct {
	ID        string
	Name      string
	Region    *string
	Residency bool
}

// PoolChange is a requested pool update; nil fields are left as they are.
// SetRegion distinguishes "set region to Region (possibly nil = clear)" from
// "leave region".
type PoolChange struct {
	Name      *string
	SetRegion bool
	Region    *string
	Residency *bool
}

// TouchesPlacement reports whether the change affects where tenants' traffic
// may go (region or residency): super-admin only (D-41) and only while no
// workspace uses the pool (D-37).
func (c PoolChange) TouchesPlacement() bool { return c.SetRegion || c.Residency != nil }

// ValidPoolName: trimmed, 1–64 runes, no control characters.
func ValidPoolName(name string) bool {
	if name == "" || name != strings.TrimSpace(name) || utf8.RuneCountInString(name) > relaypool.MaxNameRunes {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ValidatePoolFields checks a pool's region and residency together: a
// residency pool needs a region; a region must be a canonical code.
func ValidatePoolFields(region *string, residency bool) error {
	if region != nil && !relaypool.ValidRegion(*region) {
		return errInvalidRegion
	}
	if residency && region == nil {
		return errResidencyNeedsRegion
	}
	return nil
}

var (
	errInvalidRegion        = errors.New("invalid region code")
	errResidencyNeedsRegion = errors.New("a residency pool needs a region")
)

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func scanPool(row pgx.Row) (Pool, error) {
	var p Pool
	if err := row.Scan(&p.ID, &p.Name, &p.Region, &p.Residency); err != nil {
		return Pool{}, err
	}
	return p, nil
}

const poolColumns = `id::text, name, region, residency`

// CreatePool inserts a pool; inTx (the audit row) commits with it.
func (s *Store) CreatePool(ctx context.Context, name string, region *string, residency bool,
	inTx func(ctx context.Context, tx pgx.Tx, p Pool) error) (Pool, error) {
	if !ValidPoolName(name) {
		return Pool{}, errInvalidPoolName
	}
	if err := ValidatePoolFields(region, residency); err != nil {
		return Pool{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Pool{}, fmt.Errorf("begin create pool: %w", err)
	}
	defer tx.Rollback(ctx)
	p, err := scanPool(tx.QueryRow(ctx,
		`INSERT INTO relay_pools (name, region, residency) VALUES ($1, $2, $3) RETURNING `+poolColumns,
		name, region, residency))
	if isUniqueViolation(err) {
		return Pool{}, errPoolNameTaken
	}
	if err != nil {
		return Pool{}, fmt.Errorf("insert relay pool: %w", err)
	}
	if inTx != nil {
		if err := inTx(ctx, tx, p); err != nil {
			return Pool{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Pool{}, fmt.Errorf("commit create pool: %w", err)
	}
	return p, nil
}

var errInvalidPoolName = errors.New("invalid pool name")

// lockPools locks the given pool rows in id order and returns them by id. A
// missing id is ErrPoolNotFound.
func lockPools(ctx context.Context, tx pgx.Tx, ids ...string) (map[string]Pool, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+poolColumns+` FROM relay_pools WHERE id = ANY($1::uuid[]) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		return nil, fmt.Errorf("lock relay pools: %w", err)
	}
	defer rows.Close()
	out := map[string]Pool{}
	for rows.Next() {
		p, err := scanPool(rows)
		if err != nil {
			return nil, fmt.Errorf("scan relay pool: %w", err)
		}
		out[p.ID] = p
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock relay pools: %w", err)
	}
	for _, id := range ids {
		if _, ok := out[id]; !ok {
			return nil, ErrPoolNotFound
		}
	}
	return out, nil
}

// poolInUse: the pool is a workspace's primary pool, or the default pool,
// where every new workspace starts (D-41). The pool row must be locked.
func poolInUse(ctx context.Context, tx pgx.Tx, poolID string) (bool, error) {
	if poolID == relaypool.DefaultPoolID {
		return true, nil
	}
	var used bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM workspaces WHERE relay_pool_id = $1)`, poolID).Scan(&used)
	return used, err
}

// poolReferenced: any workspace names the pool, as primary or as the previous
// pool of a migration (even an expired one: the reference is still stored).
func poolReferenced(ctx context.Context, tx pgx.Tx, poolID string) (bool, error) {
	var used bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM workspaces WHERE relay_pool_id = $1 OR relay_pool_previous_id = $1)`, poolID).Scan(&used)
	return used, err
}

// UpdatePool applies change under the pool row lock. Region and residency may
// only change while no workspace uses the pool (D-37). The default pool can't
// be changed. inTx gets the pool before and after.
func (s *Store) UpdatePool(ctx context.Context, poolID string, change PoolChange,
	inTx func(ctx context.Context, tx pgx.Tx, before, after Pool) error) (changed bool, err error) {
	if poolID == relaypool.DefaultPoolID {
		return false, errPoolIsDefault
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin update pool: %w", err)
	}
	defer tx.Rollback(ctx)
	pools, err := lockPools(ctx, tx, poolID)
	if err != nil {
		return false, err
	}
	before := pools[poolID]
	after := before
	if change.Name != nil {
		if !ValidPoolName(*change.Name) {
			return false, errInvalidPoolName
		}
		after.Name = *change.Name
	}
	if change.SetRegion {
		after.Region = change.Region
	}
	if change.Residency != nil {
		after.Residency = *change.Residency
	}
	if err := ValidatePoolFields(after.Region, after.Residency); err != nil {
		return false, err
	}
	placementChanged := !equalStringPtr(before.Region, after.Region) || before.Residency != after.Residency
	if after.Name == before.Name && !placementChanged {
		return false, nil
	}
	if placementChanged {
		used, err := poolReferenced(ctx, tx, poolID)
		if err != nil {
			return false, fmt.Errorf("check pool use: %w", err)
		}
		if used {
			return false, errPoolInUse
		}
	}
	_, err = tx.Exec(ctx,
		`UPDATE relay_pools SET name = $2, region = $3, residency = $4, updated_at = NOW() WHERE id = $1`,
		poolID, after.Name, after.Region, after.Residency)
	if isUniqueViolation(err) {
		return false, errPoolNameTaken
	}
	if err != nil {
		return false, fmt.Errorf("update relay pool: %w", err)
	}
	if inTx != nil {
		if err := inTx(ctx, tx, before, after); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit update pool: %w", err)
	}
	return true, nil
}

func equalStringPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// DeletePool deletes an empty pool: no relay other than deleted ones, and no
// workspace referencing it. Deleted relays (kept for history) move to the
// default pool first. The default pool can't be deleted.
func (s *Store) DeletePool(ctx context.Context, poolID string,
	inTx func(ctx context.Context, tx pgx.Tx, p Pool) error) error {
	if poolID == relaypool.DefaultPoolID {
		return errPoolIsDefault
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin delete pool: %w", err)
	}
	defer tx.Rollback(ctx)
	// The default pool is locked too: deleted relays move into it.
	pools, err := lockPools(ctx, tx, poolID, relaypool.DefaultPoolID)
	if err != nil {
		return err
	}
	var relays int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM relays WHERE pool_id = $1 AND status <> 'deleted'`, poolID).Scan(&relays); err != nil {
		return fmt.Errorf("count pool relays: %w", err)
	}
	used, err := poolReferenced(ctx, tx, poolID)
	if err != nil {
		return fmt.Errorf("check pool use: %w", err)
	}
	if relays > 0 || used {
		return errPoolNotEmpty
	}
	if _, err := tx.Exec(ctx,
		`UPDATE relays SET pool_id = $2 WHERE pool_id = $1`, poolID, relaypool.DefaultPoolID); err != nil {
		return fmt.Errorf("move deleted relays to the default pool: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM relay_pools WHERE id = $1`, poolID); err != nil {
		return fmt.Errorf("delete relay pool: %w", err)
	}
	if inTx != nil {
		if err := inTx(ctx, tx, pools[poolID]); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// relayPoolOf reads a relay's pool without a lock (the caller locks the pool,
// then re-checks the relay under its row lock).
func relayPoolOf(ctx context.Context, tx pgx.Tx, relayID string) (string, error) {
	var poolID string
	err := tx.QueryRow(ctx, `SELECT pool_id::text FROM relays WHERE id = $1`, relayID).Scan(&poolID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrRelayNotFound
	}
	return poolID, err
}

// lockedRelays is the result of locking a relay and its pool's eligible relays.
type lockedRelays struct {
	found, draining, eligible bool
	status, poolID            string
	eligibleInPool            int
}

// lockRelayAndPoolPeers locks relayID and every eligible relay in poolID, in
// id order, and describes them. The caller holds poolID's row lock.
func lockRelayAndPoolPeers(ctx context.Context, tx pgx.Tx, relayID, poolID string) (lockedRelays, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, status, drain_started_at IS NOT NULL, `+eligible.Where+`, pool_id::text
		  FROM relays
		 WHERE id = $1 OR (pool_id = $2 AND `+eligible.Where+`)
		 ORDER BY id
		   FOR UPDATE`, relayID, poolID)
	if err != nil {
		return lockedRelays{}, fmt.Errorf("lock relays: %w", err)
	}
	defer rows.Close()
	var l lockedRelays
	for rows.Next() {
		var id, status, pool string
		var draining, isEligible bool
		if err := rows.Scan(&id, &status, &draining, &isEligible, &pool); err != nil {
			return lockedRelays{}, fmt.Errorf("scan relay: %w", err)
		}
		if isEligible && pool == poolID {
			l.eligibleInPool++
		}
		if id == relayID {
			l.found, l.status, l.draining, l.eligible, l.poolID = true, status, draining, isEligible, pool
		}
	}
	if err := rows.Err(); err != nil {
		return lockedRelays{}, fmt.Errorf("lock relays: %w", err)
	}
	return l, nil
}

// withPoolRetry runs fn in a fresh transaction, retrying a few times when the
// relay moved to another pool between reading its pool and locking it.
func (s *Store) withPoolRetry(ctx context.Context, what string, fn func(tx pgx.Tx) error) error {
	for attempt := 0; ; attempt++ {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin %s: %w", what, err)
		}
		err = fn(tx)
		if err == nil {
			err = tx.Commit(ctx)
			if err != nil {
				err = fmt.Errorf("commit %s: %w", what, err)
			}
		}
		_ = tx.Rollback(ctx)
		if errors.Is(err, errPoolChangedConcurrently) && attempt < 3 {
			continue
		}
		return err
	}
}

// MoveRelay moves a relay to another pool (D-41: relay-ops). Refused when the
// relay is the last eligible relay of a pool in use, so a move can't leave
// tenants without a relay. A deleted or revoked relay can't be moved. inTx
// gets the source and destination pools. changed=false: already there.
func (s *Store) MoveRelay(ctx context.Context, relayID, toPoolID string,
	inTx func(ctx context.Context, tx pgx.Tx, from, to Pool) error) (changed bool, err error) {
	err = s.withPoolRetry(ctx, "move relay", func(tx pgx.Tx) error {
		changed = false
		fromID, err := relayPoolOf(ctx, tx, relayID)
		if err != nil {
			return err
		}
		pools, err := lockPools(ctx, tx, fromID, toPoolID)
		if err != nil {
			return err
		}
		l, err := lockRelayAndPoolPeers(ctx, tx, relayID, fromID)
		if err != nil {
			return err
		}
		switch {
		case !l.found:
			return ErrRelayNotFound
		case l.poolID != fromID:
			return errPoolChangedConcurrently
		case l.status == "deleted":
			return errAlreadyDeleted
		case l.status == "revoked":
			return errAlreadyRevoked
		case fromID == toPoolID:
			return nil
		}
		if l.eligible && l.eligibleInPool == 1 {
			used, err := poolInUse(ctx, tx, fromID)
			if err != nil {
				return fmt.Errorf("check pool use: %w", err)
			}
			if used {
				return errLastEligibleRelay
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE relays SET pool_id = $2, updated_at = NOW() WHERE id = $1`, relayID, toPoolID); err != nil {
			return fmt.Errorf("move relay: %w", err)
		}
		if inTx != nil {
			if err := inTx(ctx, tx, pools[fromID], pools[toPoolID]); err != nil {
				return err
			}
		}
		changed = true
		return nil
	})
	return changed, err
}

// PoolAssignment is a workspace's relay pool assignment.
type PoolAssignment struct {
	PoolID            string
	PreviousPoolID    *string
	PreviousPoolUntil *time.Time
}

// AssignWorkspacePool sets a workspace's primary pool (D-38, D-41:
// super-admin). The target pool must have an eligible relay. graceful keeps
// the old pool authorized for window (D-40: a planned move; residency checks
// accept it until then); otherwise the old pool loses authorization at once.
// inTx gets the assignment before and after. changed=false: already there.
func (s *Store) AssignWorkspacePool(ctx context.Context, workspaceID, toPoolID string, graceful bool, window time.Duration,
	inTx func(ctx context.Context, tx pgx.Tx, before, after PoolAssignment) error) (changed bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin assign pool: %w", err)
	}
	defer tx.Rollback(ctx)

	var before PoolAssignment
	err = tx.QueryRow(ctx, `
		SELECT relay_pool_id::text, relay_pool_previous_id::text, relay_pool_previous_until
		  FROM workspaces WHERE id = $1 FOR UPDATE`, workspaceID).
		Scan(&before.PoolID, &before.PreviousPoolID, &before.PreviousPoolUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrWorkspaceNotFound
	}
	if err != nil {
		return false, fmt.Errorf("lock workspace: %w", err)
	}
	if before.PoolID == toPoolID {
		return false, nil
	}
	// Lock the target pool: a concurrent drain or move of its last eligible
	// relay waits for this assignment (and then sees the pool in use), or this
	// assignment waits for it (and then sees no eligible relay).
	if _, err := lockPools(ctx, tx, toPoolID); err != nil {
		return false, err
	}
	var eligibleCount int
	if err := tx.QueryRow(ctx,
		`SELECT count(*) FROM relays WHERE pool_id = $1 AND `+eligible.Where, toPoolID).Scan(&eligibleCount); err != nil {
		return false, fmt.Errorf("count eligible relays: %w", err)
	}
	if eligibleCount == 0 {
		return false, errPoolHasNoEligibleRelay
	}

	after := PoolAssignment{PoolID: toPoolID}
	if graceful {
		from := before.PoolID
		until := time.Now().UTC().Add(window)
		after.PreviousPoolID, after.PreviousPoolUntil = &from, &until
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workspaces
		   SET relay_pool_id = $2, relay_pool_previous_id = $3, relay_pool_previous_until = $4, updated_at = NOW()
		 WHERE id = $1`, workspaceID, toPoolID, after.PreviousPoolID, after.PreviousPoolUntil); err != nil {
		return false, fmt.Errorf("assign workspace pool: %w", err)
	}
	if inTx != nil {
		if err := inTx(ctx, tx, before, after); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit assign pool: %w", err)
	}
	return true, nil
}

// PlacementAllowed reports whether a connector of workspaceID may be recorded
// as placed on relayID (D-40): always when the workspace's primary pool is not
// a residency pool; otherwise only when the relay is in one of the
// workspace's authorized pools (relaypool.AuthorizedPools). An unknown
// workspace is allowed (nothing to enforce); an unknown relay of a residency
// workspace is refused.
func (s *Store) PlacementAllowed(ctx context.Context, workspaceID, relayID string) (bool, error) {
	var residency, authorized bool
	err := s.pool.QueryRow(ctx, `
		SELECT p.residency,
		       EXISTS (SELECT 1 FROM relays r
		                WHERE r.id::text = $2 AND `+relaypool.AuthorizedPools("r.pool_id")+`)
		  FROM workspaces w
		  JOIN relay_pools p ON p.id = w.relay_pool_id
		 WHERE w.id::text = $1`, workspaceID, relayID).Scan(&residency, &authorized)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("check relay placement: %w", err)
	}
	return !residency || authorized, nil
}
