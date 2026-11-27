// Package metricstore is the F23 metric sampler (Sprint 24 M1, Decision
// Record D-49): every minute it records the current relay, tenant and fleet
// values into metric_samples, rolls each finished hour up into
// metric_rollups, and prunes both by their retention.
//
// The golden rule: an unknown value is never written. A missing sample is a
// gap (a silent relay, no R1 report), which readers draw as a break, never
// as zero. Reads live in internal/providerquery (metrics.go).
package metricstore

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/yourorg/ztna/controller/internal/providerquery"
	"github.com/yourorg/ztna/controller/internal/relay"
)

// DB is what the sampler needs: the providerquery reads plus Exec.
// *pgxpool.Pool satisfies it.
type DB interface {
	providerquery.Querier
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// TrafficSource is the R1 per-workspace relay traffic (relay.WorkspaceTrafficTracker).
type TrafficSource interface {
	Snapshot() []relay.RelayWorkspaceTraffic
}

// Sample is one value to store.
type Sample struct {
	Series      string
	SubjectType string
	SubjectID   string
	Value       float64
}

// Sampler records and maintains the metric store.
type Sampler struct {
	DB DB
	// Traffic may be nil: no per-tenant relay series.
	Traffic TrafficSource
	// TrafficMaxAge: an R1 view older than this is not reported (the relay
	// may have gone silent). main.go uses two heartbeat DB-write windows
	// plus a minute.
	TrafficMaxAge time.Duration
	// Heartbeats is the Valkey relay liveness (relay.Service); nil uses the
	// persisted heartbeat only. LiveMaxAge: a relay whose last heartbeat is
	// older has no byte samples (F23 R1-3 B); zero means RelayLiveMaxAge.
	Heartbeats providerquery.HeartbeatSource
	LiveMaxAge time.Duration
	// Retention (D-49); zero means the default. Shorter values are for tests.
	SampleRetention time.Duration
	RollupRetention time.Duration
	Now             func() time.Time

	lastRolledHour time.Time
}

func (s *Sampler) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// RelayLiveMaxAge: two 30 s relay heartbeats plus 15 s (F23 R1-3 B). Below
// the 90 s eviction, so a silent relay's last rates stop before eviction.
const RelayLiveMaxAge = 75 * time.Second

func (s *Sampler) liveMaxAge() time.Duration {
	if s.LiveMaxAge > 0 {
		return s.LiveMaxAge
	}
	return RelayLiveMaxAge
}

// relayLive: the relay's freshest heartbeat (Valkey liveness, else the
// persisted one) is within maxAge.
func relayLive(ctx context.Context, hb providerquery.HeartbeatSource, relayID string, persisted *time.Time, now time.Time, maxAge time.Duration) bool {
	last := providerquery.EffectiveHeartbeat(ctx, hb, relayID, persisted)
	return last != nil && now.Sub(*last) <= maxAge
}

func (s *Sampler) sampleRetention() time.Duration {
	if s.SampleRetention > 0 {
		return s.SampleRetention
	}
	return providerquery.SampleRetention
}

func (s *Sampler) rollupRetention() time.Duration {
	if s.RollupRetention > 0 {
		return s.RollupRetention
	}
	return providerquery.RollupRetention
}

// Run samples once a minute until ctx ends. Each tick writes the minute's
// samples, then rolls up any finished hour not yet rolled up in this process
// and prunes expired rows. A failed step is logged and retried next tick.
func (s *Sampler) Run(ctx context.Context) {
	log.Printf("metric sampler: every minute (samples kept %s, roll-ups %s)", s.sampleRetention(), s.rollupRetention())
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick runs one sampling round at Now.
func (s *Sampler) Tick(ctx context.Context) {
	now := s.now()
	samples, err := s.Collect(ctx, now)
	if err != nil {
		log.Printf("metric sampler: collect: %v", err)
	}
	if len(samples) > 0 {
		if err := s.Write(ctx, now, samples); err != nil {
			log.Printf("metric sampler: write: %v", err)
		}
	}
	hour := now.Truncate(time.Hour)
	if !s.lastRolledHour.Equal(hour) {
		// The previous two hours, so a restart near an hour boundary still
		// rolls up the hour it missed. Upserts: re-running is harmless.
		ok := true
		for _, h := range []time.Time{hour.Add(-2 * time.Hour), hour.Add(-time.Hour)} {
			if err := s.Rollup(ctx, h); err != nil {
				log.Printf("metric sampler: roll up %s: %v", h.Format(time.RFC3339), err)
				ok = false
			}
		}
		if err := s.Prune(ctx, now); err != nil {
			log.Printf("metric sampler: prune: %v", err)
			ok = false
		}
		if ok {
			s.lastRolledHour = hour
		}
	}
}

// Collect reads the current values. It returns what it could read together
// with the first error: a failed tenant read doesn't drop the relay samples.
func (s *Sampler) Collect(ctx context.Context, now time.Time) ([]Sample, error) {
	var out []Sample
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	relays, truncated, err := providerquery.RelayLoads(ctx, s.DB)
	keep(err)
	active := map[string]bool{}
	live := map[string]bool{}
	if err == nil {
		for _, r := range relays {
			if r.Status == "active" && relayLive(ctx, s.Heartbeats, r.ID, r.LastHeartbeatAt, now, s.liveMaxAge()) {
				live[r.ID] = true
			}
		}
		out = append(out, relaySamples(relays, truncated, func(id string) bool { return live[id] }, active)...)
	}

	tenants, err := s.tenantCounts(ctx, now)
	keep(err)
	if err == nil {
		out = append(out, tenantSamples(tenants)...)
		// R1 series need the relay list too (only active, live relays count).
		if s.Traffic != nil && relays != nil {
			out = append(out, tenantRelaySamples(tenants, live, s.Traffic.Snapshot(), now, s.TrafficMaxAge)...)
		}
	}
	return out, firstErr
}

// relaySamples: per active relay, its connections and measured rates; fleet
// totals over the relays that report. An inactive (offline) relay has no
// sample: a gap. A relay that isn't live (F23 R1-3 B) keeps its connections
// but has no byte samples: its last window's rate would be repeated. active
// collects the active relay ids.
func relaySamples(relays []providerquery.RelaySummary, truncated bool, live func(string) bool, active map[string]bool) []Sample {
	var out []Sample
	var conns, toClients, toConnectors float64
	var nConns, nClients, nConnectors int
	for _, r := range relays {
		if r.Status != "active" {
			continue
		}
		active[r.ID] = true
		out = append(out, Sample{providerquery.SeriesRelayConnections, providerquery.MetricSubjectRelay, r.ID, float64(r.ConnectionCount)})
		conns += float64(r.ConnectionCount)
		nConns++
		if bw := r.Bandwidth; bw != nil && live(r.ID) {
			if bw.ToClientBytesPerSec != nil {
				out = append(out, Sample{providerquery.SeriesRelayBytesToClients, providerquery.MetricSubjectRelay, r.ID, *bw.ToClientBytesPerSec})
				toClients += *bw.ToClientBytesPerSec
				nClients++
			}
			if bw.ToConnectorBytesPerSec != nil {
				out = append(out, Sample{providerquery.SeriesRelayBytesToConnectors, providerquery.MetricSubjectRelay, r.ID, *bw.ToConnectorBytesPerSec})
				toConnectors += *bw.ToConnectorBytesPerSec
				nConnectors++
			}
		}
	}
	// Fleet totals: the sum over the relays that report; a gap only when
	// none does (agreed 2026-11-21). A truncated relay list (over
	// RelayLoadsCap) can't give a total.
	if !truncated {
		fleet := providerquery.FleetSubjectID
		if nConns > 0 {
			out = append(out, Sample{providerquery.SeriesFleetConnections, providerquery.MetricSubjectFleet, fleet, conns})
		}
		if nClients > 0 {
			out = append(out, Sample{providerquery.SeriesFleetBytesToClients, providerquery.MetricSubjectFleet, fleet, toClients})
		}
		if nConnectors > 0 {
			out = append(out, Sample{providerquery.SeriesFleetBytesToConnectors, providerquery.MetricSubjectFleet, fleet, toConnectors})
		}
	}
	return out
}

type tenantCount struct {
	id                         string
	sessions, connectors, shds int64
}

// tenantCounts reads every active tenant (status 'active'; a suspended one
// keeps that status, D-45) with its live sessions and online agents.
func (s *Sampler) tenantCounts(ctx context.Context, now time.Time) ([]tenantCount, error) {
	rows, err := s.DB.Query(ctx, `
SELECT w.id::text,
       (SELECT count(*) FROM client_devices d
         WHERE d.workspace_id = w.id AND d.revoked_at IS NULL
           AND d.session_idle_until > $1 AND d.session_max_until > $1),
       (SELECT count(*) FROM connectors c WHERE c.tenant_id = w.id AND c.status = 'active'),
       (SELECT count(*) FROM shields sh WHERE sh.tenant_id = w.id AND sh.status = 'active')
  FROM workspaces w
 WHERE w.status = 'active'
 ORDER BY w.id`, now)
	if err != nil {
		return nil, fmt.Errorf("tenant counts: %w", err)
	}
	defer rows.Close()
	var out []tenantCount
	for rows.Next() {
		var t tenantCount
		if err := rows.Scan(&t.id, &t.sessions, &t.connectors, &t.shds); err != nil {
			return nil, fmt.Errorf("tenant counts: scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tenant counts: %w", err)
	}
	return out, nil
}

func tenantSamples(tenants []tenantCount) []Sample {
	var out []Sample
	var sessions, connectors, shields float64
	for _, t := range tenants {
		out = append(out,
			Sample{providerquery.SeriesTenantActiveSessions, providerquery.MetricSubjectTenant, t.id, float64(t.sessions)},
			Sample{providerquery.SeriesTenantConnectorsOnline, providerquery.MetricSubjectTenant, t.id, float64(t.connectors)},
			Sample{providerquery.SeriesTenantShieldsOnline, providerquery.MetricSubjectTenant, t.id, float64(t.shds)},
		)
		sessions += float64(t.sessions)
		connectors += float64(t.connectors)
		shields += float64(t.shds)
	}
	fleet := providerquery.FleetSubjectID
	return append(out,
		Sample{providerquery.SeriesFleetActiveSessions, providerquery.MetricSubjectFleet, fleet, sessions},
		Sample{providerquery.SeriesFleetConnectorsOnline, providerquery.MetricSubjectFleet, fleet, connectors},
		Sample{providerquery.SeriesFleetShieldsOnline, providerquery.MetricSubjectFleet, fleet, shields},
	)
}

// tenantRelaySamples: each tenant's relay traffic summed over the active,
// live relays (usable) whose R1 view is fresh (D-54 refined: a workspace missing from a
// reporting relay's view had no traffic there). No fresh view at all → no
// sample for any tenant (not reported). A tenant whose rate is unknown on a
// reporting relay this window gets no byte samples; its stream count is
// still known. A fresh view without a window (a relay restart or first
// reading) gives no byte samples for any tenant (F23 R1-2): it can't say
// which tenants it carried, and a partial sum would read as the real total.
func tenantRelaySamples(tenants []tenantCount, usable map[string]bool, views []relay.RelayWorkspaceTraffic, now time.Time, maxAge time.Duration) []Sample {
	var fresh []relay.RelayWorkspaceTraffic
	bytesKnown := true
	for _, v := range views {
		if usable[v.RelayID] && now.Sub(v.MeasuredAt) <= maxAge {
			fresh = append(fresh, v)
			// F23 R1-2: a view without a window (restart, first reading)
			// can't say which tenants it carried: no tenant bytes this minute.
			if v.WindowSeconds == 0 {
				bytesKnown = false
			}
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	var out []Sample
	for _, t := range tenants {
		var toClients, toConnectors, streams float64
		known := true
		for _, v := range fresh {
			rate, ok := v.Workspaces[t.id]
			if !ok {
				continue
			}
			streams += float64(rate.ActiveStreams)
			if rate.ToClientBytesPerSec == nil || rate.ToConnectorBytesPerSec == nil {
				known = false
				continue
			}
			toClients += *rate.ToClientBytesPerSec
			toConnectors += *rate.ToConnectorBytesPerSec
		}
		out = append(out, Sample{providerquery.SeriesTenantRelayStreams, providerquery.MetricSubjectTenant, t.id, streams})
		if known && bytesKnown {
			out = append(out,
				Sample{providerquery.SeriesTenantRelayBytesToClient, providerquery.MetricSubjectTenant, t.id, toClients},
				Sample{providerquery.SeriesTenantRelayBytesToConn, providerquery.MetricSubjectTenant, t.id, toConnectors},
			)
		}
	}
	return out
}

// Write stores samples at now's minute. A sample already stored for that
// minute (a restart in the same minute) is kept, not replaced.
func (s *Sampler) Write(ctx context.Context, now time.Time, samples []Sample) error {
	ts := now.UTC().Truncate(time.Minute)
	series := make([]string, len(samples))
	types := make([]string, len(samples))
	subjects := make([]string, len(samples))
	values := make([]float64, len(samples))
	for i, sm := range samples {
		series[i], types[i], subjects[i], values[i] = sm.Series, sm.SubjectType, sm.SubjectID, sm.Value
	}
	_, err := s.DB.Exec(ctx, `
INSERT INTO metric_samples (series, subject_type, subject_id, ts, value)
SELECT u.series, u.subject_type, u.subject_id, $4, u.value
  FROM unnest($1::text[], $2::text[], $3::uuid[], $5::float8[]) AS u(series, subject_type, subject_id, value)
ON CONFLICT (series, subject_id, ts) DO NOTHING`, series, types, subjects, ts, values)
	if err != nil {
		return fmt.Errorf("insert %d samples: %w", len(samples), err)
	}
	return nil
}

// Rollup aggregates the hour starting at hourStart into metric_rollups
// (min, max, mean clamped into [min, max], number of minutes). Re-running an
// hour replaces its rows.
func (s *Sampler) Rollup(ctx context.Context, hourStart time.Time) error {
	start := hourStart.UTC().Truncate(time.Hour)
	_, err := s.DB.Exec(ctx, `
INSERT INTO metric_rollups (series, subject_type, subject_id, ts, min, max, avg, count)
SELECT series, subject_type, subject_id, $1,
       min(value), max(value), LEAST(GREATEST(avg(value), min(value)), max(value)), count(*)
  FROM metric_samples
 WHERE ts >= $1 AND ts < $2
 GROUP BY series, subject_type, subject_id
ON CONFLICT (series, subject_id, ts) DO UPDATE
   SET min = EXCLUDED.min, max = EXCLUDED.max, avg = EXCLUDED.avg, count = EXCLUDED.count`,
		start, start.Add(time.Hour))
	if err != nil {
		return fmt.Errorf("roll up: %w", err)
	}
	return nil
}

// Prune deletes samples and roll-ups older than their retention.
func (s *Sampler) Prune(ctx context.Context, now time.Time) error {
	if _, err := s.DB.Exec(ctx, `DELETE FROM metric_samples WHERE ts < $1`, now.Add(-s.sampleRetention())); err != nil {
		return fmt.Errorf("prune samples: %w", err)
	}
	if _, err := s.DB.Exec(ctx, `DELETE FROM metric_rollups WHERE ts < $1`, now.Add(-s.rollupRetention())); err != nil {
		return fmt.Errorf("prune roll-ups: %w", err)
	}
	return nil
}
