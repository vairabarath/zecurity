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

	"github.com/jackc/pgx/v5"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func ids(items []RelaySummary) []string {
	out := make([]string, len(items))
	for i, s := range items {
		out[i] = s.ID
	}
	return out
}

func TestListRelays_StatusFilter(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	byStatus := map[string]string{}
	for i, st := range RelayStatuses {
		byStatus[st] = seedRelay(t, pool, "relay-"+st, st, t0.Add(time.Duration(i)*time.Minute))
	}

	page, err := ListRelays(ctx, pool, nil, RelayFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, s := range page.Items {
		got[s.Status] = true
	}
	if got["deleted"] || len(page.Items) != 4 {
		t.Fatalf("default list = %v, want every status except deleted", got)
	}

	for _, st := range RelayStatuses {
		page, err := ListRelays(ctx, pool, nil, RelayFilter{Status: st})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) != 1 || page.Items[0].ID != byStatus[st] || page.Items[0].Status != st {
			t.Errorf("status=%s: got %+v", st, page.Items)
		}
	}

	for _, bad := range []string{"Active", "online", "' OR 1=1 --"} {
		if _, err := ListRelays(ctx, pool, nil, RelayFilter{Status: bad}); !errors.Is(err, ErrInvalidFilter) {
			t.Errorf("status %q: err = %v, want ErrInvalidFilter", bad, err)
		}
	}
}

func TestListRelays_KeysetPagination(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	var all []string
	for i := 0; i < 7; i++ {
		all = append(all, seedRelay(t, pool, fmt.Sprintf("r%d", i), "active", t0.Add(time.Duration(i)*time.Second)))
	}
	// Two relays sharing one created_at: the id breaks the tie.
	tie := t0.Add(time.Hour)
	all = append(all, seedRelay(t, pool, "tie-a", "active", tie), seedRelay(t, pool, "tie-b", "active", tie))

	var seen []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		page, err := ListRelays(ctx, pool, nil, RelayFilter{Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > 3 {
			t.Fatalf("page size %d > limit", len(page.Items))
		}
		seen = append(seen, ids(page.Items)...)
		if page.NextCursor == nil {
			break
		}
		cursor = *page.NextCursor
	}

	// Exactly every relay once, newest first (created_at DESC, id DESC).
	want := mustOrderedIDs(t, pool)
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("pages = %v\nwant    %v", seen, want)
	}
	if len(seen) != len(all) {
		t.Fatalf("saw %d relays, seeded %d", len(seen), len(all))
	}

	if _, err := ListRelays(ctx, pool, nil, RelayFilter{Cursor: "garbage"}); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("bad cursor: err = %v", err)
	}
}

