package provider_test

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Usage reads (Sprint 25 U2, F33; D-57, D-59, D-60): super-admin only,
// audited and fail-closed; buckets with completeness; a purged tenant named
// by its snapshot; the fleet list sorted and paged.

type usageResp struct {
	Tenant struct {
		ID       string  `json:"id"`
		Name     string  `json:"name"`
		Slug     string  `json:"slug"`
		PlanName *string `json:"plan_name"`
		Purged   bool    `json:"purged"`
	} `json:"tenant"`
	Granularity string `json:"granularity"`
	Buckets     []struct {
		Start            time.Time        `json:"start"`
		Complete         bool             `json:"complete"`
		CollectedMinutes int              `json:"collected_minutes"`
		Minutes          int              `json:"minutes"`
		Meters           map[string]int64 `json:"meters"`
		ActiveUsers      *int64           `json:"active_users"`
	} `json:"buckets"`
}

type fleetResp struct {
	Month            time.Time `json:"month"`
	Complete         bool      `json:"complete"`
	CollectedMinutes int       `json:"collected_minutes"`
	Sort             string    `json:"sort"`
	Items            []struct {
		Tenant struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Purged bool   `json:"purged"`
		} `json:"tenant"`
		Meters      map[string]int64 `json:"meters"`
		ActiveUsers int64            `json:"active_users"`
	} `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

func usagePath(params map[string]string) string {
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	return "/provider/usage?" + q.Encode()
}

func (e *readEnv) usage(t *testing.T, tenant, meter string, hour time.Time, n int64) {
	t.Helper()
	exec(t, e.pool, `INSERT INTO usage_hourly (tenant_id, meter, hour, quantity) VALUES ($1, $2, $3, $4)`, tenant, meter, hour, n)
}

// collectHour records every minute of an hour as collected (except skip).
func (e *readEnv) collectHour(t *testing.T, hour time.Time, skip int) {
	t.Helper()
	exec(t, e.pool, `INSERT INTO usage_collector_minutes (minute)
		SELECT $1::timestamptz + make_interval(mins => g) FROM generate_series(0, 59 - $2::int) g`, hour, skip)
}

func TestUsageReads_SuperAdminOnly(t *testing.T) {
	e := newReadEnv(t)
	for _, p := range []string{usagePath(map[string]string{"tenant_id": e.tenantID}), "/provider/usage/tenants"} {
		before := e.totalAudit(t)
		if r := e.get(t, p, e.opsTok); r.code != 403 {
			t.Errorf("relay-ops %s = %d, want 403 (%s)", p, r.code, r.body)
		}
		if r := e.get(t, p, e.tenantJWT); r.code != 401 {
			t.Errorf("tenant JWT %s = %d, want 401", p, r.code)
		}
		if n := e.totalAudit(t); n != before {
			t.Errorf("%s: a refused read wrote %d audit rows", p, n-before)
		}
		if r := e.get(t, p, e.superTok); r.code != 200 {
			t.Errorf("super-admin %s = %d %s", p, r.code, r.body)
		}
	}
}

func TestUsageReads_TenantBucketsCompletenessAndAudit(t *testing.T) {
	e := newReadEnv(t)
	day := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	// Nov 20 10:00: complete hour; 11:00: one minute missed (incomplete).
	e.collectHour(t, day.Add(10*time.Hour), 0)
	e.collectHour(t, day.Add(11*time.Hour), 1)
	e.usage(t, e.tenantID, "relay_bytes_to_clients", day.Add(10*time.Hour), 1000)
	e.usage(t, e.tenantID, "relay_bytes_to_clients", day.Add(11*time.Hour), 500)
	e.usage(t, e.tenantID, "session_minutes", day.Add(10*time.Hour), 60)
	// Another tenant's usage never shows.
	other := scalar[string](t, e.pool, `INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ('other','Other','active','other.zecurity.test') RETURNING id::text`)
	e.usage(t, other, "relay_bytes_to_clients", day.Add(10*time.Hour), 99999)

	before := e.auditCount(t, "tenant.usage.read")
	r := e.get(t, usagePath(map[string]string{"tenant_id": e.tenantID, "granularity": "hour",
		"from": day.Add(10 * time.Hour).Format(time.RFC3339), "to": day.Add(12 * time.Hour).Format(time.RFC3339)}), e.superTok)
	if r.code != 200 {
		t.Fatalf("%d %s", r.code, r.body)
	}
	var u usageResp
	if err := json.Unmarshal([]byte(r.body), &u); err != nil {
		t.Fatal(err)
	}
	if u.Tenant.Name != "Acme" || u.Tenant.Purged || len(u.Buckets) != 2 {
		t.Fatalf("tenant %+v, %d buckets", u.Tenant, len(u.Buckets))
	}
	b0, b1 := u.Buckets[0], u.Buckets[1]
	if !b0.Complete || b0.Meters["relay_bytes_to_clients"] != 1000 || b0.Meters["session_minutes"] != 60 || b0.Meters["shield_minutes"] != 0 {
		t.Errorf("10:00 bucket %+v", b0)
	}
	if b1.Complete || b1.CollectedMinutes != 59 || b1.Minutes != 60 || b1.Meters["relay_bytes_to_clients"] != 500 {
		t.Errorf("11:00 bucket %+v (one minute missed: incomplete)", b1)
	}
	if b0.ActiveUsers != nil {
		t.Error("active users on an hour bucket")
	}
	if n := e.auditCount(t, "tenant.usage.read"); n != before+1 {
		t.Fatalf("tenant.usage.read rows %d → %d, want +1", before, n)
	}
	details := scalar[string](t, e.pool, `SELECT target_type || ':' || target_id || ':' || (details->>'granularity') FROM provider_audit_logs WHERE action = 'tenant.usage.read' ORDER BY created_at DESC LIMIT 1`)
	if details != "tenant:"+e.tenantID+":hour" {
		t.Errorf("audit row %q", details)
	}

	// Day granularity aligns the window to whole UTC days; the day is incomplete.
	r = e.get(t, usagePath(map[string]string{"tenant_id": e.tenantID, "granularity": "day",
		"from": day.Add(5 * time.Hour).Format(time.RFC3339), "to": day.Add(6 * time.Hour).Format(time.RFC3339)}), e.superTok)
	u = usageResp{}
	_ = json.Unmarshal([]byte(r.body), &u)
	if r.code != 200 || len(u.Buckets) != 1 || !u.Buckets[0].Start.Equal(day) || u.Buckets[0].Complete || u.Buckets[0].Meters["relay_bytes_to_clients"] != 1500 {
		t.Fatalf("day read %d %s", r.code, r.body)
	}
}

func TestUsageReads_MonthActiveUsersAndPurgedTenant(t *testing.T) {
	e := newReadEnv(t)
	nov := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	exec(t, e.pool, `INSERT INTO usage_active_users VALUES ($1, '2026-11-01', gen_random_uuid(), now()), ($1, '2026-11-01', gen_random_uuid(), now())`, e.tenantID)
	r := e.get(t, usagePath(map[string]string{"tenant_id": e.tenantID, "granularity": "month",
		"from": nov.Format(time.RFC3339), "to": nov.AddDate(0, 1, 0).Format(time.RFC3339)}), e.superTok)
	var u usageResp
	_ = json.Unmarshal([]byte(r.body), &u)
	if r.code != 200 || len(u.Buckets) != 1 || u.Buckets[0].ActiveUsers == nil || *u.Buckets[0].ActiveUsers != 2 {
		t.Fatalf("month read %d %s", r.code, r.body)
	}

	// A purged tenant: no workspace, only its snapshot, usage and counts.
	purged := "dddddddd-0000-0000-0000-0000000000d1"
	exec(t, e.pool, `INSERT INTO usage_tenants (tenant_id, name, slug, plan_name, purged_at) VALUES ($1, 'Gone Ltd', 'gone', 'Free', now())`, purged)
	exec(t, e.pool, `INSERT INTO usage_active_user_counts VALUES ($1, '2026-11-01', 7)`, purged)
	e.usage(t, purged, "connector_minutes", nov.Add(time.Hour), 120)
	r = e.get(t, usagePath(map[string]string{"tenant_id": purged, "granularity": "month",
		"from": nov.Format(time.RFC3339), "to": nov.AddDate(0, 1, 0).Format(time.RFC3339)}), e.superTok)
	u = usageResp{}
	_ = json.Unmarshal([]byte(r.body), &u)
	if r.code != 200 || !u.Tenant.Purged || u.Tenant.Name != "Gone Ltd" || *u.Buckets[0].ActiveUsers != 7 || u.Buckets[0].Meters["connector_minutes"] != 120 {
		t.Fatalf("purged tenant read %d %s", r.code, r.body)
	}

	// Unknown tenant: 404, not audited.
	before := e.auditCount(t, "tenant.usage.read")
	if r := e.get(t, usagePath(map[string]string{"tenant_id": "eeeeeeee-0000-0000-0000-0000000000e1"}), e.superTok); r.code != 404 {
		t.Fatalf("unknown tenant = %d", r.code)
	}
	if n := e.auditCount(t, "tenant.usage.read"); n != before {
		t.Fatal("a 404 usage read was audited")
	}
}

func TestUsageReads_BadRequests(t *testing.T) {
	e := newReadEnv(t)
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		path, code string
	}{
		{usagePath(map[string]string{"tenant_id": "not-a-uuid"}), "invalid_id"},
		{usagePath(map[string]string{"tenant_id": e.tenantID, "granularity": "week"}), "invalid_granularity"},
		{usagePath(map[string]string{"tenant_id": e.tenantID, "from": "yesterday"}), "invalid_range"},
		// Over the limits: hour ≤ 31 days, day ≤ 400 days, month ≤ 25 months.
		{usagePath(map[string]string{"tenant_id": e.tenantID, "granularity": "hour", "from": from.Format(time.RFC3339), "to": from.AddDate(0, 0, 32).Format(time.RFC3339)}), "invalid_range"},
		{usagePath(map[string]string{"tenant_id": e.tenantID, "granularity": "day", "from": from.Format(time.RFC3339), "to": from.AddDate(0, 0, 401).Format(time.RFC3339)}), "invalid_range"},
		{usagePath(map[string]string{"tenant_id": e.tenantID, "granularity": "month", "from": from.Format(time.RFC3339), "to": from.AddDate(0, 26, 0).Format(time.RFC3339)}), "invalid_range"},
		{"/provider/usage/tenants?month=2026-13", "invalid_month"},
		{"/provider/usage/tenants?sort=bytes", "invalid_sort"},
		{"/provider/usage/tenants?cursor=bad", "invalid_cursor"},
		{"/provider/usage/tenants?cursor=" + url.QueryEscape(base64.RawURLEncoding.EncodeToString([]byte("5|not-a-uuid"))), "invalid_cursor"},
	}
	before := e.totalAudit(t)
	for _, c := range cases {
		r := e.get(t, c.path, e.superTok)
		if r.code != 400 || !strings.Contains(r.body, c.code) {
			t.Errorf("%s = %d %s, want 400 %s", c.path, r.code, r.body, c.code)
		}
	}
	if n := e.totalAudit(t); n != before {
		t.Errorf("rejected reads wrote %d audit rows", n-before)
	}
}

func TestUsageReads_FailClosedWhenAuditFails(t *testing.T) {
	e := newReadEnv(t)
	e.usage(t, e.tenantID, "relay_bytes_to_clients", time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC), 1000)
	e.db.failExec = true
	for _, p := range []string{usagePath(map[string]string{"tenant_id": e.tenantID}), "/provider/usage/tenants"} {
		r := e.get(t, p, e.superTok)
		if r.code != 500 || strings.Contains(r.body, "relay_bytes") || strings.Contains(r.body, "Acme") {
			t.Errorf("%s with a failing audit = %d %s, want 500 without data", p, r.code, r.body)
		}
	}
}

func TestUsageReads_FleetSortedPagedAndAudited(t *testing.T) {
	e := newReadEnv(t)
	month := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	b := scalar[string](t, e.pool, `INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ('beta','Beta','active','beta.zecurity.test') RETURNING id::text`)
	purged := "dddddddd-0000-0000-0000-0000000000d1"
	exec(t, e.pool, `INSERT INTO usage_tenants (tenant_id, name, slug, purged_at) VALUES ($1, 'Gone Ltd', 'gone', now())`, purged)
	e.usage(t, e.tenantID, "relay_bytes_to_clients", month.Add(time.Hour), 300)
	e.usage(t, b, "relay_bytes_to_clients", month.Add(2*time.Hour), 500)
	e.usage(t, purged, "relay_bytes_to_clients", month.Add(3*time.Hour), 400)
	e.usage(t, e.tenantID, "relay_bytes_to_clients", month.AddDate(0, 1, 0), 99999) // November: not October
	exec(t, e.pool, `INSERT INTO usage_active_users VALUES ($1, '2026-10-01', gen_random_uuid(), now())`, e.tenantID)

	before := e.auditCount(t, "usage.fleet.read")
	var names []string
	cursor := ""
	for page := 0; page < 5; page++ {
		p := "/provider/usage/tenants?month=2026-10&limit=2"
		if cursor != "" {
			p += "&cursor=" + url.QueryEscape(cursor)
		}
		r := e.get(t, p, e.superTok)
		if r.code != 200 {
			t.Fatalf("%d %s", r.code, r.body)
		}
		var f fleetResp
		if err := json.Unmarshal([]byte(r.body), &f); err != nil {
			t.Fatal(err)
		}
		if f.Sort != "relay_bytes_to_clients" || !f.Month.Equal(month) || f.Complete {
			t.Fatalf("page header %+v", f)
		}
		for _, it := range f.Items {
			names = append(names, it.Tenant.Name)
			want := map[string]int64{"Acme": 300, "Beta": 500, "Gone Ltd": 400}[it.Tenant.Name]
			if got := it.Meters["relay_bytes_to_clients"]; got != want {
				t.Errorf("%s October bytes = %d, want %d (only October counts)", it.Tenant.Name, got, want)
			}
			if it.Tenant.Name == "Gone Ltd" && !it.Tenant.Purged {
				t.Error("purged tenant not marked")
			}
		}
		if f.NextCursor == nil {
			break
		}
		cursor = *f.NextCursor
	}
	// Largest first; Acme 300, Beta 500, Gone 400; no repeats across pages.
	if strings.Join(names, ",") != "Beta,Gone Ltd,Acme" {
		t.Fatalf("order %v", names)
	}
	if n := e.auditCount(t, "usage.fleet.read"); n != before+2 {
		t.Fatalf("usage.fleet.read rows %d → %d, want one per request (2)", before, n)
	}
	// Sorted by active users: Acme first.
	r := e.get(t, "/provider/usage/tenants?month=2026-10&sort=active_users&limit=1", e.superTok)
	var f fleetResp
	_ = json.Unmarshal([]byte(r.body), &f)
	if r.code != 200 || len(f.Items) != 1 || f.Items[0].Tenant.Name != "Acme" || f.Items[0].ActiveUsers != 1 {
		t.Fatalf("by active users %d %s", r.code, r.body)
	}
}
