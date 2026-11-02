package resolvers

// PENDING-19 R1 DB-backed gate: setMemberRole, the live role check behind
// @requires, R-9 and fail-closed behaviour. Must run, not skip, in the gate.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/db"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/rbac"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

type rbacFixture struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	r     *Resolver
	srv   *handler.Server
	ws    string
	users map[string]string // label → user id
}

func newRBACFixture(t *testing.T) *rbacFixture {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := "resolvers_rbac_" + strconv.Itoa(os.Getpid()) + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	adminPool, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("admin pool: %v", err)
	}
	t.Cleanup(adminPool.Close)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() { _, _ = adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName) })
	parsed, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	parsed.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	files, _ := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}

	f := &rbacFixture{t: t, ctx: ctx, pool: pool, users: map[string]string{}}
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ('rbac', 'rbac', 'active', 'rbac.test') RETURNING id::text`).Scan(&f.ws); err != nil {
		t.Fatalf("workspace: %v", err)
	}
	for _, u := range []struct{ label, role string }{
		{"admin", "admin"}, {"admin2", "admin"}, {"viewer", "viewer"}, {"devops", "devops"},
		{"security", "security"}, {"helpdesk", "helpdesk"}, {"member", "member"},
	} {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
			 VALUES ($1, $2, 'google', $2, $3, 'active') RETURNING id::text`,
			f.ws, u.label+"@rbac.test", u.role).Scan(&id); err != nil {
			t.Fatalf("user %s: %v", u.label, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO workspace_members (workspace_id, user_id, email, role, status, joined_at)
			 VALUES ($1, $2, $3, $4, 'active', NOW())`, f.ws, id, u.label+"@rbac.test", u.role); err != nil {
			t.Fatalf("member %s: %v", u.label, err)
		}
		f.users[u.label] = id
	}
	f.r = &Resolver{
		Pool:           pool,
		TenantDB:       db.NewTenantDB(pool),
		PolicyStore:    policy.NewStore(pool),
		PolicyNotifier: policy.NewNotifier(policy.NewSnapshotCache()),
		Roles:          rbac.NewChecker(pool),
	}
	f.srv = handler.NewDefaultServer(graph.NewExecutableSchema(graph.Config{
		Resolvers:  f.r,
		Directives: graph.DirectiveRoot{Requires: NewRequires(f.r.Roles)},
	}))
	f.srv.SetErrorPresenter(ErrorPresenter)
	return f
}

// as returns a context for label. jwtRole is the stale JWT claim; it must
// never matter, so it is always "admin" here.
func (f *rbacFixture) as(label string) context.Context {
	return tenant.Set(f.ctx, tenant.TenantContext{
		TenantID: f.ws, UserID: f.users[label], Role: "admin", Email: label + "@rbac.test",
	})
}

type rbacResult struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (f *rbacFixture) do(label, query string, vars map[string]any) rbacResult {
	f.t.Helper()
	body, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req := httptest.NewRequest("POST", "/graphql", bytes.NewReader(body)).WithContext(f.as(label))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	var res rbacResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		f.t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	return res
}

func (f *rbacFixture) wantOK(label, query string, vars map[string]any) rbacResult {
	f.t.Helper()
	res := f.do(label, query, vars)
	if len(res.Errors) != 0 {
		f.t.Fatalf("%s: %s → errors %v", label, query, res.Errors)
	}
	return res
}

func (f *rbacFixture) wantErr(label, query string, vars map[string]any, substr string) {
	f.t.Helper()
	res := f.do(label, query, vars)
	if len(res.Errors) == 0 || !strings.Contains(res.Errors[0].Message, substr) {
		f.t.Fatalf("%s: %s → errors %v, want %q", label, query, res.Errors, substr)
	}
}

func (f *rbacFixture) role(label string) string {
	f.t.Helper()
	var users, members string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT u.role, COALESCE(m.role, '') FROM users u
		   LEFT JOIN workspace_members m ON m.user_id = u.id AND m.workspace_id = u.tenant_id
		  WHERE u.id = $1`, f.users[label]).Scan(&users, &members); err != nil {
		f.t.Fatalf("read role: %v", err)
	}
	if members != "" && members != users {
		f.t.Fatalf("%s: users.role=%q but workspace_members.role=%q", label, users, members)
	}
	return users
}

