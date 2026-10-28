package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeycompat"

	"github.com/yourorg/ztna/controller/internal/pki"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// Sprint 22 Phase A (D-17, D-31) over the real relay and provider stores on a
// throwaway database. Requires PKI_TEST_DATABASE_URL; Create additionally
// needs the isolated Valkey (AUTH_TEST_VALKEY_URL).

type adminEnv struct {
	pool     *pgxpool.Pool
	store    *Store
	handler  *AdminHandler
	mux      *http.ServeMux
	superID  string
	opsID    string
	mu       sync.Mutex
	fanout   []string
	rdb      valkeycompat.Cmdable
	rdbClose func()
}

func newAdminEnv(t *testing.T, withValkey bool) *adminEnv {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := uniqueRelayTestDBName(t)
	adminPool := mustConnectRelayPool(t, ctx, adminDSN)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		adminPool.Close()
		t.Fatalf("create test database: %v", err)
	}
	testDSN, err := withRelayTestDBName(adminDSN, dbName)
	if err != nil {
		t.Fatal(err)
	}
	pool := mustConnectRelayPool(t, ctx, testDSN)
	t.Cleanup(func() {
		pool.Close()
		if _, err := adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
		adminPool.Close()
	})
	if err := applyRelayMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	e := &adminEnv{pool: pool, store: NewStore(pool)}
	if err := pool.QueryRow(ctx, `INSERT INTO provider_users (email, role) VALUES ('admin@provider.test', 'super-admin') RETURNING id::text`).Scan(&e.superID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO provider_users (email, role) VALUES ('ops@provider.test', 'relay-ops') RETURNING id::text`).Scan(&e.opsID); err != nil {
		t.Fatal(err)
	}
	if withValkey {
		e.rdb, e.rdbClose = testValkey(t)
	}
	e.handler = &AdminHandler{
		Store:         e.store,
		Redis:         e.rdb,
		JWTSecret:     "phase-a-test-relay-provisioning-secret-0123456789",
		Authz:         provider.NewAuthz(),
		ProviderStore: provider.NewStore(pool),
		CAFingerprint: func(ctx context.Context) (string, error) {
			return pki.IntermediateFingerprint(ctx, pool)
		},
		ControllerGRPCAddr: "controller.provider.test:9090",
		ControllerHTTPAddr: "controller.provider.test:8080",
		OnRelayRevoked: func(_ context.Context, relayID string) {
			e.mu.Lock()
			e.fanout = append(e.fanout, relayID)
			e.mu.Unlock()
		},
	}
	e.mux = http.NewServeMux()
	e.mux.HandleFunc("POST /provider/relays", e.handler.Create)
	e.mux.HandleFunc("POST /provider/relays/{id}/revoke", e.handler.Revoke)
	e.mux.HandleFunc("DELETE /provider/relays/{id}", e.handler.Delete)
	return e
}

// testValkey connects to the isolated test Valkey. Keys written by Create
// (relay:provisioning:jti:<jti>) are deleted by the test that created them.
func testValkey(t *testing.T) (valkeycompat.Cmdable, func()) {
	t.Helper()
	raw := os.Getenv("AUTH_TEST_VALKEY_URL")
	if raw == "" {
		t.Skip("AUTH_TEST_VALKEY_URL not set")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		t.Fatalf("bad AUTH_TEST_VALKEY_URL %q: %v", raw, err)
	}
	client, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{u.Host}})
	if err != nil {
		t.Fatalf("valkey client: %v", err)
	}
	t.Cleanup(client.Close)
	return valkeycompat.NewAdapter(client), client.Close
}

func (e *adminEnv) actor(role string) provider.Actor {
	if role == provider.RoleSuperAdmin {
		return provider.Actor{UserID: e.superID, Email: "admin@provider.test", Role: role}
	}
	return provider.Actor{UserID: e.opsID, Email: "ops@provider.test", Role: role}
}

type resp struct {
	code   int
	body   string
	header http.Header
}

func (e *adminEnv) do(t *testing.T, role, method, path string, body any, hdr map[string]string) resp {
	t.Helper()
	var rdr *strings.Reader
	switch b := body.(type) {
	case nil:
		rdr = strings.NewReader("")
	case string:
		rdr = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rdr = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(method, path, rdr)
	req.RemoteAddr = "192.0.2.10:54321"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req = req.WithContext(provider.WithActor(req.Context(), e.actor(role)))
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return resp{code: rec.Code, body: rec.Body.String(), header: rec.Header()}
}

func errorCode(t *testing.T, body string) string {
	t.Helper()
	var v struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("error body is not JSON: %q", body)
	}
	return v.Error
}

func (e *adminEnv) relay(t *testing.T, name string) string {
	t.Helper()
	ctx := context.Background()
	id, err := e.store.CreateRelay(ctx, name, []string{}, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.store.RecordIssuedCert(ctx, id, name+"-serial", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *adminEnv) status(t *testing.T, id string) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(), `SELECT status FROM relays WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *adminEnv) auditRows(t *testing.T, action string) []map[string]any {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT provider_email, target_id, COALESCE(details::text, '{}'), COALESCE(ip_address, '') FROM provider_audit_logs WHERE action = $1 ORDER BY created_at`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var email, target, details, ip string
		if err := rows.Scan(&email, &target, &details, &ip); err != nil {
			t.Fatal(err)
		}
		var d map[string]any
		_ = json.Unmarshal([]byte(details), &d)
		out = append(out, map[string]any{"email": email, "target": target, "details": d, "ip": ip})
	}
	return out
}

func (e *adminEnv) fanoutCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.fanout)
}

func confirm(name, reason string) map[string]string {
	return map[string]string{"reason": reason, "confirm_name": name}
}

// --- D-31 over HTTP ---

func TestAdminDB_RelayOpsCannotRevokeOrDelete(t *testing.T) {
	e := newAdminEnv(t, false)
	id := e.relay(t, "relay-a")
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/provider/relays/" + id + "/revoke"},
		{http.MethodDelete, "/provider/relays/" + id},
	} {
		r := e.do(t, provider.RoleRelayOps, tc.method, tc.path, confirm("relay-a", "x"), nil)
		if r.code != http.StatusForbidden || errorCode(t, r.body) != "forbidden" {
			t.Errorf("relay-ops %s %s: %d %s", tc.method, tc.path, r.code, r.body)
		}
	}
	if s := e.status(t, id); s != "pending" {
		t.Fatalf("relay changed by a refused request: %s", s)
	}
	if n := len(e.auditRows(t, provider.ActionRelayRevoke)) + len(e.auditRows(t, provider.ActionRelayDelete)); n != 0 || e.fanoutCount() != 0 {
		t.Fatalf("refused requests wrote %d audit rows / %d fan-outs", n, e.fanoutCount())
	}
}

func TestAdminDB_SuperAdminRevokeAndDelete(t *testing.T) {
	e := newAdminEnv(t, false)
	a, b := e.relay(t, "relay-a"), e.relay(t, "relay-b")
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, "/provider/relays/"+a+"/revoke", confirm("relay-a", "  key compromised  "), nil); r.code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodDelete, "/provider/relays/"+b, confirm("relay-b", "host retired"), nil); r.code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", r.code, r.body)
	}
	if e.status(t, a) != "revoked" || e.status(t, b) != "deleted" {
		t.Fatalf("statuses: %s %s", e.status(t, a), e.status(t, b))
	}
	var reason string
	_ = e.pool.QueryRow(context.Background(), `SELECT COALESCE(revocation_reason, '') FROM relay_certificates WHERE relay_id = $1`, a).Scan(&reason)
	if reason != "key compromised" {
		t.Fatalf("revocation_reason %q, want the trimmed operator reason", reason)
	}
	rev := e.auditRows(t, provider.ActionRelayRevoke)
	del := e.auditRows(t, provider.ActionRelayDelete)
	if len(rev) != 1 || len(del) != 1 {
		t.Fatalf("audit rows: revoke=%d delete=%d", len(rev), len(del))
	}
	for _, row := range []map[string]any{rev[0], del[0]} {
		d := row["details"].(map[string]any)
		if row["email"] != "admin@provider.test" || row["ip"] != "192.0.2.10" || d["reason"] == nil || d["serials_revoked"] == nil {
			t.Fatalf("audit row: %+v", row)
		}
	}
	if rev[0]["details"].(map[string]any)["reason"] != "key compromised" || del[0]["details"].(map[string]any)["reason"] != "host retired" {
		t.Fatalf("audit reasons: %+v / %+v", rev[0], del[0])
	}
	if e.fanoutCount() != 2 {
		t.Fatalf("fan-out calls: %d, want 2", e.fanoutCount())
	}
}

// --- validation and errors ---

func TestAdminDB_ValidationCodes(t *testing.T) {
	e := newAdminEnv(t, false)
	id := e.relay(t, "Relay-A")
	revoke := "/provider/relays/" + id + "/revoke"
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   any
		status int
		code   string
	}{
		{"no body", http.MethodPost, revoke, nil, http.StatusBadRequest, "invalid_request"},
		{"not json", http.MethodPost, revoke, "reason=x", http.StatusBadRequest, "invalid_request"},
		{"unknown field", http.MethodPost, revoke, map[string]string{"reason": "x", "confirm_name": "Relay-A", "force": "1"}, http.StatusBadRequest, "invalid_request"},
		{"no reason", http.MethodPost, revoke, map[string]string{"confirm_name": "Relay-A"}, http.StatusBadRequest, "reason_required"},
		{"blank reason", http.MethodPost, revoke, confirm("Relay-A", "   "), http.StatusBadRequest, "reason_required"},
		{"long reason", http.MethodPost, revoke, confirm("Relay-A", strings.Repeat("r", 501)), http.StatusBadRequest, "reason_too_long"},
		{"no confirm", http.MethodPost, revoke, map[string]string{"reason": "x"}, http.StatusBadRequest, "confirmation_mismatch"},
		{"wrong case", http.MethodPost, revoke, confirm("relay-a", "x"), http.StatusBadRequest, "confirmation_mismatch"},
		{"padded name", http.MethodPost, revoke, confirm(" Relay-A", "x"), http.StatusBadRequest, "confirmation_mismatch"},
		{"non-uuid id", http.MethodPost, "/provider/relays/not-a-uuid/revoke", confirm("Relay-A", "x"), http.StatusNotFound, "not_found"},
		{"unknown id", http.MethodPost, "/provider/relays/00000000-0000-4000-8000-000000000000/revoke", confirm("Relay-A", "x"), http.StatusNotFound, "not_found"},
		{"delete non-uuid", http.MethodDelete, "/provider/relays/1;DROP", confirm("Relay-A", "x"), http.StatusNotFound, "not_found"},
		{"delete no body", http.MethodDelete, "/provider/relays/" + id, nil, http.StatusBadRequest, "invalid_request"},
	} {
		r := e.do(t, provider.RoleSuperAdmin, tc.method, tc.path, tc.body, nil)
		if r.code != tc.status || errorCode(t, r.body) != tc.code {
			t.Errorf("%s: %d %s, want %d %s", tc.name, r.code, r.body, tc.status, tc.code)
		}
		if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s: content-type %q", tc.name, ct)
		}
	}
	if s := e.status(t, id); s != "pending" {
		t.Fatalf("relay changed by rejected requests: %s", s)
	}
	if n := len(e.auditRows(t, provider.ActionRelayRevoke)); n != 0 || e.fanoutCount() != 0 {
		t.Fatalf("rejected requests wrote %d audit rows / %d fan-outs", n, e.fanoutCount())
	}
}

func TestAdminDB_RepeatedOperations(t *testing.T) {
	e := newAdminEnv(t, false)
	id := e.relay(t, "relay-a")
	rev := "/provider/relays/" + id + "/revoke"
	del := "/provider/relays/" + id
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, rev, confirm("relay-a", "one"), nil); r.code != http.StatusNoContent {
		t.Fatalf("first revoke: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, rev, confirm("relay-a", "two"), nil); r.code != http.StatusConflict || errorCode(t, r.body) != "already_revoked" {
		t.Fatalf("revoke/revoke: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodDelete, del, confirm("relay-a", "remove"), nil); r.code != http.StatusNoContent {
		t.Fatalf("revoke/delete must be allowed: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, rev, confirm("relay-a", "three"), nil); r.code != http.StatusConflict || errorCode(t, r.body) != "already_deleted" {
		t.Fatalf("delete/revoke: %d %s", r.code, r.body)
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodDelete, del, confirm("relay-a", "again"), nil); r.code != http.StatusConflict || errorCode(t, r.body) != "already_deleted" {
		t.Fatalf("delete/delete: %d %s", r.code, r.body)
	}
	if e.status(t, id) != "deleted" {
		t.Fatalf("final status %s", e.status(t, id))
	}
	if len(e.auditRows(t, provider.ActionRelayRevoke)) != 1 || len(e.auditRows(t, provider.ActionRelayDelete)) != 1 || e.fanoutCount() != 2 {
		t.Fatal("refused repeats must write no audit row and call no fan-out")
	}
}

func TestAdminDB_ConcurrentOperations(t *testing.T) {
	e := newAdminEnv(t, false)
	type op struct{ method, path, reason string }
	run := func(t *testing.T, ops ...op) []resp {
		var wg sync.WaitGroup
		out := make([]resp, len(ops))
		start := make(chan struct{})
		for i, o := range ops {
			wg.Add(1)
			go func(i int, o op) {
				defer wg.Done()
				<-start
				out[i] = e.do(t, provider.RoleSuperAdmin, o.method, o.path, confirm("relay-c", o.reason), nil)
			}(i, o)
		}
		close(start)
		wg.Wait()
		return out
	}
	codes := func(rs []resp) map[string]int {
		m := map[string]int{}
		for _, r := range rs {
			if r.code == http.StatusNoContent {
				m["204"]++
			} else {
				m[errorCode(t, r.body)]++
			}
		}
		return m
	}

	t.Run("revoke/revoke", func(t *testing.T) {
		id := e.relay(t, "relay-c")
		got := codes(run(t, op{http.MethodPost, "/provider/relays/" + id + "/revoke", "a"}, op{http.MethodPost, "/provider/relays/" + id + "/revoke", "b"}))
		if got["204"] != 1 || got["already_revoked"] != 1 {
			t.Fatalf("concurrent revokes: %v", got)
		}
	})
	t.Run("revoke/delete", func(t *testing.T) {
		id := e.relay(t, "relay-c")
		got := codes(run(t, op{http.MethodPost, "/provider/relays/" + id + "/revoke", "a"}, op{http.MethodDelete, "/provider/relays/" + id, "b"}))
		// Revoke first: both succeed. Delete first: the revoke gets already_deleted.
		if !(got["204"] == 2 || (got["204"] == 1 && got["already_deleted"] == 1)) || e.status(t, id) != "deleted" {
			t.Fatalf("concurrent revoke+delete: %v final=%s", got, e.status(t, id))
		}
	})
	t.Run("delete/delete", func(t *testing.T) {
		id := e.relay(t, "relay-c")
		got := codes(run(t, op{http.MethodDelete, "/provider/relays/" + id, "a"}, op{http.MethodDelete, "/provider/relays/" + id, "b"}))
		if got["204"] != 1 || got["already_deleted"] != 1 {
			t.Fatalf("concurrent deletes: %v", got)
		}
	})
}

// D-14 for revoke/delete: a failed audit write rolls the whole mutation back,
// and the 500 carries no internal detail.
func TestAdminDB_AuditFailureRollsBackAndLeaksNothing(t *testing.T) {
	e := newAdminEnv(t, false)
	ctx := context.Background()
	id := e.relay(t, "relay-a")
	if _, err := e.pool.Exec(ctx, `ALTER TABLE provider_audit_logs ADD CONSTRAINT phase_a_test_block CHECK (action NOT IN ('relay.revoke', 'relay.delete'))`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/provider/relays/" + id + "/revoke"},
		{http.MethodDelete, "/provider/relays/" + id},
	} {
		r := e.do(t, provider.RoleSuperAdmin, tc.method, tc.path, confirm("relay-a", "x"), nil)
		if r.code != http.StatusInternalServerError || strings.TrimSpace(r.body) != `{"error":"server_error"}` {
			t.Fatalf("%s with failing audit: %d %q", tc.method, r.code, r.body)
		}
	}
	var revokedCerts int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM relay_certificates WHERE relay_id = $1 AND revoked_at IS NOT NULL`, id).Scan(&revokedCerts)
	if e.status(t, id) != "pending" || revokedCerts != 0 || e.fanoutCount() != 0 {
		t.Fatalf("mutation not rolled back: status=%s revokedCerts=%d fanout=%d", e.status(t, id), revokedCerts, e.fanoutCount())
	}
}

