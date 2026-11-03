package providerquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedWorkspace inserts a bare workspace (no remote network) and returns its id.
func seedWorkspace(t *testing.T, pool *pgxpool.Pool, slug, status string, createdAt time.Time) string {
	t.Helper()
	return mustScalar[string](t, pool,
		`INSERT INTO workspaces (slug, name, status, trust_domain, created_at)
		 VALUES ($1, 'Tenant ' || $1, $2, $1 || '.zecurity.test', $3) RETURNING id::text`, slug, status, createdAt)
}

func seedNetwork(t *testing.T, pool *pgxpool.Pool, tenant, name, status string, createdAt time.Time) string {
	t.Helper()
	return mustScalar[string](t, pool,
		`INSERT INTO remote_networks (tenant_id, name, location, status, created_at) VALUES ($1,$2,'office',$3,$4) RETURNING id::text`,
		tenant, name, status, createdAt)
}

func seedConnector(t *testing.T, pool *pgxpool.Pool, tenant, network, name, status string, createdAt time.Time) string {
	t.Helper()
	return mustScalar[string](t, pool,
		`INSERT INTO connectors (tenant_id, remote_network_id, name, status, created_at) VALUES ($1,$2,$3,$4,$5) RETURNING id::text`,
		tenant, network, name, status, createdAt)
}

func seedShield(t *testing.T, pool *pgxpool.Pool, tenant, network, connector, name, status string, createdAt time.Time) string {
	t.Helper()
	return mustScalar[string](t, pool,
		`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status, created_at) VALUES ($1,$2,$3,$4,$5,$6) RETURNING id::text`,
		tenant, network, connector, name, status, createdAt)
}

func seedUser(t *testing.T, pool *pgxpool.Pool, tenant, sub, status string) string {
	t.Helper()
	return mustScalar[string](t, pool,
		`INSERT INTO users (tenant_id, email, provider, provider_sub, status) VALUES ($1, $2 || '@tenant.test', 'google', $2, $3) RETURNING id::text`,
		tenant, sub, status)
}

func seedDevice(t *testing.T, pool *pgxpool.Pool, tenant, user, status string, revoked bool) {
	t.Helper()
	var revokedAt any
	if revoked {
		revokedAt = t0
	}
	mustExec(t, pool, `INSERT INTO client_devices (user_id, workspace_id, name, os, status, revoked_at) VALUES ($1,$2,'dev','linux',$3,$4)`,
		user, tenant, status, revokedAt)
}

func tenantIDs(items []TenantSummary) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = s.ID
	}
	return out
}

func TestListTenants_StatusFilter(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	byStatus := map[string]string{}
	for i, st := range TenantStatuses {
		byStatus[st] = seedWorkspace(t, pool, "t-"+st, st, t0.Add(time.Duration(i)*time.Minute))
	}

	page, err := ListTenants(ctx, pool, TenantFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("default list has %d tenants, want 3 (deleted excluded)", len(page.Items))
	}
	for _, s := range page.Items {
		if s.Status == "deleted" {
			t.Fatal("deleted tenant in default list")
		}
	}
	for _, st := range TenantStatuses {
		page, err := ListTenants(ctx, pool, TenantFilter{Status: st})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Items[0].ID != byStatus[st] || page.Items[0].Status != st {
			t.Errorf("status=%s: got %+v", st, page.Items)
		}
	}
	for _, bad := range []string{"Active", "disabled", "' OR 1=1 --"} {
		if _, err := ListTenants(ctx, pool, TenantFilter{Status: bad}); !errors.Is(err, ErrInvalidFilter) {
			t.Errorf("status %q: err = %v, want ErrInvalidFilter", bad, err)
		}
	}
}

