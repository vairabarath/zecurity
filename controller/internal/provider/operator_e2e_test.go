package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/middleware"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// Phase U end-to-end: operator management over the real store (throwaway DB),
// middleware and handlers, wired like cmd/server/main.go. Reuses call(),
// session, allowAllLimiter and the e2e* constants from e2e_test.go.

type operatorEnv struct {
	srv   *httptest.Server
	pool  *pgxpool.Pool
	admin string // full session token of the bootstrap super-admin
}

func newOperatorEnv(t *testing.T) *operatorEnv {
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
	authz := provider.NewAuthz()
	auth := provider.NewAuthHandlers(ids, provider.NewLocalPasswordAuthenticator(store), store, allowAllLimiter{})
	handlers := provider.NewHandlers(store, authz)
	ops := provider.NewOperatorHandlers(store, authz)
	require := middleware.RequireProvider(ids, store)

	mux := http.NewServeMux()
	mux.Handle("POST /provider/auth/login", http.HandlerFunc(auth.Login))
	mux.Handle("POST /provider/auth/password",
		middleware.RequireProvider(ids, store, middleware.AllowPasswordChangeToken())(http.HandlerFunc(auth.ChangePassword)))
	mux.Handle("GET /provider/me", require(http.HandlerFunc(handlers.Me)))
	mux.Handle("GET /provider/users", require(http.HandlerFunc(handlers.ListUsers)))
	mux.Handle("POST /provider/users", require(http.HandlerFunc(ops.Create)))
	mux.Handle("PATCH /provider/users/{id}", require(http.HandlerFunc(ops.ChangeRole)))
	mux.Handle("POST /provider/users/{id}/disable", require(http.HandlerFunc(ops.Disable)))
	mux.Handle("POST /provider/users/{id}/enable", require(http.HandlerFunc(ops.Enable)))
	mux.Handle("POST /provider/users/{id}/reset-password", require(http.HandlerFunc(ops.ResetPassword)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	env := &operatorEnv{srv: srv, pool: pool}
	env.admin = env.activate(t, e2eEmail, e2eTempPass, e2eFinalPass)
	return env
}

// loginAs posts credentials and returns status + session.
func (e *operatorEnv) loginAs(t *testing.T, email, password string) (int, session) {
	t.Helper()
	code, body := call(t, e.srv, http.MethodPost, "/provider/auth/login", "", map[string]string{"email": email, "password": password})
	var s session
	_ = json.Unmarshal([]byte(body), &s)
	return code, s
}

// activate logs in with a temporary password, completes the forced change and
// returns the full session token.
func (e *operatorEnv) activate(t *testing.T, email, temp, final string) string {
	t.Helper()
	code, s := e.loginAs(t, email, temp)
	if code != http.StatusOK || !s.PasswordChangeRequired {
		t.Fatalf("login %s with temporary password: %d %+v", email, code, s)
	}
	code, body := call(t, e.srv, http.MethodPost, "/provider/auth/password", s.Token,
		map[string]string{"current_password": temp, "new_password": final})
	if code != http.StatusOK {
		t.Fatalf("forced change for %s: %d %s", email, code, body)
	}
	var full session
	_ = json.Unmarshal([]byte(body), &full)
	return full.Token
}

type createdOperator struct {
	User              provider.OperatorView `json:"user"`
	TemporaryPassword string                `json:"temporary_password"`
}

func (e *operatorEnv) create(t *testing.T, email, role string) createdOperator {
	t.Helper()
	code, body := call(t, e.srv, http.MethodPost, "/provider/users", e.admin, map[string]string{"email": email, "role": role})
	if code != http.StatusCreated {
		t.Fatalf("create %s: %d %s", email, code, body)
	}
	var c createdOperator
	if err := json.Unmarshal([]byte(body), &c); err != nil || c.TemporaryPassword == "" {
		t.Fatalf("create response %s: %v", body, err)
	}
	return c
}

func (e *operatorEnv) reset(t *testing.T, id string) string {
	t.Helper()
	code, body := call(t, e.srv, http.MethodPost, "/provider/users/"+id+"/reset-password", e.admin, nil)
	if code != http.StatusOK {
		t.Fatalf("reset %s: %d %s", id, code, body)
	}
	var r struct {
		TemporaryPassword string `json:"temporary_password"`
	}
	_ = json.Unmarshal([]byte(body), &r)
	return r.TemporaryPassword
}

func errCode(body string) string {
	var m map[string]string
	_ = json.Unmarshal([]byte(body), &m)
	return m["error"]
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestOperatorRoutes_RoleMatrix(t *testing.T) {
	e := newOperatorEnv(t)
	c := e.create(t, "ops@inkyank.com", provider.RoleRelayOps)

	// A password-change-only token can't manage operators.
	_, pwc := e.loginAs(t, "ops@inkyank.com", c.TemporaryPassword)
	if code, body := call(t, e.srv, http.MethodPost, "/provider/users", pwc.Token, map[string]string{"email": "x@inkyank.com", "role": "relay-ops"}); code != http.StatusForbidden || errCode(body) != "password_change_required" {
		t.Fatalf("pwc token: %d %s", code, body)
	}
	opsTok := e.activate(t, "ops@inkyank.com", c.TemporaryPassword, "ops-real-password-1")

	id := c.User.ID
	for _, r := range []struct{ method, path string }{
		{http.MethodGet, "/provider/users"},
		{http.MethodPost, "/provider/users"},
		{http.MethodPatch, "/provider/users/" + id},
		{http.MethodPost, "/provider/users/" + id + "/disable"},
		{http.MethodPost, "/provider/users/" + id + "/enable"},
		{http.MethodPost, "/provider/users/" + id + "/reset-password"},
	} {
		if code, _ := call(t, e.srv, r.method, r.path, opsTok, map[string]string{"email": "y@inkyank.com", "role": "relay-ops"}); code != http.StatusForbidden {
			t.Errorf("relay-ops %s %s: %d, want 403", r.method, r.path, code)
		}
		if code, _ := call(t, e.srv, r.method, r.path, "", nil); code != http.StatusUnauthorized {
			t.Errorf("no token %s %s: %d, want 401", r.method, r.path, code)
		}
	}
}

// Create → one-time temporary password → forced change → relay-ops session.
func TestCreateOperator_TempPasswordFlow(t *testing.T) {
	e := newOperatorEnv(t)
	code, body := call(t, e.srv, http.MethodPost, "/provider/users", e.admin, map[string]string{"email": "  New.Ops@InkYank.com ", "role": "relay-ops"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var c createdOperator
	_ = json.Unmarshal([]byte(body), &c)
	if c.User.Email != "new.ops@inkyank.com" || c.User.Role != "relay-ops" || !c.User.MustChangePassword || !c.User.HasPassword || len(c.TemporaryPassword) != 20 {
		t.Fatalf("create response: %+v", c)
	}
	if strings.Contains(body, "password_hash") || strings.Contains(body, "argon2") {
		t.Fatalf("create response leaks credential material: %s", body)
	}

	tok := e.activate(t, "new.ops@inkyank.com", c.TemporaryPassword, "new-ops-password-1")
	if code, body := call(t, e.srv, http.MethodGet, "/provider/me", tok, nil); code != http.StatusOK || !strings.Contains(body, `"role":"relay-ops"`) {
		t.Fatalf("/provider/me as new operator: %d %s", code, body)
	}
	if code, _ := call(t, e.srv, http.MethodGet, "/provider/users", tok, nil); code != http.StatusForbidden {
		t.Fatalf("relay-ops listing users: %d, want 403", code)
	}
}

func TestCreateOperator_DuplicatesAndValidation(t *testing.T) {
	e := newOperatorEnv(t)
	c := e.create(t, "ops@inkyank.com", provider.RoleRelayOps)

	cases := []struct {
		email, role, wantCode string
		want                  int
	}{
		{"ops@inkyank.com", "relay-ops", "already_exists", http.StatusConflict},
		{"OPS@InkYank.com", "super-admin", "already_exists", http.StatusConflict},
		{"not-an-email", "relay-ops", "invalid_email", http.StatusBadRequest},
		{"a b@inkyank.com", "relay-ops", "invalid_email", http.StatusBadRequest},
		{"valid@inkyank.com", "admin", "invalid_role", http.StatusBadRequest},
	}
	for _, tc := range cases {
		code, body := call(t, e.srv, http.MethodPost, "/provider/users", e.admin, map[string]string{"email": tc.email, "role": tc.role})
		if code != tc.want || errCode(body) != tc.wantCode {
			t.Errorf("%q/%q: %d %s, want %d %s", tc.email, tc.role, code, body, tc.want, tc.wantCode)
		}
	}

	if code, _ := call(t, e.srv, http.MethodPost, "/provider/users/"+c.User.ID+"/disable", e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("disable: %d", code)
	}
	code, body := call(t, e.srv, http.MethodPost, "/provider/users", e.admin, map[string]string{"email": "ops@inkyank.com", "role": "relay-ops"})
	if code != http.StatusConflict || errCode(body) != "exists_disabled" {
		t.Fatalf("re-adding a disabled email: %d %s, want 409 exists_disabled", code, body)
	}
}

// A role change ends the operator's existing sessions.
func TestChangeRole_EndsSessions(t *testing.T) {
	e := newOperatorEnv(t)
	c := e.create(t, "ops@inkyank.com", provider.RoleRelayOps)
	opsTok := e.activate(t, "ops@inkyank.com", c.TemporaryPassword, "ops-real-password-1")

	code, body := call(t, e.srv, http.MethodPatch, "/provider/users/"+c.User.ID, e.admin, map[string]string{"role": "super-admin"})
	if code != http.StatusOK || !strings.Contains(body, `"role":"super-admin"`) {
		t.Fatalf("promote: %d %s", code, body)
	}
	if code, _ := call(t, e.srv, http.MethodGet, "/provider/me", opsTok, nil); code != http.StatusUnauthorized {
		t.Fatalf("old token after role change: %d, want 401", code)
	}
	_, s := e.loginAs(t, "ops@inkyank.com", "ops-real-password-1")
	if code, _ := call(t, e.srv, http.MethodGet, "/provider/users", s.Token, nil); code != http.StatusOK {
		t.Fatalf("promoted operator listing users: %d, want 200", code)
	}
	// Same role again: 200, no-op.
	if code, _ := call(t, e.srv, http.MethodPatch, "/provider/users/"+c.User.ID, e.admin, map[string]string{"role": "super-admin"}); code != http.StatusOK {
		t.Fatalf("same-role PATCH: %d", code)
	}
}

// Disable ends sessions; enable clears the old credential (old password no
// longer works) and issues none; reset-password restores access.
func TestDisableEnableReset_Flow(t *testing.T) {
	e := newOperatorEnv(t)
	c := e.create(t, "ops@inkyank.com", provider.RoleRelayOps)
	const opsPass = "ops-real-password-1"
	opsTok := e.activate(t, "ops@inkyank.com", c.TemporaryPassword, opsPass)
	id := c.User.ID

	if code, _ := call(t, e.srv, http.MethodPost, "/provider/users/"+id+"/disable", e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("disable: %d", code)
	}
	if code, _ := call(t, e.srv, http.MethodGet, "/provider/me", opsTok, nil); code == http.StatusOK {
		t.Fatal("token still valid after disable")
	}
	if code, _ := e.loginAs(t, "ops@inkyank.com", opsPass); code != http.StatusUnauthorized {
		t.Fatalf("disabled login: %d, want 401", code)
	}
	if code, _ := call(t, e.srv, http.MethodPost, "/provider/users/"+id+"/disable", e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("idempotent disable: %d", code)
	}
	if code, body := call(t, e.srv, http.MethodPost, "/provider/users/"+id+"/reset-password", e.admin, nil); code != http.StatusConflict || errCode(body) != "account_disabled" {
		t.Fatalf("reset of disabled: %d %s, want 409 account_disabled", code, body)
	}

	// Enable: 204, no body, no password; the old password no longer works.
	code, body := call(t, e.srv, http.MethodPost, "/provider/users/"+id+"/enable", e.admin, nil)
	if code != http.StatusNoContent || body != "" {
		t.Fatalf("enable: %d %q", code, body)
	}
	if code, _ := e.loginAs(t, "ops@inkyank.com", opsPass); code != http.StatusUnauthorized {
		t.Fatalf("old password after enable: %d, want 401 (credential must be cleared)", code)
	}
	_, list := call(t, e.srv, http.MethodGet, "/provider/users", e.admin, nil)
	if !strings.Contains(list, `"email":"ops@inkyank.com"`) || !strings.Contains(list, `"has_password":false`) {
		t.Fatalf("list after enable should show has_password=false: %s", list)
	}

	// Enabling an enabled operator is a no-op (and must not clear anything).
	temp := e.reset(t, id)
	if code, _ := call(t, e.srv, http.MethodPost, "/provider/users/"+id+"/enable", e.admin, nil); code != http.StatusNoContent {
		t.Fatalf("idempotent enable: %d", code)
	}
	newTok := e.activate(t, "ops@inkyank.com", temp, "ops-new-password-2") // temp still valid → enable didn't clear it
	if code, _ := call(t, e.srv, http.MethodGet, "/provider/me", newTok, nil); code != http.StatusOK {
		t.Fatalf("after reset + change: %d", code)
	}
}

// Reset ends sessions and invalidates the old password.
func TestResetPassword_EndsSessions(t *testing.T) {
	e := newOperatorEnv(t)
	c := e.create(t, "ops@inkyank.com", provider.RoleRelayOps)
	opsTok := e.activate(t, "ops@inkyank.com", c.TemporaryPassword, "ops-real-password-1")

	temp := e.reset(t, c.User.ID)
	if code, _ := call(t, e.srv, http.MethodGet, "/provider/me", opsTok, nil); code != http.StatusUnauthorized {
		t.Fatalf("old token after reset: %d, want 401", code)
	}
	if code, _ := e.loginAs(t, "ops@inkyank.com", "ops-real-password-1"); code != http.StatusUnauthorized {
		t.Fatalf("old password after reset: %d, want 401", code)
	}
	if _, s := e.loginAs(t, "ops@inkyank.com", temp); !s.PasswordChangeRequired {
		t.Fatal("reset password must require a change at login")
	}
}

func TestOperatorGuards_HTTP(t *testing.T) {
	e := newOperatorEnv(t)
	code, body := call(t, e.srv, http.MethodGet, "/provider/me", e.admin, nil)
	var me struct {
		UserID string `json:"user_id"`
	}
	_ = json.Unmarshal([]byte(body), &me)
	if code != http.StatusOK || me.UserID == "" {
		t.Fatalf("/provider/me: %d %s", code, body)
	}

	for _, r := range []struct {
		method, path string
		body         any
		want         int
		wantCode     string
	}{
		{http.MethodPatch, "/provider/users/" + me.UserID, map[string]string{"role": "relay-ops"}, http.StatusConflict, "cannot_modify_self"},
		{http.MethodPost, "/provider/users/" + me.UserID + "/disable", nil, http.StatusConflict, "cannot_modify_self"},
		{http.MethodPost, "/provider/users/" + me.UserID + "/reset-password", nil, http.StatusConflict, "cannot_modify_self"},
		{http.MethodPost, "/provider/users/00000000-0000-0000-0000-000000000000/disable", nil, http.StatusNotFound, "not_found"},
		{http.MethodPost, "/provider/users/not-a-uuid/enable", nil, http.StatusNotFound, "not_found"},
		{http.MethodPatch, "/provider/users/00000000-0000-0000-0000-000000000000", map[string]string{"role": "bogus"}, http.StatusBadRequest, "invalid_role"},
	} {
		code, body := call(t, e.srv, r.method, r.path, e.admin, r.body)
		if code != r.want || errCode(body) != r.wantCode {
			t.Errorf("%s %s: %d %s, want %d %s", r.method, r.path, code, body, r.want, r.wantCode)
		}
	}
}

// No password or credential material in responses (except the one-time
// temporary password), audit rows, or the process log.
func TestOperatorResponses_NoSecrets(t *testing.T) {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	e := newOperatorEnv(t)
	c := e.create(t, "ops@inkyank.com", provider.RoleRelayOps)
	temp2 := e.reset(t, c.User.ID)

	code, list := call(t, e.srv, http.MethodGet, "/provider/users", e.admin, nil)
	if code != http.StatusOK || strings.Contains(list, "password_hash") || strings.Contains(list, "argon2") ||
		strings.Contains(list, "SessionGeneration") || strings.Contains(list, c.TemporaryPassword) || strings.Contains(list, temp2) {
		t.Fatalf("list leaks credential material: %s", list)
	}

	// Cache-Control on create/reset responses (they carry a one-time secret).
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/provider/users/"+c.User.ID+"/reset-password", nil)
	req.Header.Set("Authorization", "Bearer "+e.admin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("reset response Cache-Control = %q, want no-store", resp.Header.Get("Cache-Control"))
	}

	var audit string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT COALESCE(string_agg(COALESCE(details::text, ''), ' '), '') FROM provider_audit_logs`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{c.TemporaryPassword, temp2, "argon2"} {
		if strings.Contains(audit, secret) {
			t.Fatalf("audit details contain credential material %q", secret)
		}
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("process log contains credential material %q", secret)
		}
	}
}