// The audit IP is the connection's address; client-supplied forwarding
// headers are not trusted (no trusted-proxy boundary exists).
func TestAdminDB_ForwardedHeadersIgnored(t *testing.T) {
	e := newAdminEnv(t, false)
	id := e.relay(t, "relay-a")
	r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, "/provider/relays/"+id+"/revoke", confirm("relay-a", "x"),
		map[string]string{"X-Forwarded-For": "203.0.113.99", "X-Real-IP": "203.0.113.98", "Forwarded": "for=203.0.113.97"})
	if r.code != http.StatusNoContent {
		t.Fatalf("revoke: %d %s", r.code, r.body)
	}
	if ip := e.auditRows(t, provider.ActionRelayRevoke)[0]["ip"]; ip != "192.0.2.10" {
		t.Fatalf("audit ip %v, want the connection address 192.0.2.10", ip)
	}
}

// --- Create ---

func TestAdminDB_CreateValidationLeavesNoState(t *testing.T) {
	e := newAdminEnv(t, true)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		body any
		code string
	}{
		{"not json", "{", "invalid_request"},
		{"unknown field", map[string]any{"name": "r", "extra": 1}, "invalid_request"},
		{"no name", map[string]any{"dns_allowlist": []string{}}, "invalid_name"},
		{"blank name", map[string]any{"name": "   "}, "invalid_name"},
		{"long name", map[string]any{"name": strings.Repeat("n", 129)}, "invalid_name"},
		{"control char", map[string]any{"name": "relay\x07"}, "invalid_name"},
		{"bad dns", map[string]any{"name": "r", "dns_allowlist": []string{"*.bad.example"}}, "invalid_dns_san"},
		{"bad ip", map[string]any{"name": "r", "ip_allowlist": []string{"10.0.0.01"}}, "invalid_ip_san"},
		{"oversized", `{"name":"` + strings.Repeat("x", 8<<10) + `"}`, "invalid_request"},
	} {
		r := e.do(t, provider.RoleRelayOps, http.MethodPost, "/provider/relays", tc.body, nil)
		if r.code != http.StatusBadRequest || errorCode(t, r.body) != tc.code {
			t.Errorf("%s: %d %s, want 400 %s", tc.name, r.code, r.body, tc.code)
		}
	}
	var relays, audits int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM relays`).Scan(&relays)
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM provider_audit_logs WHERE action = 'relay.create'`).Scan(&audits)
	if relays != 0 || audits != 0 {
		t.Fatalf("invalid creates left %d relays / %d audit rows", relays, audits)
	}
}

