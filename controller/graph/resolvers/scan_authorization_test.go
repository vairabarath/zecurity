package resolvers

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
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

// request records requestID as sent to w's connector, as triggerScan does.
func (f *scanFixture) request(t *testing.T, w scanWS, requestID string) {
	t.Helper()
	ok, err := discovery.RecordScanRequest(context.Background(), f.pool, w.id, w.conn, requestID)
	if err != nil || !ok {
		t.Fatalf("record scan request %s: %v %v", requestID, ok, err)
	}
}

func (f *scanFixture) storedRows(t *testing.T, requestID string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM connector_scan_results WHERE request_id = $1`, requestID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
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
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
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
	// Operators still see which connector each refusal named (FU-3).
	for _, id := range []string{f.b.conn, "not-a-uuid"} {
		if !strings.Contains(logs.String(), fmt.Sprintf("connector=%q reason=not_in_workspace", id)) {
			t.Errorf("refusal for %q not logged with its connector: %q", id, logs.String())
		}
	}
	// No refused or unsent request stays bound to a connector (FU-5).
	var left int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM scan_requests`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d scan requests left after refused dispatches", left)
	}
}

// O-7 (live run 2026-10-29): a target the connector never scans is refused
// with a clear message before anything is recorded or sent.
func TestScanIsolation_UnscannableTargetRefused(t *testing.T) {
	f := newScanFixture(t)
	for _, targets := range [][]string{{"127.0.0.1"}, {"10.0.0.1", "224.0.0.0/4"}, {"not-an-ip"}} {
		if id, err := f.mr.TriggerScan(f.a.ctx, f.a.conn, targets, []int{22}); err == nil {
			t.Errorf("%v: dispatched request %s", targets, id)
		} else if msg := clientMessage(f.a.ctx, err); !strings.Contains(msg, "loopback, multicast and unspecified") {
			t.Errorf("%v: client sees %q", targets, msg)
		}
	}
	var left int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM scan_requests`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d scan requests left after refused targets", left)
	}
}

func TestScanIsolation_ResultsScopedToWorkspace(t *testing.T) {
	f := newScanFixture(t)
	f.request(t, f.a, "req-a")
	f.request(t, f.b, "req-b")
	f.report(t, f.a.conn, "req-a", "10.1.0.1")
	f.report(t, f.b.conn, "req-b", "10.2.0.1")

	// Positive controls: each workspace reads its own results.
	if got := f.results(t, f.a, "req-a"); len(got) != 1 || got[0] != "10.1.0.1" {
		t.Errorf("A reads req-a = %v, want [10.1.0.1]", got)
	}
	if got := f.results(t, f.b, "req-b"); len(got) != 1 || got[0] != "10.2.0.1" {
		t.Errorf("B reads req-b = %v, want [10.2.0.1]", got)
	}
	// Foreign, missing and malformed request IDs all answer an empty list.
	for _, id := range []string{"req-b", "req-missing", "not a request id"} {
		if got := f.results(t, f.a, id); len(got) != 0 {
			t.Errorf("A reads %q = %v, want empty", id, got)
		}
	}
}

// FU-5 (FU-3 residual): a request is bound to the connector it was sent to.
// A report for it from any other connector, another workspace's or another of
// the same workspace's, before or after the owner's, is dropped: it can neither
// hide the owner's row nor add rows to the request.
func TestScanIsolation_UnsolicitedReportIsDropped(t *testing.T) {
	for _, foreignFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("foreign report first=%v", foreignFirst), func(t *testing.T) {
			f := newScanFixture(t)
			var peer string
			if err := f.pool.QueryRow(context.Background(),
				`INSERT INTO connectors (tenant_id, remote_network_id, name, status)
				 SELECT tenant_id, remote_network_id, 'peer', 'active' FROM connectors WHERE id = $1
				 RETURNING id`, f.a.conn).Scan(&peer); err != nil {
				t.Fatal(err)
			}
			f.request(t, f.a, "req-a")
			foreign := func() {
				f.report(t, f.b.conn, "req-a", "10.1.0.1")
				f.report(t, peer, "req-a", "10.1.0.2")
			}
			if foreignFirst {
				foreign()
			}
			f.report(t, f.a.conn, "req-a", "10.1.0.1")
			if !foreignFirst {
				foreign()
			}
			// The same connector reporting again is still a no-op.
			f.report(t, f.a.conn, "req-a", "10.1.0.1")

			if n := f.storedRows(t, "req-a"); n != 1 {
				t.Errorf("stored %d rows for req-a, want only the bound connector's 1", n)
			}
			res, err := f.qr.GetScanResults(f.a.ctx, "req-a")
			if err != nil {
				t.Fatal(err)
			}
			if len(res) != 1 || res[0].ReachableFrom != f.a.conn || res[0].IP != "10.1.0.1" {
				t.Errorf("A reads %+v, want exactly its connector's row", res)
			}
			if got := f.results(t, f.b, "req-a"); len(got) != 0 {
				t.Errorf("B reads A's request: %v", got)
			}
		})
	}
}

// RecordScanRequest binds only a connector of the caller's workspace, and a
// purge removes old requests with their results.
func TestScanIsolation_RecordAndPurgeRequests(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()
	for _, c := range []struct {
		name, conn string
		want       bool
	}{
		{"own connector", f.a.conn, true},
		{"foreign connector", f.b.conn, false},
		{"missing connector", "00000000-0000-4000-8000-0000000000ff", false},
		{"malformed id", "not-a-uuid", false},
	} {
		ok, err := discovery.RecordScanRequest(ctx, f.pool, f.a.id, c.conn, "req-"+c.name)
		if err != nil || ok != c.want {
			t.Errorf("%s: recorded=%v err=%v, want %v", c.name, ok, err, c.want)
		}
	}
	f.report(t, f.a.conn, "req-own connector", "10.1.0.1")
	if err := discovery.PurgeScanResults(ctx, f.pool, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var requests int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM scan_requests`).Scan(&requests)
	if requests != 0 || f.storedRows(t, "req-own connector") != 0 {
		t.Errorf("after purge: %d requests, %d results", requests, f.storedRows(t, "req-own connector"))
	}

	// A deleted connector takes its requests and their results with it.
	f.request(t, f.b, "req-b")
	f.report(t, f.b.conn, "req-b", "10.2.0.1")
	if f.storedRows(t, "req-b") != 1 {
		t.Fatal("fixture: B's result not stored")
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM connectors WHERE id = $1`, f.b.conn); err != nil {
		t.Fatal(err)
	}
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM scan_requests`).Scan(&requests)
	if requests != 0 || f.storedRows(t, "req-b") != 0 {
		t.Errorf("after deleting the connector: %d requests, %d results", requests, f.storedRows(t, "req-b"))
	}
}
