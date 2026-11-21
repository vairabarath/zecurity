package providerquery

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// Sprint 24 M1 (F23, Decision Record D-49): the metric series and their
// reads. The sampler (internal/metricstore) writes them; this package only
// reads (no Exec, D-04: no net/http).
//
// A missing point is a gap: the value was unknown (a silent relay, no R1
// report) and nothing was stored. Readers draw it as a break, never as zero.

// Metric subject types; each is the prefix of its series' names.
const (
	MetricSubjectRelay  = "relay"
	MetricSubjectTenant = "tenant"
	MetricSubjectFleet  = "fleet"
)

// FleetSubjectID is the subject_id of fleet series (primary key columns
// can't be NULL).
const FleetSubjectID = "00000000-0000-0000-0000-000000000000"

// MetricSeriesDef describes one series.
type MetricSeriesDef struct {
	Name    string
	Subject string
	// TenantData marks a series derived from tenant data: per-tenant series
	// and fleet totals of tenant data. Super-admin only (D-40 extended);
	// relay and fleet relay series are readable by relay-ops too.
	TenantData bool
	// Unit is "count" or "bytes_per_sec".
	Unit string
}

// Series names.
const (
	SeriesRelayConnections         = "relay.connections"
	SeriesRelayBytesToClients      = "relay.bytes_to_clients_per_sec"
	SeriesRelayBytesToConnectors   = "relay.bytes_to_connectors_per_sec"
	SeriesTenantActiveSessions     = "tenant.active_sessions"
	SeriesTenantConnectorsOnline   = "tenant.connectors_online"
	SeriesTenantShieldsOnline      = "tenant.shields_online"
	SeriesTenantRelayBytesToClient = "tenant.relay_bytes_to_clients_per_sec"
	SeriesTenantRelayBytesToConn   = "tenant.relay_bytes_to_connectors_per_sec"
	SeriesTenantRelayStreams       = "tenant.relay_streams"
	SeriesFleetConnections         = "fleet.connections"
	SeriesFleetBytesToClients      = "fleet.bytes_to_clients_per_sec"
	SeriesFleetBytesToConnectors   = "fleet.bytes_to_connectors_per_sec"
	SeriesFleetActiveSessions      = "fleet.active_sessions"
	SeriesFleetConnectorsOnline    = "fleet.connectors_online"
	SeriesFleetShieldsOnline       = "fleet.shields_online"
)

var metricSeries = map[string]MetricSeriesDef{}

func init() {
	for _, d := range []MetricSeriesDef{
		{SeriesRelayConnections, MetricSubjectRelay, false, "count"},
		{SeriesRelayBytesToClients, MetricSubjectRelay, false, "bytes_per_sec"},
		{SeriesRelayBytesToConnectors, MetricSubjectRelay, false, "bytes_per_sec"},
		{SeriesTenantActiveSessions, MetricSubjectTenant, true, "count"},
		{SeriesTenantConnectorsOnline, MetricSubjectTenant, true, "count"},
		{SeriesTenantShieldsOnline, MetricSubjectTenant, true, "count"},
		{SeriesTenantRelayBytesToClient, MetricSubjectTenant, true, "bytes_per_sec"},
		{SeriesTenantRelayBytesToConn, MetricSubjectTenant, true, "bytes_per_sec"},
		{SeriesTenantRelayStreams, MetricSubjectTenant, true, "count"},
		{SeriesFleetConnections, MetricSubjectFleet, false, "count"},
		{SeriesFleetBytesToClients, MetricSubjectFleet, false, "bytes_per_sec"},
		{SeriesFleetBytesToConnectors, MetricSubjectFleet, false, "bytes_per_sec"},
		{SeriesFleetActiveSessions, MetricSubjectFleet, true, "count"},
		{SeriesFleetConnectorsOnline, MetricSubjectFleet, true, "count"},
		{SeriesFleetShieldsOnline, MetricSubjectFleet, true, "count"},
	} {
		metricSeries[d.Name] = d
	}
}

// LookupMetricSeries returns the series definition, or false if unknown.
func LookupMetricSeries(name string) (MetricSeriesDef, bool) {
	d, ok := metricSeries[name]
	return d, ok
}