func TestAdminDB_CreateByRelayOps(t *testing.T) {
	e := newAdminEnv(t, true)
	e.seedIntermediate(t)
	ctx := context.Background()
	r := e.do(t, provider.RoleRelayOps, http.MethodPost, "/provider/relays", map[string]any{
		"name": " relay-new ", "dns_allowlist": []string{"relay-new.example.com"}, "ip_allowlist": []string{"203.0.113.5"},
	}, nil)
	if r.code != http.StatusCreated {
		t.Fatalf("create: %d %s, want 201", r.code, r.body)
	}
	if cc := r.header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("token-bearing response Cache-Control %q, want no-store", cc)
	}
	var out struct {
		RelayID           string `json:"relay_id"`
		ProvisioningToken string `json:"provisioning_token"`
	}
	if err := json.Unmarshal([]byte(r.body), &out); err != nil || out.RelayID == "" || out.ProvisioningToken == "" {
		t.Fatalf("create body: %s %v", r.body, err)
	}
	var name, jti string
	_ = e.pool.QueryRow(ctx, `SELECT name, COALESCE(enrollment_token_jti, '') FROM relays WHERE id = $1`, out.RelayID).Scan(&name, &jti)
	t.Cleanup(func() { _ = e.rdb.Del(context.Background(), provisioningJTIPrefix+jti).Err() })
	if name != "relay-new" || jti == "" {
		t.Fatalf("stored relay: name=%q jti=%q", name, jti)
	}
	// The committed relay's token is usable: its jti is live and names it.
	if got, err := e.rdb.Get(ctx, provisioningJTIPrefix+jti).Result(); err != nil || got != out.RelayID {
		t.Fatalf("valkey jti -> %q, %v; want %s", got, err, out.RelayID)
	}
	rows := e.auditRows(t, provider.ActionRelayCreate)
	if len(rows) != 1 || rows[0]["ip"] != "192.0.2.10" || strings.Contains(rows[0]["details"].(map[string]any)["name"].(string), out.ProvisioningToken) {
		t.Fatalf("create audit: %+v", rows)
	}
	for _, row := range rows {
		raw, _ := json.Marshal(row)
		if strings.Contains(string(raw), out.ProvisioningToken) {
			t.Fatal("provisioning token found in the audit row")
		}
	}
}