func mustOrderedIDs(t *testing.T, q Querier) []string {
	t.Helper()
	rows, err := q.Query(context.Background(), `SELECT id::text FROM relays WHERE status <> 'deleted' ORDER BY created_at DESC, id DESC`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

func TestListRelays_LimitClampedAt200(t *testing.T) {
	pool := newTestDB(t)
	mustExec(t, pool, `INSERT INTO relays (name, status, created_at)
	                   SELECT 'bulk-' || g, 'active', $1::timestamptz + g * interval '1 second' FROM generate_series(1, 205) g`, t0)
	page, err := ListRelays(context.Background(), pool, nil, RelayFilter{Limit: 100000})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != MaxLimit || page.NextCursor == nil {
		t.Fatalf("got %d items (next=%v), want %d and a next cursor", len(page.Items), page.NextCursor != nil, MaxLimit)
	}
	page, err = ListRelays(context.Background(), pool, nil, RelayFilter{})
	if err != nil || len(page.Items) != DefaultLimit {
		t.Fatalf("default limit: %d items, err %v", len(page.Items), err)
	}
}

func TestListRelays_FieldsAndAttachedCount(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	id := seedRelay(t, pool, "relay-1", "active", t0)
	other := seedRelay(t, pool, "relay-2", "active", t0.Add(time.Minute))
	mustExec(t, pool, `UPDATE relays SET version='0.21.0', hostname='relay-1.local', public_addr='203.0.113.5:9093',
	                     address_scope='public', capacity_label='medium', connection_count=12, max_connections=500,
	                     cert_serial='abc123', cert_not_after=$2, last_heartbeat_at=$3 WHERE id=$1`,
		id, t0.Add(30*24*time.Hour), t0.Add(-time.Minute))
	tenant, network := seedTenant(t, pool, "acme")
	for i := 0; i < 3; i++ {
		cid := mustScalar[string](t, pool, `INSERT INTO connectors (tenant_id, remote_network_id, name) VALUES ($1,$2,$3) RETURNING id::text`, tenant, network, fmt.Sprintf("c%d", i))
		mustExec(t, pool, `INSERT INTO connector_relay_placement (connector_id, relay_id, source) VALUES ($1,$2,'heartbeat')`, cid, id)
	}

	page, err := ListRelays(ctx, pool, nil, RelayFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var r RelaySummary
	for _, s := range page.Items {
		if s.ID == id {
			r = s
		}
		if s.ID == other && s.AttachedConnectors != 0 {
			t.Errorf("other relay attached = %d", s.AttachedConnectors)
		}
	}
	if r.AttachedConnectors != 3 || r.Name != "relay-1" || *r.Version != "0.21.0" || *r.Hostname != "relay-1.local" ||
		*r.PublicAddr != "203.0.113.5:9093" || *r.AddressScope != "public" || r.CapacityLabel != "medium" ||
		r.ConnectionCount != 12 || r.MaxConnections != 500 || *r.CertSerial != "abc123" ||
		!r.CertNotAfter.Equal(t0.Add(30*24*time.Hour)) || !r.LastHeartbeatAt.Equal(t0.Add(-time.Minute)) ||
		r.CreatedAt.Location() != time.UTC {
		t.Fatalf("relay = %+v", r)
	}
}

func TestEffectiveHeartbeat(t *testing.T) {
	ctx := context.Background()
	db := t0
	older, newer := t0.Add(-time.Minute), t0.Add(4*time.Minute)
	cases := []struct {
		name string
		hb   HeartbeatSource
		db   *time.Time
		want *time.Time
	}{
		{"no source", nil, &db, &db},
		{"valkey fresher", fakeHeartbeats{at: map[string]time.Time{"r": newer}}, &db, &newer},
		{"db fresher", fakeHeartbeats{at: map[string]time.Time{"r": older}}, &db, &db},
		{"no db value", fakeHeartbeats{at: map[string]time.Time{"r": older}}, nil, &older},
		{"key missing", fakeHeartbeats{}, &db, &db},
		{"valkey error falls back", fakeHeartbeats{err: errors.New("down")}, &db, &db},
		{"nothing at all", fakeHeartbeats{}, nil, nil},
	}
	for _, c := range cases {
		got := effectiveHeartbeat(ctx, c.hb, "r", c.db)
		if (got == nil) != (c.want == nil) || (got != nil && !got.Equal(*c.want)) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestGetRelay_NotFoundAndInvalidID(t *testing.T) {
	pool := newTestDB(t)
	if _, err := GetRelay(context.Background(), pool, nil, "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: err = %v", err)
	}
	for _, bad := range []string{"", "relay-1", "1' OR '1'='1"} {
		if _, err := GetRelay(context.Background(), pool, nil, bad); !errors.Is(err, ErrInvalidID) {
			t.Errorf("id %q: err = %v, want ErrInvalidID", bad, err)
		}
	}
}

func TestGetRelay_CertHistoryAndAttachments(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	id := seedRelay(t, pool, "relay-d", "deleted", t0) // deleted relays stay readable by id
	mustExec(t, pool, `UPDATE relays SET dns_allowlist='{relay.example.com}', ip_allowlist='{203.0.113.5}', cert_serial='s3' WHERE id=$1`, id)
	mustExec(t, pool, `INSERT INTO relay_certificates (relay_id, serial, issued_at, not_after, revoked_at, revocation_reason) VALUES
	                     ($1,'s1',$2,$3,$4,'superseded'), ($1,'s2',$5,$6,NULL,NULL), ($1,'s3',$7,$8,NULL,NULL)`,
		id, t0, t0.Add(time.Hour), t0.Add(30*time.Minute),
		t0.Add(time.Hour), t0.Add(2*time.Hour),
		t0.Add(2*time.Hour), t0.Add(3*time.Hour))
	tenant, network := seedTenant(t, pool, "acme")
	cid := mustScalar[string](t, pool, `INSERT INTO connectors (tenant_id, remote_network_id, name) VALUES ($1,$2,'c1') RETURNING id::text`, tenant, network)
	mustExec(t, pool, `INSERT INTO connector_relay_placement (connector_id, relay_id, source, attached_at, last_confirmed) VALUES ($1,$2,'event',$3,$4)`,
		cid, id, t0, t0.Add(time.Minute))

	d, err := GetRelay(ctx, pool, nil, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != "deleted" || !reflect.DeepEqual(d.DNSAllowlist, []string{"relay.example.com"}) ||
		!reflect.DeepEqual(d.IPAllowlist, []string{"203.0.113.5"}) {
		t.Fatalf("detail = %+v", d)
	}
	var serials []string
	for _, c := range d.Certificates {
		serials = append(serials, c.Serial)
	}
	if !reflect.DeepEqual(serials, []string{"s3", "s2", "s1"}) {
		t.Fatalf("history order = %v, want newest first", serials)
	}
	if !d.Certificates[0].IsCurrent || d.Certificates[1].IsCurrent || d.Certificates[2].IsCurrent {
		t.Fatalf("is_current = %+v", d.Certificates)
	}
	if d.Certificates[2].RevokedAt == nil || *d.Certificates[2].RevocationReason != "superseded" || d.Certificates[1].RevokedAt != nil {
		t.Fatalf("revocation fields = %+v", d.Certificates)
	}
	if d.CertificatesTruncated || d.AttachmentsTruncated {
		t.Fatal("small lists flagged truncated")
	}
	want := RelayAttachment{ConnectorID: cid, TenantID: tenant, AttachedAt: t0, LastConfirmed: t0.Add(time.Minute), Source: "event"}
	if len(d.Attachments) != 1 || d.Attachments[0] != want {
		t.Fatalf("attachments = %+v, want %+v", d.Attachments, want)
	}
}

func TestGetRelay_NoCurrentCertWhenSerialUnset(t *testing.T) {
	pool := newTestDB(t)
	id := seedRelay(t, pool, "relay-p", "pending", t0)
	mustExec(t, pool, `INSERT INTO relay_certificates (relay_id, serial, not_after) VALUES ($1,'only',$2)`, id, t0.Add(time.Hour))
	d, err := GetRelay(context.Background(), pool, nil, id)
	if err != nil || len(d.Certificates) != 1 || d.Certificates[0].IsCurrent {
		t.Fatalf("certs = %+v, err %v", d.Certificates, err)
	}
	if d.DNSAllowlist == nil || d.IPAllowlist == nil || d.Attachments == nil {
		t.Fatal("empty collections must be [] not null")
	}
}

func TestGetRelay_NestedCollectionsCapped(t *testing.T) {
	for _, n := range []int{DetailCap, DetailCap + 1} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			pool := newTestDB(t)
			id := seedRelay(t, pool, "busy", "active", t0)
			mustExec(t, pool, `INSERT INTO relay_certificates (relay_id, serial, issued_at, not_after)
			                   SELECT $1, 'serial-' || g, $2::timestamptz + g * interval '1 minute', $2::timestamptz + interval '1 year'
			                     FROM generate_series(1, $3) g`, id, t0, n)
			tenant, network := seedTenant(t, pool, "busy")
			mustExec(t, pool, `WITH c AS (
			                     INSERT INTO connectors (tenant_id, remote_network_id, name)
			                     SELECT $1, $2, 'c-' || g FROM generate_series(1, $4) g RETURNING id)
			                   INSERT INTO connector_relay_placement (connector_id, relay_id, source)
			                   SELECT c.id, $3, 'heartbeat' FROM c`, tenant, network, id, n)

			d, err := GetRelay(context.Background(), pool, nil, id)
			if err != nil {
				t.Fatal(err)
			}
			truncated := n > DetailCap
			if len(d.Certificates) != DetailCap || d.CertificatesTruncated != truncated {
				t.Errorf("certificates: %d, truncated=%v; want %d, %v", len(d.Certificates), d.CertificatesTruncated, DetailCap, truncated)
			}
			if len(d.Attachments) != DetailCap || d.AttachmentsTruncated != truncated {
				t.Errorf("attachments: %d, truncated=%v; want %d, %v", len(d.Attachments), d.AttachmentsTruncated, DetailCap, truncated)
			}
			// The newest certificate is always kept.
			if d.Certificates[0].Serial != "serial-"+strconv.Itoa(n) {
				t.Errorf("first certificate = %s", d.Certificates[0].Serial)
			}
		})
	}
}

// No relay response carries enrollment_token_jti, observed_ip or
// observed_port, by key or by value.
func TestRelayResponses_ExcludeSensitiveFields(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	id := seedRelay(t, pool, "sensitive", "active", t0)
	mustExec(t, pool, `UPDATE relays SET enrollment_token_jti='jti-SECRET-7f3a', observed_ip='198.51.100.77', observed_port=45678 WHERE id=$1`, id)

	page, err := ListRelays(ctx, pool, nil, RelayFilter{})
	if err != nil {
		t.Fatal(err)
	}
	detail, err := GetRelay(ctx, pool, nil, id)
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]any{"list": page, "detail": detail} {
		b, _ := json.Marshal(v)
		body := strings.ToLower(string(b))
		for _, bad := range []string{"jti", "secret-7f3a", "observed", "198.51.100.77", "45678", "token", "private", "encrypted"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s response contains %q: %s", name, bad, b)
			}
		}
	}
}

