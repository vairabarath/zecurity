package providerquery

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type auditSeed struct {
	email, action, targetType, targetID string
	details                             any // nil → NULL
	ip                                  any // nil → NULL
	userID                              any // nil → NULL
	at                                  time.Time
}

func seedAudit(t *testing.T, pool *pgxpool.Pool, s auditSeed) string {
	t.Helper()
	var details any
	if s.details != nil {
		b, _ := json.Marshal(s.details)
		details = b
	}
	return mustScalar[string](t, pool, `
INSERT INTO provider_audit_logs (provider_user_id, provider_email, action, target_type, target_id, details, ip_address, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id::text`,
		s.userID, s.email, s.action, s.targetType, s.targetID, details, s.ip, s.at)
}

func auditIDs(items []AuditEntry) []string {
	out := make([]string, len(items))
	for i, e := range items {
		out[i] = e.ID
	}
	return out
}

func mustAudit(t *testing.T, q Querier, f AuditFilter) Page[AuditEntry] {
	t.Helper()
	p, err := QueryProviderAudit(context.Background(), q, f)
	if err != nil {
		t.Fatalf("QueryProviderAudit(%+v): %v", f, err)
	}
	return p
}

func sameSet(a, b []string) bool {
	x, y := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(x)
	sort.Strings(y)
	return reflect.DeepEqual(x, y)
}

func TestQueryProviderAudit_EmptyResult(t *testing.T) {
	pool := newTestDB(t)
	p := mustAudit(t, pool, AuditFilter{})
	if p.Items == nil || len(p.Items) != 0 || p.NextCursor != nil {
		t.Fatalf("empty page = %+v", p)
	}
	b, _ := json.Marshal(p)
	if string(b) != `{"items":[],"next_cursor":null}` {
		t.Fatalf("empty page JSON = %s", b)
	}
}

// fixture: a small, varied audit log.
type auditFixture struct {
	pool                                   *pgxpool.Pool
	login, create, roleChg, tenantRead, rc string
	tenantRead2                            string
}

func newAuditFixture(t *testing.T) auditFixture {
	pool := newTestDB(t)
	f := auditFixture{pool: pool}
	f.login = seedAudit(t, pool, auditSeed{email: "admin@provider.test", action: "provider_auth.login", targetType: "provider_user", targetID: "u-admin", at: t0, ip: "10.0.0.5"})
	f.create = seedAudit(t, pool, auditSeed{email: "admin@provider.test", action: "provider_user.create", targetType: "provider_user", targetID: "u-ops", at: t0.Add(1 * time.Minute)})
	f.roleChg = seedAudit(t, pool, auditSeed{email: "boss@provider.test", action: "provider_user.role_change", targetType: "provider_user", targetID: "u-ops", at: t0.Add(2 * time.Minute)})
	f.tenantRead = seedAudit(t, pool, auditSeed{email: "boss@provider.test", action: "tenant.read", targetType: "tenant", targetID: "t-1", details: map[string]any{"view": "detail"}, at: t0.Add(3 * time.Minute)})
	f.rc = seedAudit(t, pool, auditSeed{email: "admin@provider.test", action: "relay.create", targetType: "relay", targetID: "r-1", at: t0.Add(4 * time.Minute)})
	f.tenantRead2 = seedAudit(t, pool, auditSeed{email: "admin@provider.test", action: "tenant.read", targetType: "tenant", targetID: "t-2", details: map[string]any{"view": "detail"}, at: t0.Add(5 * time.Minute)})
	return f
}

func TestQueryProviderAudit_NewestFirst(t *testing.T) {
	f := newAuditFixture(t)
	got := auditIDs(mustAudit(t, f.pool, AuditFilter{}).Items)
	want := []string{f.tenantRead2, f.rc, f.tenantRead, f.roleChg, f.create, f.login}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v\nwant    %v", got, want)
	}
}

