package provider_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/middleware"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// Sprint 27 K1 (D-76) end-to-end: API keys over the real store (throwaway
// DB), middleware and handlers, wired like cmd/server/main.go.

// spyVerifier counts JWT verifications of API keys: a key must never reach
// the session (JWT) path.
type spyVerifier struct {
	inner   middleware.ProviderTokenVerifier
	keySeen atomic.Int32
}

func (s *spyVerifier) Verify(tok string) (*provider.ProviderClaims, error) {
	if strings.HasPrefix(tok, "zpk_") {
		s.keySeen.Add(1)
	}
	return s.inner.Verify(tok)
}

type failingLimiter struct{}

func (failingLimiter) Allow(string, time.Time) (bool, error) {
	return false, errors.New("limiter down")
}

type keyEnv struct {
	srv    *httptest.Server
	pool   *pgxpool.Pool
	admin  string
	auth   *provider.APIKeyAuthenticator
	spy    *spyVerifier
	tenant string
	clock  time.Time
}

func newKeyEnv(t *testing.T) *keyEnv {
	t.Helper()
	store, pool := provider.NewTestStore(t)
	ctx := context.Background()
	if res, _, err := provider.Bootstrap(ctx, store, e2eEmail, e2eTempPass); err != nil || res != provider.BootstrapCreated {
		t.Fatalf("bootstrap: res=%v err=%v", res, err)
	}
	ids, err := provider.NewIdentityService(e2eKey)
	if err != nil {
		t.Fatal(err)
	}
	e := &keyEnv{pool: pool, clock: time.Now()}
	e.spy = &spyVerifier{inner: ids}
	e.auth = provider.NewAPIKeyAuthenticator(store)
	e.auth.Now = func() time.Time { return e.clock }
	authz := provider.NewAuthz()
	auth := provider.NewAuthHandlers(ids, provider.NewLocalPasswordAuthenticator(store), store, allowAllLimiter{})
	handlers := provider.NewHandlers(store, authz)
	ops := provider.NewOperatorHandlers(store, authz)
	keys := &provider.APIKeyHandlers{Store: store, Authz: authz}
	reads := provider.NewReadHandlers(pool, authz, store, nil, nil)
	inv := &provider.InvoiceHandler{DB: pool, Authz: authz, Store: store}
	bs := &provider.BillingSettingsHandler{DB: pool, Authz: authz, Store: store}
	session := middleware.RequireProvider(e.spy, store)
	orKey := middleware.RequireProvider(e.spy, store, middleware.AcceptAPIKeys(e.auth))

	mux := http.NewServeMux()
	mux.Handle("POST /provider/auth/login", middleware.RefuseProviderAPIKey(http.HandlerFunc(auth.Login)))
	mux.Handle("POST /provider/auth/password",
		middleware.RequireProvider(e.spy, store, middleware.AllowPasswordChangeToken())(http.HandlerFunc(auth.ChangePassword)))
	mux.Handle("POST /provider/auth/logout", session(http.HandlerFunc(auth.Logout)))
	mux.Handle("GET /provider/me", session(http.HandlerFunc(handlers.Me)))
	mux.Handle("GET /provider/users", session(http.HandlerFunc(handlers.ListUsers)))
	mux.Handle("POST /provider/users", session(http.HandlerFunc(ops.Create)))
	mux.Handle("GET /provider/api-keys", session(http.HandlerFunc(keys.List)))
	mux.Handle("POST /provider/api-keys", session(http.HandlerFunc(keys.Create)))
	mux.Handle("POST /provider/api-keys/{id}/revoke", session(http.HandlerFunc(keys.Revoke)))
	mux.Handle("GET /provider/relays", orKey(http.HandlerFunc(reads.ListRelays)))
	mux.Handle("GET /provider/tenants", orKey(http.HandlerFunc(reads.ListTenants)))
	mux.Handle("GET /provider/tenants/{id}", orKey(http.HandlerFunc(reads.GetTenant)))
	mux.Handle("GET /provider/audit", orKey(http.HandlerFunc(reads.QueryAudit)))
	mux.Handle("GET /provider/invoices", orKey(http.HandlerFunc(inv.List)))
	mux.Handle("POST /provider/invoices/{id}/issue", orKey(http.HandlerFunc(inv.Issue)))
	mux.Handle("PATCH /provider/billing/settings", orKey(http.HandlerFunc(bs.Update)))
	rig := addTOTPRoutes(mux, ids, store)
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	rig.register(t, e.srv)

	op := &operatorEnv{srv: e.srv, pool: pool}
	e.admin = op.activate(t, e2eEmail, e2eTempPass, e2eFinalPass)
	if err := pool.QueryRow(ctx, `INSERT INTO workspaces (slug, name, status, trust_domain)
	                                 VALUES ('acme', 'Acme', 'active', 'acme.zecurity.test') RETURNING id::text`).Scan(&e.tenant); err != nil {
		t.Fatal(err)
	}
	return e
}

