package providerquery

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Sprint 25 U2 (F33; Decision Record D-57, D-59, D-60): reads of the usage
// ledger (071_usage, written by the U1 collector). Usage is tenant data:
// the handlers gate every read with tenant.usage.read (super-admin only).
//
// Completeness (D-59) comes from usage_collector_minutes, the minutes the
// collector counted (fleet-wide): a bucket is complete when every one of its
// minutes was collected. The current bucket, and any bucket before metering
// started or across a controller outage, is incomplete. A complete bucket
// without a usage row means 0; an incomplete one means "at least this".

// Usage meters (D-57), as stored by the collector.
const (
	UsageMeterRelayBytesToClients    = "relay_bytes_to_clients"
	UsageMeterRelayBytesToConnectors = "relay_bytes_to_connectors"
	UsageMeterSessionMinutes         = "session_minutes"
	UsageMeterConnectorMinutes       = "connector_minutes"
	UsageMeterShieldMinutes          = "shield_minutes"
	// UsageMeterActiveUsers is monthly only (distinct users in the month).
	UsageMeterActiveUsers = "active_users"
)

// UsageHourlyMeters are the meters stored per hour, in display order.
var UsageHourlyMeters = []string{
	UsageMeterRelayBytesToClients, UsageMeterRelayBytesToConnectors,
	UsageMeterSessionMinutes, UsageMeterConnectorMinutes, UsageMeterShieldMinutes,
}

// IsUsageMeter reports whether m is a meter (hourly or active users).
func IsUsageMeter(m string) bool {
	if m == UsageMeterActiveUsers {
		return true
	}
	for _, h := range UsageHourlyMeters {
		if h == m {
			return true
		}
	}
	return false
}

// Usage granularities and their range limits.
const (
	UsageHour  = "hour"
	UsageDay   = "day"
	UsageMonth = "month"

	maxUsageHours  = 31 * 24
	maxUsageDays   = 400
	maxUsageMonths = 25
)

// UsageQueryError is a rejected read; Code is the API error code.
type UsageQueryError struct{ Code string }

func (e *UsageQueryError) Error() string { return "providerquery: usage: " + e.Code }

// UsageTenant names the tenant a usage read is about: from the workspace
// while it exists, else from the snapshot kept after its purge (D-58).
type UsageTenant struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Slug     string  `json:"slug"`
	PlanName *string `json:"plan_name"`
	Purged   bool    `json:"purged"`
}

// UsageBucket is one hour, day or month of a tenant's usage.
type UsageBucket struct {
	Start time.Time `json:"start"`
	// Complete: every minute of the bucket was collected (D-59).
	Complete         bool             `json:"complete"`
	CollectedMinutes int              `json:"collected_minutes"`
	Minutes          int              `json:"minutes"`
	Meters           map[string]int64 `json:"meters"`
	// ActiveUsers: month buckets only.
	ActiveUsers *int64 `json:"active_users,omitempty"`
}

// TenantUsageData is the response of GET /provider/usage.
type TenantUsageData struct {
	Tenant      UsageTenant   `json:"tenant"`
	Granularity string        `json:"granularity"`
	From        time.Time     `json:"from"`
	To          time.Time     `json:"to"`
	Buckets     []UsageBucket `json:"buckets"` // ascending, every bucket of [from, to)
}

// UsageWindow is a validated tenant usage read.
type UsageWindow struct {
	Granularity string
	From, To    time.Time // aligned to bucket starts, UTC
}

// NewUsageWindow validates [from, to) at a granularity ("" = day). from is
// aligned down and to up to whole UTC buckets. Codes: invalid_granularity,
// invalid_range.
func NewUsageWindow(granularity string, from, to time.Time) (UsageWindow, error) {
	if granularity == "" {
		granularity = UsageDay
	}
	w := UsageWindow{Granularity: granularity}
	switch granularity {
	case UsageHour, UsageDay, UsageMonth:
	default:
		return w, &UsageQueryError{"invalid_granularity"}
	}
	w.From = bucketStart(granularity, from.UTC())
	w.To = bucketStart(granularity, to.UTC())
	if w.To.Before(to.UTC()) {
		w.To = nextBucket(granularity, w.To)
	}
	if !w.From.Before(w.To) {
		return w, &UsageQueryError{"invalid_range"}
	}
	n, limit := 0, 0
	for t := w.From; t.Before(w.To); t = nextBucket(granularity, t) {
		n++
		if n > maxUsageMonths*31*24 { // a guard, never reached with the limits below
			break
		}
	}
	switch granularity {
	case UsageHour:
		limit = maxUsageHours
	case UsageDay:
		limit = maxUsageDays
	case UsageMonth:
		limit = maxUsageMonths
	}
	if n > limit {
		return w, &UsageQueryError{"invalid_range"}
	}
	return w, nil
}

