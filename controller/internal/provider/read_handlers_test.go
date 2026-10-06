package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/middleware"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// HTTP integration tests for the Phase R read routes: real RequireProvider,
// real Authz, real providerquery against a throwaway database.

const readTestSecret = "read-handlers-test-secret-0123456789abcdef"

// ── harness ─────────────────────────────────────────────────────────────────

func newReadTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("provider_read_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}
	parsed, _ := url.Parse(adminDSN)
	parsed.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName)
		admin.Close()
	})
	files, _ := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if len(files) == 0 {
		t.Fatal("no migrations found")
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	return pool
}

func scalar[T any](t *testing.T, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := q.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return v
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

// spyDB wraps the pool to observe and break the tenant-detail transaction.
type spyDB struct {
	*pgxpool.Pool
	begins     int
	iso        pgx.TxIsoLevel
	poolReads  int // statements run on the pool (outside any transaction)
	txReads    int // statements run inside the transaction
	failExec   bool
	failCommit bool
	onCommit   func()
}

func (s *spyDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	s.poolReads++
	return s.Pool.Query(ctx, sql, args...)
}

func (s *spyDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	s.poolReads++
	return s.Pool.QueryRow(ctx, sql, args...)
}

func (s *spyDB) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	s.begins++
	s.iso = opts.IsoLevel
	tx, err := s.Pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &spyTx{Tx: tx, s: s}, nil
}

type spyTx struct {
	pgx.Tx
	s *spyDB
}

func (t *spyTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	t.s.txReads++
	return t.Tx.Query(ctx, sql, args...)
}

func (t *spyTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	t.s.txReads++
	return t.Tx.QueryRow(ctx, sql, args...)
}

func (t *spyTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if t.s.failExec {
		return pgconn.CommandTag{}, errors.New("injected audit insert failure")
	}
	return t.Tx.Exec(ctx, sql, args...)
}

func (t *spyTx) Commit(ctx context.Context) error {
	if t.s.onCommit != nil {
		t.s.onCommit()
	}
	if t.s.failCommit {
		_ = t.Tx.Rollback(ctx)
		return errors.New("injected commit failure")
	}
	return t.Tx.Commit(ctx)
}

// trackingWriter records whether anything was written to the client yet.
type trackingWriter struct {
	*httptest.ResponseRecorder
	wrote bool
}

func (w *trackingWriter) WriteHeader(code int) { w.wrote = true; w.ResponseRecorder.WriteHeader(code) }
func (w *trackingWriter) Write(b []byte) (int, error) {
	w.wrote = true
	return w.ResponseRecorder.Write(b)
}

type fakeCtl struct{ notAfter time.Time }

func (f fakeCtl) CurrentCertInfo() (string, time.Time, time.Time, bool) {
	return "c0ffee", f.notAfter.Add(-time.Hour), f.notAfter, true
}

type readEnv struct {
	pool        *pgxpool.Pool
	db          *spyDB
	mux         http.Handler
	ids         *provider.IdentityService
	superID     string
	superTok    string
	opsTok      string
	tenantJWT   string
	relayID     string
	tenantID    string
	auditRowIDs []string
}

