package providerquery

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// Incident timeline (Sprint 24 T1, F27). One read-only, time-ordered list
// merging two stored sources; it adds no data of its own:
//
//   - alert_events (A1): fired, renotified and resolved transitions. These
//     are also the only stored record of relay and pool state changes that
//     no operator made: relay status (active/inactive) is kept as current
//     state only, so a relay going offline appears here as its
//     relay.offline alert (after the rule's "for" delay), never as an
//     invented transition.
//   - provider_audit_logs (F17): operator actions on relays, pools, tenants
//     and alert rules/channels, and the system-written pool failover and
//     failback rows (relay_pool.failover / relay_pool.failback).
//
// Visibility is decided by the caller (the alerting handler): relay-ops get
// relay and pool alert rows, and the audit rows of relay and pool operations
// only (an action allowlist, actor and reason, no raw details; D-55). The
// full audit log stays super-admin (audit.view). This file only applies the
// filter it is given.

// TimelineAuditTargets are the provider audit target types the timeline
// shows; operator-account and auth rows (provider_user, …) are not incident
// context and stay on the audit page. Read rows (tenant.read: every tenant
// detail view is audited, OQ-1) are left out too: they are not events.
var TimelineAuditTargets = []string{"relay", "relay_pool", "tenant", "workspace", "alert_rule", "alert_channel"}

// Timeline sources.
const (
	TimelineSourceAlert = "alert"
	TimelineSourceAudit = "audit"
)

