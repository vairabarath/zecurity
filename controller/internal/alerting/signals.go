package alerting

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yourorg/ztna/controller/internal/health"
	"github.com/yourorg/ztna/controller/internal/providerquery"
	"github.com/yourorg/ztna/controller/internal/quota"
	"github.com/yourorg/ztna/controller/internal/relay/eligible"
	"github.com/yourorg/ztna/controller/internal/relay/relaypool"
)

// Observation is one target whose condition holds right now. TenantID is ""
// for platform kinds and set for tenant kinds (D-50).
type Observation struct {
	TargetType string
	TargetID   string
	TargetName string
	TenantID   string
	Summary    string
}

// HealthSource is the control-plane health registry (F25).
// *health.Registry satisfies it.
type HealthSource interface {
	Evaluate(ctx context.Context) *health.DiagnosticReport
}

// Sources are what the kinds read. Every read is a query on current state;
// nothing is cached across ticks.
type Sources struct {
	DB         providerquery.Querier
	Health     HealthSource                       // nil: controlplane.degraded can't evaluate
	Controller providerquery.ControllerCertSource // may be nil (no controller certificate entry)
}

// errNoSource: a kind's source isn't wired; the evaluator leaves its states
// unchanged (never resolves on missing data).
var errNoSource = errors.New("alerting: source not configured")

// maxObservations bounds one kind's result (a runaway query must not flood
// the state table). Exceeding it is an error: states stay unchanged.
const maxObservations = 5000

// Observe evaluates one rule's condition. An error means "unknown": the
// caller must not change any state of that rule.
func (s Sources) Observe(ctx context.Context, kind string, p Params, now time.Time) ([]Observation, error) {
	var (
		obs []Observation
		err error
	)
	switch kind {
	case KindRelayOffline:
		obs, err = s.relayOffline(ctx)
	case KindRelayOverCapacity:
		obs, err = s.relayOverCapacity(ctx, p)
	case KindPoolNoEligibleRelay:
		obs, err = s.poolNoEligibleRelay(ctx)
	case KindControlPlane:
		obs, err = s.controlPlane(ctx, p)
	case KindCertificateExpiring:
		obs, err = s.certificateExpiring(ctx, p, now)
	case KindConnectorOffline:
		obs, err = s.agentOffline(ctx, "connector")
	case KindShieldOffline:
		obs, err = s.agentOffline(ctx, "shield")
	case KindTenantNoRelayPath:
		obs, err = s.tenantNoRelayPath(ctx)
	case KindTenantOverQuota:
		obs, err = s.tenantOverQuota(ctx, p)
	case KindTenantUsageOver:
		obs, err = s.tenantUsageOver(ctx, p, now)
	default:
		return nil, fmt.Errorf("alerting: unknown kind %q", kind)
	}
	if err != nil {
		return nil, err
	}
	if len(obs) > maxObservations {
		return nil, fmt.Errorf("alerting: %s: %d targets exceed the cap of %d", kind, len(obs), maxObservations)
	}
	// A kind's scope decides whether a tenant is required (D-50); refuse a
	// mismatch here rather than let the database CHECK fail the whole tick.
	for _, o := range obs {
		if (kindScope[kind] == ScopeTenant) != (o.TenantID != "") {
			return nil, fmt.Errorf("alerting: %s: observation %s/%s has the wrong scope", kind, o.TargetType, o.TargetID)
		}
	}
	return obs, nil
}

// relayLoads reads the shared relay reader (providerquery.RelayLoads, the
// same one the F23 sampler uses). A truncated read is an error: a relay
// missing from the list would look like a resolved alert.
func (s Sources) relayLoads(ctx context.Context) ([]providerquery.RelaySummary, error) {
	relays, truncated, err := providerquery.RelayLoads(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, fmt.Errorf("alerting: more than %d relays; relay rules not evaluated", providerquery.RelayLoadsCap)
	}
	return relays, nil
}

