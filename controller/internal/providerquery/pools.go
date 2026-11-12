package providerquery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yourorg/ztna/controller/internal/relay/eligible"
	"github.com/yourorg/ztna/controller/internal/relay/relaypool"
)

// Relay pool reads (D-37 … D-41). Pool summaries carry counts only: relay-ops
// may read pools (relay.read) but never tenant names (D-41, D-06); the tenant
// list of a pool is added only for callers allowed tenant.read.

// RelayPoolSummary is one row of GET /provider/relay-pools.
type RelayPoolSummary struct {
	RelayPoolRef
	IsDefault bool `json:"is_default"`
	// Relays: relays in the pool, except deleted ones. EligibleRelays: those in
	// the list connectors receive (eligible.Where). Tenants: workspaces whose
	// primary pool this is.
	Relays         int64     `json:"relays"`
	EligibleRelays int64     `json:"eligible_relays"`
	Tenants        int64     `json:"tenants"`
	CreatedAt      time.Time `json:"created_at"`
	// Failover (D-39). FailoverTenants: workspaces using this pool as their
	// failover pool. State: healthy | failed_over | recovering (its tenants
	// with a failover pool use that pool unless healthy). ExhaustedAt and
	// EligibleSince are the stored state behind it.
	FailoverTenants int64      `json:"failover_tenants"`
	State           string     `json:"state"`
	ExhaustedAt     *time.Time `json:"exhausted_at"`
	EligibleSince   *time.Time `json:"eligible_since"`
}

// RelayPoolRelay is one relay in pool detail.
type RelayPoolRelay struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Eligible bool   `json:"eligible"`
}

// RelayPoolTenant is one tenant in pool detail (super-admin only).
type RelayPoolTenant struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
}

// RelayPoolDetail is GET /provider/relay-pools/{id}. Tenants is null unless
// the caller may read tenants.
type RelayPoolDetail struct {
	RelayPoolSummary
	RelayList     []RelayPoolRelay   `json:"relay_list"`
	RelayListCut  bool               `json:"relay_list_truncated"`
	TenantList    *[]RelayPoolTenant `json:"tenant_list"`
	TenantListCut bool               `json:"tenant_list_truncated"`
}

const relayPoolSummarySelect = `
SELECT p.id::text, p.name, p.region, p.residency, p.created_at,
       (SELECT count(*) FROM relays WHERE pool_id = p.id AND status <> 'deleted'),
       (SELECT count(*) FROM relays WHERE pool_id = p.id AND ` + eligible.Where + `),
       (SELECT count(*) FROM workspaces WHERE relay_pool_id = p.id),
       (SELECT count(*) FROM workspaces WHERE relay_failover_pool_id = p.id),
       p.exhausted_at, p.eligible_since
  FROM relay_pools p`

func scanRelayPoolSummary(row pgx.Row) (RelayPoolSummary, error) {
	var s RelayPoolSummary
	if err := row.Scan(&s.ID, &s.Name, &s.Region, &s.Residency, &s.CreatedAt,
		&s.Relays, &s.EligibleRelays, &s.Tenants, &s.FailoverTenants, &s.ExhaustedAt, &s.EligibleSince); err != nil {
		return RelayPoolSummary{}, err
	}
	s.CreatedAt = s.CreatedAt.UTC()
	s.ExhaustedAt, s.EligibleSince = utcPtr(s.ExhaustedAt), utcPtr(s.EligibleSince)
	s.State = relaypool.State(s.ExhaustedAt, s.EligibleSince)
	s.IsDefault = s.ID == relaypool.DefaultPoolID
	return s, nil
}

// ListRelayPools returns every pool, the default pool first, then by name.
// Pools are few (operator-created); no pagination.
func ListRelayPools(ctx context.Context, q Querier) ([]RelayPoolSummary, error) {
	rows, err := q.Query(ctx, relayPoolSummarySelect+`
 ORDER BY (p.id = '`+relaypool.DefaultPoolID+`'::uuid) DESC, p.name, p.id`)
	if err != nil {
		return nil, fmt.Errorf("list relay pools: %w", err)
	}
	defer rows.Close()
	out := []RelayPoolSummary{}
	for rows.Next() {
		s, err := scanRelayPoolSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("scan relay pool: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list relay pools: %w", err)
	}
	return out, nil
}