// --- Phase E: the Create response carries what the install command needs ---

// seedIntermediate stores a self-signed certificate as the Intermediate CA row
// and returns its PEM.
func (e *adminEnv) seedIntermediate(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		Subject:      pkix.Name{CommonName: "phase-e intermediate"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	if _, err := e.pool.Exec(context.Background(),
		`INSERT INTO ca_intermediate (encrypted_key, nonce, certificate_pem, not_before, not_after)
		 VALUES ('x', 'x', $1, now() - interval '1 hour', now() + interval '1 hour')`, certPEM); err != nil {
		t.Fatal(err)
	}
	return certPEM
}

func TestAdminDB_CreateReturnsInstallParameters(t *testing.T) {
	e := newAdminEnv(t, true)
	certPEM := e.seedIntermediate(t)
	block, _ := pem.Decode([]byte(certPEM))
	sum := sha256.Sum256(block.Bytes)

	r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, "/provider/relays", map[string]any{"name": "relay-e"}, nil)
	if r.code != http.StatusCreated {
		t.Fatalf("create: %d %s, want 201", r.code, r.body)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(r.body), &out); err != nil {
		t.Fatal(err)
	}
	var jti string
	_ = e.pool.QueryRow(context.Background(), `SELECT COALESCE(enrollment_token_jti, '') FROM relays WHERE id = $1`, out["relay_id"]).Scan(&jti)
	t.Cleanup(func() { _ = e.rdb.Del(context.Background(), provisioningJTIPrefix+jti).Err() })

	if out["ca_fingerprint"] != hex.EncodeToString(sum[:]) {
		t.Errorf("ca_fingerprint %v, want %s", out["ca_fingerprint"], hex.EncodeToString(sum[:]))
	}
	if out["controller_grpc_addr"] != "controller.provider.test:9090" || out["controller_http_addr"] != "controller.provider.test:8080" {
		t.Errorf("controller addresses %v / %v", out["controller_grpc_addr"], out["controller_http_addr"])
	}
}