func bucketStart(g string, t time.Time) time.Time {
	t = t.UTC()
	switch g {
	case UsageHour:
		return t.Truncate(time.Hour)
	case UsageDay:
		return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	default:
		return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
}

func nextBucket(g string, t time.Time) time.Time {
	switch g {
	case UsageHour:
		return t.Add(time.Hour)
	case UsageDay:
		return t.AddDate(0, 0, 1)
	default:
		return t.AddDate(0, 1, 0)
	}
}

// ReadUsageTenant names a tenant for a usage read: the workspace if it
// exists, else its usage snapshot (purged). ErrNotFound when neither.
func ReadUsageTenant(ctx context.Context, q Querier, tenantID string) (UsageTenant, error) {
	t := UsageTenant{ID: tenantID}
	err := q.QueryRow(ctx, `
SELECT w.name, w.slug, p.name, false
  FROM workspaces w LEFT JOIN plans p ON p.id = w.plan_id
 WHERE w.id = $1
UNION ALL
SELECT u.name, u.slug, u.plan_name, true
  FROM usage_tenants u
 WHERE u.tenant_id = $1 AND NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.id = $1)
 LIMIT 1`, tenantID).Scan(&t.Name, &t.Slug, &t.PlanName, &t.Purged)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	if err != nil {
		return t, fmt.Errorf("usage tenant: %w", err)
	}
	return t, nil
}

// collectedMinutesByHour counts the collected minutes per UTC hour in [from, to).
func collectedMinutesByHour(ctx context.Context, q Querier, from, to time.Time) (map[time.Time]int, error) {
	rows, err := q.Query(ctx, `
SELECT to_timestamp(floor(extract(epoch FROM minute) / 3600) * 3600) AS hour, count(*)
  FROM usage_collector_minutes
 WHERE minute >= $1 AND minute < $2
 GROUP BY 1`, from, to)
	if err != nil {
		return nil, fmt.Errorf("usage completeness: %w", err)
	}
	defer rows.Close()
	out := map[time.Time]int{}
	for rows.Next() {
		var h time.Time
		var n int
		if err := rows.Scan(&h, &n); err != nil {
			return nil, fmt.Errorf("usage completeness: %w", err)
		}
		out[h.UTC()] = n
	}
	return out, rows.Err()
}

// ReadTenantUsage returns every bucket of w for a tenant (t from
// ReadUsageTenant), with completeness, ascending.
func ReadTenantUsage(ctx context.Context, q Querier, t UsageTenant, w UsageWindow) (TenantUsageData, error) {
	out := TenantUsageData{Tenant: t, Granularity: w.Granularity, From: w.From, To: w.To}
	index := map[time.Time]int{}
	for s := w.From; s.Before(w.To); s = nextBucket(w.Granularity, s) {
		index[s] = len(out.Buckets)
		out.Buckets = append(out.Buckets, UsageBucket{
			Start: s, Minutes: int(nextBucket(w.Granularity, s).Sub(s) / time.Minute), Meters: map[string]int64{},
		})
	}
	for i := range out.Buckets {
		for _, m := range UsageHourlyMeters {
			out.Buckets[i].Meters[m] = 0
		}
	}
	rows, err := q.Query(ctx, `SELECT meter, hour, quantity FROM usage_hourly
		 WHERE tenant_id = $1 AND hour >= $2 AND hour < $3`, t.ID, w.From, w.To)
	if err != nil {
		return out, fmt.Errorf("usage: %w", err)
	}
	for rows.Next() {
		var meter string
		var hour time.Time
		var qty int64
		if err := rows.Scan(&meter, &hour, &qty); err != nil {
			rows.Close()
			return out, fmt.Errorf("usage: scan: %w", err)
		}
		if i, ok := index[bucketStart(w.Granularity, hour)]; ok {
			out.Buckets[i].Meters[meter] += qty
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("usage: %w", err)
	}
	collected, err := collectedMinutesByHour(ctx, q, w.From, w.To)
	if err != nil {
		return out, err
	}
	for h, n := range collected {
		if i, ok := index[bucketStart(w.Granularity, h)]; ok {
			out.Buckets[i].CollectedMinutes += n
		}
	}
	for i := range out.Buckets {
		out.Buckets[i].Complete = out.Buckets[i].CollectedMinutes == out.Buckets[i].Minutes
	}
	if w.Granularity == UsageMonth {
		users, err := activeUsersByMonth(ctx, q, t.ID, w.From, w.To)
		if err != nil {
			return out, err
		}
		for i := range out.Buckets {
			n := users[out.Buckets[i].Start]
			out.Buckets[i].ActiveUsers = &n
		}
	}
	return out, nil
}

// activeUsersByMonth: distinct users per month (rows for a live tenant,
// the kept counts for a purged one; D-58).
func activeUsersByMonth(ctx context.Context, q Querier, tenantID string, from, to time.Time) (map[time.Time]int64, error) {
	rows, err := q.Query(ctx, `
SELECT month, sum(n)::bigint FROM (
    SELECT month, count(*) AS n FROM usage_active_users
     WHERE tenant_id = $1 AND month >= $2::date AND month < $3::date GROUP BY month
    UNION ALL
    SELECT month, users FROM usage_active_user_counts
     WHERE tenant_id = $1 AND month >= $2::date AND month < $3::date
) x GROUP BY month`, tenantID, from, to)
	if err != nil {
		return nil, fmt.Errorf("usage active users: %w", err)
	}
	defer rows.Close()
	out := map[time.Time]int64{}
	for rows.Next() {
		var m time.Time
		var n int64
		if err := rows.Scan(&m, &n); err != nil {
			return nil, fmt.Errorf("usage active users: %w", err)
		}
		out[time.Date(m.Year(), m.Month(), 1, 0, 0, 0, 0, time.UTC)] = n
	}
	return out, rows.Err()
}

// TenantMonthUsage is one tenant's usage in one month (fleet list, and the
// U4 usage alert's month-to-date reader).
type TenantMonthUsage struct {
	Tenant      UsageTenant      `json:"tenant"`
	Meters      map[string]int64 `json:"meters"`
	ActiveUsers int64            `json:"active_users"`
}

// MonthUsage is every tenant's usage in one UTC month, with the month's
// completeness. Tenants listed: every counted tenant (zeros included) and
// every tenant with usage in the month (purged ones named by their snapshot).
type MonthUsage struct {
	Month            time.Time          `json:"month"`
	Complete         bool               `json:"complete"`
	CollectedMinutes int                `json:"collected_minutes"`
	Minutes          int                `json:"minutes"`
	Tenants          []TenantMonthUsage `json:"-"`
}

// ReadMonthUsage reads every tenant's usage for the UTC month containing month.
func ReadMonthUsage(ctx context.Context, q Querier, month time.Time) (MonthUsage, error) {
	start := bucketStart(UsageMonth, month)
	end := nextBucket(UsageMonth, start)
	out := MonthUsage{Month: start, Minutes: int(end.Sub(start) / time.Minute)}
	rows, err := q.Query(ctx, `
WITH tenants AS (
    SELECT w.id AS tenant_id FROM workspaces w WHERE `+CountedTenantSQL+`
    UNION SELECT tenant_id FROM usage_hourly WHERE hour >= $1 AND hour < $2
    UNION SELECT tenant_id FROM usage_active_users WHERE month = $1::date
    UNION SELECT tenant_id FROM usage_active_user_counts WHERE month = $1::date
)
SELECT t.tenant_id::text,
       COALESCE(w.name, u.name, ''), COALESCE(w.slug, u.slug, ''), COALESCE(p.name, u.plan_name),
       w.id IS NULL,
       COALESCE((SELECT sum(quantity) FILTER (WHERE meter = 'relay_bytes_to_clients') FROM usage_hourly h WHERE h.tenant_id = t.tenant_id AND h.hour >= $1 AND h.hour < $2), 0)::bigint,
       COALESCE((SELECT sum(quantity) FILTER (WHERE meter = 'relay_bytes_to_connectors') FROM usage_hourly h WHERE h.tenant_id = t.tenant_id AND h.hour >= $1 AND h.hour < $2), 0)::bigint,
       COALESCE((SELECT sum(quantity) FILTER (WHERE meter = 'session_minutes') FROM usage_hourly h WHERE h.tenant_id = t.tenant_id AND h.hour >= $1 AND h.hour < $2), 0)::bigint,
       COALESCE((SELECT sum(quantity) FILTER (WHERE meter = 'connector_minutes') FROM usage_hourly h WHERE h.tenant_id = t.tenant_id AND h.hour >= $1 AND h.hour < $2), 0)::bigint,
       COALESCE((SELECT sum(quantity) FILTER (WHERE meter = 'shield_minutes') FROM usage_hourly h WHERE h.tenant_id = t.tenant_id AND h.hour >= $1 AND h.hour < $2), 0)::bigint,
       (SELECT count(*) FROM usage_active_users a WHERE a.tenant_id = t.tenant_id AND a.month = $1::date)
     + COALESCE((SELECT users FROM usage_active_user_counts c WHERE c.tenant_id = t.tenant_id AND c.month = $1::date), 0)
  FROM tenants t
  LEFT JOIN workspaces w ON w.id = t.tenant_id
  LEFT JOIN plans p ON p.id = w.plan_id
  LEFT JOIN usage_tenants u ON u.tenant_id = t.tenant_id`, start, end)
	if err != nil {
		return out, fmt.Errorf("month usage: %w", err)
	}
	for rows.Next() {
		var r TenantMonthUsage
		var bc, bn, sm, cm, shm int64
		if err := rows.Scan(&r.Tenant.ID, &r.Tenant.Name, &r.Tenant.Slug, &r.Tenant.PlanName, &r.Tenant.Purged,
			&bc, &bn, &sm, &cm, &shm, &r.ActiveUsers); err != nil {
			rows.Close()
			return out, fmt.Errorf("month usage: scan: %w", err)
		}
		r.Meters = map[string]int64{
			UsageMeterRelayBytesToClients: bc, UsageMeterRelayBytesToConnectors: bn,
			UsageMeterSessionMinutes: sm, UsageMeterConnectorMinutes: cm, UsageMeterShieldMinutes: shm,
		}
		out.Tenants = append(out.Tenants, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("month usage: %w", err)
	}
	collected, err := collectedMinutesByHour(ctx, q, start, end)
	if err != nil {
		return out, err
	}
	for _, n := range collected {
		out.CollectedMinutes += n
	}
	out.Complete = out.CollectedMinutes == out.Minutes
	return out, nil
}

// Value returns the tenant's quantity of meter.
func (r TenantMonthUsage) Value(meter string) int64 {
	if meter == UsageMeterActiveUsers {
		return r.ActiveUsers
	}
	return r.Meters[meter]
}

// FleetUsagePage is the response of GET /provider/usage/tenants.
type FleetUsagePage struct {
	MonthUsage
	Sort       string             `json:"sort"`
	Items      []TenantMonthUsage `json:"items"`
	NextCursor *string            `json:"next_cursor"`
}

// PageFleetUsage sorts m's tenants by meter, largest first (ties by tenant
// id), and returns one keyset page. The cursor encodes (value, tenant id).
// Codes: invalid_sort, invalid_cursor.
func PageFleetUsage(m MonthUsage, meter, cursor string, limit int) (FleetUsagePage, error) {
	if meter == "" {
		meter = UsageMeterRelayBytesToClients
	}
	page := FleetUsagePage{MonthUsage: m, Sort: meter, Items: []TenantMonthUsage{}}
	if !IsUsageMeter(meter) {
		return page, &UsageQueryError{"invalid_sort"}
	}
	limit = ClampLimit(limit)
	items := append([]TenantMonthUsage(nil), m.Tenants...)
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i].Value(meter), items[j].Value(meter)
		if a != b {
			return a > b
		}
		return items[i].Tenant.ID < items[j].Tenant.ID
	})
	start := 0
	if cursor != "" {
		cv, cid, err := decodeUsageCursor(cursor)
		if err != nil {
			return page, &UsageQueryError{"invalid_cursor"}
		}
		start = sort.Search(len(items), func(i int) bool {
			v := items[i].Value(meter)
			return v < cv || (v == cv && items[i].Tenant.ID > cid)
		})
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	page.Items = append(page.Items, items[start:end]...)
	if end < len(items) {
		last := items[end-1]
		c := encodeUsageCursor(last.Value(meter), last.Tenant.ID)
		page.NextCursor = &c
	}
	return page, nil
}

func encodeUsageCursor(v int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(v, 10) + "|" + id))
}

func decodeUsageCursor(c string) (int64, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, "", err
	}
	v, id, ok := strings.Cut(string(b), "|")
	if !ok || !IsUUID(id) {
		return 0, "", errors.New("bad cursor")
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, id, err
}
