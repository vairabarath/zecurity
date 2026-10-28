package resolvers

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/connector"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/resource"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// B-1 (cross-tenant access rules): an admin of one workspace must not be able
// to read or change another workspace's groups, memberships or access rules
// through the policy resolvers, whatever IDs they send. Two fabricated
// workspaces on a migrated throwaway database. Set RESOURCE_TEST_DATABASE_URL
// to run; skipped otherwise.

type isoWorkspace struct {
	id, rn, res, group, user string
	ctx                      context.Context
}

type isoFixture struct {
	pool     *pgxpool.Pool
	notifier *policy.Notifier
	mr       *mutationResolver
	qr       *queryResolver
	a, b     isoWorkspace
}

func newIsoFixture(t *testing.T) *isoFixture {
	t.Helper()
	dsn := os.Getenv("RESOURCE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("RESOURCE_TEST_DATABASE_URL not set")
	}
	pool := newMigratedTestPool(t, dsn, "resolvers_iso")
	seed := func(tag string) isoWorkspace {
		t.Helper()
		ctx := context.Background()
		w := isoWorkspace{}
		slug := fmt.Sprintf("iso-%s-%d", tag, time.Now().UnixNano())
		scan := func(dst *string, sql string, args ...any) {
			t.Helper()
			if err := pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
				t.Fatalf("seed %s: %v", tag, err)
			}
		}
		scan(&w.id, `INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ($1, $1, 'active', $2) RETURNING id`, slug, "ws-"+slug+".zecurity.in")
		scan(&w.rn, `INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, $2, 'other') RETURNING id`, w.id, "net-"+tag)
		scan(&w.res, `INSERT INTO resources (tenant_id, remote_network_id, name, host, protocol, port_from, port_to, status)
		              VALUES ($1, $2, $3, $4, 'tcp', 5432, 5432, 'unprotected') RETURNING id`, w.id, w.rn, "res-"+tag, "10.60.0."+map[string]string{"a": "1", "b": "2"}[tag])
		scan(&w.group, `INSERT INTO groups (workspace_id, name) VALUES ($1, $2) RETURNING id`, w.id, "group-"+tag)
		scan(&w.user, `INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		               VALUES ($1, $2, 'google', $3, 'member', 'active') RETURNING id`, w.id, "member-"+tag+"@iso.test", "sub-"+tag)
		if _, err := pool.Exec(ctx, `INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`, w.group, w.user); err != nil {
			t.Fatalf("seed %s member: %v", tag, err)
		}
		w.ctx = tenant.Set(ctx, tenant.TenantContext{TenantID: w.id, UserID: w.user, Role: "admin", Email: "admin-" + tag + "@iso.test"})
		return w
	}
	n := policy.NewNotifier(policy.NewSnapshotCache())
	r := &Resolver{
		Pool:              pool,
		ResourceCfg:       resource.Config{DB: pool},
		PolicyStore:       policy.NewStore(pool),
		PolicyNotifier:    n,
		ConnectorRegistry: connector.NewConnectorRegistry(),
	}
	f := &isoFixture{pool: pool, notifier: n, mr: &mutationResolver{r}, qr: &queryResolver{r}, a: seed("a"), b: seed("b")}
	// B's own, legitimate rule.
	if _, err := f.mr.AssignResourceToGroup(f.b.ctx, f.b.res, f.b.group); err != nil {
		t.Fatalf("B assigns its own resource: %v", err)
	}
	return f
}