func TestAdminDB_CreateUnconfiguredAddressesAreEmptyStrings(t *testing.T) {
	e := newAdminEnv(t, true)
	e.seedIntermediate(t)
	e.handler.ControllerGRPCAddr, e.handler.ControllerHTTPAddr = "", ""

	r := e.do(t, provider.RoleRelayOps, http.MethodPost, "/provider/relays", map[string]any{"name": "relay-e2"}, nil)
	if r.code != http.StatusCreated {
		t.Fatalf("create: %d %s, want 201", r.code, r.body)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(r.body), &out)
	var jti string
	_ = e.pool.QueryRow(context.Background(), `SELECT COALESCE(enrollment_token_jti, '') FROM relays WHERE id = $1`, out["relay_id"]).Scan(&jti)
	t.Cleanup(func() { _ = e.rdb.Del(context.Background(), provisioningJTIPrefix+jti).Err() })
	// Present and empty: the console shows placeholders for the operator to fill in.
	for _, k := range []string{"controller_grpc_addr", "controller_http_addr"} {
		if v, ok := out[k]; !ok || v != "" {
			t.Errorf("%s = %v (present %v), want empty string", k, v, ok)
		}
	}
}

func TestAdminDB_CreateWithoutIntermediateLeavesNoState(t *testing.T) {
	e := newAdminEnv(t, true) // no ca_intermediate row
	r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, "/provider/relays", map[string]any{"name": "relay-e3"}, nil)
	if r.code != http.StatusInternalServerError || strings.TrimSpace(r.body) != `{"error":"server_error"}` {
		t.Fatalf("create without CA: %d %q, want 500 server_error", r.code, r.body)
	}
	var relays, audits int
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM relays`).Scan(&relays)
	_ = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM provider_audit_logs`).Scan(&audits)
	if relays != 0 || audits != 0 {
		t.Fatalf("failed create left %d relays / %d audit rows", relays, audits)
	}
}

