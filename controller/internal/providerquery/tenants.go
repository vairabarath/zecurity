package providerquery

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Tenant reads (Phase R, D-15 cross-tenant reads; super-admin only).
//
// OQ-2 (locked): no tenant user rows and no identity data. Users appear only
// as counts by status; no query here selects email, name, provider_sub,
// role, last_login_at, a device's user_id or any other identity column.
// Detail is infrastructure only: remote networks, connectors and shields
// with the planned fields, never public_ip, lan_addr, lan_ip,
// interface_addr, hostname or SPIFFE ids, and never key material.

// TenantStatuses is the workspaces.status vocabulary (CHECK constraint, 001).
var TenantStatuses = []string{"provisioning", "active", "suspended", "deleted"}

// AgentCounts counts connectors or shields by status.
type AgentCounts struct {
	Pending      int64 `json:"pending"`
	Active       int64 `json:"active"`
	Disconnected int64 `json:"disconnected"`
	Revoked      int64 `json:"revoked"`
}

// UserCounts counts the tenant's users by status. Counts only (OQ-2).
type UserCounts struct {
	Active    int64 `json:"active"`
	Suspended int64 `json:"suspended"`
	Locked    int64 `json:"locked"`
	Deleted   int64 `json:"deleted"`
}

// DeviceCounts counts client devices. A device with revoked_at set counts as
// revoked whatever its status; the others are counted by status.
type DeviceCounts struct {
	Active           int64 `json:"active"`
	ReEnrollRequired int64 `json:"re_enroll_required"`
	RenewPending     int64 `json:"renew_pending"`
	Revoked          int64 `json:"revoked"`
}

// TenantCounts is the per-tenant count block shared by list and detail.
type TenantCounts struct {
	Connectors     AgentCounts  `json:"connectors"`
	Shields        AgentCounts  `json:"shields"`
	RemoteNetworks int64        `json:"remote_networks"` // status = 'active'
	Users          UserCounts   `json:"users"`
	ClientDevices  DeviceCounts `json:"client_devices"`
}

// TenantSummary is one row of GET /provider/tenants: the whole allowed
// response surface for the list.
type TenantSummary struct {
	ID          string       `json:"id"`
	Slug        string       `json:"slug"`
	Name        string       `json:"name"`
	Status      string       `json:"status"`
	TrustDomain string       `json:"trust_domain"`
	CreatedAt   time.Time    `json:"created_at"`
	CANotAfter  *time.Time   `json:"ca_not_after"`
	Counts      TenantCounts `json:"counts"`
}

// TenantRemoteNetwork is one remote network in tenant detail.
type TenantRemoteNetwork struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
}

// TenantConnector is one connector in tenant detail.
type TenantConnector struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Status          string     `json:"status"`
	RemoteNetworkID string     `json:"remote_network_id"`
	Version         *string    `json:"version"`
	CertNotAfter    *time.Time `json:"cert_not_after"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at"`
	RevokedAt       *time.Time `json:"revoked_at"`
}

// TenantShield is one shield in tenant detail.
type TenantShield struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Status          string     `json:"status"`
	ConnectorID     string     `json:"connector_id"`
	RemoteNetworkID string     `json:"remote_network_id"`
	CertNotAfter    *time.Time `json:"cert_not_after"`
	LastHeartbeatAt *time.Time `json:"last_heartbeat_at"`
}

// TenantDetail is GET /provider/tenants/{id}.
type TenantDetail struct {
	TenantSummary
	RemoteNetworks          []TenantRemoteNetwork `json:"remote_networks"`
	RemoteNetworksTruncated bool                  `json:"remote_networks_truncated"`
	Connectors              []TenantConnector     `json:"connectors"`
	ConnectorsTruncated     bool                  `json:"connectors_truncated"`
	Shields                 []TenantShield        `json:"shields"`
	ShieldsTruncated        bool                  `json:"shields_truncated"`
}

// TenantFilter narrows ListTenants. Status "" means every status except
// deleted; "deleted" must be asked for explicitly.
type TenantFilter struct {
	Status string
	Limit  int
	Cursor string
}

