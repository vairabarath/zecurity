package auditexport

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/audit"
)

// fakeS3 is a TLS endpoint that stores PUT objects by path.
type fakeS3 struct {
	mu      sync.Mutex
	srv     *httptest.Server
	objects map[string][]byte
	auth    []string
	fail    int
}

func newFakeS3(t *testing.T) *fakeS3 {
	f := &fakeS3{objects: map[string][]byte{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.fail != 0 {
			w.WriteHeader(f.fail)
			return
		}
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		f.objects[r.URL.Path] = b
		f.auth = append(f.auth, r.Header.Get("Authorization"))
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeS3) client() *http.Client {
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.srv.Certificate().Raw})
	return NewClient(ClientOptions{AllowPrivate: true, ExtraRootsPEM: pemBytes})
}

// events decodes every stored object of one stream, in key order.
func (f *fakeS3) events(t *testing.T, stream string) (keys []string, evs []Event) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.objects {
		if strings.Contains(k, "/"+stream+"/") {
			keys = append(keys, k)
		}
	}
	sortStrings(keys)
	for _, k := range keys {
		for _, m := range readNDJSONGz(t, f.objects[k]) {
			b, _ := json.Marshal(m)
			var ev Event
			if err := json.Unmarshal(b, &ev); err != nil {
				t.Fatal(err)
			}
			evs = append(evs, ev)
		}
	}
	return keys, evs
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func s3Setup(t *testing.T) (app *pgxpool.Pool, ws string, store *Store, f *fakeS3, exp *Exporter, cfg json.RawMessage) {
	_, app, ws = newExportDB(t)
	store = NewStore(app, fakeSealer{}, DefaultRegistry(), true)
	f = newFakeS3(t)
	exp = NewExporter(app, store, f.client())
	cfg = s3cfg(t, S3Config{Bucket: "zecurity-audit", Region: "eu-west-1", Prefix: "siem", Endpoint: f.srv.URL,
		AccessKeyID: "AKIAIOSFODNN7EXAMPLE"})
	return
}

func s3Secret(v string) *AuthHeader { return &AuthHeader{Name: S3SecretSlot, Value: v} }