func newReadEnv(t *testing.T) *readEnv {
	t.Helper()
	pool := newReadTestDB(t)
	ids, err := provider.NewIdentityService(readTestSecret)
	if err != nil {
		t.Fatal(err)
	}
	store := provider.NewStore(pool)
	env := &readEnv{pool: pool, db: &spyDB{Pool: pool}, ids: ids}

	issue := func(email, role string) (string, string) {
		id := scalar[string](t, pool, `INSERT INTO provider_users (email, role) VALUES ($1, $2) RETURNING id::text`, email, role)
		tok, _, err := ids.IssueSession(&provider.ProviderUser{ID: id, Email: email, Role: role, SessionGeneration: 1}, []string{provider.AMRPassword}, false)
		if err != nil {
			t.Fatal(err)
		}
		return id, tok
	}
	env.superID, env.superTok = issue("admin@provider.test", provider.RoleSuperAdmin)
	_, env.opsTok = issue("ops@provider.test", provider.RoleRelayOps)

	now := time.Now()
	env.tenantJWT, _ = jwt.NewWithClaims(jwt.SigningMethodHS256, middleware.Claims{
		TenantID: "tenant-123", Role: "admin", Email: "admin@provider.test",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: env.superID, Issuer: appmeta.ControllerIssuer,
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Minute)),
		},
	}).SignedString([]byte(readTestSecret))

	// Data: one relay with a certificate, one tenant with infrastructure.
	env.relayID = scalar[string](t, pool, `INSERT INTO relays (name, status, cert_serial, cert_not_after, last_heartbeat_at)
	   VALUES ('relay-1', 'active', 'abc', now() + interval '10 days', now()) RETURNING id::text`)
	env.tenantID = scalar[string](t, pool, `INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ('acme', 'Acme', 'active', 'acme.zecurity.test') RETURNING id::text`)
	net := scalar[string](t, pool, `INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'hq', 'office') RETURNING id::text`, env.tenantID)
	conn := scalar[string](t, pool, `INSERT INTO connectors (tenant_id, remote_network_id, name, status, cert_not_after) VALUES ($1,$2,'c1','active', now() + interval '5 days') RETURNING id::text`, env.tenantID, net)
	exec(t, pool, `INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status) VALUES ($1,$2,$3,'s1','active')`, env.tenantID, net, conn)
	exec(t, pool, `INSERT INTO users (tenant_id, email, provider, provider_sub, role) VALUES ($1, 'tenant.admin.canary@acme.example', 'google', 'SUBCANARY', 'admin')`, env.tenantID)

	h := provider.NewReadHandlers(env.db, provider.NewAuthz(), store, nil, fakeCtl{notAfter: now.Add(12 * time.Hour)})
	rp := middleware.RequireProvider(ids, store)
	mux := http.NewServeMux()
	// The six read routes exactly as main.go registers them, alongside the
	// existing provider relay routes they share paths with (a conflicting
	// pattern would panic here).
	mux.Handle("GET /provider/relays", rp(http.HandlerFunc(h.ListRelays)))
	mux.Handle("GET /provider/relays/{id}", rp(http.HandlerFunc(h.GetRelay)))
	mux.Handle("GET /provider/tenants", rp(http.HandlerFunc(h.ListTenants)))
	mux.Handle("GET /provider/tenants/{id}", rp(http.HandlerFunc(h.GetTenant)))
	mux.Handle("GET /provider/audit", rp(http.HandlerFunc(h.QueryAudit)))
	mux.Handle("GET /provider/certificates", rp(http.HandlerFunc(h.Certificates)))
	noop := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	mux.Handle("POST /provider/relays", noop)
	mux.Handle("POST /provider/relays/{id}/revoke", noop)
	mux.Handle("DELETE /provider/relays/{id}", noop)
	env.mux = mux
	return env
}

type resp struct {
	code   int
	header http.Header
	body   string
	wrote  bool
}

func (e *readEnv) get(t *testing.T, path, token string) resp {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = "203.0.113.9:51234"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := &trackingWriter{ResponseRecorder: httptest.NewRecorder()}
	e.mux.ServeHTTP(w, req)
	return resp{code: w.Code, header: w.Header(), body: w.Body.String(), wrote: w.wrote}
}

func (e *readEnv) auditCount(t *testing.T, action string) int {
	t.Helper()
	return scalar[int](t, e.pool, `SELECT count(*) FROM provider_audit_logs WHERE action = $1`, action)
}

func (e *readEnv) totalAudit(t *testing.T) int {
	t.Helper()
	return scalar[int](t, e.pool, `SELECT count(*) FROM provider_audit_logs`)
}

func readErrCode(t *testing.T, r resp) string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal([]byte(r.body), &m); err != nil {
		t.Fatalf("error body %q: %v", r.body, err)
	}
	return m["error"]
}

// ── auth matrix ─────────────────────────────────────────────────────────────

func TestReadRoutes_AuthMatrix(t *testing.T) {
	e := newReadEnv(t)
	routes := []struct {
		path     string
		relayOps int
	}{
		{"/provider/relays", 200},
		{"/provider/relays/" + e.relayID, 200},
		{"/provider/tenants", 403},
		{"/provider/tenants/" + e.tenantID, 403},
		{"/provider/audit", 403},
		{"/provider/certificates", 403},
	}
	for _, rt := range routes {
		if r := e.get(t, rt.path, e.superTok); r.code != 200 {
			t.Errorf("super-admin %s = %d %s", rt.path, r.code, r.body)
		}
		r := e.get(t, rt.path, e.opsTok)
		if r.code != rt.relayOps {
			t.Errorf("relay-ops %s = %d, want %d", rt.path, r.code, rt.relayOps)
		}
		if r.code == 403 && readErrCode(t, r) != "forbidden" {
			t.Errorf("relay-ops %s error = %s", rt.path, r.body)
		}
		for name, tok := range map[string]string{"no token": "", "tenant JWT": e.tenantJWT, "garbage": "not-a-jwt"} {
			if r := e.get(t, rt.path, tok); r.code != 401 {
				t.Errorf("%s %s = %d, want 401", name, rt.path, r.code)
			}
		}
	}
	// One super-admin tenant detail read happened above; nothing else audited.
	if got := e.totalAudit(t); got != 1 || e.auditCount(t, provider.ActionTenantRead) != 1 {
		t.Fatalf("audit rows = %d, want exactly the one tenant.read", got)
	}
}