type createdKey struct {
	APIKey provider.APIKey `json:"api_key"`
	Key    string          `json:"key"`
}

func (e *keyEnv) createKey(t *testing.T, name, role string, cidrs []string) createdKey {
	t.Helper()
	body := map[string]any{"name": name, "role": role, "reason": "k1 test",
		"expires_at": time.Now().Add(90 * 24 * time.Hour).UTC().Format(time.RFC3339)}
	if cidrs != nil {
		body["allowed_cidrs"] = cidrs
	}
	code, raw := call(t, e.srv, http.MethodPost, "/provider/api-keys", e.admin, body)
	if code != http.StatusCreated {
		t.Fatalf("create key %s: %d %s", name, code, raw)
	}
	var c createdKey
	if err := json.Unmarshal([]byte(raw), &c); err != nil || !strings.HasPrefix(c.Key, "zpk_") {
		t.Fatalf("create response: %s %v", raw, err)
	}
	return c
}

func (e *keyEnv) allAudit(t *testing.T) string {
	t.Helper()
	var s string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT coalesce(string_agg(provider_email || ' ' || action || ' ' || coalesce(details::text, ''), E'\n'), '') FROM provider_audit_logs`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Create: the key once (no-store), the list shows the prefix only, the role
// and expiry are bounded (handler and DB), a reason is required, the audit
// holds neither the key nor its hash.
func TestAPIKeys_CreateListRevoke(t *testing.T) {
	e := newKeyEnv(t)
	exp := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)

	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/provider/api-keys",
		strings.NewReader(`{"name":"ci-reader","role":"auditor","expires_at":"`+exp+`","allowed_cidrs":["10.1.2.3/8"],"reason":"ci"}`))
	req.Header.Set("Authorization", "Bearer "+e.admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var c createdKey
	_ = json.NewDecoder(resp.Body).Decode(&c)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("create: %d cache-control %q", resp.StatusCode, resp.Header.Get("Cache-Control"))
	}
	if len(c.Key) != 4+32+1+43 || c.APIKey.Prefix != c.Key[:12] || c.APIKey.Role != "auditor" ||
		c.APIKey.Status != "active" || len(c.APIKey.AllowedCIDRs) != 1 || c.APIKey.AllowedCIDRs[0] != "10.0.0.0/8" {
		t.Fatalf("created key: %+v (len %d)", c.APIKey, len(c.Key))
	}
	sum := sha256.Sum256([]byte(c.Key))
	secretPart := c.Key[strings.LastIndex(c.Key, "_")+1:]

	code, list := call(t, e.srv, http.MethodGet, "/provider/api-keys", e.admin, nil)
	if code != 200 || !strings.Contains(list, c.APIKey.Prefix) || strings.Contains(list, secretPart) || strings.Contains(list, hex.EncodeToString(sum[:])) {
		t.Fatalf("list: %d %s", code, list)
	}
	for _, bad := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"super-admin role", map[string]any{"name": "x", "role": "super-admin", "expires_at": exp, "reason": "r"}, "invalid_role"},
		{"unknown role", map[string]any{"name": "x", "role": "owner", "expires_at": exp, "reason": "r"}, "invalid_role"},
		{"366 days", map[string]any{"name": "x", "role": "auditor", "expires_at": time.Now().Add(366 * 24 * time.Hour).Format(time.RFC3339), "reason": "r"}, "invalid_expires_at"},
		{"past", map[string]any{"name": "x", "role": "auditor", "expires_at": time.Now().Add(-time.Hour).Format(time.RFC3339), "reason": "r"}, "invalid_expires_at"},
		{"no reason", map[string]any{"name": "x", "role": "auditor", "expires_at": exp}, "reason_required"},
		{"bad cidr", map[string]any{"name": "x", "role": "auditor", "expires_at": exp, "allowed_cidrs": []string{"nope"}, "reason": "r"}, "invalid_cidr"},
		{"empty name", map[string]any{"name": " ", "role": "auditor", "expires_at": exp, "reason": "r"}, "invalid_name"},
	} {
		if code, body := call(t, e.srv, http.MethodPost, "/provider/api-keys", e.admin, bad.body); code != 400 || errCode(body) != bad.want {
			t.Errorf("%s: %d %s", bad.name, code, body)
		}
	}
	if code, body := call(t, e.srv, http.MethodPost, "/provider/api-keys", e.admin,
		map[string]any{"name": "CI-Reader", "role": "billing", "expires_at": exp, "reason": "r"}); code != 409 || errCode(body) != "name_taken" {
		t.Fatalf("duplicate name: %d %s", code, body)
	}

	// The DB refuses what the handler refuses.
	ctx := context.Background()
	var adminID string
	_ = e.pool.QueryRow(ctx, `SELECT id::text FROM provider_users WHERE email = $1`, e2eEmail).Scan(&adminID)
	for name, q := range map[string]string{
		"super-admin": `INSERT INTO provider_api_keys (name, role, prefix, secret_sha256, created_by, expires_at)
		               VALUES ('db1', 'super-admin', 'zpk_00000000', decode(repeat('00', 32), 'hex'), $1, NOW() + interval '1 day')`,
		"366 days": `INSERT INTO provider_api_keys (name, role, prefix, secret_sha256, created_by, expires_at)
		               VALUES ('db2', 'auditor', 'zpk_00000000', decode(repeat('00', 32), 'hex'), $1, NOW() + interval '366 days')`,
	} {
		if _, err := e.pool.Exec(ctx, q, adminID); err == nil {
			t.Errorf("DB accepted a key with %s", name)
		}
	}

	// Revoke: typed name, reason, terminal.
	path := "/provider/api-keys/" + c.APIKey.ID + "/revoke"
	if code, body := call(t, e.srv, http.MethodPost, path, e.admin, map[string]any{"reason": "rotate", "confirm_name": "wrong"}); code != 400 || errCode(body) != "confirmation_mismatch" {
		t.Fatalf("revoke wrong name: %d %s", code, body)
	}
	if code, body := call(t, e.srv, http.MethodPost, path, e.admin, map[string]any{"confirm_name": "ci-reader"}); code != 400 || errCode(body) != "reason_required" {
		t.Fatalf("revoke without reason: %d %s", code, body)
	}
	if code, body := call(t, e.srv, http.MethodPost, path, e.admin, map[string]any{"reason": "rotate", "confirm_name": "ci-reader"}); code != 200 || !strings.Contains(body, `"status":"revoked"`) {
		t.Fatalf("revoke: %d %s", code, body)
	}
	if code, _ := call(t, e.srv, http.MethodPost, path, e.admin, map[string]any{"reason": "again", "confirm_name": "ci-reader"}); code != 409 {
		t.Fatalf("revoke twice: %d", code)
	}
	if _, err := e.pool.Exec(ctx, `UPDATE provider_api_keys SET revoked_at = NULL, revoked_by = NULL, revoke_reason = NULL WHERE id = $1`, c.APIKey.ID); err == nil {
		t.Fatal("DB let a revoked key be un-revoked")
	}
	// A revoked key's name is free again.
	e.createKey(t, "ci-reader", "auditor", nil)

	audit := e.allAudit(t)
	if !strings.Contains(audit, "api_key.create") || !strings.Contains(audit, "api_key.revoke") || !strings.Contains(audit, `"reason": "rotate"`) {
		t.Fatalf("audit lacks key rows: %s", audit)
	}
	if strings.Contains(audit, secretPart) || strings.Contains(audit, c.Key) || strings.Contains(audit, hex.EncodeToString(sum[:])) {
		t.Fatal("the key or its hash is in the audit log")
	}
}

// Auth: each role's reads work, writes are refused, every bad key gets the
// same 401, session-only routes refuse keys, and no key reaches the JWT
// verifier.
func TestAPIKeys_Auth(t *testing.T) {
	e := newKeyEnv(t)
	support := e.createKey(t, "support-bot", "support", nil)
	billing := e.createKey(t, "billing-bot", "billing", nil)
	relayOps := e.createKey(t, "relay-bot", "relay-ops", nil)
	auditor := e.createKey(t, "audit-bot", "auditor", nil)

	for _, c := range []struct {
		key          string
		method, path string
		want         int
	}{
		{support.Key, "GET", "/provider/tenants/" + e.tenant, 200},
		{support.Key, "GET", "/provider/tenants", 200},
		{support.Key, "GET", "/provider/invoices", 403},
		{support.Key, "GET", "/provider/audit", 403},
		{billing.Key, "GET", "/provider/invoices", 200},
		{billing.Key, "GET", "/provider/tenants", 200},
		{billing.Key, "GET", "/provider/tenants/" + e.tenant, 403},
		{billing.Key, "PATCH", "/provider/billing/settings", 403},
		{billing.Key, "POST", "/provider/invoices/00000000-0000-0000-0000-000000000001/issue", 403},
		{relayOps.Key, "GET", "/provider/relays", 200},
		{relayOps.Key, "GET", "/provider/tenants", 403},
		{auditor.Key, "GET", "/provider/audit", 200},
		{auditor.Key, "GET", "/provider/invoices", 200},
		{auditor.Key, "PATCH", "/provider/billing/settings", 403},
		// Session-only routes refuse every key (D-76).
		{auditor.Key, "GET", "/provider/me", 403},
		{auditor.Key, "GET", "/provider/users", 403},
		{auditor.Key, "POST", "/provider/users", 403},
		{auditor.Key, "GET", "/provider/api-keys", 403},
		{auditor.Key, "POST", "/provider/api-keys", 403},
		{auditor.Key, "POST", "/provider/auth/logout", 403},
		{auditor.Key, "POST", "/provider/auth/password", 403},
		{auditor.Key, "POST", "/provider/auth/login", 403},
		{auditor.Key, "POST", "/provider/auth/totp/verify", 403},
		{auditor.Key, "POST", "/provider/auth/totp/enroll", 403},
	} {
		body := map[string]any{"reason": "k1", "tax_label": "VAT", "email": "x@inkyank.com", "role": "relay-ops", "password": "x"}
		if code, raw := call(t, e.srv, c.method, c.path, c.key, body); code != c.want {
			t.Errorf("%s %s %s: %d %s, want %d", c.key[:12], c.method, c.path, code, raw, c.want)
		}
	}

	// The support key's tenant read is audited as the key: no user id,
	// actor api_key:<name> (<prefix>), key_id in details.
	var email, details string
	var uid *string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT provider_email, provider_user_id::text, details::text FROM provider_audit_logs
		  WHERE action = 'tenant.read' ORDER BY created_at DESC LIMIT 1`).Scan(&email, &uid, &details); err != nil {
		t.Fatal(err)
	}
	if email != "api_key:support-bot ("+support.APIKey.Prefix+")" || uid != nil || !strings.Contains(details, support.APIKey.ID) {
		t.Fatalf("tenant.read audit: email=%q uid=%v details=%s", email, uid, details)
	}
	var lastUsed *time.Time
	_ = e.pool.QueryRow(context.Background(), `SELECT last_used_at FROM provider_api_keys WHERE id = $1`, support.APIKey.ID).Scan(&lastUsed)
	if lastUsed == nil {
		t.Fatal("last_used_at not recorded")
	}

	// Every bad key: 401 with the same body.
	bad := []string{
		auditor.Key[:len(auditor.Key)-1] + map[bool]string{true: "A", false: "B"}[auditor.Key[len(auditor.Key)-1] != 'A'], // wrong secret
		"zpk_" + strings.Repeat("0", 32) + "_" + auditor.Key[len(auditor.Key)-43:],                                        // unknown id
		"zpk_nonsense",
		"zpk_" + strings.ToUpper(auditor.Key[4:36]) + auditor.Key[36:],
	}
	for _, k := range bad {
		if code, body := call(t, e.srv, "GET", "/provider/audit", k, nil); code != 401 || body != `{"error":"invalid_api_key"}`+"\n" {
			t.Errorf("bad key %q: %d %q", k[:16], code, body)
		}
	}
	// Revoked → 401 on the next call.
	if code, body := call(t, e.srv, "POST", "/provider/api-keys/"+relayOps.APIKey.ID+"/revoke", e.admin,
		map[string]any{"reason": "leaked", "confirm_name": "relay-bot"}); code != 200 {
		t.Fatalf("revoke: %d %s", code, body)
	}
	if code, _ := call(t, e.srv, "GET", "/provider/relays", relayOps.Key, nil); code != 401 {
		t.Fatalf("revoked key: %d", code)
	}
	// Expired → 401 (the authenticator's clock moves past expiry).
	e.clock = time.Now().Add(91 * 24 * time.Hour)
	if code, _ := call(t, e.srv, "GET", "/provider/audit", auditor.Key, nil); code != 401 {
		t.Fatalf("expired key: %d", code)
	}
	e.clock = time.Now()

	if n := e.spy.keySeen.Load(); n != 0 {
		t.Fatalf("an API key reached the JWT verifier %d times", n)
	}
}