// D-14 for create (Sprint 22 FU-1): a failed audit write rolls back the relay
// row and its jti, and the Valkey jti written inside the transaction is
// removed again, so no token is left for a relay that doesn't exist.
func TestAdminDB_CreateAuditFailureRollsBack(t *testing.T) {
	e := newAdminEnv(t, true)
	e.seedIntermediate(t)
	ctx := context.Background()
	if _, err := e.pool.Exec(ctx, `ALTER TABLE provider_audit_logs ADD CONSTRAINT fu1_test_block CHECK (action <> 'relay.create')`); err != nil {
		t.Fatal(err)
	}
	before := provisioningKeys(t, e)
	r := e.do(t, provider.RoleRelayOps, http.MethodPost, "/provider/relays", map[string]any{"name": "relay-x"}, nil)
	if r.code != http.StatusInternalServerError || strings.TrimSpace(r.body) != `{"error":"server_error"}` {
		t.Fatalf("create with failing audit: %d %q", r.code, r.body)
	}
	var relays int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM relays`).Scan(&relays)
	if relays != 0 {
		t.Fatalf("failed create left %d relay rows", relays)
	}
	if after := provisioningKeys(t, e); len(after) != len(before) {
		t.Fatalf("failed create left provisioning jtis: before %d, after %d", len(before), len(after))
	}
}

// A Valkey failure while storing the jti creates nothing: no relay row, no
// audit row.
func TestAdminDB_CreateValkeyFailureLeavesNoRelay(t *testing.T) {
	e := newAdminEnv(t, true)
	e.seedIntermediate(t)
	ctx := context.Background()
	e.handler.Redis = failingSet{e.rdb}
	r := e.do(t, provider.RoleRelayOps, http.MethodPost, "/provider/relays", map[string]any{"name": "relay-y"}, nil)
	if r.code != http.StatusInternalServerError {
		t.Fatalf("create with failing valkey: %d %q", r.code, r.body)
	}
	var relays, audits int
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM relays`).Scan(&relays)
	_ = e.pool.QueryRow(ctx, `SELECT count(*) FROM provider_audit_logs WHERE action = 'relay.create'`).Scan(&audits)
	if relays != 0 || audits != 0 {
		t.Fatalf("failed create left %d relays / %d audit rows", relays, audits)
	}
}

type failingSet struct{ valkeycompat.Cmdable }

func (failingSet) Set(context.Context, string, any, time.Duration) *valkeycompat.StatusCmd {
	cmd := &valkeycompat.StatusCmd{}
	cmd.SetErr(errors.New("valkey unavailable"))
	return cmd
}

func provisioningKeys(t *testing.T, e *adminEnv) []string {
	t.Helper()
	keys, err := e.rdb.Keys(context.Background(), provisioningJTIPrefix+"*").Result()
	if err != nil {
		t.Fatal(err)
	}
	return keys
}