func (f *rbacFixture) auditCount(target string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx,
		`SELECT COUNT(*) FROM audit_logs WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
		f.ws, actionMemberRoleChanged, target).Scan(&n); err != nil {
		f.t.Fatalf("audit count: %v", err)
	}
	return n
}

const (
	qUsers   = `{ users { id } }`
	qMe      = `{ me { id role capabilities } }`
	qDevices = `{ myDevices { id } }`
	mSetRole = `mutation($u: ID!, $r: Role!, $why: String!) { setMemberRole(userId: $u, role: $r, reason: $why) { id role capabilities } }`
)

// Each role against a read and a write, through the real directive and live
// lookup, with a JWT that always claims "admin".
func TestRBAC_LiveRoleGatesFields(t *testing.T) {
	f := newRBACFixture(t)
	for _, label := range []string{"admin", "viewer", "devops", "security", "helpdesk"} {
		f.wantOK(label, qUsers, nil)
	}
	f.wantErr("member", qUsers, nil, "forbidden")

	// A Member keeps self-service: me, myDevices.
	res := f.wantOK("member", qMe, nil)
	if !strings.Contains(string(res.Data["me"]), `"capabilities":[]`) {
		t.Fatalf("member me = %s, want empty capabilities", res.Data["me"])
	}
	f.wantOK("member", qDevices, nil)

	// setMemberRole is MEMBERS_ADMIN: refused for every non-Admin.
	for _, label := range []string{"viewer", "devops", "security", "helpdesk", "member"} {
		f.wantErr(label, mSetRole, map[string]any{"u": f.users["member"], "r": "VIEWER", "why": "x"}, "forbidden")
	}
}

func TestRBAC_SetMemberRole(t *testing.T) {
	f := newRBACFixture(t)
	vars := func(label, role, why string) map[string]any {
		return map[string]any{"u": f.users[label], "r": role, "why": why}
	}

	// Warm the viewer's cache entry, then demote: the very next request is denied.
	f.wantOK("viewer", qUsers, nil)
	res := f.wantOK("admin", mSetRole, vars("viewer", "MEMBER", "  left the team  "))
	if !strings.Contains(string(res.Data["setMemberRole"]), `"role":"MEMBER"`) {
		t.Fatalf("setMemberRole result = %s", res.Data["setMemberRole"])
	}
	if f.role("viewer") != "member" {
		t.Fatal("users.role not updated")
	}
	f.wantErr("viewer", qUsers, nil, "forbidden")
	f.wantOK("viewer", qMe, nil) // still logged in, client untouched

	// Promotion applies at once too.
	f.wantOK("admin", mSetRole, vars("viewer", "DEVOPS", "on-call"))
	f.wantOK("viewer", qUsers, nil)

	// Audit: one row per change, reason trimmed, from/to recorded.
	if n := f.auditCount(f.users["viewer"]); n != 2 {
		t.Fatalf("audit rows = %d, want 2", n)
	}
	var details string
	if err := f.pool.QueryRow(f.ctx,
		`SELECT details::text FROM audit_logs WHERE target_id = $1 AND action = $2 ORDER BY created_at LIMIT 1`,
		f.users["viewer"], actionMemberRoleChanged).Scan(&details); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"reason": "left the team"`, `"from_role": "viewer"`, `"to_role": "member"`} {
		if !strings.Contains(details, want) {
			t.Fatalf("audit details %s missing %s", details, want)
		}
	}

	// No-op: succeeds, no audit row.
	f.wantOK("admin", mSetRole, vars("viewer", "DEVOPS", "again"))
	if n := f.auditCount(f.users["viewer"]); n != 2 {
		t.Fatalf("no-op wrote an audit row (%d)", n)
	}

	// Refusals.
	f.wantErr("admin", mSetRole, vars("admin", "MEMBER", "x"), "your own role")
	f.wantErr("admin", mSetRole, vars("viewer", "MEMBER", "   "), "reason is required")
	f.wantErr("admin", mSetRole, vars("viewer", "MEMBER", strings.Repeat("r", maxRoleReasonLen+1)), "too long")
	f.wantErr("admin", mSetRole, map[string]any{"u": "00000000-0000-0000-0000-000000000000", "r": "MEMBER", "why": "x"}, "user not found")

	// Demote a second Admin: allowed while another remains.
	f.wantOK("admin", mSetRole, vars("admin2", "SECURITY", "role split"))
	if f.role("admin2") != "security" {
		t.Fatal("admin2 not demoted")
	}
}

// Two Admins demote each other at the same time: exactly one succeeds and the
// workspace keeps one active Admin. Repeated to give the race a chance.
func TestRBAC_ConcurrentDemotionKeepsOneAdmin(t *testing.T) {
	f := newRBACFixture(t)
	for i := 0; i < 10; i++ {
		if _, err := f.pool.Exec(f.ctx,
			`UPDATE users SET role = 'admin' WHERE id = ANY($1)`,
			[]string{f.users["admin"], f.users["admin2"]}); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for j, pair := range [][2]string{{"admin", "admin2"}, {"admin2", "admin"}} {
			wg.Add(1)
			go func(j int, actor, target string) {
				defer wg.Done()
				<-start
				_, errs[j] = f.r.setMemberRole(f.as(actor), f.users[target], graph.RoleMember, "race")
			}(j, pair[0], pair[1])
		}
		close(start)
		wg.Wait()
		ok := 0
		for _, err := range errs {
			if err == nil {
				ok++
			} else if !strings.Contains(err.Error(), "last active admin") {
				t.Fatalf("round %d: unexpected error %v", i, err)
			}
		}
		var admins int
		if err := f.pool.QueryRow(f.ctx,
			`SELECT COUNT(*) FROM users WHERE tenant_id = $1 AND role = 'admin' AND status = 'active'`,
			f.ws).Scan(&admins); err != nil {
			t.Fatal(err)
		}
		if ok != 1 || admins != 1 {
			t.Fatalf("round %d: %d succeeded, %d active admins; want 1 and 1 (errs %v)", i, ok, admins, errs)
		}
	}
}