// CIDR allowlist and rate limit (fail closed).
func TestAPIKeys_CIDRAndRateLimit(t *testing.T) {
	e := newKeyEnv(t)
	outside := e.createKey(t, "outside", "auditor", []string{"10.0.0.0/8"})
	inside := e.createKey(t, "inside", "auditor", []string{"10.0.0.0/8", "127.0.0.0/8", "::1/128"})
	if code, _ := call(t, e.srv, "GET", "/provider/audit", outside.Key, nil); code != 401 {
		t.Fatalf("outside CIDR: %d", code)
	}
	if code, _ := call(t, e.srv, "GET", "/provider/audit", inside.Key, nil); code != 200 {
		t.Fatalf("inside CIDR: %d", code)
	}

	e.auth.Limiter = provider.NewTokenBucketLimiter(0.0001, 3)
	got := []int{}
	for i := 0; i < 4; i++ {
		code, _ := call(t, e.srv, "GET", "/provider/audit", inside.Key, nil)
		got = append(got, code)
	}
	if got[0] != 200 || got[2] != 200 || got[3] != 429 {
		t.Fatalf("burst of 3 then limited: %v", got)
	}
	e.auth.Limiter = failingLimiter{}
	if code, _ := call(t, e.srv, "GET", "/provider/audit", inside.Key, nil); code != 429 {
		t.Fatalf("limiter error must refuse: %d", code)
	}
}