// TimelineItem is one row of GET /provider/timeline. Alert fields are set on
// alert rows and audit fields on audit rows; the rest is common.
type TimelineItem struct {
	ID         string    `json:"id"`
	Source     string    `json:"source"` // alert | audit
	At         time.Time `json:"at"`
	TargetType string    `json:"target_type"`
	TargetID   string    `json:"target_id"`
	TargetName string    `json:"target_name"`
	TenantID   *string   `json:"tenant_id"`

	// Alert rows.
	Kind       *string    `json:"kind,omitempty"`
	Transition *string    `json:"transition,omitempty"`
	Severity   *string    `json:"severity,omitempty"`
	Summary    *string    `json:"summary,omitempty"`
	StartedAt  *time.Time `json:"started_at,omitempty"`

	// Audit rows.
	Action  *string         `json:"action,omitempty"`
	Actor   *string         `json:"actor,omitempty"`
	Reason  *string         `json:"reason,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

// TimelineFilter narrows QueryTimeline. At most one of RelayID, PoolID and
// TenantID is set (all UUIDs). AlertKinds nil means every kind; a non-nil
// empty list matches no alert. AlertScope "" means both scopes.
type TimelineFilter struct {
	IncludeAlerts bool
	IncludeAudit  bool
	AlertScope    string
	AlertKinds    []string
	// AuditActions nil means every action (of TimelineAuditTargets); a
	// non-nil list is an allowlist (relay-ops, D-55).
	AuditActions []string
	// OmitAuditDetails leaves details out of audit rows (relay-ops: actor,
	// reason and target only).
	OmitAuditDetails bool

	RelayID  string
	PoolID   string // the pool's rows and those of the relays now in it
	TenantID string

	Since  *time.Time // inclusive
	Until  *time.Time // exclusive
	Limit  int
	Cursor string
}

func (f TimelineFilter) validate() error {
	set := 0
	for name, v := range map[string]string{"relay_id": f.RelayID, "pool_id": f.PoolID, "tenant_id": f.TenantID} {
		if v == "" {
			continue
		}
		if !IsUUID(v) {
			return fmt.Errorf("%w: %s", ErrInvalidFilter, name)
		}
		set++
	}
	if set > 1 {
		return fmt.Errorf("%w: only one of relay_id, pool_id, tenant_id", ErrInvalidFilter)
	}
	if f.AlertScope != "" && f.AlertScope != "platform" && f.AlertScope != "tenant" {
		return fmt.Errorf("%w: scope", ErrInvalidFilter)
	}
	if f.Since != nil && f.Until != nil && f.Since.After(*f.Until) {
		return fmt.Errorf("%w: since is after until", ErrInvalidFilter)
	}
	return nil
}

// QueryTimeline returns one page of the merged timeline, newest first by
// (time, id), in one statement. Both sources' ids are random UUIDs, so the
// (time, id) keyset is unique across the union.
func QueryTimeline(ctx context.Context, q Querier, f TimelineFilter) (Page[TimelineItem], error) {
	if err := f.validate(); err != nil {
		return Page[TimelineItem]{}, err
	}
	cur, err := DecodeCursor(f.Cursor)
	if err != nil {
		return Page[TimelineItem]{}, err
	}
	limit := ClampLimit(f.Limit)
	ct, cid := keysetArgs(cur)

	// $1 alerts?  $2 audit?  $3 scope  $4 kinds  $5 relay  $6 pool  $7 tenant
	// $8 since  $9 until  $10/$11 cursor  $12 limit  $13 audit target types
	// $14 audit action allowlist  $15 omit audit details
	rows, err := q.Query(ctx, `
WITH pool_relays AS (
    SELECT id::text AS id FROM relays WHERE $6 <> '' AND pool_id = NULLIF($6, '')::uuid
)
SELECT source, id, at, target_type, target_id, target_name, tenant_id,
       kind, transition, severity, summary, started_at,
       action, actor, reason, details
  FROM (
    (SELECT 'alert' AS source, e.id::text AS id, e.occurred_at AS at, e.target_type, e.target_id, e.target_name,
            e.tenant_id::text AS tenant_id,
            e.kind, e.transition, e.severity, e.summary, e.started_at,
            NULL::text AS action, NULL::text AS actor, NULL::text AS reason, NULL::jsonb AS details
       FROM alert_events e
      WHERE $1
        AND ($3 = '' OR e.scope = $3)
        AND ($4::text[] IS NULL OR e.kind = ANY($4))
        AND ($5 = '' OR (e.target_type = 'relay' AND e.target_id = $5))
        AND ($6 = '' OR (e.target_type = 'pool' AND e.target_id = $6)
                     OR (e.target_type = 'relay' AND e.target_id IN (SELECT id FROM pool_relays)))
        AND ($7 = '' OR e.tenant_id = NULLIF($7, '')::uuid)
        AND ($8::timestamptz IS NULL OR e.occurred_at >= $8)
        AND ($9::timestamptz IS NULL OR e.occurred_at < $9)
        AND ($10::timestamptz IS NULL OR (e.occurred_at, e.id) < ($10, $11::uuid))
      ORDER BY e.occurred_at DESC, e.id DESC
      LIMIT $12)
    UNION ALL
    (SELECT 'audit', a.id::text, a.created_at, a.target_type, a.target_id,
            -- The name as recorded, else the target's current name (several
            -- relay rows, e.g. relay.capacity.set, record no name).
            COALESCE(CASE WHEN a.target_type = 'relay_pool' THEN a.details->>'pool_name' END,
                     a.details->>'name', a.details->'after'->>'name', a.details->'before'->>'name',
                     CASE a.target_type
                         WHEN 'relay'         THEN (SELECT r.name FROM relays r WHERE r.id::text = a.target_id)
                         WHEN 'relay_pool'    THEN (SELECT p.name FROM relay_pools p WHERE p.id::text = a.target_id)
                         WHEN 'tenant'        THEN (SELECT w.name FROM workspaces w WHERE w.id::text = a.target_id)
                         WHEN 'workspace'     THEN (SELECT w.name FROM workspaces w WHERE w.id::text = a.target_id)
                         WHEN 'alert_rule'    THEN (SELECT ar.name FROM alert_rules ar WHERE ar.id::text = a.target_id)
                         WHEN 'alert_channel' THEN (SELECT ac.name FROM alert_channels ac WHERE ac.id::text = a.target_id)
                     END, ''),
            CASE WHEN a.target_type IN ('tenant', 'workspace') THEN a.target_id END,
            NULL, NULL, NULL, NULL, NULL,
            a.action, a.provider_email, a.details->>'reason', CASE WHEN $15 THEN NULL ELSE a.details END
       FROM provider_audit_logs a
      WHERE $2
        AND a.target_type = ANY($13)
        AND a.action NOT LIKE '%.read'
        AND ($14::text[] IS NULL OR a.action = ANY($14))
        AND ($5 = '' OR (a.target_type = 'relay' AND a.target_id = $5))
        AND ($6 = '' OR (a.target_type = 'relay_pool' AND a.target_id = $6)
                     OR (a.target_type = 'relay' AND a.target_id IN (SELECT id FROM pool_relays)))
        AND ($7 = '' OR (a.target_type IN ('tenant', 'workspace') AND a.target_id = $7))
        AND ($8::timestamptz IS NULL OR a.created_at >= $8)
        AND ($9::timestamptz IS NULL OR a.created_at < $9)
        AND ($10::timestamptz IS NULL OR (a.created_at, a.id) < ($10, $11::uuid))
      ORDER BY a.created_at DESC, a.id DESC
      LIMIT $12)
  ) u
 ORDER BY at DESC, id DESC
 LIMIT $12`,
		f.IncludeAlerts, f.IncludeAudit, f.AlertScope, f.AlertKinds, f.RelayID, f.PoolID, f.TenantID,
		optTime(f.Since), optTime(f.Until), ct, cid, limit+1, TimelineAuditTargets, f.AuditActions, f.OmitAuditDetails)
	if err != nil {
		return Page[TimelineItem]{}, fmt.Errorf("query timeline: %w", err)
	}
	defer rows.Close()

	items := make([]TimelineItem, 0, limit)
	for rows.Next() {
		var it TimelineItem
		var details []byte
		if err := rows.Scan(&it.Source, &it.ID, &it.At, &it.TargetType, &it.TargetID, &it.TargetName, &it.TenantID,
			&it.Kind, &it.Transition, &it.Severity, &it.Summary, &it.StartedAt,
			&it.Action, &it.Actor, &it.Reason, &details); err != nil {
			return Page[TimelineItem]{}, fmt.Errorf("scan timeline: %w", err)
		}
		it.At = it.At.UTC()
		it.StartedAt = utcPtr(it.StartedAt)
		if details != nil {
			it.Details = json.RawMessage(details)
		}
		items = append(items, it)
	}
	if err := rows.Err(); err != nil {
		return Page[TimelineItem]{}, fmt.Errorf("query timeline: %w", err)
	}
	page := Page[TimelineItem]{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1]
		next := EncodeCursor(last.At, last.ID)
		page.NextCursor = &next
	}
	return page, nil
}