// rules returns every access_rules row as "workspace/resource/group/enabled".
func (f *isoFixture) rules(t *testing.T) []string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT workspace_id::text || '/' || resource_id::text || '/' || group_id::text || '/' || enabled::text
		   FROM access_rules ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func (f *isoFixture) members(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM group_members`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// presented is what a GraphQL client would see for err.
func presented(ctx context.Context, err error) string {
	return ErrorPresenter(ctx, err).Message
}

const missingID = "00000000-0000-4000-8000-0000000000ff"

func TestPolicyIsolation_GroupQueryHidesForeignGroup(t *testing.T) {
	f := newIsoFixture(t)
	g, err := f.qr.Group(f.a.ctx, f.b.group)
	if err == nil {
		t.Fatalf("group(foreign) returned %q with %d members", g.Name, len(g.Members))
	}
	_, missErr := f.qr.Group(f.a.ctx, missingID)
	if missErr == nil {
		t.Fatal("group(missing) succeeded")
	}
	foreign, missing := presented(f.a.ctx, err), presented(f.a.ctx, missErr)
	if foreign != missing {
		t.Errorf("foreign and missing group differ: %q vs %q", foreign, missing)
	}
	if strings.Contains(foreign, "member-b") || strings.Contains(foreign, "10.60.0.2") {
		t.Errorf("error discloses foreign data: %q", foreign)
	}
	// The caller's own group still loads, with its member.
	own, err := f.qr.Group(f.a.ctx, f.a.group)
	if err != nil || len(own.Members) != 1 || own.Members[0].Email != "member-a@iso.test" {
		t.Fatalf("own group: %v %+v", err, own)
	}
}

func TestPolicyIsolation_AssignRejectsForeignIDs(t *testing.T) {
	f := newIsoFixture(t)
	before := f.rules(t)
	vA, vB := f.notifier.Version(f.a.id), f.notifier.Version(f.b.id)
	cases := []struct{ name, res, group string }{
		{"foreign resource", f.b.res, f.a.group},
		{"foreign group", f.a.res, f.b.group},
		{"both foreign (existing B rule: ON CONFLICT)", f.b.res, f.b.group},
		{"missing resource", missingID, f.a.group},
		{"missing group", f.a.res, missingID},
		{"malformed id", "not-a-uuid", f.a.group},
	}
	var want string
	for _, c := range cases {
		res, err := f.mr.AssignResourceToGroup(f.a.ctx, c.res, c.group)
		if err == nil {
			t.Errorf("%s: accepted, response host=%s", c.name, res.Host)
			continue
		}
		msg := presented(f.a.ctx, err)
		if want == "" {
			want = msg
		}
		if msg != want {
			t.Errorf("%s: client sees %q, want the same message as the others (%q)", c.name, msg, want)
		}
		if strings.Contains(msg, "10.60.0.2") || strings.Contains(msg, "group-b") || strings.Contains(strings.ToLower(msg), "constraint") {
			t.Errorf("%s: error discloses detail: %q", c.name, msg)
		}
	}
	if after := f.rules(t); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("rejected assignments changed access_rules:\nbefore %v\nafter  %v", before, after)
	}
	if f.notifier.Version(f.a.id) != vA || f.notifier.Version(f.b.id) != vB {
		t.Errorf("rejected assignments bumped a policy version")
	}
}

func TestPolicyIsolation_ConflictCannotTouchForeignRow(t *testing.T) {
	f := newIsoFixture(t)
	// B disables its rule out of band; A's assign of B's pair must not re-enable it.
	if _, err := f.pool.Exec(context.Background(), `UPDATE access_rules SET enabled = FALSE WHERE workspace_id = $1`, f.b.id); err != nil {
		t.Fatal(err)
	}
	before := f.rules(t)
	if _, err := f.mr.AssignResourceToGroup(f.a.ctx, f.b.res, f.b.group); err == nil {
		t.Fatal("assign over B's existing rule accepted")
	}
	if after := f.rules(t); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("B's rule changed: before %v after %v", before, after)
	}

	// A row owned by B on A's own (resource, group) pair (only possible from
	// inconsistent data): A owns both IDs, so only the ON CONFLICT guard stops
	// A from rewriting B's row.
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO access_rules (workspace_id, resource_id, group_id, enabled) VALUES ($1, $2, $3, FALSE)`,
		f.b.id, f.a.res, f.a.group); err != nil {
		t.Fatal(err)
	}
	before = f.rules(t)
	if _, err := f.mr.AssignResourceToGroup(f.a.ctx, f.a.res, f.a.group); err == nil {
		t.Error("assign rewrote a row owned by another workspace")
	}
	if after := f.rules(t); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("foreign-owned row changed: before %v after %v", before, after)
	}
}

func TestPolicyIsolation_UnassignCannotDeleteForeignRule(t *testing.T) {
	f := newIsoFixture(t)
	before := f.rules(t)
	vB := f.notifier.Version(f.b.id)
	_, err := f.mr.UnassignResourceFromGroup(f.a.ctx, f.b.res, f.b.group)
	if err == nil {
		t.Fatal("unassign of B's rule accepted")
	}
	_, missErr := f.mr.UnassignResourceFromGroup(f.a.ctx, f.a.res, f.a.group) // never assigned
	if missErr == nil {
		t.Fatal("unassign of a missing rule accepted")
	}
	if a, b := presented(f.a.ctx, err), presented(f.a.ctx, missErr); a != b {
		t.Errorf("foreign and missing rule differ: %q vs %q", a, b)
	}
	if after := f.rules(t); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Errorf("B's rule changed: before %v after %v", before, after)
	}
	if f.notifier.Version(f.b.id) != vB {
		t.Errorf("B's policy version changed by A's rejected unassign")
	}
}