// The DTOs are the allowed response surface: adding a field must be a
// deliberate change to this list.
func TestRelayDTOSurface(t *testing.T) {
	summary := []string{"address_scope", "attached_connectors", "capacity_label", "cert_not_after", "cert_serial",
		"connection_count", "created_at", "hostname", "id", "last_heartbeat_at", "max_connections", "name",
		"public_addr", "status", "version"}
	detail := append(append([]string{}, summary...), "attachments", "attachments_truncated", "certificates",
		"certificates_truncated", "dns_allowlist", "ip_allowlist")
	sort.Strings(detail)
	for name, c := range map[string]struct {
		v    any
		want []string
	}{
		"RelaySummary":     {RelaySummary{}, summary},
		"RelayDetail":      {RelayDetail{}, detail},
		"RelayCertificate": {RelayCertificate{}, []string{"is_current", "issued_at", "not_after", "revocation_reason", "revoked_at", "serial"}},
		"RelayAttachment":  {RelayAttachment{}, []string{"attached_at", "connector_id", "last_confirmed", "source", "tenant_id"}},
	} {
		b, _ := json.Marshal(c.v)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		var keys []string
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if !reflect.DeepEqual(keys, c.want) {
			t.Errorf("%s keys = %v\nwant %v", name, keys, c.want)
		}
	}
}