// GetRelayPool returns one pool with its relays (capped at DetailCap) and,
// when withTenants, the tenants whose primary pool it is (capped).
func GetRelayPool(ctx context.Context, q Querier, id string, withTenants bool) (RelayPoolDetail, error) {
	if !IsUUID(id) {
		return RelayPoolDetail{}, ErrInvalidID
	}
	s, err := scanRelayPoolSummary(q.QueryRow(ctx, relayPoolSummarySelect+`
 WHERE p.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return RelayPoolDetail{}, ErrNotFound
	}
	if err != nil {
		return RelayPoolDetail{}, fmt.Errorf("get relay pool: %w", err)
	}
	d := RelayPoolDetail{RelayPoolSummary: s}
	if d.RelayList, d.RelayListCut, err = collect(ctx, q, `
SELECT id::text, name, status, `+eligible.Where+`
  FROM relays WHERE pool_id = $1 AND status <> 'deleted'
 ORDER BY name, id LIMIT $2`, id, func(r pgx.Rows) (RelayPoolRelay, error) {
		var x RelayPoolRelay
		err := r.Scan(&x.ID, &x.Name, &x.Status, &x.Eligible)
		return x, err
	}); err != nil {
		return RelayPoolDetail{}, fmt.Errorf("relay pool relays: %w", err)
	}
	if withTenants {
		tenants, cut, err := collect(ctx, q, `
SELECT id::text, slug, name
  FROM workspaces WHERE relay_pool_id = $1
 ORDER BY slug, id LIMIT $2`, id, func(r pgx.Rows) (RelayPoolTenant, error) {
			var t RelayPoolTenant
			err := r.Scan(&t.ID, &t.Slug, &t.Name)
			return t, err
		})
		if err != nil {
			return RelayPoolDetail{}, fmt.Errorf("relay pool tenants: %w", err)
		}
		d.TenantList, d.TenantListCut = &tenants, cut
	}
	return d, nil
}

// TenantRelayPool is the relay pool section of tenant detail (D-38, D-39,
// D-40). Previous is set while a graceful migration keeps the old pool
// authorized. Failover is the failover pool, if any; UsingFailover reports
// that the primary is exhausted, so connectors get the failover pool's relays
// since FailedOverSince.
type TenantRelayPool struct {
	RelayPoolRef
	EligibleRelays  int64                    `json:"eligible_relays"`
	Previous        *TenantRelayPoolPrevious `json:"previous"`
	Failover        *TenantFailoverPool      `json:"failover"`
	UsingFailover   bool                     `json:"using_failover"`
	FailedOverSince *time.Time               `json:"failed_over_since"`
}

// TenantFailoverPool is a tenant's failover pool.
type TenantFailoverPool struct {
	RelayPoolRef
	EligibleRelays int64 `json:"eligible_relays"`
}

// TenantRelayPoolPrevious is the pool a tenant is migrating away from.
type TenantRelayPoolPrevious struct {
	RelayPoolRef
	Until time.Time `json:"until"`
}

// tenantRelayPool reads a tenant's pool assignment.
func tenantRelayPool(ctx context.Context, q Querier, workspaceID string) (TenantRelayPool, error) {
	var t TenantRelayPool
	var prevID, prevName, prevRegion *string
	var prevResidency *bool
	var prevUntil *time.Time
	var foID, foName, foRegion *string
	var foResidency *bool
	var foEligible *int64
	var exhaustedAt *time.Time
	err := q.QueryRow(ctx, `
SELECT p.id::text, p.name, p.region, p.residency,
       (SELECT count(*) FROM relays WHERE pool_id = p.id AND `+eligible.Where+`),
       pp.id::text, pp.name, pp.region, pp.residency, w.relay_pool_previous_until,
       fo.id::text, fo.name, fo.region, fo.residency,
       CASE WHEN fo.id IS NULL THEN NULL
            ELSE (SELECT count(*) FROM relays WHERE pool_id = fo.id AND `+eligible.Where+`) END,
       p.exhausted_at
  FROM workspaces w
  JOIN relay_pools p ON p.id = w.relay_pool_id
  LEFT JOIN relay_pools pp ON pp.id = w.relay_pool_previous_id AND w.relay_pool_previous_until > NOW()
  LEFT JOIN relay_pools fo ON fo.id = w.relay_failover_pool_id
 WHERE w.id = $1`, workspaceID).Scan(&t.ID, &t.Name, &t.Region, &t.Residency, &t.EligibleRelays,
		&prevID, &prevName, &prevRegion, &prevResidency, &prevUntil,
		&foID, &foName, &foRegion, &foResidency, &foEligible, &exhaustedAt)
	if err != nil {
		return TenantRelayPool{}, fmt.Errorf("tenant relay pool: %w", err)
	}
	if foID != nil {
		t.Failover = &TenantFailoverPool{
			RelayPoolRef:   RelayPoolRef{ID: *foID, Name: *foName, Region: foRegion, Residency: *foResidency},
			EligibleRelays: *foEligible,
		}
		// The same rule as relaypool.EffectivePool.
		if exhaustedAt != nil {
			t.UsingFailover = true
			t.FailedOverSince = utcPtr(exhaustedAt)
		}
	}
	if prevID != nil && prevUntil != nil {
		t.Previous = &TenantRelayPoolPrevious{
			RelayPoolRef: RelayPoolRef{ID: *prevID, Name: *prevName, Region: prevRegion, Residency: *prevResidency},
			Until:        prevUntil.UTC(),
		}
	}
	return t, nil
}
