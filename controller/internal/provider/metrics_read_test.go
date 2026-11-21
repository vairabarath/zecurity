package provider_test

import (
	"context"
	"encoding/json"
	"net/url"
	"testing"
	"time"
)

// Metric reads (Sprint 24 M1, F23, D-49; D-40 extended): relay and fleet
// relay series for relay-ops and super-admin, tenant series and fleet
// totals of tenant data for super-admin only; gaps stay gaps; resolution,
// range and cap rules.

type metricResp struct {
	Series  string  `json:"series"`
	Subject *string `json:"subject"`
	Unit    string  `json:"unit"`
	Step    string  `json:"step"`
	Points  []struct {
		TS    time.Time `json:"ts"`
		Value float64   `json:"value"`
		Min   *float64  `json:"min"`
		Max   *float64  `json:"max"`
		Count *int      `json:"count"`
	} `json:"points"`
}

func metricPath(series string, params map[string]string) string {
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	return "/provider/metrics/" + series + "?" + q.Encode()
}

func (e *readEnv) sample(t *testing.T, series, subjectType, subject string, ts time.Time, v float64) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO metric_samples (series, subject_type, subject_id, ts, value) VALUES ($1,$2,$3,$4,$5)`,
		series, subjectType, subject, ts, v); err != nil {
		t.Fatal(err)
	}
}

const fleetID = "00000000-0000-0000-0000-000000000000"

func TestMetricReads_RoleMatrix(t *testing.T) {
	e := newReadEnv(t)
	now := time.Now().UTC()
	from := now.Add(-time.Hour).Format(time.RFC3339)
	cases := []struct {
		series, subject string
		opsAllowed      bool
	}{
		{"relay.connections", e.relayID, true},
		{"relay.bytes_to_clients_per_sec", e.relayID, true},
		{"fleet.connections", "", true},
		{"fleet.bytes_to_connectors_per_sec", "", true},
		{"tenant.active_sessions", e.tenantID, false},
		{"tenant.relay_bytes_to_clients_per_sec", e.tenantID, false},
		{"tenant.relay_streams", e.tenantID, false},
		{"fleet.active_sessions", "", false},
		{"fleet.connectors_online", "", false},
	}
	for _, c := range cases {
		p := metricPath(c.series, map[string]string{"subject": c.subject, "from": from})
		if r := e.get(t, p, e.superTok); r.code != 200 {
			t.Errorf("super-admin %s = %d %s", c.series, r.code, r.body)
		}
		want := 403
		if c.opsAllowed {
			want = 200
		}
		if r := e.get(t, p, e.opsTok); r.code != want {
			t.Errorf("relay-ops %s = %d, want %d (%s)", c.series, r.code, want, r.body)
		}
	}
	// A tenant JWT never authenticates a provider route.
	if r := e.get(t, metricPath("fleet.connections", nil), e.tenantJWT); r.code != 401 {
		t.Fatalf("tenant JWT = %d", r.code)
	}
}

func TestMetricReads_MinutePointsWithGaps(t *testing.T) {
	e := newReadEnv(t)
	base := time.Now().UTC().Truncate(time.Minute).Add(-30 * time.Minute)
	e.sample(t, "relay.connections", "relay", e.relayID, base, 3)
	// base+1m missing: a gap.
	e.sample(t, "relay.connections", "relay", e.relayID, base.Add(2*time.Minute), 5)
	e.sample(t, "relay.connections", "relay", e.relayID, base.Add(-48*time.Hour), 99) // outside the range

	r := e.get(t, metricPath("relay.connections", map[string]string{
		"subject": e.relayID, "from": base.Format(time.RFC3339), "to": base.Add(10 * time.Minute).Format(time.RFC3339),
	}), e.opsTok)
	if r.code != 200 {
		t.Fatalf("read = %d %s", r.code, r.body)
	}
	var d metricResp
	if err := json.Unmarshal([]byte(r.body), &d); err != nil {
		t.Fatal(err)
	}
	if d.Step != "1m" || d.Unit != "count" || d.Subject == nil || *d.Subject != e.relayID {
		t.Fatalf("header %+v", d)
	}
	if len(d.Points) != 2 || d.Points[0].Value != 3 || d.Points[1].Value != 5 ||
		!d.Points[1].TS.Equal(base.Add(2*time.Minute)) || d.Points[0].Count != nil {
		t.Fatalf("points %+v (the missing minute must stay missing, never 0)", d.Points)
	}
}

func TestMetricReads_HourlyRollups(t *testing.T) {
	e := newReadEnv(t)
	hour := time.Now().UTC().Truncate(time.Hour).Add(-5 * 24 * time.Hour)
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO metric_rollups VALUES ('fleet.connections','fleet',$1,$2,1,9,4,60)`, fleetID, hour); err != nil {
		t.Fatal(err)
	}
	r := e.get(t, metricPath("fleet.connections", map[string]string{
		"from": hour.Add(-time.Hour).Format(time.RFC3339), "to": hour.Add(48 * time.Hour).Format(time.RFC3339),
	}), e.superTok)
	var d metricResp
	if err := json.Unmarshal([]byte(r.body), &d); err != nil || r.code != 200 {
		t.Fatalf("read %d %s: %v", r.code, r.body, err)
	}
	if d.Step != "1h" || d.Subject != nil || len(d.Points) != 1 {
		t.Fatalf("auto step for a 49 h range must be 1h, fleet subject null: %+v", d)
	}
	p := d.Points[0]
	if p.Value != 4 || p.Min == nil || *p.Min != 1 || *p.Max != 9 || *p.Count != 60 {
		t.Fatalf("roll-up point %+v", p)
	}
}