func TestQueryProviderAudit_EachFilter(t *testing.T) {
	f := newAuditFixture(t)
	at := func(m int) *time.Time { v := t0.Add(time.Duration(m) * time.Minute); return &v }
	cases := []struct {
		name   string
		filter AuditFilter
		want   []string
	}{
		{"action", AuditFilter{Action: "tenant.read"}, []string{f.tenantRead, f.tenantRead2}},
		{"action is exact, not a prefix", AuditFilter{Action: "tenant"}, nil},
		{"target_type", AuditFilter{TargetType: "provider_user"}, []string{f.login, f.create, f.roleChg}},
		{"target_id", AuditFilter{TargetID: "u-ops"}, []string{f.create, f.roleChg}},
		{"provider_email", AuditFilter{ProviderEmail: "boss@provider.test"}, []string{f.roleChg, f.tenantRead}},
		{"provider_email case-insensitive", AuditFilter{ProviderEmail: "BOSS@Provider.Test"}, []string{f.roleChg, f.tenantRead}},
		{"since is inclusive", AuditFilter{Since: at(4)}, []string{f.rc, f.tenantRead2}},
		{"until is exclusive", AuditFilter{Until: at(2)}, []string{f.login, f.create}},
		{"since and until", AuditFilter{Since: at(1), Until: at(4)}, []string{f.create, f.roleChg, f.tenantRead}},
		{"equal since and until is empty", AuditFilter{Since: at(3), Until: at(3)}, nil},
		// combinations
		{"action + target_id", AuditFilter{Action: "tenant.read", TargetID: "t-2"}, []string{f.tenantRead2}},
		{"target_type + target_id", AuditFilter{TargetType: "provider_user", TargetID: "u-ops"}, []string{f.create, f.roleChg}},
		{"email + time range", AuditFilter{ProviderEmail: "admin@provider.test", Since: at(1), Until: at(5)}, []string{f.create, f.rc}},
		{"all six", AuditFilter{Action: "tenant.read", TargetType: "tenant", TargetID: "t-1", ProviderEmail: "boss@provider.test", Since: at(0), Until: at(10)}, []string{f.tenantRead}},
		{"no match", AuditFilter{Action: "tenant.read", ProviderEmail: "nobody@provider.test"}, nil},
		{"injection-shaped value is just a string", AuditFilter{Action: "' OR 1=1 --"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := auditIDs(mustAudit(t, f.pool, c.filter).Items)
			if !sameSet(got, c.want) {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestQueryProviderAudit_KeysetPagination(t *testing.T) {
	pool := newTestDB(t)
	for i := 0; i < 7; i++ {
		seedAudit(t, pool, auditSeed{email: "a@provider.test", action: "relay.create", targetType: "relay", targetID: "r", at: t0.Add(time.Duration(i) * time.Second)})
	}
	tie := t0.Add(time.Hour)
	seedAudit(t, pool, auditSeed{email: "a@provider.test", action: "relay.create", targetType: "relay", targetID: "tie", at: tie})
	seedAudit(t, pool, auditSeed{email: "a@provider.test", action: "relay.create", targetType: "relay", targetID: "tie", at: tie})

	var seen []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		p := mustAudit(t, pool, AuditFilter{Limit: 2, Cursor: cursor})
		if len(p.Items) > 2 {
			t.Fatalf("page size %d", len(p.Items))
		}
		seen = append(seen, auditIDs(p.Items)...)
		if pages == 0 {
			// A row written after the first page is newer than the cursor:
			// it must not appear on later pages (no duplicates, no shifting).
			seedAudit(t, pool, auditSeed{email: "a@provider.test", action: "relay.create", targetType: "relay", targetID: "late", at: t0.Add(2 * time.Hour)})
		}
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
	}
	rows, err := pool.Query(context.Background(), `SELECT id::text FROM provider_audit_logs WHERE target_id <> 'late' ORDER BY created_at DESC, id DESC`)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		want = append(want, id)
	}
	rows.Close()
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("pages = %v\nwant    %v", seen, want)
	}

	// Filters apply on every page.
	var filtered []string
	cursor = ""
	for {
		p := mustAudit(t, pool, AuditFilter{TargetID: "tie", Limit: 1, Cursor: cursor})
		filtered = append(filtered, auditIDs(p.Items)...)
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
	}
	if len(filtered) != 2 {
		t.Fatalf("filtered pagination returned %d rows, want 2", len(filtered))
	}
}

func TestQueryProviderAudit_Limits(t *testing.T) {
	pool := newTestDB(t)
	mustExec(t, pool, `INSERT INTO provider_audit_logs (provider_email, action, target_type, target_id, created_at)
	                   SELECT 'a@provider.test', 'relay.create', 'relay', 'r-' || g, $1::timestamptz + g * interval '1 second'
	                     FROM generate_series(1, 205) g`, t0)
	if p := mustAudit(t, pool, AuditFilter{}); len(p.Items) != DefaultLimit || p.NextCursor == nil {
		t.Fatalf("default: %d items", len(p.Items))
	}
	if p := mustAudit(t, pool, AuditFilter{Limit: 9999}); len(p.Items) != MaxLimit || p.NextCursor == nil {
		t.Fatalf("clamped: %d items", len(p.Items))
	}
	if p := mustAudit(t, pool, AuditFilter{Limit: 205}); len(p.Items) != MaxLimit {
		t.Fatalf("205 requested: %d items, want %d", len(p.Items), MaxLimit)
	}
}

func TestQueryProviderAudit_InvalidInput(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	if _, err := QueryProviderAudit(ctx, pool, AuditFilter{Cursor: "%%%"}); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("bad cursor: %v", err)
	}
	later, earlier := t0.Add(time.Hour), t0
	if _, err := QueryProviderAudit(ctx, pool, AuditFilter{Since: &later, Until: &earlier}); !errors.Is(err, ErrInvalidFilter) {
		t.Errorf("since after until: %v", err)
	}
	long := strings.Repeat("x", AuditFilterMaxLen+1)
	for _, f := range []AuditFilter{{Action: long}, {TargetType: long}, {TargetID: long}, {ProviderEmail: long}} {
		if _, err := QueryProviderAudit(ctx, pool, f); !errors.Is(err, ErrInvalidFilter) {
			t.Errorf("over-long filter %+v: %v", f, err)
		}
	}
	if _, err := QueryProviderAudit(ctx, pool, AuditFilter{Action: strings.Repeat("x", AuditFilterMaxLen)}); err != nil {
		t.Errorf("filter at the max length rejected: %v", err)
	}
}

func TestQueryProviderAudit_FieldsAndOneStatement(t *testing.T) {
	pool := newTestDB(t)
	uid := mustScalar[string](t, pool, `INSERT INTO provider_users (email, role) VALUES ('admin@provider.test', 'super-admin') RETURNING id::text`)
	withAll := seedAudit(t, pool, auditSeed{email: "admin@provider.test", action: "tenant.read", targetType: "tenant", targetID: "t-1",
		details: map[string]any{"view": "detail"}, ip: "10.0.0.5", userID: uid, at: t0})
	bare := seedAudit(t, pool, auditSeed{email: "system:bootstrap", action: "provider_user.bootstrap", targetType: "provider_user", targetID: "u-1", at: t0.Add(time.Minute)})

	cq := &countingQuerier{Querier: pool}
	p := mustAudit(t, cq, AuditFilter{})
	if cq.n != 1 {
		t.Fatalf("ran %d statements, want 1", cq.n)
	}
	byID := map[string]AuditEntry{}
	for _, e := range p.Items {
		byID[e.ID] = e
	}
	e := byID[withAll]
	if e.ProviderUserID == nil || *e.ProviderUserID != uid || e.ProviderEmail != "admin@provider.test" || e.Action != "tenant.read" ||
		e.TargetType != "tenant" || e.TargetID != "t-1" || *e.IPAddress != "10.0.0.5" || !e.CreatedAt.Equal(t0) ||
		e.CreatedAt.Location() != time.UTC {
		t.Fatalf("entry = %+v", e)
	}
	var details map[string]any
	if err := json.Unmarshal(e.Details, &details); err != nil || !reflect.DeepEqual(details, map[string]any{"view": "detail"}) {
		t.Fatalf("details = %s (%v)", e.Details, err)
	}
	b, _ := json.Marshal(byID[bare])
	for _, want := range []string{`"provider_user_id":null`, `"details":null`, `"ip_address":null`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("bare entry JSON missing %s: %s", want, b)
		}
	}
}

