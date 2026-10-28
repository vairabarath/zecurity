package resolvers

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/connector"
	"github.com/yourorg/ztna/controller/internal/discovery"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// FU-3: scan commands and scan results are workspace-scoped. Two fabricated
// workspaces, each with one connector, on a migrated throwaway database. The
// dispatch positive control (a connected connector receiving the command)
// lives in internal/connector/scan_dispatch_test.go: a live stream can only be
// registered from inside that package. Set RESOURCE_TEST_DATABASE_URL to run.

type scanWS struct {
	id, conn string
	ctx      context.Context
}

type scanFixture struct {
	pool *pgxpool.Pool
	qr   *queryResolver
	mr   *mutationResolver
	a, b scanWS
}

func newScanFixture(t *testing.T) *scanFixture {
	t.Helper()
	dsn := os.Getenv("RESOURCE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("RESOURCE_TEST_DATABASE_URL not set")
	}
	pool := newMigratedTestPool(t, dsn, "resolvers_scan")
	seed := func(tag string) scanWS {
		t.Helper()
		ctx := context.Background()
		w := scanWS{}
		slug := fmt.Sprintf("scan-%s-%d", tag, time.Now().UnixNano())
		var rn string
		scan := func(dst *string, sql string, args ...any) {
			t.Helper()
			if err := pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
				t.Fatalf("seed %s: %v", tag, err)
			}
		}
		scan(&w.id, `INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ($1, $1, 'active', $2) RETURNING id`, slug, "ws-"+slug+".zecurity.in")
		scan(&rn, `INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'net', 'other') RETURNING id`, w.id)
		scan(&w.conn, `INSERT INTO connectors (tenant_id, remote_network_id, name, status) VALUES ($1, $2, 'conn', 'active') RETURNING id`, w.id, rn)
		w.ctx = tenant.Set(ctx, tenant.TenantContext{TenantID: w.id, UserID: "00000000-0000-0000-0000-000000000000", Role: "admin", Email: "admin-" + tag + "@scan.test"})
		return w
	}
	r := &Resolver{Pool: pool, ConnectorRegistry: connector.NewConnectorRegistry()}
	return &scanFixture{pool: pool, qr: &queryResolver{r}, mr: &mutationResolver{r}, a: seed("a"), b: seed("b")}
}

// clientMessage is what a GraphQL client would see for err.
func clientMessage(ctx context.Context, err error) string {
	return ErrorPresenter(ctx, err).Message
}

func (f *scanFixture) report(t *testing.T, connID, requestID, ip string) {
	t.Helper()
	if err := discovery.UpsertScanResults(context.Background(), f.pool, connID, []discovery.ScanResult{
		{RequestID: requestID, IP: ip, Port: 22, Protocol: "tcp", ServiceName: "ssh"},
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *scanFixture) results(t *testing.T, w scanWS, requestID string) []string {
	t.Helper()
	res, err := f.qr.GetScanResults(w.ctx, requestID)
	if err != nil {
		t.Fatalf("getScanResults(%q): %v", requestID, err)
	}
	ips := []string{}
	for _, r := range res {
		ips = append(ips, r.IP)
	}
	return ips
}

func TestScanIsolation_TriggerScanSameAnswerForEveryUnavailableConnector(t *testing.T) {
	f := newScanFixture(t)
	var want string
	for _, c := range []struct{ name, id string }{
		{"foreign connector", f.b.conn},
		{"missing connector", "00000000-0000-4000-8000-0000000000ff"},
		{"malformed id", "not-a-uuid"},
		{"own connector, not connected", f.a.conn},
	} {
		id, err := f.mr.TriggerScan(f.a.ctx, c.id, []string{"10.0.0.0/30"}, []int{22})
		if err == nil {
			t.Errorf("%s: dispatched request %s", c.name, id)
			continue
		}
		msg := clientMessage(f.a.ctx, err)
		if want == "" {
			want = msg
		}
		if msg != "connector not found or not connected" || msg != want {
			t.Errorf("%s: client sees %q, want %q for every case", c.name, msg, "connector not found or not connected")
		}
	}
}

func TestScanIsolation_ResultsScopedToWorkspace(t *testing.T) {
	f := newScanFixture(t)
	f.report(t, f.a.conn, "req-a", "10.1.0.1")
	f.report(t, f.b.conn, "req-b", "10.2.0.1")
	// One request ID reported by both workspaces' connectors (a request ID is
	// not a secret; a connector can report any ID it likes).
	f.report(t, f.a.conn, "req-shared", "10.1.0.9")
	f.report(t, f.b.conn, "req-shared", "10.2.0.9")

	// Positive controls: each workspace reads its own results.
	if got := f.results(t, f.a, "req-a"); len(got) != 1 || got[0] != "10.1.0.1" {
		t.Errorf("A reads req-a = %v, want [10.1.0.1]", got)
	}
	if got := f.results(t, f.b, "req-b"); len(got) != 1 || got[0] != "10.2.0.1" {
		t.Errorf("B reads req-b = %v, want [10.2.0.1]", got)
	}
	// A can't read B's request, and sees only its own rows of a shared ID.
	if got := f.results(t, f.a, "req-b"); len(got) != 0 {
		t.Errorf("A reads B's request: %v", got)
	}
	if got := f.results(t, f.a, "req-shared"); len(got) != 1 || got[0] != "10.1.0.9" {
		t.Errorf("A reads req-shared = %v, want only its own row", got)
	}
	if got := f.results(t, f.b, "req-shared"); len(got) != 1 || got[0] != "10.2.0.9" {
		t.Errorf("B reads req-shared = %v, want only its own row", got)
	}
	// Foreign, missing and malformed request IDs all answer an empty list.
	for _, id := range []string{"req-b", "req-missing", "not a request id"} {
		if got := f.results(t, f.a, id); len(got) != 0 {
			t.Errorf("A reads %q = %v, want empty", id, got)
		}
	}
}

func TestScanIsolation_LateReportCannotTakeOverResult(t *testing.T) {
	f := newScanFixture(t)
	f.report(t, f.a.conn, "req-a", "10.1.0.1")
	// B's connector reports the same (request, ip, port, protocol) later.
	f.report(t, f.b.conn, "req-a", "10.1.0.1")

	var owner string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT connector_id::text FROM connector_scan_results WHERE request_id = 'req-a' AND ip = '10.1.0.1'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != f.a.conn {
		t.Errorf("result row now owned by connector %s, want A's %s", owner, f.a.conn)
	}
	if got := f.results(t, f.a, "req-a"); len(got) != 1 {
		t.Errorf("A lost its result: %v", got)
	}
	if got := f.results(t, f.b, "req-a"); len(got) != 0 {
		t.Errorf("B reads A's result through its late report: %v", got)
	}
}