func TestListTenants_KeysetPagination(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		seedWorkspace(t, pool, fmt.Sprintf("p%d", i), "active", t0.Add(time.Duration(i)*time.Second))
	}
	tie := t0.Add(time.Hour)
	seedWorkspace(t, pool, "tie-a", "active", tie)
	seedWorkspace(t, pool, "tie-b", "active", tie)
	seedWorkspace(t, pool, "gone", "deleted", t0.Add(2*time.Hour)) // never paged by default

	var seen []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		page, err := ListTenants(ctx, pool, TenantFilter{Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > 3 {
			t.Fatalf("page size %d > limit", len(page.Items))
		}
		seen = append(seen, tenantIDs(page.Items)...)
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}

	rows, err := pool.Query(ctx, `SELECT id::text FROM workspaces WHERE status <> 'deleted' ORDER BY created_at DESC, id DESC`)
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
	if !reflect.DeepEqual(seen, want) || len(seen) != 9 {
		t.Fatalf("pages = %v\nwant    %v", seen, want)
	}

	// Exactly one full page: no next cursor.
	page, err := ListTenants(ctx, pool, TenantFilter{Limit: 9})
	if err != nil || len(page.Items) != 9 || page.NextCursor != nil {
		t.Fatalf("exact page: %d items, next=%v, err %v", len(page.Items), page.NextCursor != nil, err)
	}
	if _, err := ListTenants(ctx, pool, TenantFilter{Cursor: "not-a-cursor"}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("bad cursor: err = %v", err)
	}
}

func TestListTenants_LimitClampedAt200(t *testing.T) {
	pool := newTestDB(t)
	mustExec(t, pool, `INSERT INTO workspaces (slug, name, status, trust_domain, created_at)
	                   SELECT 'bulk-' || g, 'Bulk ' || g, 'active', 'bulk-' || g || '.zecurity.test', $1::timestamptz + g * interval '1 second'
	                     FROM generate_series(1, 205) g`, t0)
	page, err := ListTenants(context.Background(), pool, TenantFilter{Limit: 5000})
	if err != nil || len(page.Items) != MaxLimit || page.NextCursor == nil {
		t.Fatalf("got %d items, next=%v, err %v; want %d and a cursor", len(page.Items), page.NextCursor != nil, err, MaxLimit)
	}
	page, err = ListTenants(context.Background(), pool, TenantFilter{})
	if err != nil || len(page.Items) != DefaultLimit {
		t.Fatalf("default limit: %d items, err %v", len(page.Items), err)
	}
}

// seedCountedTenant builds a tenant with a known mix of everything counted.
func seedCountedTenant(t *testing.T, pool *pgxpool.Pool, slug string, createdAt time.Time) string {
	t.Helper()
	id := seedWorkspace(t, pool, slug, "active", createdAt)
	n1 := seedNetwork(t, pool, id, "hq", "active", t0)
	seedNetwork(t, pool, id, "branch", "active", t0.Add(time.Minute))
	seedNetwork(t, pool, id, "old", "deleted", t0.Add(2*time.Minute))
	var conn string
	for i, st := range []string{"pending", "active", "active", "disconnected", "revoked"} {
		conn = seedConnector(t, pool, id, n1, fmt.Sprintf("c%d", i), st, t0.Add(time.Duration(i)*time.Second))
	}
	for i, st := range []string{"active", "active", "active", "disconnected", "revoked", "revoked", "pending"} {
		seedShield(t, pool, id, n1, conn, fmt.Sprintf("s%d", i), st, t0.Add(time.Duration(i)*time.Second))
	}
	var u string
	for i, st := range []string{"active", "active", "active", "suspended", "locked", "deleted", "deleted"} {
		u = seedUser(t, pool, id, fmt.Sprintf("%s-u%d", slug, i), st)
	}
	seedDevice(t, pool, id, u, "active", false)
	seedDevice(t, pool, id, u, "active", false)
	seedDevice(t, pool, id, u, "re_enroll_required", false)
	seedDevice(t, pool, id, u, "renew_pending", false)
	seedDevice(t, pool, id, u, "active", true) // revoked wins over status
	seedDevice(t, pool, id, u, "renew_pending", true)
	mustExec(t, pool, `INSERT INTO workspace_ca_keys (tenant_id, encrypted_private_key, nonce, certificate_pem, not_before, not_after)
	                   VALUES ($1, 'enc', 'nonce', 'pem', $2, $3)`, id, t0, t0.Add(365*24*time.Hour))
	return id
}