// ── validation, 404s and response conventions ───────────────────────────────

func TestReadRoutes_BadParams400(t *testing.T) {
	e := newReadEnv(t)
	cases := map[string]string{
		"/provider/relays?limit=abc":                                            "invalid_limit",
		"/provider/relays?limit=0":                                              "invalid_limit",
		"/provider/relays?limit=-3":                                             "invalid_limit",
		"/provider/relays?status=online":                                        "invalid_status",
		"/provider/relays?cursor=garbage":                                       "invalid_cursor",
		"/provider/relays/not-a-uuid":                                           "invalid_id",
		"/provider/tenants?status=gone":                                         "invalid_status",
		"/provider/tenants?cursor=%25%25":                                       "invalid_cursor",
		"/provider/tenants/acme":                                                "invalid_id",
		"/provider/audit?since=yesterday":                                       "invalid_since",
		"/provider/audit?until=2026-13-01T00:00:00Z":                            "invalid_until",
		"/provider/audit?since=2026-10-02T00:00:00Z&until=2026-10-01T00:00:00Z": "invalid_range",
		"/provider/audit?action=" + strings.Repeat("x", 300):                    "invalid_filter",
		"/provider/audit?limit=1.5":                                             "invalid_limit",
		"/provider/certificates?within=30d":                                     "invalid_within",
		"/provider/certificates?within=-1h":                                     "invalid_within",
		"/provider/certificates?within=0s":                                      "invalid_within",
		"/provider/certificates?within=8761h":                                   "invalid_within",
		"/provider/certificates?kind=ssh":                                       "invalid_kind",
		"/provider/certificates?tenant_id=acme":                                 "invalid_tenant_id",
		"/provider/certificates?cursor=bad":                                     "invalid_cursor",
	}
	for path, want := range cases {
		r := e.get(t, path, e.superTok)
		if r.code != 400 || readErrCode(t, r) != want {
			t.Errorf("%s = %d %s, want 400 %s", path, r.code, r.body, want)
		}
		if r.header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control = %q", path, r.header.Get("Cache-Control"))
		}
	}
	if r := e.get(t, "/provider/certificates?within=8760h", e.superTok); r.code != 200 {
		t.Errorf("max within = %d %s", r.code, r.body)
	}
	if r := e.get(t, "/provider/relays?limit=5000", e.superTok); r.code != 200 {
		t.Errorf("large limit is clamped, not rejected: %d", r.code)
	}
	if n := e.totalAudit(t); n != 0 {
		t.Fatalf("400 responses wrote %d audit rows", n)
	}
}

func TestReadRoutes_NotFound404(t *testing.T) {
	e := newReadEnv(t)
	missing := "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0"
	for _, p := range []string{"/provider/relays/" + missing, "/provider/tenants/" + missing} {
		r := e.get(t, p, e.superTok)
		if r.code != 404 || readErrCode(t, r) != "not_found" || r.header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s = %d %s", p, r.code, r.body)
		}
	}
	if n := e.totalAudit(t); n != 0 {
		t.Fatalf("404 tenant detail wrote %d audit rows", n)
	}
}

var timeLike = regexp.MustCompile(`"(\d{4}-\d{2}-\d{2}T[^"]+)"`)