// relayOffline: a relay the controller marked inactive (heartbeats stopped;
// the eviction job moves active → inactive).
func (s Sources) relayOffline(ctx context.Context) ([]Observation, error) {
	relays, err := s.relayLoads(ctx)
	if err != nil {
		return nil, err
	}
	var out []Observation
	for _, r := range relays {
		if r.Status != "inactive" {
			continue
		}
		since := "unknown"
		if r.LastHeartbeatAt != nil {
			since = r.LastHeartbeatAt.UTC().Format(time.RFC3339)
		}
		out = append(out, Observation{
			TargetType: "relay", TargetID: r.ID, TargetName: r.Name,
			Summary: fmt.Sprintf("Relay %s is offline (last heartbeat %s)", r.Name, since),
		})
	}
	return out, nil
}

// relayOverCapacity: an active relay whose measured traffic is at or above
// threshold_percent of its configured link capacity (B2 headroom, D-42).
// A relay without a configured capacity or a measured rate never fires.
func (s Sources) relayOverCapacity(ctx context.Context, p Params) ([]Observation, error) {
	relays, err := s.relayLoads(ctx)
	if err != nil {
		return nil, err
	}
	var out []Observation
	for _, r := range relays {
		if r.Status != "active" || r.Headroom == nil || r.LinkCapacityBps == nil {
			continue
		}
		pct := r.Headroom.Utilization * 100
		if pct < float64(p.ThresholdPercent) {
			continue
		}
		word := "near"
		if r.Headroom.OverCapacity {
			word = "over"
		}
		out = append(out, Observation{
			TargetType: "relay", TargetID: r.ID, TargetName: r.Name,
			Summary: fmt.Sprintf("Relay %s is %s capacity: %.0f%% of its link (%s of %s)",
				r.Name, word, pct, rate(r.Headroom.UsedBytesPerSec), rate(float64(*r.LinkCapacityBps))),
		})
	}
	return out, nil
}

// poolNoEligibleRelay: a pool that active (not suspended) tenants use,
// as primary or failover, and that has no eligible relay (F11-1). An
// unused pool, or one used only by suspended tenants, doesn't fire.
func (s Sources) poolNoEligibleRelay(ctx context.Context) ([]Observation, error) {
	rows, err := s.DB.Query(ctx, `
SELECT p.id::text, p.name,
       (SELECT count(*) FROM workspaces w WHERE `+activeTenant+`
           AND (w.relay_pool_id = p.id OR w.relay_failover_pool_id = p.id))
  FROM relay_pools p
 WHERE NOT EXISTS (SELECT 1 FROM relays WHERE pool_id = p.id AND `+eligible.Where+`)
 ORDER BY p.id`)
	if err != nil {
		return nil, fmt.Errorf("pools without a relay: %w", err)
	}
	all, err := collect(rows, func(r pgx.Rows) (Observation, error) {
		var id, name string
		var tenants int64
		if err := r.Scan(&id, &name, &tenants); err != nil {
			return Observation{}, err
		}
		if tenants == 0 {
			return Observation{}, nil
		}
		return Observation{
			TargetType: "pool", TargetID: id, TargetName: name,
			Summary: fmt.Sprintf("Relay pool %s has no eligible relay (%d active tenants use it)", name, tenants),
		}, nil
	})
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, o := range all {
		if o.TargetID != "" {
			out = append(out, o)
		}
	}
	return out, nil
}