var wantCounts = TenantCounts{
	Connectors:     AgentCounts{Pending: 1, Active: 2, Disconnected: 1, Revoked: 1},
	Shields:        AgentCounts{Pending: 1, Active: 3, Disconnected: 1, Revoked: 2},
	RemoteNetworks: 2,
	Users:          UserCounts{Active: 3, Suspended: 1, Locked: 1, Deleted: 2},
	ClientDevices:  DeviceCounts{Active: 2, ReEnrollRequired: 1, RenewPending: 1, Revoked: 2},
}

func TestListTenants_Counts(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	a := seedCountedTenant(t, pool, "acme", t0)
	b := seedCountedTenant(t, pool, "beta", t0.Add(time.Minute)) // same mix; must not leak into acme's counts
	empty := seedWorkspace(t, pool, "empty", "provisioning", t0.Add(2*time.Minute))

	page, err := ListTenants(ctx, pool, TenantFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]TenantSummary{}
	for _, s := range page.Items {
		got[s.ID] = s
	}
	for _, id := range []string{a, b} {
		if got[id].Counts != wantCounts {
			t.Errorf("tenant %s counts = %+v\nwant %+v", id, got[id].Counts, wantCounts)
		}
		if got[id].CANotAfter == nil || !got[id].CANotAfter.Equal(t0.Add(365*24*time.Hour)) {
			t.Errorf("tenant %s ca_not_after = %v", id, got[id].CANotAfter)
		}
	}
	if got[empty].Counts != (TenantCounts{}) || got[empty].CANotAfter != nil {
		t.Errorf("empty tenant = %+v, want zero counts and no CA", got[empty])
	}
	s := got[a]
	if s.Slug != "acme" || s.Name != "Tenant acme" || s.Status != "active" || s.TrustDomain != "acme.zecurity.test" ||
		!s.CreatedAt.Equal(t0) || s.CreatedAt.Location() != time.UTC {
		t.Errorf("summary fields = %+v", s)
	}
}

// No N+1: a page of tenants and all their counts is one statement, and a
// detail read is a fixed four, however much data the tenants hold.
func TestTenantQueries_FixedStatementCount(t *testing.T) {
	pool := newTestDB(t)
	var first string
	for i := 0; i < 12; i++ {
		id := seedCountedTenant(t, pool, fmt.Sprintf("n%d", i), t0.Add(time.Duration(i)*time.Minute))
		if i == 0 {
			first = id
		}
	}
	cq := &countingQuerier{Querier: pool}
	page, err := ListTenants(context.Background(), cq, TenantFilter{Limit: 200})
	if err != nil || len(page.Items) != 12 {
		t.Fatalf("list: %d items, err %v", len(page.Items), err)
	}
	if cq.n != 1 {
		t.Fatalf("ListTenants ran %d statements for 12 tenants, want 1", cq.n)
	}
	for _, s := range page.Items {
		if s.Counts != wantCounts {
			t.Fatalf("counts wrong under the single query: %+v", s.Counts)
		}
	}
	cq.n = 0
	if _, err := GetTenant(context.Background(), cq, first); err != nil {
		t.Fatal(err)
	}
	if cq.n != 4 {
		t.Fatalf("GetTenant ran %d statements, want 4 (tenant+counts, networks, connectors, shields)", cq.n)
	}
}

func TestGetTenant_NotFoundAndInvalidID(t *testing.T) {
	pool := newTestDB(t)
	if _, err := GetTenant(context.Background(), pool, "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: err = %v", err)
	}
	for _, bad := range []string{"", "acme", "1' OR '1'='1", "0f1e2d3c4b5a69788796a5b4c3d2e1f0"} {
		if _, err := GetTenant(context.Background(), pool, bad); !errors.Is(err, ErrInvalidID) {
			t.Errorf("id %q: err = %v, want ErrInvalidID", bad, err)
		}
	}
}