// Only provider operator audit rows are returned: tenant audit_logs rows are
// never read, and nothing from provider_users (password_hash etc.) is joined.
func TestQueryProviderAudit_SeparationAndNoSecrets(t *testing.T) {
	pool := newTestDB(t)
	tenant := seedWorkspace(t, pool, "acme", "active", t0)
	mustExec(t, pool, `INSERT INTO audit_logs (tenant_id, actor_email, action, target_type, target_id, details)
	                   VALUES ($1, 'tenant.canary@acme.example', 'resource.force_delete', 'resource', 'TENANTCANARY', '{"x":"TENANTCANARY"}')`, tenant)
	uid := mustScalar[string](t, pool, `INSERT INTO provider_users (email, role, password_hash) VALUES ('admin@provider.test', 'super-admin', 'HASHCANARY') RETURNING id::text`)
	seedAudit(t, pool, auditSeed{email: "admin@provider.test", action: "provider_auth.login", targetType: "provider_user", targetID: uid, userID: uid, at: t0})

	p := mustAudit(t, pool, AuditFilter{})
	if len(p.Items) != 1 || p.Items[0].Action != "provider_auth.login" {
		t.Fatalf("items = %+v, want only the provider row", p.Items)
	}
	// Even an exact filter for the tenant row finds nothing.
	if q := mustAudit(t, pool, AuditFilter{Action: "resource.force_delete"}); len(q.Items) != 0 {
		t.Fatalf("tenant audit row returned: %+v", q.Items)
	}
	b, _ := json.Marshal(p)
	body := strings.ToLower(string(b))
	for _, bad := range []string{"tenantcanary", "tenant.canary", "hashcanary", "password", "hash", "token", "secret"} {
		if strings.Contains(body, bad) {
			t.Errorf("audit response contains %q: %s", bad, b)
		}
	}
}

func TestAuditDTOSurface(t *testing.T) {
	want := []string{"action", "created_at", "details", "id", "ip_address", "provider_email", "provider_user_id", "target_id", "target_type"}
	if got := jsonKeys(t, AuditEntry{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("AuditEntry keys = %v\nwant %v", got, want)
	}
}
