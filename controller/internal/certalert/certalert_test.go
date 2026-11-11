package certalert

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeycompat"

	"github.com/yourorg/ztna/controller/internal/provider"
	"github.com/yourorg/ztna/controller/internal/providerquery"
)

// F14 (D-36): the daily certificate expiry email.

// --- composition (no database) ---

func entry(kind, name string, tenant *string, serial string, at time.Time, bucket string) providerquery.CertificateEntry {
	e := providerquery.CertificateEntry{Kind: kind, TenantID: tenant, EntityID: "x", NotAfter: at, Bucket: bucket}
	if name != "" {
		e.EntityName = &name
	}
	if serial != "" {
		e.Serial = &serial
	}
	return e
}

func TestCompose_SectionsSubjectAndWording(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	ws := "aaaaaaaa-0000-4000-8000-000000000001"
	entries := []providerquery.CertificateEntry{
		entry("relay", "relay-a", nil, "2a", now.Add(-2*time.Hour), providerquery.BucketExpired),
		entry("controller_grpc", "Controller gRPC", nil, "c1", now.Add(3*time.Hour), providerquery.BucketLT24h),
		entry("workspace_ca", "Acme\r\nBcc: evil@x", &ws, "", now.Add(5*24*time.Hour), providerquery.BucketLT7d),
		entry("root", "Root CA", nil, "1", now.Add(20*24*time.Hour), providerquery.BucketLT30d),
	}
	subject, body := Compose(entries, now)
	if subject != "Zecurity: 4 certificates need attention" {
		t.Fatalf("subject %q", subject)
	}
	if s, _ := Compose(entries[:1], now); s != "Zecurity: 1 certificate needs attention" {
		t.Fatalf("singular subject %q", s)
	}
	for _, want := range []string{
		"Certificate expiry report for 2026-11-11 (UTC).",
		"Expired (1)\r\n  - Relay \"relay-a\", serial 2a: expired 2026-11-11 07:00 UTC",
		"Expires within 24 hours (1)\r\n  - Controller gRPC certificate, serial c1: expires 2026-11-11 12:00 UTC",
		"Expires within 7 days (1)\r\n  - Workspace CA \"AcmeBcc: evil@x\" (workspace " + ws + "): expires 2026-11-16 09:00 UTC",
		"Expires within 30 days (1)\r\n  - Root CA, serial 1: expires 2026-12-01 09:00 UTC",
		"Tenant agent certificates (connectors,",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	// Order: expired first, then by window.
	if strings.Index(body, "Expired") > strings.Index(body, "within 24 hours") ||
		strings.Index(body, "within 24 hours") > strings.Index(body, "within 7 days") ||
		strings.Index(body, "within 7 days") > strings.Index(body, "within 30 days") {
		t.Fatalf("section order:\n%s", body)
	}
	// A tenant-chosen name can't add a line (no header-looking line).
	for _, l := range strings.Split(body, "\r\n") {
		if strings.HasPrefix(l, "Bcc:") {
			t.Fatalf("name broke onto its own line:\n%s", body)
		}
	}
}

func TestCompose_ListsAtMostMaxPerSection(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	var entries []providerquery.CertificateEntry
	for i := 0; i < MaxListed+7; i++ {
		entries = append(entries, entry("relay", fmt.Sprintf("r%03d", i), nil, "", now.Add(-time.Hour), providerquery.BucketExpired))
	}
	_, body := Compose(entries, now)
	if n := strings.Count(body, "  - Relay "); n != MaxListed {
		t.Fatalf("listed %d, want %d", n, MaxListed)
	}
	if !strings.Contains(body, fmt.Sprintf("Expired (%d)", MaxListed+7)) || !strings.Contains(body, "... and 7 more") {
		t.Fatalf("overflow note missing:\n%s", body)
	}
}

// --- the job, against a throwaway database and the isolated test Valkey ---

type fakeSender struct {
	mu   sync.Mutex
	sent map[string][]string // to -> subjects
	body string
	fail map[string]bool
}

func (f *fakeSender) SendPlain(to, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail[to] {
		return errors.New("smtp refused")
	}
	if f.sent == nil {
		f.sent = map[string][]string{}
	}
	f.sent[to] = append(f.sent[to], subject)
	f.body = body
	return nil
}

type fakeController struct {
	notAfter time.Time
	ok       bool
}

func (f fakeController) CurrentCertInfo() (string, time.Time, time.Time, bool) {
	return "c0ffee", f.notAfter.Add(-30 * 24 * time.Hour), f.notAfter, f.ok
}

type env struct {
	pool   *pgxpool.Pool
	store  *provider.Store
	marker valkeycompat.Cmdable
	sender *fakeSender
	now    time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	pool := newTestDB(t)
	marker := testValkey(t)
	// A day far from today and unique to this test, so markers never collide.
	now := time.Date(2190+int(time.Now().UnixNano()%500), 3, 4, 9, 0, 0, 0, time.UTC)
	t.Cleanup(func() { _ = marker.Del(context.Background(), markerPrefix+now.Format("2006-01-02")).Err() })
	return &env{pool: pool, store: provider.NewStore(pool), marker: marker, sender: &fakeSender{}, now: now}
}

func (e *env) job(ctl providerquery.ControllerCertSource) *Job {
	return &Job{DB: e.pool, Controller: ctl, Sender: e.sender, Audit: e.store, Marker: e.marker, Now: func() time.Time { return e.now }}
}

func (e *env) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func (e *env) auditRows(t *testing.T) []string {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT provider_email || '|' || target_id || '|' || details::text FROM provider_audit_logs WHERE action = $1`, AuditAction)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

// seed: two active super-admins, a disabled one, a relay-ops operator;
// certificates in and out of scope.
func (e *env) seed(t *testing.T) {
	t.Helper()
	for _, u := range []struct{ email, role, disabled string }{
		{"a@provider.test", "super-admin", "NULL"},
		{"b@provider.test", "super-admin", "NULL"},
		{"gone@provider.test", "super-admin", "NOW()"},
		{"ops@provider.test", "relay-ops", "NULL"},
	} {
		e.exec(t, `INSERT INTO provider_users (email, role, disabled_at) VALUES ($1, $2, `+u.disabled+`)`, u.email, u.role)
	}
	h := func(d time.Duration) time.Time { return e.now.Add(d) }
	// Relays (auto-renew: only within 24 h or expired).
	e.exec(t, `INSERT INTO relays (name, status, cert_serial, cert_not_after) VALUES
	             ('relay-expired', 'active', 'a1', $1), ('relay-soon', 'active', 'a2', $2),
	             ('relay-fine', 'active', 'a3', $3), ('relay-revoked', 'revoked', 'a4', $1)`,
		h(-time.Hour), h(5*time.Hour), h(3*24*time.Hour))
	// Workspace CAs (no auto-renew: within 30 days).
	var ws1, ws2, net string
	_ = e.pool.QueryRow(context.Background(),
		`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ('acme','Acme','active','acme.zecurity.test') RETURNING id::text`).Scan(&ws1)
	_ = e.pool.QueryRow(context.Background(),
		`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ('beta','Beta','active','beta.zecurity.test') RETURNING id::text`).Scan(&ws2)
	e.exec(t, `INSERT INTO workspace_ca_keys (tenant_id, encrypted_private_key, nonce, certificate_pem, not_before, not_after)
	             VALUES ($1, 'x', 'x', 'not a pem', $3, $4), ($2, 'x', 'x', 'not a pem', $3, $5)`,
		ws1, ws2, h(-300*24*time.Hour), h(20*24*time.Hour), h(90*24*time.Hour))
	// A tenant connector about to expire: out of scope.
	_ = e.pool.QueryRow(context.Background(),
		`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'n', 'office') RETURNING id::text`, ws1).Scan(&net)
	e.exec(t, `INSERT INTO connectors (tenant_id, remote_network_id, name, status, cert_serial, cert_not_after)
	             VALUES ($1, $2, 'conn-soon', 'active', 'cc', $3)`, ws1, net, h(-time.Hour))
}

func TestJob_SendsOnceADayToActiveSuperAdmins(t *testing.T) {
	e := newEnv(t)
	e.seed(t)
	ctl := fakeController{notAfter: e.now.Add(2 * time.Hour), ok: true}
	ctx := context.Background()

	res, err := e.job(ctl).Check(ctx)
	if err != nil || res.Sent != 2 || res.Recipients != 2 || res.Entries != 4 {
		t.Fatalf("first check: %+v %v", res, err)
	}
	if len(e.sender.sent["a@provider.test"]) != 1 || len(e.sender.sent["b@provider.test"]) != 1 ||
		e.sender.sent["gone@provider.test"] != nil || e.sender.sent["ops@provider.test"] != nil {
		t.Fatalf("recipients: %v", e.sender.sent)
	}
	body := e.sender.body
	for _, want := range []string{`Relay "relay-expired"`, `Relay "relay-soon"`, "Controller gRPC certificate, serial c0ffee", `Workspace CA "Acme"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	for _, not := range []string{"relay-fine", "relay-revoked", `"Beta"`, "conn-soon", "x\r\n"} {
		if strings.Contains(body, not) {
			t.Fatalf("body must not mention %q:\n%s", not, body)
		}
	}

	// Same day again (a restart, or the next hourly check): nothing sent.
	res, err = e.job(ctl).Check(ctx)
	if err != nil || res.Skipped != "already_sent" || len(e.sender.sent["a@provider.test"]) != 1 {
		t.Fatalf("second check: %+v %v (sent %v)", res, err, e.sender.sent)
	}

	audits := e.auditRows(t)
	if len(audits) != 1 || !strings.HasPrefix(audits[0], SystemActor+"|"+e.now.Format("2006-01-02")+"|") ||
		!strings.Contains(audits[0], `"sent": 2`) || !strings.Contains(audits[0], `"expired": 1`) ||
		strings.Contains(audits[0], "@provider.test") {
		t.Fatalf("audit rows: %v", audits)
	}
}