func TestMetricReads_Refusals(t *testing.T) {
	e := newReadEnv(t)
	now := time.Now().UTC()
	rfc := func(t time.Time) string { return t.Format(time.RFC3339) }
	cases := []struct {
		name   string
		path   string
		code   int
		errStr string
	}{
		{"unknown series", metricPath("relay.nonsense", nil), 404, "not_found"},
		{"relay series without subject", metricPath("relay.connections", nil), 400, "invalid_subject"},
		{"subject not a uuid", metricPath("tenant.active_sessions", map[string]string{"subject": "x"}), 400, "invalid_subject"},
		{"fleet with a subject", metricPath("fleet.connections", map[string]string{"subject": e.relayID}), 400, "invalid_subject"},
		{"from after to", metricPath("fleet.connections", map[string]string{"from": rfc(now), "to": rfc(now.Add(-time.Hour))}), 400, "invalid_range"},
		{"bad time", metricPath("fleet.connections", map[string]string{"from": "yesterday"}), 400, "invalid_range"},
		{"minute data past 7 days", metricPath("fleet.connections", map[string]string{"from": rfc(now.Add(-8 * 24 * time.Hour)), "to": rfc(now.Add(-8*24*time.Hour + time.Hour)), "step": "1m"}), 400, "invalid_range"},
		{"bad step", metricPath("fleet.connections", map[string]string{"step": "5m"}), 400, "invalid_step"},
		{"too many points", metricPath("fleet.connections", map[string]string{"from": rfc(now.Add(-48 * time.Hour)), "step": "1m"}), 400, "too_many_points"},
		{"beyond retention", metricPath("fleet.connections", map[string]string{"from": rfc(now.Add(-401 * 24 * time.Hour))}), 400, "invalid_range"},
	}
	for _, c := range cases {
		r := e.get(t, c.path, e.superTok)
		if r.code != c.code || readErrCode(t, r) != c.errStr {
			t.Errorf("%s: %d %s, want %d %s", c.name, r.code, r.body, c.code, c.errStr)
		}
	}
	// Unknown series answers 404 before authorization, to relay-ops too: it
	// reveals nothing a role check would hide.
	if r := e.get(t, metricPath("tenant.nonsense", nil), e.opsTok); r.code != 404 {
		t.Fatalf("relay-ops unknown series = %d", r.code)
	}
}
