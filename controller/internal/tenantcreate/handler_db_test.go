package tenantcreate

// Sprint 23 S2 (D-44): POST /provider/tenants over a real database.
// DB-backed: must run, not skip, in the gate.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/bootstrap"
	"github.com/yourorg/ztna/controller/internal/invitation"
	"github.com/yourorg/ztna/controller/internal/pki"
	"github.com/yourorg/ztna/controller/internal/provider"
)

type sentMail struct {
	mu   sync.Mutex
	invs []invitation.Invitation
	done chan struct{}
}

func (s *sentMail) SendInvitation(inv *invitation.Invitation, _ string) error {
	s.mu.Lock()
	s.invs = append(s.invs, *inv)
	s.mu.Unlock()
	s.done <- struct{}{}
	return nil
}

type env struct {
	pool    *pgxpool.Pool
	mux     *http.ServeMux
	mail    *sentMail
	superID string
	opsID   string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("tenantcreate_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}
	u, _ := url.Parse(adminDSN)
	u.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		admin.Close()
	})
	files, _ := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	sort.Strings(files)
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	t.Setenv("PKI_MASTER_SECRET", "s2-tenantcreate-handler-secret")
	p, err := pki.Init(ctx, pool)
	if err != nil {
		t.Fatalf("pki.Init: %v", err)
	}
	e := &env{pool: pool, mail: &sentMail{done: make(chan struct{}, 8)}}
	if err := pool.QueryRow(ctx, `INSERT INTO provider_users (email, role) VALUES ('admin@provider.test', 'super-admin') RETURNING id::text`).Scan(&e.superID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO provider_users (email, role) VALUES ('ops@provider.test', 'relay-ops') RETURNING id::text`).Scan(&e.opsID); err != nil {
		t.Fatal(err)
	}
	h := &Handler{
		Creator:       &bootstrap.Service{Pool: pool, PKIService: p},
		Authz:         provider.NewAuthz(),
		ProviderStore: provider.NewStore(pool),
		Sender:        e.mail,
	}
	e.mux = http.NewServeMux()
	e.mux.HandleFunc("POST /provider/tenants", h.Create)
	return e
}

type resp struct {
	code int
	body string
}

func (e *env) post(t *testing.T, role string, body any) resp {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/provider/tenants", strings.NewReader(string(raw)))
	req.RemoteAddr = "192.0.2.10:54321"
	actor := provider.Actor{UserID: e.superID, Email: "admin@provider.test", Role: provider.RoleSuperAdmin}
	if role == provider.RoleRelayOps {
		actor = provider.Actor{UserID: e.opsID, Email: "ops@provider.test", Role: provider.RoleRelayOps}
	}
	req = req.WithContext(provider.WithActor(req.Context(), actor))
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return resp{code: rec.Code, body: rec.Body.String()}
}

func errCode(r resp) string {
	var v struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(r.body), &v)
	return v.Error
}

func (e *env) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func valid() map[string]any {
	return map[string]any{"slug": "acme", "name": "Acme Inc", "admin_email": "first@acme.test", "reason": "new customer"}
}

// tenant.create is super-admin only (D-06): relay-ops get 403 and nothing is
// created or audited.
func TestCreateTenantHTTP_SuperAdminOnly(t *testing.T) {
	e := newEnv(t)
	r := e.post(t, provider.RoleRelayOps, valid())
	if r.code != http.StatusForbidden || errCode(r) != "forbidden" {
		t.Fatalf("relay-ops: %d %s", r.code, r.body)
	}
	if n := e.count(t, `SELECT (SELECT count(*) FROM workspaces) + (SELECT count(*) FROM provider_audit_logs WHERE action = 'tenant.create')`); n != 0 {
		t.Fatalf("a refused create left %d rows", n)
	}
}

// A create returns the tenant and the invitation's id and expiry, audits the
// provider actor, reason and invitation operation, and never returns or
// audits the token; the token goes only to the invitation email.
func TestCreateTenantHTTP_CreatedAuditedNoToken(t *testing.T) {
	e := newEnv(t)
	r := e.post(t, provider.RoleSuperAdmin, valid())
	if r.code != http.StatusCreated {
		t.Fatalf("create: %d %s", r.code, r.body)
	}
	var got TenantCreatedJSON
	if err := json.Unmarshal([]byte(r.body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Slug != "acme" || got.Name != "Acme Inc" || got.TrustDomain != "ws-acme.zecurity.in" || got.PlanName != "Free" ||
		got.Invitation.Email != "first@acme.test" || got.Invitation.ID == "" || got.Invitation.ExpiresAt.Before(time.Now().Add(6*24*time.Hour)) {
		t.Fatalf("response = %+v", got)
	}

	var token string
	if err := e.pool.QueryRow(context.Background(), `SELECT token FROM invitations WHERE id = $1`, got.Invitation.ID).Scan(&token); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.body, token) || strings.Contains(strings.ToLower(r.body), "token") {
		t.Fatalf("response carries the invitation token: %s", r.body)
	}

	var actorID, actorEmail, targetID, ip string
	var details []byte
	if err := e.pool.QueryRow(context.Background(),
		`SELECT provider_user_id::text, provider_email, target_id, ip_address, details
		   FROM provider_audit_logs WHERE action = 'tenant.create'`).Scan(&actorID, &actorEmail, &targetID, &ip, &details); err != nil {
		t.Fatalf("tenant.create audit: %v", err)
	}
	if actorID != e.superID || actorEmail != "admin@provider.test" || targetID != got.ID {
		t.Fatalf("audit actor/target = %s %s %s", actorID, actorEmail, targetID)
	}
	if strings.Contains(string(details), token) || strings.Contains(string(details), "token") {
		t.Fatalf("audit details carry the token: %s", details)
	}
	var d struct {
		Reason     string         `json:"reason"`
		Invitation map[string]any `json:"invitation"`
		After      map[string]any `json:"after"`
	}
	_ = json.Unmarshal(details, &d)
	if d.Reason != "new customer" || d.Invitation["operation"] != "first_admin.invite" || d.Invitation["id"] != got.Invitation.ID ||
		d.Invitation["role"] != "admin" || d.After["slug"] != "acme" || d.After["plan_name"] != "Free" {
		t.Fatalf("audit details = %s", details)
	}
	// The tenant's own stream has its creation, without the token either.
	if n := e.count(t, `SELECT count(*) FROM audit_logs WHERE tenant_id = $1 AND action = 'workspace.create'
		AND position($2 in details::text) = 0`, got.ID, token); n != 1 {
		t.Fatalf("tenant workspace.create audits without token = %d", n)
	}

	select {
	case <-e.mail.done:
	case <-time.After(5 * time.Second):
		t.Fatal("no invitation email")
	}
	e.mail.mu.Lock()
	defer e.mail.mu.Unlock()
	if len(e.mail.invs) != 1 || e.mail.invs[0].Token != token || e.mail.invs[0].Email != "first@acme.test" {
		t.Fatalf("invitation email = %+v", e.mail.invs)
	}
}

func TestCreateTenantHTTP_Errors(t *testing.T) {
	e := newEnv(t)
	if r := e.post(t, provider.RoleSuperAdmin, valid()); r.code != http.StatusCreated {
		t.Fatalf("first create: %d %s", r.code, r.body)
	}
	with := func(k string, v any) map[string]any {
		b := valid()
		b["slug"] = "other"
		if v == nil {
			delete(b, k)
		} else {
			b[k] = v
		}
		return b
	}
	cases := []struct {
		name string
		body map[string]any
		code int
		err  string
	}{
		{"slug taken", valid(), http.StatusConflict, "slug_taken"},
		{"invalid slug", with("slug", "Not_A_Slug"), http.StatusBadRequest, "invalid_slug"},
		{"invalid email", with("admin_email", "nope"), http.StatusBadRequest, "invalid_email"},
		{"plan not found", with("plan_id", "11111111-1111-1111-1111-111111111111"), http.StatusBadRequest, "plan_not_found"},
		{"plan id not a uuid", with("plan_id", "x"), http.StatusBadRequest, "plan_not_found"},
		{"no reason", with("reason", nil), http.StatusBadRequest, "reason_required"},
		{"blank reason", with("reason", "   "), http.StatusBadRequest, "reason_required"},
		{"long reason", with("reason", strings.Repeat("r", 501)), http.StatusBadRequest, "reason_too_long"},
		{"empty name", with("name", " "), http.StatusBadRequest, "invalid_name"},
		{"unknown field", with("token", "x"), http.StatusBadRequest, "invalid_request"},
	}
	for _, c := range cases {
		r := e.post(t, provider.RoleSuperAdmin, c.body)
		if r.code != c.code || errCode(r) != c.err {
			t.Errorf("%s: %d %s, want %d %s", c.name, r.code, r.body, c.code, c.err)
		}
	}
	if n := e.count(t, `SELECT count(*) FROM workspaces`); n != 1 {
		t.Fatalf("refused creates left %d workspaces, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM provider_audit_logs WHERE action = 'tenant.create'`); n != 1 {
		t.Fatalf("tenant.create audits = %d, want 1", n)
	}
	// A chosen plan is honoured.
	b := valid()
	b["slug"], b["plan_id"] = "legacy-co", "00000000-0000-0000-0001-000000000002"
	r := e.post(t, provider.RoleSuperAdmin, b)
	if r.code != http.StatusCreated || !strings.Contains(r.body, `"plan_name":"Legacy"`) {
		t.Fatalf("create on Legacy: %d %s", r.code, r.body)
	}
}