// tenantSelect returns the tenant columns plus every count, computed with one
// LEFT JOIN LATERAL aggregate per counted table, so a whole page of tenants
// and all their counts come back in a single statement (no N+1). The users
// lateral reads only users.status. Column order matches scanTenantSummary.
const tenantSelect = `
SELECT w.id::text, w.slug, w.name, w.status, w.trust_domain, w.created_at, k.not_after,
       cc.pending, cc.active, cc.disconnected, cc.revoked,
       sc.pending, sc.active, sc.disconnected, sc.revoked,
       rn.active,
       uc.active, uc.suspended, uc.locked, uc.deleted,
       dc.active, dc.re_enroll_required, dc.renew_pending, dc.revoked
  FROM workspaces w
  LEFT JOIN workspace_ca_keys k ON k.tenant_id = w.id
  LEFT JOIN LATERAL (
       SELECT count(*) FILTER (WHERE status = 'pending')      AS pending,
              count(*) FILTER (WHERE status = 'active')       AS active,
              count(*) FILTER (WHERE status = 'disconnected') AS disconnected,
              count(*) FILTER (WHERE status = 'revoked')      AS revoked
         FROM connectors WHERE tenant_id = w.id) cc ON true
  LEFT JOIN LATERAL (
       SELECT count(*) FILTER (WHERE status = 'pending')      AS pending,
              count(*) FILTER (WHERE status = 'active')       AS active,
              count(*) FILTER (WHERE status = 'disconnected') AS disconnected,
              count(*) FILTER (WHERE status = 'revoked')      AS revoked
         FROM shields WHERE tenant_id = w.id) sc ON true
  LEFT JOIN LATERAL (
       SELECT count(*) FILTER (WHERE status = 'active') AS active
         FROM remote_networks WHERE tenant_id = w.id) rn ON true
  LEFT JOIN LATERAL (
       SELECT count(*) FILTER (WHERE status = 'active')    AS active,
              count(*) FILTER (WHERE status = 'suspended') AS suspended,
              count(*) FILTER (WHERE status = 'locked')    AS locked,
              count(*) FILTER (WHERE status = 'deleted')   AS deleted
         FROM users WHERE tenant_id = w.id) uc ON true
  LEFT JOIN LATERAL (
       SELECT count(*) FILTER (WHERE revoked_at IS NULL AND status = 'active')             AS active,
              count(*) FILTER (WHERE revoked_at IS NULL AND status = 're_enroll_required') AS re_enroll_required,
              count(*) FILTER (WHERE revoked_at IS NULL AND status = 'renew_pending')      AS renew_pending,
              count(*) FILTER (WHERE revoked_at IS NOT NULL)                               AS revoked
         FROM client_devices WHERE workspace_id = w.id) dc ON true`

func scanTenantSummary(row pgx.Row) (TenantSummary, error) {
	var s TenantSummary
	c := &s.Counts
	if err := row.Scan(
		&s.ID, &s.Slug, &s.Name, &s.Status, &s.TrustDomain, &s.CreatedAt, &s.CANotAfter,
		&c.Connectors.Pending, &c.Connectors.Active, &c.Connectors.Disconnected, &c.Connectors.Revoked,
		&c.Shields.Pending, &c.Shields.Active, &c.Shields.Disconnected, &c.Shields.Revoked,
		&c.RemoteNetworks,
		&c.Users.Active, &c.Users.Suspended, &c.Users.Locked, &c.Users.Deleted,
		&c.ClientDevices.Active, &c.ClientDevices.ReEnrollRequired, &c.ClientDevices.RenewPending, &c.ClientDevices.Revoked,
	); err != nil {
		return TenantSummary{}, err
	}
	s.CreatedAt = s.CreatedAt.UTC()
	s.CANotAfter = utcPtr(s.CANotAfter)
	return s, nil
}

func validTenantStatus(s string) bool {
	for _, v := range TenantStatuses {
		if s == v {
			return true
		}
	}
	return false
}

// ListTenants returns one page of tenants with their counts, newest first,
// in one statement.
func ListTenants(ctx context.Context, q Querier, f TenantFilter) (Page[TenantSummary], error) {
	if f.Status != "" && !validTenantStatus(f.Status) {
		return Page[TenantSummary]{}, fmt.Errorf("%w: status %q", ErrInvalidFilter, f.Status)
	}
	cur, err := DecodeCursor(f.Cursor)
	if err != nil {
		return Page[TenantSummary]{}, err
	}
	limit := ClampLimit(f.Limit)
	var status any
	if f.Status != "" {
		status = f.Status
	}
	ct, cid := keysetArgs(cur)

	rows, err := q.Query(ctx, tenantSelect+`
 WHERE (($1::text IS NULL AND w.status <> 'deleted') OR w.status = $1)
   AND ($2::timestamptz IS NULL OR (w.created_at, w.id) < ($2, $3::uuid))
 ORDER BY w.created_at DESC, w.id DESC
 LIMIT $4`, status, ct, cid, limit+1)
	if err != nil {
		return Page[TenantSummary]{}, fmt.Errorf("list tenants: %w", err)
	}
	defer rows.Close()

	items := make([]TenantSummary, 0, limit)
	for rows.Next() {
		s, err := scanTenantSummary(rows)
		if err != nil {
			return Page[TenantSummary]{}, fmt.Errorf("scan tenant: %w", err)
		}
		items = append(items, s)
	}
	if err := rows.Err(); err != nil {
		return Page[TenantSummary]{}, fmt.Errorf("list tenants: %w", err)
	}
	page := Page[TenantSummary]{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1]
		next := EncodeCursor(last.CreatedAt, last.ID)
		page.NextCursor = &next
	}
	return page, nil
}