func TestGetTenant_DetailCollections(t *testing.T) {
	pool := newTestDB(t)
	id := seedWorkspace(t, pool, "acme", "deleted", t0) // deleted tenants stay readable by id
	hq := seedNetwork(t, pool, id, "hq", "active", t0)
	old := seedNetwork(t, pool, id, "old", "deleted", t0.Add(time.Minute))
	c1 := seedConnector(t, pool, id, hq, "hq-1", "active", t0)
	c2 := seedConnector(t, pool, id, hq, "hq-2", "revoked", t0.Add(time.Minute))
	mustExec(t, pool, `UPDATE connectors SET version='0.21.0', cert_not_after=$2, last_heartbeat_at=$3 WHERE id=$1`, c1, t0.Add(48*time.Hour), t0.Add(-time.Minute))
	mustExec(t, pool, `UPDATE connectors SET revoked_at=$2 WHERE id=$1`, c2, t0.Add(time.Hour))
	s1 := seedShield(t, pool, id, hq, c1, "db-1", "active", t0)
	mustExec(t, pool, `UPDATE shields SET cert_not_after=$2, last_heartbeat_at=$3 WHERE id=$1`, s1, t0.Add(72*time.Hour), t0.Add(-2*time.Minute))
	other, otherNet := seedTenant(t, pool, "other") // must not appear in acme's detail
	seedConnector(t, pool, other, otherNet, "foreign", "active", t0)

	d, err := GetTenant(context.Background(), pool, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != id || d.Status != "deleted" || d.Counts.RemoteNetworks != 1 || d.Counts.Connectors.Active != 1 || d.Counts.Connectors.Revoked != 1 {
		t.Fatalf("summary = %+v", d.TenantSummary)
	}
	wantNets := []TenantRemoteNetwork{{ID: old, Name: "old", Status: "deleted"}, {ID: hq, Name: "hq", Status: "active"}}
	if !reflect.DeepEqual(d.RemoteNetworks, wantNets) {
		t.Errorf("remote networks = %+v\nwant %+v", d.RemoteNetworks, wantNets)
	}
	if len(d.Connectors) != 2 || d.Connectors[0].ID != c2 || d.Connectors[1].ID != c1 {
		t.Fatalf("connectors = %+v", d.Connectors)
	}
	cA := d.Connectors[1]
	if cA.Name != "hq-1" || cA.Status != "active" || cA.RemoteNetworkID != hq || *cA.Version != "0.21.0" ||
		!cA.CertNotAfter.Equal(t0.Add(48*time.Hour)) || !cA.LastHeartbeatAt.Equal(t0.Add(-time.Minute)) || cA.RevokedAt != nil {
		t.Errorf("connector = %+v", cA)
	}
	if d.Connectors[0].RevokedAt == nil || !d.Connectors[0].RevokedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("revoked connector revoked_at = %v", d.Connectors[0].RevokedAt)
	}
	wantShield := TenantShield{ID: s1, Name: "db-1", Status: "active", ConnectorID: c1, RemoteNetworkID: hq}
	got := d.Shields[0]
	if len(d.Shields) != 1 || got.ID != wantShield.ID || got.Name != wantShield.Name || got.ConnectorID != c1 ||
		got.RemoteNetworkID != hq || !got.CertNotAfter.Equal(t0.Add(72*time.Hour)) || !got.LastHeartbeatAt.Equal(t0.Add(-2*time.Minute)) {
		t.Errorf("shields = %+v", d.Shields)
	}
	if d.RemoteNetworksTruncated || d.ConnectorsTruncated || d.ShieldsTruncated {
		t.Error("small lists flagged truncated")
	}
}

func TestGetTenant_EmptyCollectionsSerializeAsArrays(t *testing.T) {
	pool := newTestDB(t)
	id := seedWorkspace(t, pool, "bare", "provisioning", t0)
	d, err := GetTenant(context.Background(), pool, id)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d)
	for _, want := range []string{`"remote_networks":[]`, `"connectors":[]`, `"shields":[]`, `"ca_not_after":null`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("detail JSON missing %s: %s", want, b)
		}
	}
}