// R-9: only an Admin may add or remove themselves from a group.
func TestRBAC_SelfGroupMembership(t *testing.T) {
	f := newRBACFixture(t)
	var group string
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO groups (workspace_id, name) VALUES ($1, 'ops') RETURNING id::text`, f.ws).Scan(&group); err != nil {
		t.Fatalf("group: %v", err)
	}
	add := `mutation($g: ID!, $u: ID!) { addGroupMember(groupId: $g, userId: $u) { id } }`
	rm := `mutation($g: ID!, $u: ID!) { removeGroupMember(groupId: $g, userId: $u) { id } }`
	v := func(label string) map[string]any { return map[string]any{"g": group, "u": f.users[label]} }

	f.wantErr("security", add, v("security"), "only an Admin")
	f.wantOK("security", add, v("member"))
	f.wantOK("security", rm, v("member"))
	f.wantOK("admin", add, v("admin"))
	f.wantOK("admin", add, v("security"))
	f.wantErr("security", rm, v("security"), "only an Admin")
	f.wantOK("admin", rm, v("security"))
	f.wantErr("devops", add, v("devops"), "forbidden") // no ACCESS_WRITE at all
}

// Fail closed: suspended, deleted or unknown users and a failed lookup all deny.
func TestRBAC_FailClosed(t *testing.T) {
	f := newRBACFixture(t)
	f.wantOK("devops", qUsers, nil)
	if _, err := f.pool.Exec(f.ctx, `UPDATE users SET status = 'suspended' WHERE id = $1`, f.users["devops"]); err != nil {
		t.Fatal(err)
	}
	f.r.Roles.Invalidate(f.ws, f.users["devops"]) // as the SCIM/IdP paths do
	f.wantErr("devops", qUsers, nil, "forbidden")

	f.users["ghost"] = "00000000-0000-0000-0000-0000000000aa"
	f.wantErr("ghost", qUsers, nil, "forbidden")

	broken := handler.NewDefaultServer(graph.NewExecutableSchema(graph.Config{
		Resolvers:  f.r,
		Directives: graph.DirectiveRoot{Requires: NewRequires(failingRoles{})},
	}))
	broken.SetErrorPresenter(ErrorPresenter)
	body := []byte(`{"query":"{ users { id } }"}`)
	req := httptest.NewRequest("POST", "/graphql", bytes.NewReader(body)).WithContext(f.as("admin"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	broken.ServeHTTP(rec, req)
	var res rbacResult
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if len(res.Errors) == 0 || len(res.Data["users"]) > 0 && string(res.Data["users"]) != "null" {
		t.Fatalf("lookup failure must deny; body=%s", rec.Body.String())
	}
}

type failingRoles struct{}

func (failingRoles) Role(context.Context, string, string) (string, error) {
	return "", errors.New("db down")
}

// IDs reach the guards as client strings, but Postgres matches uuid input in
// any case (and {…} form). A non-canonical spelling of your own id must not
// slip past the self-change, last-Admin or R-9 checks.
func TestRBAC_NonCanonicalIDsCannotBypassGuards(t *testing.T) {
	f := newRBACFixture(t)
	if _, err := f.pool.Exec(f.ctx, `UPDATE users SET role = 'viewer' WHERE id = $1`, f.users["admin2"]); err != nil {
		t.Fatal(err)
	}
	upper := strings.ToUpper(f.users["admin"])
	braced := "{" + f.users["admin"] + "}"
	for _, id := range []string{upper, braced} {
		res := f.do("admin", mSetRole, map[string]any{"u": id, "r": "MEMBER", "why": "x"})
		if len(res.Errors) == 0 {
			t.Fatalf("sole admin demoted themselves via %q", id)
		}
	}
	if f.role("admin") != "admin" {
		t.Fatal("sole admin lost the role")
	}

	var group string
	if err := f.pool.QueryRow(f.ctx,
		`INSERT INTO groups (workspace_id, name) VALUES ($1, 'g') RETURNING id::text`, f.ws).Scan(&group); err != nil {
		t.Fatal(err)
	}
	add := `mutation($g: ID!, $u: ID!) { addGroupMember(groupId: $g, userId: $u) { id } }`
	f.wantErr("security", add, map[string]any{"g": group, "u": strings.ToUpper(f.users["security"])}, "only an Admin")
}
