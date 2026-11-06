package auditexport

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/audit"
)

// fakeSealer is a reversible stand-in for pki.EncryptSecret that still binds
// the context and never stores the plaintext as-is.
type fakeSealer struct{}

func (fakeSealer) EncryptSecret(p []byte, ctx string) (string, string, error) {
	x := make([]byte, len(p))
	for i := range p {
		x[i] = p[i] ^ 0x5a
	}
	return base64.StdEncoding.EncodeToString(x), base64.StdEncoding.EncodeToString([]byte(ctx)), nil
}

func (fakeSealer) DecryptSecret(ct, nonce, ctx string) ([]byte, error) {
	n, _ := base64.StdEncoding.DecodeString(nonce)
	if string(n) != ctx {
		return nil, errors.New("context mismatch")
	}
	x, err := base64.StdEncoding.DecodeString(ct)
	if err != nil {
		return nil, err
	}
	for i := range x {
		x[i] ^= 0x5a
	}
	return x, nil
}

// newExportDB creates a throwaway database from PKI_TEST_DATABASE_URL with
// every schema file and one workspace. owner is the migration role; app runs
// as ztna_app, the controller's role (D-12): everything the exporter and the
// store do must work as app.
func newExportDB(t *testing.T) (owner, app *pgxpool.Pool, ws string) {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("audit_export_test_%d_%d", os.Getpid(), time.Now().UnixNano())
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
	owner, err = pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if app != nil {
			app.Close()
		}
		owner.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)")
		admin.Close()
	})
	files, _ := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := owner.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	cfg, _ := pgxpool.ParseConfig(parsed.String())
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE ztna_app")
		return err
	}
	app, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	slug := fmt.Sprintf("exp-%d", time.Now().UnixNano())
	if err := app.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ($1, $1, 'active', $2) RETURNING id`,
		slug, slug+".example").Scan(&ws); err != nil {
		t.Fatal(err)
	}
	return owner, app, ws
}

// receiver is a TLS webhook endpoint that checks signatures.
type receiver struct {
	mu      sync.Mutex
	srv     *httptest.Server
	secret  []byte
	fail    int // answer this status while non-zero
	bodies  []WebhookBody
	headers []http.Header
	badSigs int
}

func newReceiver(t *testing.T) *receiver {
	rc := &receiver{}
	rc.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		defer rc.mu.Unlock()
		if !rc.validSig(r.Header.Get("X-Zecurity-Signature"), body) {
			rc.badSigs++
		}
		if rc.fail != 0 {
			w.WriteHeader(rc.fail)
			return
		}
		var wb WebhookBody
		if err := json.Unmarshal(body, &wb); err != nil {
			w.WriteHeader(400)
			return
		}
		rc.bodies = append(rc.bodies, wb)
		rc.headers = append(rc.headers, r.Header.Clone())
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *receiver) validSig(h string, body []byte) bool {
	parts := strings.Split(h, ",")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "t=") || !strings.HasPrefix(parts[1], "v1=") {
		return false
	}
	m := hmac.New(sha256.New, rc.secret)
	m.Write([]byte(strings.TrimPrefix(parts[0], "t=") + "."))
	m.Write(body)
	return hmac.Equal([]byte(hex.EncodeToString(m.Sum(nil))), []byte(strings.TrimPrefix(parts[1], "v1=")))
}

func (rc *receiver) events(stream string) []Event {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var out []Event
	for _, b := range rc.bodies {
		if b.Stream == stream {
			out = append(out, b.Events...)
		}
	}
	return out
}

func (rc *receiver) client() *http.Client {
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rc.srv.Certificate().Raw})
	return NewClient(ClientOptions{AllowPrivate: true, ExtraRootsPEM: pemBytes})
}

func setup(t *testing.T) (owner, app *pgxpool.Pool, ws string, store *Store, rc *receiver, exp *Exporter) {
	owner, app, ws = newExportDB(t)
	store = NewStore(app, fakeSealer{}, DefaultRegistry(), true)
	rc = newReceiver(t)
	exp = NewExporter(app, store, rc.client())
	return
}

func drain(t *testing.T, exp *Exporter, sinkID string, s audit.Stream) {
	t.Helper()
	for i := 0; i < 50; i++ {
		n, err := exp.DeliverOnce(context.Background(), sinkID, s)
		if err != nil {
			t.Fatalf("DeliverOnce %s: %v", s, err)
		}
		if n == 0 {
			return
		}
	}
	t.Fatal("stream did not drain")
}

// checkChain verifies what a SIEM would: contiguous seqs from 1, prev links
// from the genesis hash, and row_hash = sha256(prev_hash || canonical).
func checkChain(t *testing.T, ws string, s audit.Stream, evs []Event) {
	t.Helper()
	prev := audit.Genesis(ws, s)
	for i, ev := range evs {
		if ev.Seq != int64(i+1) {
			t.Fatalf("%s event %d has seq %d", s, i, ev.Seq)
		}
		ph, _ := hex.DecodeString(ev.PrevHash)
		rh, _ := hex.DecodeString(ev.RowHash)
		canon, err := base64.StdEncoding.DecodeString(ev.Canonical)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(ph, prev) {
			t.Fatalf("%s seq %d: prev_hash link broken", s, ev.Seq)
		}
		if !bytes.Equal(rh, audit.RowHash(ph, canon)) {
			t.Fatalf("%s seq %d: row_hash != sha256(prev || canonical)", s, ev.Seq)
		}
		if ev.DetailsCanonical != nil && !bytes.Contains(canon, []byte(*ev.DetailsCanonical)) {
			t.Fatalf("%s seq %d: details_canonical is not the hashed text", s, ev.Seq)
		}
		prev = rh
	}
}

func cursor(t *testing.T, db *pgxpool.Pool, sinkID string, s audit.Stream) (seq int64, attempts int, lastErr *string) {
	t.Helper()
	if err := db.QueryRow(context.Background(),
		`SELECT last_seq, attempts, last_error FROM audit_export_cursors WHERE sink_id = $1 AND stream = $2`,
		sinkID, string(s)).Scan(&seq, &attempts, &lastErr); err != nil {
		t.Fatal(err)
	}
	return
}

func TestExport_DeliversBothStreamsInOrderAndVerifiable(t *testing.T) {
	_, app, ws, store, rc, exp := setup(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := audit.Record(ctx, app, audit.Entry{TenantID: ws, ActorEmail: "a@example.com", Action: "group.create",
			TargetType: "group", TargetID: strconv.Itoa(i), Details: map[string]any{"name": "g" + strconv.Itoa(i), "ü": 1.5}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := app.Exec(ctx,
			`INSERT INTO connector_logs (workspace_id, connector_id, message, destination, port, protocol, action, occurred_at)
			 VALUES ($1, 'c1', 'm', '10.0.0.9', 22, 'tcp', 'allow', now())`, ws); err != nil {
			t.Fatal(err)
		}
	}
	sink, secret, err := store.Create(ctx, CreateInput{WorkspaceID: ws, Name: "siem", Kind: "webhook", URL: rc.srv.URL + "/hook",
		Streams: []string{"admin", "access"}, Enabled: true, Actor: ChangedBy{Email: "admin@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, "zsk_") {
		t.Fatalf("secret = %q", secret)
	}
	rc.secret = []byte(secret)

	drain(t, exp, sink.ID, audit.StreamAdmin)
	drain(t, exp, sink.ID, audit.StreamAccess)

	admin := rc.events("admin")
	access := rc.events("access")
	if len(admin) != 4 { // 3 group.create + audit_export.sink.create
		t.Fatalf("admin events = %d, want 4", len(admin))
	}
	if len(access) != 2 || access[0].Action != "resource.access" || access[0].Outcome != "allow" {
		t.Fatalf("access events = %+v", access)
	}
	checkChain(t, ws, audit.StreamAdmin, admin)
	checkChain(t, ws, audit.StreamAccess, access)
	if admin[3].Action != "audit_export.sink.create" {
		t.Fatalf("last admin event = %s", admin[3].Action)
	}
	if rc.badSigs != 0 {
		t.Fatalf("%d deliveries had a bad signature", rc.badSigs)
	}
	for _, h := range rc.headers {
		if h.Get("X-Zecurity-Delivery") == "" {
			t.Fatal("missing X-Zecurity-Delivery")
		}
	}
	if seq, _, _ := cursor(t, app, sink.ID, audit.StreamAdmin); seq != 4 {
		t.Fatalf("admin cursor = %d, want 4", seq)
	}
	got, err := store.Get(ctx, ws, sink.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range got.Status {
		if st.Lag() != 0 || st.LastSuccessAt == nil || st.Lagging {
			t.Fatalf("status after drain = %+v", st)
		}
	}
}

func TestExport_FailureBacksOffThenCatchesUpWithoutGaps(t *testing.T) {
	_, app, ws, store, rc, exp := setup(t)
	ctx := context.Background()
	sink, secret, err := store.Create(ctx, CreateInput{WorkspaceID: ws, Name: "siem", Kind: "webhook", URL: rc.srv.URL,
		Streams: []string{"admin"}, Enabled: true, StartFrom: StartNow})
	if err != nil {
		t.Fatal(err)
	}
	rc.secret = []byte(secret)
	// StartNow: the cursor sits at the head before the sink's own create row,
	// so that row is the first event delivered.
	start, _, _ := cursor(t, app, sink.ID, audit.StreamAdmin)
	if start != 0 {
		t.Fatalf("start cursor = %d, want 0", start)
	}
	for i := 0; i < 5; i++ {
		_ = audit.Record(ctx, app, audit.Entry{TenantID: ws, ActorEmail: "a@example.com", Action: "x.y", TargetType: "t", TargetID: strconv.Itoa(i)})
	}

	rc.fail = http.StatusServiceUnavailable
	if _, err := exp.DeliverOnce(ctx, sink.ID, audit.StreamAdmin); err == nil {
		t.Fatal("delivery to a 503 succeeded")
	}
	seq, attempts, lastErr := cursor(t, app, sink.ID, audit.StreamAdmin)
	if seq != 0 || attempts != 1 || lastErr == nil || *lastErr != "destination answered HTTP 503" {
		t.Fatalf("after failure: seq=%d attempts=%d err=%v", seq, attempts, lastErr)
	}
	due, err := exp.dueCursors(ctx)
	if err != nil || len(due) != 0 {
		t.Fatalf("a backing-off cursor is due: %v %v", due, err)
	}

	rc.fail = 0
	drain(t, exp, sink.ID, audit.StreamAdmin)
	evs := rc.events("admin")
	if len(evs) != 6 || evs[0].Action != "audit_export.sink.create" {
		t.Fatalf("caught-up events: %d", len(evs))
	}
	checkChain(t, ws, audit.StreamAdmin, evs)
	seq, attempts, lastErr = cursor(t, app, sink.ID, audit.StreamAdmin)
	if seq != 6 || attempts != 0 || lastErr != nil {
		t.Fatalf("after recovery: seq=%d attempts=%d err=%v", seq, attempts, lastErr)
	}
}

// Rows purged by retention before a sink received them produce one
// export.gap event with the checkpoint, never a silent skip (P-4).
func TestExport_RetentionGapIsReported(t *testing.T) {
	owner, app, ws, store, rc, exp := setup(t)
	ctx := context.Background()
	sink, secret, err := store.Create(ctx, CreateInput{WorkspaceID: ws, Name: "siem", Kind: "webhook", URL: rc.srv.URL,
		Streams: []string{"access"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	rc.secret = []byte(secret)
	// Back-dated rows (the owner may set created_at; the chain hashes it).
	for i := 0; i < 3; i++ {
		if _, err := owner.Exec(ctx,
			`INSERT INTO connector_logs (workspace_id, connector_id, message, created_at)
			 VALUES ($1, 'c1', 'old', now() - interval '120 days')`, ws); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := app.Exec(ctx, `INSERT INTO connector_logs (workspace_id, connector_id, message) VALUES ($1, 'c1', 'new')`, ws); err != nil {
		t.Fatal(err)
	}
	ret := &audit.AccessRetention{Pool: app, Days: 30} // clamped to 90
	if n, err := ret.PurgeOnce(ctx); err != nil || n != 3 {
		t.Fatalf("purge: n=%d err=%v", n, err)
	}

	drain(t, exp, sink.ID, audit.StreamAccess)
	evs := rc.events("access")
	if len(evs) != 2 || evs[0].Action != ActionGap || evs[1].Seq != 4 {
		t.Fatalf("events = %+v", evs)
	}
	d := evs[0].Details.(map[string]any)
	if d["from_seq"] != float64(1) || d["to_seq"] != float64(3) || d["checkpoint_hash"] == "" {
		t.Fatalf("gap details = %v", d)
	}
	// The chain continues from the checkpoint.
	ck, _ := hex.DecodeString(d["checkpoint_hash"].(string))
	if evs[1].PrevHash != hex.EncodeToString(ck) {
		t.Fatal("first event after the gap does not link to the checkpoint")
	}
	if res, err := audit.VerifyStream(ctx, app, ws, audit.StreamAccess); err != nil || !res.OK() {
		t.Fatalf("verify after purge: %+v %v", res, err)
	}
}

func TestStore_LimitSecretsAndAudit(t *testing.T) {
	owner, app, ws, store, rc, _ := setup(t)
	ctx := context.Background()
	actor := ChangedBy{Email: "sec@example.com"}
	var secrets []string
	for i := 0; i < MaxSinksPerWorkspace; i++ {
		_, s, err := store.Create(ctx, CreateInput{WorkspaceID: ws, Name: "s" + strconv.Itoa(i), Kind: "webhook", URL: rc.srv.URL,
			Streams: []string{"admin"}, Enabled: true, AuthHeader: &AuthHeader{Name: "X-Api-Key", Value: "hdr-secret-" + strconv.Itoa(i)}, Actor: actor})
		if err != nil {
			t.Fatal(err)
		}
		secrets = append(secrets, s)
	}
	_, _, err := store.Create(ctx, CreateInput{WorkspaceID: ws, Name: "s4", Kind: "webhook", URL: rc.srv.URL, Streams: []string{"admin"}})
	var ie *InputError
	if !errors.As(err, &ie) {
		t.Fatalf("4th sink: err = %v, want limit InputError", err)
	}
	if _, _, err := store.Create(ctx, CreateInput{WorkspaceID: ws, Name: "hec", Kind: "splunk_hec", URL: rc.srv.URL, Streams: []string{"admin"}}); !errors.As(err, &ie) {
		t.Fatalf("HEC without token: %v", err)
	}

	sinks, err := store.List(ctx, ws)
	if err != nil || len(sinks) != 3 {
		t.Fatalf("list: %d %v", len(sinks), err)
	}
	id := sinks[0].ID
	if _, err := store.Update(ctx, UpdateInput{WorkspaceID: ws, ID: id, Streams: []string{"admin", "access"}, Actor: actor}); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = app.QueryRow(ctx, `SELECT count(*) FROM audit_export_cursors WHERE sink_id = $1`, id).Scan(&n)
	if n != 2 {
		t.Fatalf("cursors after adding a stream = %d", n)
	}
	// The stored auth header doesn't follow the sink to another host.
	if _, err := store.Update(ctx, UpdateInput{WorkspaceID: ws, ID: id, URL: strPtr("https://attacker.example.net/x"), Actor: actor}); !errors.As(err, &ie) {
		t.Fatalf("host change without re-entering the header: err = %v", err)
	}
	if _, err := store.Update(ctx, UpdateInput{WorkspaceID: ws, ID: id, URL: strPtr(rc.srv.URL + "/other-path"), Actor: actor}); !errors.As(err, &ie) {
		t.Fatalf("same-host path change without re-entering the header: err = %v", err)
	}
	if _, err := store.Update(ctx, UpdateInput{WorkspaceID: ws, ID: id, Enabled: boolPtr(false), Actor: actor}); err != nil {
		t.Fatalf("non-URL change: %v", err)
	}
	if _, err := store.Update(ctx, UpdateInput{WorkspaceID: ws, ID: id, URL: strPtr("https://new.example.net/x"),
		AuthHeader: &AuthHeader{Name: "X-Api-Key", Value: "hdr-secret-0"}, Actor: actor}); err != nil {
		t.Fatalf("host change with a new header: %v", err)
	}
	_, rotated, err := store.RotateSecret(ctx, ws, id, actor)
	if err != nil || rotated == secrets[0] {
		t.Fatalf("rotate: %v", err)
	}
	tg, err := store.loadTarget(ctx, id)
	if err != nil || string(tg.SigningKey) != rotated || tg.AuthHeaderValue != "hdr-secret-0" {
		t.Fatalf("loadTarget after rotate: %v", err)
	}
	if err := store.Delete(ctx, ws, sinks[1].ID, actor); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, ws, sinks[1].ID, actor); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	// Another workspace can't see or touch these sinks.
	if _, err := store.Get(ctx, "00000000-0000-0000-0000-000000000000", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace get: %v", err)
	}

	// No secret appears in plaintext in the table or any audit row.
	var dump string
	if err := owner.QueryRow(ctx,
		`SELECT coalesce((SELECT string_agg(t::text, '|') FROM audit_export_sinks t), '') ||
		        coalesce((SELECT string_agg(a::text, '|') FROM audit_logs a), '')`).Scan(&dump); err != nil {
		t.Fatal(err)
	}
	for _, s := range append(secrets, rotated, "hdr-secret-0", "hdr-secret-1", "hdr-secret-2") {
		if strings.Contains(dump, s) {
			t.Fatalf("secret %q stored in plaintext", s)
		}
	}
	var actions []string
	rows, _ := app.Query(ctx, `SELECT action FROM audit_logs WHERE tenant_id = $1 AND action LIKE 'audit_export.%' ORDER BY seq`, ws)
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actions = append(actions, a)
	}
	rows.Close()
	want := "audit_export.sink.create audit_export.sink.create audit_export.sink.create audit_export.sink.update audit_export.sink.update audit_export.sink.update audit_export.sink.rotate_secret audit_export.sink.delete"
	if strings.Join(actions, " ") != want {
		t.Fatalf("audit actions = %v", actions)
	}
}

func TestSplunkHEC_Delivers(t *testing.T) {
	var gotAuth string
	var lines []map[string]any
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		dec := json.NewDecoder(r.Body)
		for dec.More() {
			var m map[string]any
			if err := dec.Decode(&m); err != nil {
				w.WriteHeader(400)
				return
			}
			lines = append(lines, m)
		}
		_, _ = w.Write([]byte(`{"text":"Success","code":0}`))
	}))
	defer srv.Close()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	c := NewClient(ClientOptions{AllowPrivate: true, ExtraRootsPEM: pemBytes})
	d := Destination{URL: srv.URL + "/services/collector/event", SigningKey: []byte("k"),
		AuthHeaderName: "Authorization", AuthHeaderValue: "Splunk tok-1", Config: json.RawMessage(`{"index":"sec"}`)}
	now := time.Now()
	b := Batch{DeliveryID: "d", WorkspaceID: "w", Stream: "admin", Events: []Event{TestEvent("w", "s", now), TestEvent("w", "s", now)}}
	if err := (SplunkHEC{}).Deliver(context.Background(), c, d, b); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Splunk tok-1" || len(lines) != 2 || lines[0]["index"] != "sec" || lines[0]["sourcetype"] != "zecurity:audit:v1" {
		t.Fatalf("auth=%q lines=%v", gotAuth, lines)
	}
	if err := (SplunkHEC{}).ValidateConfig(json.RawMessage(`{"index":"bad index"}`)); err == nil {
		t.Fatal("invalid index accepted")
	}
	if err := (SplunkHEC{}).ValidateConfig(json.RawMessage(`{"unknown":"x"}`)); err == nil {
		t.Fatal("unknown field accepted")
	}
}

func strPtr(s string) *string { return &s }

func boolPtr(b bool) *bool { return &b }

// Run drains a backlog beyond one batch without waiting a poll per batch.
func TestExporterRun_CatchesUpWithinOnePoll(t *testing.T) {
	_, app, ws, store, rc, exp := setup(t)
	ctx := context.Background()
	if _, err := app.Exec(ctx,
		`INSERT INTO audit_logs (tenant_id, actor_email, action, target_type, target_id)
		 SELECT $1, 'a@example.com', 'bulk.x', 't', g::text FROM generate_series(1, 1200) g`, ws); err != nil {
		t.Fatal(err)
	}
	sink, secret, err := store.Create(ctx, CreateInput{WorkspaceID: ws, Name: "siem", Kind: "webhook", URL: rc.srv.URL,
		Streams: []string{"admin"}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	rc.secret = []byte(secret)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { exp.Run(runCtx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(PollInterval) // well under the 3 polls one-batch-per-poll would need
	for time.Now().Before(deadline) {
		if seq, _, _ := cursor(t, app, sink.ID, audit.StreamAdmin); seq == 1201 {
			checkChain(t, ws, audit.StreamAdmin, rc.events("admin"))
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	seq, _, _ := cursor(t, app, sink.ID, audit.StreamAdmin)
	t.Fatalf("cursor = %d after one poll interval, want 1201", seq)
}