// countingQuerier counts statements so tests can pin "one query per page"
// (no N+1) independently of how many rows come back.
type countingQuerier struct {
	Querier
	n int
}

func (c *countingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.n++
	return c.Querier.Query(ctx, sql, args...)
}

func (c *countingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.n++
	return c.Querier.QueryRow(ctx, sql, args...)
}

func TestRelayQueries_FixedStatementCount(t *testing.T) {
	pool := newTestDB(t)
	tenant, network := seedTenant(t, pool, "acme")
	var first string
	for i := 0; i < 25; i++ {
		id := seedRelay(t, pool, fmt.Sprintf("r%d", i), "active", t0.Add(time.Duration(i)*time.Second))
		if i == 0 {
			first = id
		}
		cid := mustScalar[string](t, pool, `INSERT INTO connectors (tenant_id, remote_network_id, name) VALUES ($1,$2,$3) RETURNING id::text`, tenant, network, fmt.Sprintf("c%d", i))
		mustExec(t, pool, `INSERT INTO connector_relay_placement (connector_id, relay_id, source) VALUES ($1,$2,'heartbeat')`, cid, id)
		mustExec(t, pool, `INSERT INTO relay_certificates (relay_id, serial, not_after) VALUES ($1,$2,$3)`, id, fmt.Sprintf("s%d", i), t0.Add(time.Hour))
	}
	cq := &countingQuerier{Querier: pool}
	page, err := ListRelays(context.Background(), cq, nil, RelayFilter{Limit: 200})
	if err != nil || len(page.Items) != 25 {
		t.Fatalf("list: %d items, err %v", len(page.Items), err)
	}
	if cq.n != 1 {
		t.Fatalf("ListRelays ran %d statements for 25 relays, want 1", cq.n)
	}
	cq.n = 0
	if _, err := GetRelay(context.Background(), cq, nil, first); err != nil {
		t.Fatal(err)
	}
	if cq.n != 3 {
		t.Fatalf("GetRelay ran %d statements, want 3 (relay, certificates, attachments)", cq.n)
	}
}