func TestGetTenant_NestedCollectionsCapped(t *testing.T) {
	for _, n := range []int{DetailCap, DetailCap + 1} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			pool := newTestDB(t)
			id := seedWorkspace(t, pool, "big", "active", t0)
			hq := seedNetwork(t, pool, id, "hq", "active", t0.Add(-time.Hour))
			// n remote networks in total, including hq.
			mustExec(t, pool, `INSERT INTO remote_networks (tenant_id, name, location, created_at)
			                   SELECT $1, 'net-' || g, 'office', $2::timestamptz + g * interval '1 second' FROM generate_series(1, $3) g`,
				id, t0, n-1)
			mustExec(t, pool, `INSERT INTO connectors (tenant_id, remote_network_id, name, created_at)
			                   SELECT $1, $2, 'c-' || g, $3::timestamptz + g * interval '1 second' FROM generate_series(1, $4) g`, id, hq, t0, n)
			conn := mustScalar[string](t, pool, `SELECT id::text FROM connectors WHERE tenant_id=$1 LIMIT 1`, id)
			mustExec(t, pool, `INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, created_at)
			                   SELECT $1, $2, $3, 's-' || g, $4::timestamptz + g * interval '1 second' FROM generate_series(1, $5) g`, id, hq, conn, t0, n)

			d, err := GetTenant(context.Background(), pool, id)
			if err != nil {
				t.Fatal(err)
			}
			cut := n > DetailCap
			check := func(name string, got int, truncated bool) {
				if got != DetailCap || truncated != cut {
					t.Errorf("%s: %d items, truncated=%v; want %d, %v", name, got, truncated, DetailCap, cut)
				}
			}
			check("remote_networks", len(d.RemoteNetworks), d.RemoteNetworksTruncated)
			check("connectors", len(d.Connectors), d.ConnectorsTruncated)
			check("shields", len(d.Shields), d.ShieldsTruncated)
			// Newest first: the most recently created rows are kept.
			last := "-" + strconv.Itoa(n)
			if d.Connectors[0].Name != "c"+last || d.Shields[0].Name != "s"+last {
				t.Errorf("first rows = %s / %s, want the newest", d.Connectors[0].Name, d.Shields[0].Name)
			}
			// Counts are never capped.
			if d.Counts.Connectors.Pending != int64(n) || d.Counts.Shields.Pending != int64(n) || d.Counts.RemoteNetworks != int64(n) {
				t.Errorf("counts = %+v, want %d each", d.Counts, n)
			}
		})
	}
}