func TestS3Sink_CreateDerivesURLAndNeedsSecret(t *testing.T) {
	app, ws, store, f, _, cfg := s3Setup(t)
	ctx := context.Background()
	var ie *InputError
	if _, _, err := store.Create(ctx, CreateInput{WorkspaceID: ws, Name: "s3", Kind: "s3", Config: cfg,
		Streams: []string{"admin"}}); !errors.As(err, &ie) {
		t.Fatalf("no secret: %v", err)
	}
	if _, _, err := store.Create(ctx, CreateInput{WorkspaceID: ws, Name: "s3", Kind: "s3", Config: cfg, URL: "https://elsewhere.example.net",
		Streams: []string{"admin"}, AuthHeader: s3Secret("sek")}); !errors.As(err, &ie) {
		t.Fatalf("explicit URL: %v", err)
	}
	if _, _, err := store.Create(ctx, CreateInput{WorkspaceID: ws, Name: "s3", Kind: "s3", Config: json.RawMessage(`{"bucket":"BAD"}`),
		Streams: []string{"admin"}, AuthHeader: s3Secret("sek")}); !errors.As(err, &ie) {
		t.Fatalf("bad config: %v", err)
	}
	sink, _, err := store.Create(ctx, CreateInput{WorkspaceID: ws, Name: "s3", Kind: "s3", Config: cfg,
		Streams: []string{"admin", "access"}, Enabled: true, AuthHeader: s3Secret("very-secret-key"), Actor: ChangedBy{Email: "a@example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if sink.Kind != "s3" || sink.URL != f.srv.URL+"/zecurity-audit" {
		t.Fatalf("sink %+v", sink)
	}
	var leaked int
	if err := app.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE tenant_id = $1 AND details::text LIKE '%very-secret%'`, ws).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("secret in audit_logs: %d %v", leaked, err)
	}
}

func TestS3Sink_UpdateCredentialRules(t *testing.T) {
	_, ws, store, f, _, cfg := s3Setup(t)
	ctx := context.Background()
	sink, _, err := store.Create(ctx, CreateInput{WorkspaceID: ws, Name: "s3", Kind: "s3", Config: cfg,
		Streams: []string{"admin"}, Enabled: true, AuthHeader: s3Secret("sek")})
	if err != nil {
		t.Fatal(err)
	}
	var ie *InputError
	with := func(mut func(*S3Config)) json.RawMessage {
		c := S3Config{Bucket: "zecurity-audit", Region: "eu-west-1", Prefix: "siem", Endpoint: f.srv.URL, AccessKeyID: "AKIAIOSFODNN7EXAMPLE"}
		mut(&c)
		return s3cfg(t, c)
	}
	for name, c := range map[string]json.RawMessage{
		"bucket":   with(func(c *S3Config) { c.Bucket = "attacker-bucket" }),
		"endpoint": with(func(c *S3Config) { c.Endpoint = "https://attacker.example.net" }),
		"key id":   with(func(c *S3Config) { c.AccessKeyID = "AKIAOTHERKEY0000" }),
	} {
		if _, err := store.Update(ctx, UpdateInput{WorkspaceID: ws, ID: sink.ID, Config: c}); !errors.As(err, &ie) ||
			!strings.Contains(ie.Msg, "secret access key again") {
			t.Fatalf("%s change without the secret: %v", name, err)
		}
	}
	// A prefix change keeps the destination and the key pair: no secret needed.
	if _, err := store.Update(ctx, UpdateInput{WorkspaceID: ws, ID: sink.ID, Config: with(func(c *S3Config) { c.Prefix = "siem/v2" })}); err != nil {
		t.Fatalf("prefix change: %v", err)
	}
	if _, err := store.Update(ctx, UpdateInput{WorkspaceID: ws, ID: sink.ID,
		Config: with(func(c *S3Config) { c.Bucket = "other-bucket" }), AuthHeader: s3Secret("sek2")}); err != nil {
		t.Fatalf("bucket change with secret: %v", err)
	}
	if _, err := store.Update(ctx, UpdateInput{WorkspaceID: ws, ID: sink.ID, URL: strPtr("https://x.example.net")}); !errors.As(err, &ie) {
		t.Fatalf("URL on S3 sink: %v", err)
	}
	if _, err := store.Update(ctx, UpdateInput{WorkspaceID: ws, ID: sink.ID, ClearAuth: true}); !errors.As(err, &ie) {
		t.Fatalf("clear secret: %v", err)
	}
	if _, err := store.Update(ctx, UpdateInput{WorkspaceID: ws, ID: sink.ID, AuthHeader: &AuthHeader{Name: "X-Api-Key", Value: "v"}}); !errors.As(err, &ie) {
		t.Fatalf("auth header on S3 sink: %v", err)
	}
	got, err := store.Get(ctx, ws, sink.ID)
	if err != nil || got.URL != f.srv.URL+"/other-bucket" {
		t.Fatalf("after updates: %+v %v", got, err)
	}
}

func TestS3Sink_FiveMinuteWindowThenVerifiableObjects(t *testing.T) {
	app, ws, store, f, exp, cfg := s3Setup(t)
	ctx := context.Background()
	sink, secret, err := store.Create(ctx, CreateInput{WorkspaceID: ws, Name: "s3", Kind: "s3", Config: cfg,
		Streams: []string{"admin"}, Enabled: true, AuthHeader: s3Secret("sek")})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := audit.Record(ctx, app, audit.Entry{TenantID: ws, ActorEmail: "a@example.com", Action: "group.create",
			TargetType: "group", TargetID: "g", Details: map[string]any{"i": i}}); err != nil {
			t.Fatal(err)
		}
	}
	// Young events: nothing is written; the cursor is rescheduled ~5 min out.
	n, err := exp.DeliverOnce(ctx, sink.ID, audit.StreamAdmin)
	if err != nil || n != 0 {
		t.Fatalf("young batch delivered: n=%d err=%v", n, err)
	}
	if len(f.objects) != 0 {
		t.Fatal("object written inside the window")
	}
	var waitSecs float64
	if err := app.QueryRow(ctx, `SELECT extract(epoch FROM next_attempt_at - now()) FROM audit_export_cursors WHERE sink_id = $1 AND stream = 'admin'`,
		sink.ID).Scan(&waitSecs); err != nil || waitSecs < 240 || waitSecs > 301 {
		t.Fatalf("next attempt in %.0f s (%v), want ~300", waitSecs, err)
	}
	if seq, attempts, lastErr := cursor(t, app, sink.ID, audit.StreamAdmin); seq != 0 || attempts != 0 || lastErr != nil {
		t.Fatalf("cursor moved or failed: %d %d %v", seq, attempts, lastErr)
	}

	// Six minutes later the batch is due: one object, the whole chain.
	exp.now = func() time.Time { return time.Now().Add(6 * time.Minute) }
	drain(t, exp, sink.ID, audit.StreamAdmin)
	keys, evs := f.events(t, "admin")
	if len(keys) != 1 || !strings.HasSuffix(keys[0], "-4.jsonl.gz") ||
		!strings.HasPrefix(keys[0], "/zecurity-audit/siem/zecurity/v1/"+ws+"/admin/") {
		t.Fatalf("keys %v", keys)
	}
	checkChain(t, ws, audit.StreamAdmin, evs) // seq 1 is the sink's own create row
	if len(evs) != 4 || evs[0].Action != "audit_export.sink.create" {
		t.Fatalf("events %d first %s", len(evs), evs[0].Action)
	}
	if seq, _, _ := cursor(t, app, sink.ID, audit.StreamAdmin); seq != 4 {
		t.Fatalf("cursor %d", seq)
	}
	_ = secret

	// A full batch (more pending than one object holds) goes at once.
	exp.now = time.Now
	for i := 0; i < MaxBatchEvents+5; i++ {
		if err := audit.Record(ctx, app, audit.Entry{TenantID: ws, ActorEmail: "a@example.com", Action: "group.create",
			TargetType: "group", TargetID: "g"}); err != nil {
			t.Fatal(err)
		}
	}
	n, err = exp.DeliverOnce(ctx, sink.ID, audit.StreamAdmin)
	if err != nil || n != MaxBatchEvents {
		t.Fatalf("full batch: n=%d err=%v", n, err)
	}
	// The remaining 5 are young again: they wait.
	if n, err := exp.DeliverOnce(ctx, sink.ID, audit.StreamAdmin); err != nil || n != 0 {
		t.Fatalf("tail: n=%d err=%v", n, err)
	}
}

func TestS3Sink_FailureIsRecordedAndTestWritesAtOnce(t *testing.T) {
	app, ws, store, f, exp, cfg := s3Setup(t)
	ctx := context.Background()
	sink, _, err := store.Create(ctx, CreateInput{WorkspaceID: ws, Name: "s3", Kind: "s3", Config: cfg,
		Streams: []string{"admin"}, Enabled: true, AuthHeader: s3Secret("sek")})
	if err != nil {
		t.Fatal(err)
	}
	if msg, err := exp.SendTest(ctx, ws, sink.ID); err != nil || msg != "" {
		t.Fatalf("test: %q %v", msg, err)
	}
	keys, evs := f.events(t, "admin")
	if len(keys) != 1 || !strings.Contains(keys[0], "/export.test-") || len(evs) != 1 || evs[0].Action != "export.test" {
		t.Fatalf("test object %v %v", keys, evs)
	}

	f.mu.Lock()
	f.fail = http.StatusForbidden
	f.mu.Unlock()
	exp.now = func() time.Time { return time.Now().Add(6 * time.Minute) }
	if _, err := exp.DeliverOnce(ctx, sink.ID, audit.StreamAdmin); err == nil {
		t.Fatal("403 accepted")
	}
	_, attempts, lastErr := cursor(t, app, sink.ID, audit.StreamAdmin)
	if attempts != 1 || lastErr == nil || !strings.Contains(*lastErr, "HTTP 403") || strings.Contains(*lastErr, f.srv.URL) {
		t.Fatalf("failure state: %d %v", attempts, lastErr)
	}
}