// GetTenant returns one tenant with its counts and its remote networks,
// connectors and shields (newest first, each capped at DetailCap and flagged
// when cut). Deleted tenants are readable by id. Four statements; pass a
// transaction to read them from one snapshot.
func GetTenant(ctx context.Context, q Querier, id string) (TenantDetail, error) {
	if !IsUUID(id) {
		return TenantDetail{}, ErrInvalidID
	}
	s, err := scanTenantSummary(q.QueryRow(ctx, tenantSelect+`
 WHERE w.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantDetail{}, ErrNotFound
	}
	if err != nil {
		return TenantDetail{}, fmt.Errorf("get tenant: %w", err)
	}
	d := TenantDetail{TenantSummary: s}

	if d.RemoteNetworks, d.RemoteNetworksTruncated, err = collect(ctx, q, `
SELECT id::text, name, status
  FROM remote_networks WHERE tenant_id = $1
 ORDER BY created_at DESC, id DESC LIMIT $2`, id, func(r pgx.Rows) (TenantRemoteNetwork, error) {
		var n TenantRemoteNetwork
		err := r.Scan(&n.ID, &n.Name, &n.Status)
		return n, err
	}); err != nil {
		return TenantDetail{}, fmt.Errorf("tenant remote networks: %w", err)
	}

	if d.Connectors, d.ConnectorsTruncated, err = collect(ctx, q, `
SELECT id::text, name, status, remote_network_id::text, version, cert_not_after, last_heartbeat_at, revoked_at
  FROM connectors WHERE tenant_id = $1
 ORDER BY created_at DESC, id DESC LIMIT $2`, id, func(r pgx.Rows) (TenantConnector, error) {
		var c TenantConnector
		err := r.Scan(&c.ID, &c.Name, &c.Status, &c.RemoteNetworkID, &c.Version, &c.CertNotAfter, &c.LastHeartbeatAt, &c.RevokedAt)
		c.CertNotAfter, c.LastHeartbeatAt, c.RevokedAt = utcPtr(c.CertNotAfter), utcPtr(c.LastHeartbeatAt), utcPtr(c.RevokedAt)
		return c, err
	}); err != nil {
		return TenantDetail{}, fmt.Errorf("tenant connectors: %w", err)
	}

	if d.Shields, d.ShieldsTruncated, err = collect(ctx, q, `
SELECT id::text, name, status, connector_id::text, remote_network_id::text, cert_not_after, last_heartbeat_at
  FROM shields WHERE tenant_id = $1
 ORDER BY created_at DESC, id DESC LIMIT $2`, id, func(r pgx.Rows) (TenantShield, error) {
		var sh TenantShield
		err := r.Scan(&sh.ID, &sh.Name, &sh.Status, &sh.ConnectorID, &sh.RemoteNetworkID, &sh.CertNotAfter, &sh.LastHeartbeatAt)
		sh.CertNotAfter, sh.LastHeartbeatAt = utcPtr(sh.CertNotAfter), utcPtr(sh.LastHeartbeatAt)
		return sh, err
	}); err != nil {
		return TenantDetail{}, fmt.Errorf("tenant shields: %w", err)
	}
	return d, nil
}

// collect runs a capped detail query (LIMIT DetailCap+1) and reports whether
// the result was cut. It always returns a non-nil slice, so JSON shows [].
func collect[T any](ctx context.Context, q Querier, sql, id string, scan func(pgx.Rows) (T, error)) ([]T, bool, error) {
	rows, err := q.Query(ctx, sql, id, DetailCap+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := make([]T, 0)
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, false, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if len(out) > DetailCap {
		return out[:DetailCap], true, nil
	}
	return out, false, nil
}