func TestJob_NothingToReportSendsNothingAndMarksTheDay(t *testing.T) {
	e := newEnv(t)
	e.exec(t, `INSERT INTO provider_users (email, role) VALUES ('a@provider.test', 'super-admin')`)
	e.exec(t, `INSERT INTO relays (name, status, cert_serial, cert_not_after) VALUES ('ok', 'active', 'a', $1)`, e.now.Add(3*24*time.Hour))
	res, err := e.job(fakeController{notAfter: e.now.Add(10 * 24 * time.Hour), ok: true}).Check(context.Background())
	if err != nil || res.Skipped != "nothing_to_report" || len(e.sender.sent) != 0 || len(e.auditRows(t)) != 0 {
		t.Fatalf("%+v %v sent=%v", res, err, e.sender.sent)
	}
	if v, err := e.marker.Get(context.Background(), markerPrefix+e.now.Format("2006-01-02")).Result(); err != nil || v != "1" {
		t.Fatalf("day not marked: %q %v", v, err)
	}
}

func TestJob_FailedSendIsRetriedNotMarked(t *testing.T) {
	e := newEnv(t)
	e.seed(t)
	e.sender.fail = map[string]bool{"a@provider.test": true, "b@provider.test": true}
	ctx := context.Background()
	if _, err := e.job(nil).Check(ctx); err == nil {
		t.Fatal("all sends failed: want an error")
	}
	if len(e.auditRows(t)) != 0 {
		t.Fatal("nothing was sent: no audit row")
	}
	// SMTP recovers: the next check sends.
	e.sender.fail = nil
	res, err := e.job(nil).Check(ctx)
	if err != nil || res.Sent != 2 {
		t.Fatalf("retry: %+v %v", res, err)
	}
}