// OQ-2 and no-secrets: seed every identity, secret and extra network field
// with a canary value and check neither the list nor the detail response
// carries it, by key or by value.
func TestTenantResponses_NoIdentityOrSecrets(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	id := seedWorkspace(t, pool, "acme", "active", t0)
	mustExec(t, pool, `UPDATE workspaces SET ca_cert_pem='-----BEGIN CERTIFICATE-----PEMCANARY' WHERE id=$1`, id)
	mustExec(t, pool, `INSERT INTO workspace_ca_keys (tenant_id, encrypted_private_key, nonce, certificate_pem, not_before, not_after)
	                   VALUES ($1, 'ENCCANARY', 'NONCECANARY', '-----BEGIN CERTIFICATE-----CERTCANARY', $2, $3)`, id, t0, t0.Add(time.Hour))
	net := seedNetwork(t, pool, id, "hq", "active", t0)
	mustExec(t, pool, `UPDATE remote_networks SET location='aws' WHERE id=$1`, net)
	conn := seedConnector(t, pool, id, net, "c1", "active", t0)
	mustExec(t, pool, `UPDATE connectors SET enrollment_token_jti='JTICANARY', hostname='HOSTCANARY', public_ip='198.51.100.91',
	                     lan_addr='10.9.9.9:9091', cert_serial='SERIALCANARY', trust_domain='TDCANARY' WHERE id=$1`, conn)
	sh := seedShield(t, pool, id, net, conn, "s1", "active", t0)
	mustExec(t, pool, `UPDATE shields SET enrollment_token_jti='JTICANARY2', hostname='HOSTCANARY2', public_ip='198.51.100.92',
	                     lan_ip='10.8.8.8', interface_addr='100.64.0.9', cert_serial='SERIALCANARY2' WHERE id=$1`, sh)
	u := mustScalar[string](t, pool, `INSERT INTO users (tenant_id, email, provider, provider_sub, role, status, last_login_at)
	                                  VALUES ($1, 'admin.canary@tenant.example', 'google', 'SUBCANARY', 'admin', 'active', $2) RETURNING id::text`, id, t0)
	mustExec(t, pool, `INSERT INTO client_devices (user_id, workspace_id, name, os, spiffe_id, cert_serial)
	                   VALUES ($1, $2, 'OWNERCANARY laptop', 'linux', 'spiffe://acme/SPIFFECANARY', 'SERIALCANARY3')`, u, id)

	page, err := ListTenants(ctx, pool, TenantFilter{})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := GetTenant(ctx, pool, id)
	if err != nil {
		t.Fatal(err)
	}
	if page.Items[0].Counts.Users.Active != 1 || detail.Counts.ClientDevices.Active != 1 {
		t.Fatalf("counts lost the seeded user/device: %+v", detail.Counts)
	}
	forbidden := []string{
		// identity (OQ-2)
		"canary@", "@tenant.example", "subcanary", "ownercanary", "email", "provider_sub", "\"role", "last_login", "user_id", "owner",
		// secrets
		"enccanary", "noncecanary", "pemcanary", "certcanary", "begin certificate", "encrypted", "nonce", "private", "pem", "jti", "token",
		// network/identity detail outside the planned surface
		"hostcanary", "198.51.100.9", "10.9.9.9", "10.8.8.8", "100.64.0.9", "hostname", "public_ip", "lan_", "interface", "spiffe",
		"serialcanary", "cert_serial", "tdcanary", "location", "aws",
	}
	for name, v := range map[string]any{"list": page, "detail": detail} {
		b, _ := json.Marshal(v)
		body := strings.ToLower(string(b))
		for _, bad := range forbidden {
			if strings.Contains(body, bad) {
				t.Errorf("%s response contains %q", name, bad)
			}
		}
	}
}

func jsonKeys(t *testing.T, v any) []string {
	t.Helper()
	b, _ := json.Marshal(v)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// The DTOs are the allowed response surface; any new field must be a
// deliberate change to these lists.
func TestTenantDTOSurface(t *testing.T) {
	summary := []string{"ca_not_after", "counts", "created_at", "id", "name", "slug", "status", "trust_domain"}
	detail := append(append([]string{}, summary...), "connectors", "connectors_truncated", "remote_networks",
		"remote_networks_truncated", "shields", "shields_truncated")
	sort.Strings(detail)
	agent := []string{"active", "disconnected", "pending", "revoked"}
	for name, c := range map[string]struct {
		v    any
		want []string
	}{
		"TenantSummary":       {TenantSummary{}, summary},
		"TenantDetail":        {TenantDetail{}, detail},
		"TenantCounts":        {TenantCounts{}, []string{"client_devices", "connectors", "remote_networks", "shields", "users"}},
		"AgentCounts":         {AgentCounts{}, agent},
		"UserCounts":          {UserCounts{}, []string{"active", "deleted", "locked", "suspended"}},
		"DeviceCounts":        {DeviceCounts{}, []string{"active", "re_enroll_required", "renew_pending", "revoked"}},
		"TenantRemoteNetwork": {TenantRemoteNetwork{}, []string{"id", "name", "status"}},
		"TenantConnector": {TenantConnector{}, []string{"cert_not_after", "id", "last_heartbeat_at", "name",
			"remote_network_id", "revoked_at", "status", "version"}},
		"TenantShield": {TenantShield{}, []string{"cert_not_after", "connector_id", "id", "last_heartbeat_at", "name",
			"remote_network_id", "status"}},
	} {
		if got := jsonKeys(t, c.v); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s keys = %v\nwant %v", name, got, c.want)
		}
	}
}
