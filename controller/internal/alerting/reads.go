package alerting

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yourorg/ztna/controller/internal/providerquery"
)

// ActiveAlert is one pending or firing alert (GET /provider/alerts).
type ActiveAlert struct {
	RuleID       string     `json:"rule_id"`
	RuleName     string     `json:"rule_name"`
	Kind         string     `json:"kind"`
	Scope        string     `json:"scope"`
	TenantID     *string    `json:"tenant_id"`
	TargetType   string     `json:"target_type"`
	TargetID     string     `json:"target_id"`
	TargetName   string     `json:"target_name"`
	Status       string     `json:"status"`
	Severity     string     `json:"severity"`
	Summary      string     `json:"summary"`
	PendingSince time.Time  `json:"pending_since"`
	FiringSince  *time.Time `json:"firing_since"`
}

// ReadFilter narrows alert reads. Kinds nil means every kind; a non-nil
// empty list matches nothing. TenantID "" means every tenant.
type ReadFilter struct {
	Scope    string
	TenantID string
	Status   string // active reads: pending | firing | "" (both)
	Kinds    []string
}

// RelayOpsKinds are the kinds relay-ops may read (D-40 extended, D-50).
func RelayOpsKinds() []string {
	var out []string
	for _, k := range Kinds() {
		if relayOpsVisible(k) {
			out = append(out, k)
		}
	}
	return out
}

func (f ReadFilter) valid() error {
	if f.Scope != "" && f.Scope != ScopePlatform && f.Scope != ScopeTenant {
		return fmt.Errorf("%w: scope", providerquery.ErrInvalidFilter)
	}
	if f.TenantID != "" && !providerquery.IsUUID(f.TenantID) {
		return fmt.Errorf("%w: tenant_id", providerquery.ErrInvalidFilter)
	}
	if f.Status != "" && f.Status != statusPending && f.Status != statusFiring {
		return fmt.Errorf("%w: status", providerquery.ErrInvalidFilter)
	}
	return nil
}

// ListActive returns the pending and firing alerts, firing first, most
// severe first, oldest first. Tenant-scoped reads always filter by
// tenant_id (D-50).
func (s *Store) ListActive(ctx context.Context, f ReadFilter) ([]ActiveAlert, error) {
	if err := f.valid(); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
SELECT s.rule_id::text, r.name, r.kind, s.scope, s.tenant_id::text, s.target_type, s.target_id, s.target_name,
       s.status, s.severity, s.summary, s.pending_since, s.firing_since
  FROM alert_states s JOIN alert_rules r ON r.id = s.rule_id
 WHERE ($1 = '' OR s.scope = $1)
   AND ($2 = '' OR s.tenant_id = NULLIF($2, '')::uuid)
   AND ($3 = '' OR s.status = $3)
   AND ($4::text[] IS NULL OR r.kind = ANY($4))
 ORDER BY (s.status = 'firing') DESC,
          CASE s.severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,
          s.pending_since, s.rule_id, s.target_type, s.target_id
 LIMIT 1000`, f.Scope, f.TenantID, f.Status, f.Kinds)
	if err != nil {
		return nil, fmt.Errorf("list active alerts: %w", err)
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (ActiveAlert, error) {
		var a ActiveAlert
		err := r.Scan(&a.RuleID, &a.RuleName, &a.Kind, &a.Scope, &a.TenantID, &a.TargetType, &a.TargetID,
			&a.TargetName, &a.Status, &a.Severity, &a.Summary, &a.PendingSince, &a.FiringSince)
		a.PendingSince = a.PendingSince.UTC()
		if a.FiringSince != nil {
			t := a.FiringSince.UTC()
			a.FiringSince = &t
		}
		return a, err
	})
}

// ListEvents returns one page of the alert history, newest first.
func (s *Store) ListEvents(ctx context.Context, f ReadFilter, cursor string, limit int) (providerquery.Page[Event], error) {
	if err := f.valid(); err != nil {
		return providerquery.Page[Event]{}, err
	}
	if f.Status != "" {
		return providerquery.Page[Event]{}, fmt.Errorf("%w: status", providerquery.ErrInvalidFilter)
	}
	cur, err := providerquery.DecodeCursor(cursor)
	if err != nil {
		return providerquery.Page[Event]{}, err
	}
	limit = providerquery.ClampLimit(limit)
	var ct, cid any
	if cur != nil {
		ct, cid = cur.CreatedAt, cur.ID
	}
	rows, err := s.pool.Query(ctx, `
SELECT id::text, rule_id::text, kind, scope, tenant_id::text, target_type, target_id, target_name,
       transition, status, severity, summary, started_at, occurred_at
  FROM alert_events
 WHERE ($1 = '' OR scope = $1)
   AND ($2 = '' OR tenant_id = NULLIF($2, '')::uuid)
   AND ($3::text[] IS NULL OR kind = ANY($3))
   AND ($4::timestamptz IS NULL OR (occurred_at, id) < ($4, $5::uuid))
 ORDER BY occurred_at DESC, id DESC
 LIMIT $6`, f.Scope, f.TenantID, f.Kinds, ct, cid, limit+1)
	if err != nil {
		return providerquery.Page[Event]{}, fmt.Errorf("list alert events: %w", err)
	}
	items, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Event, error) {
		var e Event
		err := r.Scan(&e.ID, &e.RuleID, &e.Kind, &e.Scope, &e.TenantID, &e.TargetType, &e.TargetID, &e.TargetName,
			&e.Transition, &e.Status, &e.Severity, &e.Summary, &e.StartedAt, &e.OccurredAt)
		e.StartedAt, e.OccurredAt = e.StartedAt.UTC(), e.OccurredAt.UTC()
		return e, err
	})
	if err != nil {
		return providerquery.Page[Event]{}, fmt.Errorf("list alert events: %w", err)
	}
	page := providerquery.Page[Event]{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1]
		next := providerquery.EncodeCursor(last.OccurredAt, last.ID)
		page.NextCursor = &next
	}
	return page, nil
}