// MetricSeriesNames lists every series, sorted.
func MetricSeriesNames() []string {
	out := make([]string, 0, len(metricSeries))
	for n := range metricSeries {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Metric resolutions and limits (D-49).
const (
	MetricStepMinute = "1m"
	MetricStepHour   = "1h"
	// SampleRetention is how long 1-minute samples are kept; a 1-minute read
	// may not start earlier than this.
	SampleRetention = 7 * 24 * time.Hour
	// RollupRetention is how long hourly roll-ups are kept.
	RollupRetention = 400 * 24 * time.Hour
	// MaxMetricPoints caps one response.
	MaxMetricPoints = 2000
	// autoMinuteRange: a read without step uses 1-minute points up to this
	// range, hourly points beyond it.
	autoMinuteRange = 24 * time.Hour
)

// MetricQuery is a validated read.
type MetricQuery struct {
	Series    MetricSeriesDef
	SubjectID string // FleetSubjectID for fleet series
	From, To  time.Time
	Step      string
}

// MetricQueryError is a rejected read; Code is the API error code.
type MetricQueryError struct{ Code string }

func (e *MetricQueryError) Error() string { return "providerquery: metric query: " + e.Code }

// NewMetricQuery validates a read of series for subject over [from, to).
// An empty step picks the resolution from the range. Codes:
// invalid_subject, invalid_range, invalid_step, too_many_points.
func NewMetricQuery(def MetricSeriesDef, subject string, from, to time.Time, step string, now time.Time) (MetricQuery, error) {
	q := MetricQuery{Series: def, From: from.UTC(), To: to.UTC(), Step: step}
	switch def.Subject {
	case MetricSubjectFleet:
		if subject != "" {
			return q, &MetricQueryError{"invalid_subject"}
		}
		q.SubjectID = FleetSubjectID
	default:
		if !IsUUID(subject) {
			return q, &MetricQueryError{"invalid_subject"}
		}
		q.SubjectID = subject
	}
	if !q.From.Before(q.To) || q.To.Sub(q.From) > RollupRetention {
		return q, &MetricQueryError{"invalid_range"}
	}
	if q.Step == "" {
		q.Step = MetricStepHour
		if q.To.Sub(q.From) <= autoMinuteRange {
			q.Step = MetricStepMinute
		}
	}
	var per time.Duration
	switch q.Step {
	case MetricStepMinute:
		per = time.Minute
		if q.From.Before(now.Add(-SampleRetention)) {
			return q, &MetricQueryError{"invalid_range"} // 1-minute data is kept 7 days
		}
	case MetricStepHour:
		per = time.Hour
	default:
		return q, &MetricQueryError{"invalid_step"}
	}
	if int64(q.To.Sub(q.From)/per) > MaxMetricPoints {
		return q, &MetricQueryError{"too_many_points"}
	}
	return q, nil
}

// MetricPoint is one stored point. For 1-minute data Value is the sample and
// Min, Max and Count are omitted; for hourly data Value is the hour's mean,
// with its min, max and the number of minutes that had a sample.
type MetricPoint struct {
	TS    time.Time `json:"ts"`
	Value float64   `json:"value"`
	Min   *float64  `json:"min,omitempty"`
	Max   *float64  `json:"max,omitempty"`
	Count *int      `json:"count,omitempty"`
}

// MetricSeriesData is the response of GET /provider/metrics/{series}.
type MetricSeriesData struct {
	Series  string        `json:"series"`
	Subject *string       `json:"subject"` // null for fleet series
	Unit    string        `json:"unit"`
	Step    string        `json:"step"`
	From    time.Time     `json:"from"`
	To      time.Time     `json:"to"`
	Points  []MetricPoint `json:"points"` // ascending; a missing point is a gap
}

// ReadMetricSeries returns the stored points of q, ascending.
func ReadMetricSeries(ctx context.Context, db Querier, q MetricQuery) (MetricSeriesData, error) {
	out := MetricSeriesData{Series: q.Series.Name, Unit: q.Series.Unit, Step: q.Step, From: q.From, To: q.To, Points: []MetricPoint{}}
	if q.Series.Subject != MetricSubjectFleet {
		s := q.SubjectID
		out.Subject = &s
	}
	var sql string
	if q.Step == MetricStepMinute {
		sql = `SELECT ts, value, NULL::float8, NULL::float8, NULL::int FROM metric_samples
		        WHERE series = $1 AND subject_id = $2 AND ts >= $3 AND ts < $4 ORDER BY ts LIMIT $5`
	} else {
		sql = `SELECT ts, avg, min, max, count FROM metric_rollups
		        WHERE series = $1 AND subject_id = $2 AND ts >= $3 AND ts < $4 ORDER BY ts LIMIT $5`
	}
	rows, err := db.Query(ctx, sql, q.Series.Name, q.SubjectID, q.From, q.To, MaxMetricPoints)
	if err != nil {
		return out, fmt.Errorf("metric series %s: %w", q.Series.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var p MetricPoint
		if err := rows.Scan(&p.TS, &p.Value, &p.Min, &p.Max, &p.Count); err != nil {
			return out, fmt.Errorf("metric series %s: scan: %w", q.Series.Name, err)
		}
		p.TS = p.TS.UTC()
		out.Points = append(out.Points, p)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("metric series %s: %w", q.Series.Name, err)
	}
	return out, nil
}