func TestReadRoutes_HeadersShapeAndUTC(t *testing.T) {
	e := newReadEnv(t)
	// Seed an audit row so the audit list isn't empty (tenant detail adds one too).
	exec(t, e.pool, `INSERT INTO provider_audit_logs (provider_email, action, target_type, target_id) VALUES ('a@provider.test','relay.create','relay','r1')`)
	shapes := map[string][]string{
		"/provider/relays":                {"items", "next_cursor"},
		"/provider/relays/" + e.relayID:   nil,
		"/provider/tenants":               {"items", "next_cursor"},
		"/provider/tenants/" + e.tenantID: nil,
		"/provider/audit":                 {"items", "next_cursor"},
		"/provider/certificates":          {"items", "next_cursor", "summary"},
	}
	for path, keys := range shapes {
		r := e.get(t, path, e.superTok)
		if r.code != 200 {
			t.Fatalf("%s = %d %s", path, r.code, r.body)
		}
		if r.header.Get("Content-Type") != "application/json" || r.header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s headers = %v", path, r.header)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(r.body), &m); err != nil {
			t.Fatalf("%s body: %v", path, err)
		}
		for _, k := range keys {
			if _, ok := m[k]; !ok {
				t.Errorf("%s missing %q", path, k)
			}
		}
		for _, k := range strings.Fields("password password_hash encrypted nonce enrollment_token_jti observed_ip email_address") {
			if strings.Contains(r.body, `"`+k+`"`) {
				t.Errorf("%s exposes %q", path, k)
			}
		}
		for _, match := range timeLike.FindAllStringSubmatch(r.body, -1) {
			ts, err := time.Parse(time.RFC3339, match[1])
			if err != nil || !strings.HasSuffix(match[1], "Z") || ts.Location() != time.UTC {
				t.Errorf("%s timestamp %q is not RFC 3339 UTC", path, match[1])
			}
		}
	}
}

// ── per-route content ───────────────────────────────────────────────────────

func TestReadRoutes_RelayListAndDetail(t *testing.T) {
	e := newReadEnv(t)
	var list struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal([]byte(e.get(t, "/provider/relays", e.opsTok).body), &list)
	if len(list.Items) != 1 || list.Items[0]["id"] != e.relayID || list.Items[0]["cert_serial"] != "abc" {
		t.Fatalf("relay list = %+v", list.Items)
	}
	var d map[string]any
	_ = json.Unmarshal([]byte(e.get(t, "/provider/relays/"+e.relayID, e.opsTok).body), &d)
	for _, k := range []string{"dns_allowlist", "ip_allowlist", "certificates", "certificates_truncated", "attachments", "attachments_truncated"} {
		if _, ok := d[k]; !ok {
			t.Errorf("relay detail missing %s", k)
		}
	}
	if n := e.totalAudit(t); n != 0 {
		t.Fatalf("relay reads wrote %d audit rows", n)
	}
}

func TestReadRoutes_TenantListAndDetail(t *testing.T) {
	e := newReadEnv(t)
	r := e.get(t, "/provider/tenants", e.superTok)
	var list struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal([]byte(r.body), &list)
	if len(list.Items) != 1 || list.Items[0]["slug"] != "acme" {
		t.Fatalf("tenant list = %s", r.body)
	}
	if n := e.auditCount(t, provider.ActionTenantRead); n != 0 {
		t.Fatalf("tenant LIST wrote %d tenant.read rows", n)
	}

	d := e.get(t, "/provider/tenants/"+e.tenantID, e.superTok)
	var detail map[string]any
	_ = json.Unmarshal([]byte(d.body), &detail)
	if detail["id"] != e.tenantID || len(detail["connectors"].([]any)) != 1 || len(detail["shields"].([]any)) != 1 ||
		len(detail["remote_networks"].([]any)) != 1 {
		t.Fatalf("tenant detail = %s", d.body)
	}
	low := strings.ToLower(d.body + r.body)
	for _, bad := range []string{"canary", "@acme.example", "subcanary", "provider_sub", "\"role\"", "email"} {
		if strings.Contains(low, bad) {
			t.Errorf("tenant responses contain %q", bad)
		}
	}
}