// controlPlane: a health component (F25) that isn't up.
func (s Sources) controlPlane(ctx context.Context, p Params) ([]Observation, error) {
	if s.Health == nil {
		return nil, errNoSource
	}
	report := s.Health.Evaluate(ctx)
	if report == nil {
		return nil, errors.New("alerting: no health report")
	}
	want := map[string]bool{}
	for _, c := range p.Components {
		want[c] = true
	}
	names := make([]string, 0, len(report.Components))
	for name := range report.Components {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []Observation
	for _, name := range names {
		c := report.Components[name]
		if c.Status == "up" || (len(want) > 0 && !want[name]) {
			continue
		}
		summary := fmt.Sprintf("Control plane component %s is %s", name, c.Status)
		if c.Error != "" {
			summary += ": " + truncate(c.Error, 200)
		}
		out = append(out, Observation{TargetType: "controller", TargetID: name, TargetName: name, Summary: summary})
	}
	return out, nil
}

// certificateExpiring: provider-owned certificates near expiry, with D-36's
// scope: platform CAs (root, intermediate, workspace CAs) within `days`;
// automatically renewed ones (relays, the controller's gRPC certificate)
// only within 24 hours or expired, which means a renewal is overdue.
// Tenant agent certificates are short-lived and renew themselves: out of
// scope, as in F14.
func (s Sources) certificateExpiring(ctx context.Context, p Params, now time.Time) ([]Observation, error) {
	windows := []struct {
		kind   string
		within time.Duration
	}{
		{"root", time.Duration(p.Days) * 24 * time.Hour},
		{"intermediate", time.Duration(p.Days) * 24 * time.Hour},
		{"workspace_ca", time.Duration(p.Days) * 24 * time.Hour},
		{"relay", 24 * time.Hour},
		{"controller_grpc", 24 * time.Hour},
	}
	var out []Observation
	for _, w := range windows {
		cursor := ""
		for pages := 0; ; pages++ {
			if pages > 50 {
				return nil, fmt.Errorf("alerting: too many %s certificates", w.kind)
			}
			page, err := providerquery.CertificateExpiry(ctx, s.DB, s.Controller, providerquery.CertificateFilter{
				Within: w.within, Kind: w.kind, Limit: 200, Cursor: cursor, Now: now,
			})
			if err != nil {
				return nil, fmt.Errorf("certificates (%s): %w", w.kind, err)
			}
			for _, e := range page.Items {
				name := e.EntityID
				if e.EntityName != nil && *e.EntityName != "" {
					name = *e.EntityName
				}
				if e.Kind == "workspace_ca" {
					// A platform alert never carries a tenant (D-50): name
					// the CA by its id, not the workspace's name.
					name = "workspace CA " + e.EntityID
				}
				state := "expires " + e.NotAfter.UTC().Format(time.RFC3339)
				if !e.NotAfter.After(now) {
					state = "expired " + e.NotAfter.UTC().Format(time.RFC3339)
				}
				out = append(out, Observation{
					TargetType: "certificate", TargetID: e.Kind + ":" + e.EntityID, TargetName: name,
					Summary: fmt.Sprintf("Certificate %s (%s) %s", name, kindLabel(e.Kind), state),
				})
			}
			if page.NextCursor == nil {
				break
			}
			cursor = *page.NextCursor
		}
	}
	return out, nil
}

// activeTenant is the SQL condition for a tenant that tenant rules watch: an
// active workspace that isn't suspended (a suspended tenant's agents are
// deny-all by design, D-45/D-08; alerting on them would be noise).
const activeTenant = `w.status = 'active' AND w.suspended_at IS NULL`

// agentOffline: a connector or shield the controller marked disconnected.
func (s Sources) agentOffline(ctx context.Context, table string) ([]Observation, error) {
	var q string
	switch table {
	case "connector":
		q = `SELECT a.id::text, a.name, w.id::text, w.slug FROM connectors a JOIN workspaces w ON w.id = a.tenant_id
		      WHERE a.status = 'disconnected' AND a.revoked_at IS NULL AND ` + activeTenant + ` ORDER BY a.id`
	case "shield":
		q = `SELECT a.id::text, a.name, w.id::text, w.slug FROM shields a JOIN workspaces w ON w.id = a.tenant_id
		      WHERE a.status = 'disconnected' AND ` + activeTenant + ` ORDER BY a.id`
	default:
		return nil, fmt.Errorf("alerting: unknown agent table %q", table)
	}
	rows, err := s.DB.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("%s offline: %w", table, err)
	}
	return collect(rows, func(r pgx.Rows) (Observation, error) {
		var id, name, tenantID, slug string
		if err := r.Scan(&id, &name, &tenantID, &slug); err != nil {
			return Observation{}, err
		}
		label := "Connector"
		if table == "shield" {
			label = "Shield"
		}
		return Observation{
			TargetType: table, TargetID: id, TargetName: name, TenantID: tenantID,
			Summary: fmt.Sprintf("%s %s of tenant %s is offline", label, name, slug),
		}, nil
	})
}