func TestJob_PartialFailureCountsAsSent(t *testing.T) {
	e := newEnv(t)
	e.seed(t)
	e.sender.fail = map[string]bool{"b@provider.test": true}
	res, err := e.job(nil).Check(context.Background())
	if err != nil || res.Sent != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if a := e.auditRows(t); len(a) != 1 || !strings.Contains(a[0], `"failed": 1`) {
		t.Fatalf("audit: %v", a)
	}
}

func TestJob_NoSuperAdminsSendsNothing(t *testing.T) {
	e := newEnv(t)
	e.exec(t, `INSERT INTO relays (name, status, cert_serial, cert_not_after) VALUES ('x', 'active', 'a', $1)`, e.now.Add(-time.Hour))
	res, err := e.job(nil).Check(context.Background())
	if err != nil || res.Skipped != "no_recipients" || len(e.sender.sent) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if err := e.marker.Get(context.Background(), markerPrefix+e.now.Format("2006-01-02")).Err(); !errors.Is(err, valkeycompat.Nil) {
		t.Fatalf("day must not be marked without a send: %v", err)
	}
}

// --- harness ---

func newTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("certalert_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}
	parsed, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
		admin.Close()
	})
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list migrations: %v (%d files)", err, len(files))
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

// testValkey connects to AUTH_TEST_VALKEY_URL and selects its database
// (redis://host:port/N), so tests stay in the isolated test database.
func testValkey(t *testing.T) valkeycompat.Cmdable {
	t.Helper()
	raw := os.Getenv("AUTH_TEST_VALKEY_URL")
	if raw == "" {
		t.Skip("AUTH_TEST_VALKEY_URL not set")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		t.Fatalf("bad AUTH_TEST_VALKEY_URL: %v", err)
	}
	db, err := strconv.Atoi(strings.TrimPrefix(u.Path, "/"))
	if err != nil || db == 0 {
		t.Fatalf("AUTH_TEST_VALKEY_URL must name a non-default database (/N), got %q", u.Path)
	}
	client, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{u.Host}, SelectDB: db})
	if err != nil {
		t.Fatalf("valkey client: %v", err)
	}
	t.Cleanup(client.Close)
	return valkeycompat.NewAdapter(client)
}