func TestReadRoutes_AuditFiltersAndPagination(t *testing.T) {
	e := newReadEnv(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		exec(t, e.pool, `INSERT INTO provider_audit_logs (provider_email, action, target_type, target_id, created_at) VALUES ($1,$2,'relay','r',$3)`,
			"A@provider.test", []string{"relay.create", "relay.revoke"}[i%2], base.Add(time.Duration(i)*time.Hour))
	}
	page := func(q string) (ids []string, next *string) {
		r := e.get(t, "/provider/audit?"+q, e.superTok)
		if r.code != 200 {
			t.Fatalf("audit %s = %d %s", q, r.code, r.body)
		}
		var p struct {
			Items []struct {
				ID     string `json:"id"`
				Action string `json:"action"`
			} `json:"items"`
			Next *string `json:"next_cursor"`
		}
		_ = json.Unmarshal([]byte(r.body), &p)
		for _, it := range p.Items {
			ids = append(ids, it.ID+"/"+it.Action)
		}
		return ids, p.Next
	}
	if ids, _ := page("action=relay.revoke"); len(ids) != 2 {
		t.Errorf("action filter = %v", ids)
	}
	if ids, _ := page("provider_email=a@PROVIDER.test"); len(ids) != 5 {
		t.Errorf("email filter = %v", ids)
	}
	if ids, _ := page("since=2026-10-01T01:00:00Z&until=2026-10-01T03:00:00Z"); len(ids) != 2 {
		t.Errorf("time range (half-open) = %v", ids)
	}
	var all []string
	q := "limit=2"
	for i := 0; i < 5; i++ {
		ids, next := page(q)
		all = append(all, ids...)
		if next == nil {
			break
		}
		q = "limit=2&cursor=" + url.QueryEscape(*next)
	}
	if len(all) != 5 {
		t.Fatalf("paged %d rows, want 5: %v", len(all), all)
	}
	if n := e.totalAudit(t); n != 5 {
		t.Fatalf("audit reads were themselves audited: %d rows", n)
	}
}