func TestPolicyIsolation_AddGroupMemberRejectsForeignUser(t *testing.T) {
	f := newIsoFixture(t)
	before := f.members(t)
	g, err := f.mr.AddGroupMember(f.a.ctx, f.a.group, f.b.user)
	if err == nil {
		emails := []string{}
		for _, u := range g.Members {
			emails = append(emails, u.Email)
		}
		t.Fatalf("foreign user added; response members %v", emails)
	}
	_, missErr := f.mr.AddGroupMember(f.a.ctx, f.a.group, missingID)
	if missErr == nil {
		t.Fatal("missing user added")
	}
	foreign, missing := presented(f.a.ctx, err), presented(f.a.ctx, missErr)
	if foreign != missing {
		t.Errorf("foreign and missing user differ: %q vs %q", foreign, missing)
	}
	if strings.Contains(foreign, "member-b") {
		t.Errorf("error discloses foreign email: %q", foreign)
	}
	if after := f.members(t); after != before {
		t.Errorf("group_members changed: %d -> %d", before, after)
	}
}

func TestPolicyIsolation_ReadsIgnorePlantedForeignRows(t *testing.T) {
	f := newIsoFixture(t)
	ctx := context.Background()
	// Rows the old unscoped mutations could have written.
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO access_rules (workspace_id, resource_id, group_id) VALUES ($1, $2, $3)`, []any{f.a.id, f.b.res, f.a.group}},
		{`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`, []any{f.a.group, f.b.user}},
	} {
		if _, err := f.pool.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	g, err := f.qr.Group(f.a.ctx, f.a.group)
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range g.Members {
		if u.Email == "member-b@iso.test" {
			t.Errorf("A's group lists B's user")
		}
	}
	for _, r := range g.Resources {
		if r.ID == f.b.res {
			t.Errorf("A's group lists B's resource (host %s)", r.Host)
		}
	}
	// B's view of its own resource must not show A's group.
	res, err := f.mr.loadResourceWithGroups(f.b.ctx, f.b.id, f.b.res)
	if err != nil {
		t.Fatal(err)
	}
	for _, gg := range res.Groups {
		if gg.ID == f.a.group {
			t.Errorf("B's resource lists A's group")
		}
	}
	// And A can't load B's resource through the helper at all.
	if _, err := f.mr.loadResourceWithGroups(f.a.ctx, f.a.id, f.b.res); err == nil {
		t.Errorf("loadResourceWithGroups returned B's resource to A")
	}
}

func TestPolicyIsolation_SameWorkspaceStillWorks(t *testing.T) {
	f := newIsoFixture(t)
	vA, vB := f.notifier.Version(f.a.id), f.notifier.Version(f.b.id)

	res, err := f.mr.AssignResourceToGroup(f.a.ctx, f.a.res, f.a.group)
	if err != nil || len(res.Groups) != 1 || res.Groups[0].ID != f.a.group {
		t.Fatalf("assign own: %v %+v", err, res)
	}
	// Re-assign is idempotent (ON CONFLICT on the caller's own row).
	if _, err := f.mr.AssignResourceToGroup(f.a.ctx, f.a.res, f.a.group); err != nil {
		t.Fatalf("re-assign own: %v", err)
	}
	if f.notifier.Version(f.a.id) != vA+2 || f.notifier.Version(f.b.id) != vB {
		t.Errorf("versions after A's assigns: A %d->%d, B %d->%d; want A+2, B unchanged", vA, f.notifier.Version(f.a.id), vB, f.notifier.Version(f.b.id))
	}

	var second string
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
		 VALUES ($1, 'member2-a@iso.test', 'google', 'sub-a2', 'member', 'active') RETURNING id`, f.a.id).Scan(&second); err != nil {
		t.Fatal(err)
	}
	g, err := f.mr.AddGroupMember(f.a.ctx, f.a.group, second)
	if err != nil || len(g.Members) != 2 {
		t.Fatalf("add own member: %v %+v", err, g)
	}
	// Adding an existing member is still a no-op success.
	if _, err := f.mr.AddGroupMember(f.a.ctx, f.a.group, second); err != nil {
		t.Fatalf("re-add own member: %v", err)
	}

	res, err = f.mr.UnassignResourceFromGroup(f.a.ctx, f.a.res, f.a.group)
	if err != nil || len(res.Groups) != 0 {
		t.Fatalf("unassign own: %v %+v", err, res)
	}
	if f.notifier.Version(f.b.id) != vB {
		t.Errorf("B's version changed by A's own mutations")
	}
	// B's rule is untouched throughout.
	var n int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM access_rules WHERE workspace_id = $1 AND resource_id = $2 AND group_id = $3`, f.b.id, f.b.res, f.b.group).Scan(&n)
	if n != 1 {
		t.Errorf("B's rule rows = %d, want 1", n)
	}
}