// tenantNoRelayPath: a tenant with agents whose effective relay pool
// (relaypool.EffectivePool, the one definition, F11-2) has no eligible
// relay, so its connectors get an empty relay list (F11-1).
func (s Sources) tenantNoRelayPath(ctx context.Context) ([]Observation, error) {
	rows, err := s.DB.Query(ctx, `
SELECT w.id::text, w.slug, w.name
  FROM workspaces w
  JOIN relay_pools p ON p.id = w.relay_pool_id
 WHERE `+activeTenant+`
   AND EXISTS (SELECT 1 FROM connectors c WHERE c.tenant_id = w.id AND c.status IN ('active', 'disconnected') AND c.revoked_at IS NULL)
   AND NOT EXISTS (SELECT 1 FROM relays WHERE pool_id = `+relaypool.EffectivePool("p")+` AND `+eligible.Where+`)
 ORDER BY w.id`)
	if err != nil {
		return nil, fmt.Errorf("tenant relay path: %w", err)
	}
	return collect(rows, func(r pgx.Rows) (Observation, error) {
		var id, slug, name string
		if err := r.Scan(&id, &slug, &name); err != nil {
			return Observation{}, err
		}
		return Observation{
			TargetType: "tenant", TargetID: id, TargetName: name, TenantID: id,
			Summary: fmt.Sprintf("Tenant %s has no relay path: its relay pool has no eligible relay", slug),
		}, nil
	})
}

// tenantOverQuota: a tenant using more than a lowered limit (S3 "over").
// Informational: nothing is removed (D-47).
func (s Sources) tenantOverQuota(ctx context.Context, p Params) ([]Observation, error) {
	rows, err := s.DB.Query(ctx, `SELECT w.id::text, w.slug, w.name FROM workspaces w WHERE `+activeTenant+` ORDER BY w.id`)
	if err != nil {
		return nil, fmt.Errorf("tenant over quota: list tenants: %w", err)
	}
	type tenant struct{ id, slug, name string }
	tenants, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (tenant, error) {
		var t tenant
		return t, r.Scan(&t.id, &t.slug, &t.name)
	})
	if err != nil {
		return nil, fmt.Errorf("tenant over quota: list tenants: %w", err)
	}
	if len(tenants) > maxObservations {
		return nil, fmt.Errorf("alerting: more than %d tenants; over-quota rule not evaluated", maxObservations)
	}
	want := map[string]bool{}
	for _, k := range p.Keys {
		want[k] = true
	}
	var out []Observation
	for _, t := range tenants {
		usage, err := quota.AllUsage(ctx, s.DB, t.id)
		if errors.Is(err, quota.ErrWorkspaceNotFound) {
			continue // deleted between the two reads
		}
		if err != nil {
			return nil, fmt.Errorf("tenant over quota: %s: %w", t.slug, err)
		}
		var over []string
		for _, u := range usage {
			if u.Over && (len(want) == 0 || want[u.Key]) && u.Limit != nil {
				over = append(over, fmt.Sprintf("%s %d/%d", strings.ReplaceAll(u.Key, "_", " "), u.Used, *u.Limit))
			}
		}
		if len(over) == 0 {
			continue
		}
		out = append(out, Observation{
			TargetType: "tenant", TargetID: t.id, TargetName: t.name, TenantID: t.id,
			Summary: fmt.Sprintf("Tenant %s is over quota: %s", t.slug, strings.Join(over, ", ")),
		})
	}
	return out, nil
}