func TestReadRoutes_CertificateFiltersAndPagination(t *testing.T) {
	e := newReadEnv(t)
	get := func(q string) map[string]any {
		r := e.get(t, "/provider/certificates?"+q, e.superTok)
		if r.code != 200 {
			t.Fatalf("certificates %s = %d %s", q, r.code, r.body)
		}
		var m map[string]any
		_ = json.Unmarshal([]byte(r.body), &m)
		return m
	}
	all := get("within=8760h")
	kinds := map[string]bool{}
	for _, it := range all["items"].([]any) {
		kinds[it.(map[string]any)["kind"].(string)] = true
	}
	for _, k := range []string{"relay", "connector", "controller_grpc"} {
		if !kinds[k] {
			t.Errorf("kind %s missing: %v", k, kinds)
		}
	}
	if items := get("kind=connector&within=8760h")["items"].([]any); len(items) != 1 {
		t.Errorf("kind filter = %v", items)
	}
	if items := get("tenant_id=" + e.tenantID + "&within=8760h")["items"].([]any); len(items) != 1 {
		t.Errorf("tenant filter = %v", items)
	}
	// 12h controller cert only, within 24h; the summary still counts all three.
	narrow := get("within=24h")
	if items := narrow["items"].([]any); len(items) != 1 {
		t.Errorf("within=24h items = %v", items)
	}
	if !jsonEqual(narrow["summary"], all["summary"]) {
		t.Errorf("summary changed with within: %v vs %v", narrow["summary"], all["summary"])
	}
	p1 := get("within=8760h&limit=1")
	next, _ := p1["next_cursor"].(string)
	if next == "" {
		t.Fatal("no next cursor on a 1-item page")
	}
	p2 := get("within=8760h&limit=1&cursor=" + url.QueryEscape(next))
	if jsonEqual(p1["items"], p2["items"]) {
		t.Fatal("second page repeats the first")
	}
	if n := e.totalAudit(t); n != 0 {
		t.Fatalf("certificate reads wrote %d audit rows", n)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// ── tenant detail audit: committed, fail-closed, consistent ────────────────

func TestTenantDetail_AuditCommittedWithExactFields(t *testing.T) {
	e := newReadEnv(t)
	r := e.get(t, "/provider/tenants/"+e.tenantID, e.superTok)
	if r.code != 200 {
		t.Fatalf("detail = %d %s", r.code, r.body)
	}
	var (
		userID, email, action, targetType, targetID, ip string
		detailsOK                                       bool
		detailsText                                     string
	)
	row := e.pool.QueryRow(context.Background(), `
SELECT provider_user_id::text, provider_email, action, target_type, target_id, ip_address,
       details = '{"view":"detail"}'::jsonb, details::text
  FROM provider_audit_logs`)
	if err := row.Scan(&userID, &email, &action, &targetType, &targetID, &ip, &detailsOK, &detailsText); err != nil {
		t.Fatalf("expected exactly one audit row: %v", err)
	}
	if e.totalAudit(t) != 1 {
		t.Fatalf("audit rows = %d, want 1", e.totalAudit(t))
	}
	if userID != e.superID || email != "admin@provider.test" || action != "tenant.read" || targetType != "tenant" ||
		targetID != e.tenantID || ip != "203.0.113.9" {
		t.Fatalf("audit row = %s %s %s %s %s %s", userID, email, action, targetType, targetID, ip)
	}
	if !detailsOK {
		t.Fatalf("details = %s, want exactly {\"view\":\"detail\"}", detailsText)
	}
	// One row per detail request.
	e.get(t, "/provider/tenants/"+e.tenantID, e.superTok)
	if n := e.auditCount(t, "tenant.read"); n != 2 {
		t.Fatalf("after a second read: %d rows, want 2", n)
	}
}

func TestTenantDetail_AuditInsertFailureReturnsNoData(t *testing.T) {
	e := newReadEnv(t)
	e.db.failExec = true
	r := e.get(t, "/provider/tenants/"+e.tenantID, e.superTok)
	if r.code != 500 || readErrCode(t, r) != "server_error" {
		t.Fatalf("audit failure = %d %s, want 500", r.code, r.body)
	}
	for _, leak := range []string{e.tenantID, "acme", "Acme", "connectors", "counts"} {
		if strings.Contains(r.body, leak) {
			t.Errorf("500 body leaks %q: %s", leak, r.body)
		}
	}
	if n := e.totalAudit(t); n != 0 {
		t.Fatalf("audit rows after failed insert = %d", n)
	}
}

func TestTenantDetail_CommitFailureReturnsNoData(t *testing.T) {
	e := newReadEnv(t)
	e.db.failCommit = true
	r := e.get(t, "/provider/tenants/"+e.tenantID, e.superTok)
	if r.code != 500 || readErrCode(t, r) != "server_error" {
		t.Fatalf("commit failure = %d %s, want 500", r.code, r.body)
	}
	for _, leak := range []string{e.tenantID, "acme", "Acme", "connectors"} {
		if strings.Contains(r.body, leak) {
			t.Errorf("500 body leaks %q: %s", leak, r.body)
		}
	}
	if n := e.totalAudit(t); n != 0 {
		t.Fatalf("audit row survived a failed commit: %d", n)
	}
}

func TestTenantDetail_NothingWrittenBeforeCommit(t *testing.T) {
	e := newReadEnv(t)
	req := httptest.NewRequest(http.MethodGet, "/provider/tenants/"+e.tenantID, nil)
	req.Header.Set("Authorization", "Bearer "+e.superTok)
	req.RemoteAddr = "203.0.113.9:1"
	w := &trackingWriter{ResponseRecorder: httptest.NewRecorder()}
	committed := false
	e.db.onCommit = func() {
		committed = true
		if w.wrote {
			t.Error("response bytes were written before COMMIT")
		}
		// The audit row is in the transaction, not yet visible outside it.
		if n := scalar[int](t, e.pool, `SELECT count(*) FROM provider_audit_logs`); n != 0 {
			t.Errorf("audit row visible before commit: %d", n)
		}
	}
	e.mux.ServeHTTP(w, req)
	if !committed || w.Code != 200 || !w.wrote {
		t.Fatalf("committed=%v code=%d wrote=%v", committed, w.Code, w.wrote)
	}
}

func TestTenantDetail_OneRepeatableReadSnapshot(t *testing.T) {
	e := newReadEnv(t)
	e.db.poolReads, e.db.txReads = 0, 0
	if r := e.get(t, "/provider/tenants/"+e.tenantID, e.superTok); r.code != 200 {
		t.Fatalf("detail = %d", r.code)
	}
	if e.db.begins != 1 || e.db.iso != pgx.RepeatableRead {
		t.Fatalf("begins=%d iso=%v, want one REPEATABLE READ transaction", e.db.begins, e.db.iso)
	}
	if e.db.txReads != 4 || e.db.poolReads != 0 {
		t.Fatalf("reads: %d in the transaction, %d on the pool; want 4 and 0", e.db.txReads, e.db.poolReads)
	}
}

func TestTenantDetail_NoAuditOn400_403_404(t *testing.T) {
	e := newReadEnv(t)
	e.get(t, "/provider/tenants/not-a-uuid", e.superTok)                           // 400
	e.get(t, "/provider/tenants/"+e.tenantID, e.opsTok)                            // 403
	e.get(t, "/provider/tenants/0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0", e.superTok) // 404
	e.get(t, "/provider/tenants/"+e.tenantID, e.tenantJWT)                         // 401
	if n := e.totalAudit(t); n != 0 {
		t.Fatalf("audit rows = %d, want 0", n)
	}
	// Only the 404 lookup opens a transaction (and rolls it back); 400, 403
	// and 401 are refused before any database work.
	if e.db.begins != 1 {
		t.Fatalf("transactions started = %d, want 1 (the 404 lookup only)", e.db.begins)
	}
}