// tenantUsageOver (Sprint 25 U4, D-61): a watched tenant whose usage of
// p.Meter so far this UTC month is at or above p.Threshold. It reads the
// ledger through U2's month reader (one reader, never M1). Usage only grows
// within a month, so an alert stays firing until the month rolls over and
// usage restarts at 0 (then it resolves). A read error leaves states
// unchanged (never resolves). Warns only: nothing reads this to refuse or
// limit anything.
func (s Sources) tenantUsageOver(ctx context.Context, p Params, now time.Time) ([]Observation, error) {
	m, err := providerquery.ReadMonthUsage(ctx, s.DB, now)
	if err != nil {
		return nil, fmt.Errorf("tenant usage: %w", err)
	}
	rows, err := s.DB.Query(ctx, `SELECT w.id::text, w.slug FROM workspaces w WHERE `+activeTenant)
	if err != nil {
		return nil, fmt.Errorf("tenant usage: list tenants: %w", err)
	}
	watched := map[string]string{}
	for rows.Next() {
		var id, slug string
		if err := rows.Scan(&id, &slug); err != nil {
			rows.Close()
			return nil, fmt.Errorf("tenant usage: list tenants: %w", err)
		}
		watched[id] = slug
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tenant usage: list tenants: %w", err)
	}
	var out []Observation
	for _, t := range m.Tenants {
		slug, ok := watched[t.Tenant.ID]
		if !ok || t.Tenant.Purged {
			continue // only tenants tenant rules watch (active, not suspended)
		}
		v := t.Value(p.Meter)
		if v < p.Threshold {
			continue
		}
		out = append(out, Observation{
			TargetType: "tenant", TargetID: t.Tenant.ID, TargetName: t.Tenant.Name, TenantID: t.Tenant.ID,
			Summary: fmt.Sprintf("Tenant %s used %s %s in %s (threshold %s)", slug, usageAmount(p.Meter, v),
				usageMeterLabel(p.Meter), m.Month.Format("January 2006"), usageAmount(p.Meter, p.Threshold)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TargetID < out[j].TargetID })
	return out, nil
}

// usageMeterLabel names a usage meter in an alert summary.
func usageMeterLabel(meter string) string {
	switch meter {
	case providerquery.UsageMeterRelayBytesToClients:
		return "relayed to clients"
	case providerquery.UsageMeterRelayBytesToConnectors:
		return "relayed to connectors"
	case providerquery.UsageMeterSessionMinutes:
		return "of client sessions"
	case providerquery.UsageMeterConnectorMinutes:
		return "of connectors online"
	case providerquery.UsageMeterShieldMinutes:
		return "of shields online"
	case providerquery.UsageMeterActiveUsers:
		return "active users"
	}
	return meter
}

// usageAmount formats a usage quantity like the console: bytes in decimal
// units, minutes as hours, users as a count.
func usageAmount(meter string, v int64) string {
	switch meter {
	case providerquery.UsageMeterRelayBytesToClients, providerquery.UsageMeterRelayBytesToConnectors:
		units := []string{"B", "kB", "MB", "GB", "TB", "PB", "EB"}
		f, i := float64(v), 0
		for f >= 1000 && i < len(units)-1 {
			f /= 1000
			i++
		}
		if i == 0 {
			return fmt.Sprintf("%d B", v)
		}
		return fmt.Sprintf("%.1f %s", f, units[i])
	case providerquery.UsageMeterActiveUsers:
		return fmt.Sprintf("%d", v)
	}
	return fmt.Sprintf("%.1f h", float64(v)/60)
}

func collect(rows pgx.Rows, scan func(pgx.Rows) (Observation, error)) ([]Observation, error) {
	defer rows.Close()
	var out []Observation
	for rows.Next() {
		o, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
		if len(out) > maxObservations {
			return nil, fmt.Errorf("alerting: more than %d targets", maxObservations)
		}
	}
	return out, rows.Err()
}

func kindLabel(kind string) string {
	switch kind {
	case "root":
		return "root CA"
	case "intermediate":
		return "intermediate CA"
	case "workspace_ca":
		return "workspace CA"
	case "controller_grpc":
		return "controller gRPC"
	}
	return kind
}

// rate formats bytes per second like the console (decimal units).
func rate(bps float64) string {
	units := []string{"B/s", "kB/s", "MB/s", "GB/s"}
	i := 0
	for bps >= 1000 && i < len(units)-1 {
		bps /= 1000
		i++
	}
	return fmt.Sprintf("%.1f %s", bps, units[i])
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
