package alerting

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/yourorg/ztna/controller/internal/auditexport"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// testSealer is a reversible stand-in for pki.EncryptSecret: it checks
// the context round-trips.
type testSealer struct{}

func (testSealer) EncryptSecret(p []byte, ctx string) (string, string, error) {
	return hex.EncodeToString(p), ctx, nil
}
func (testSealer) DecryptSecret(ct, nonce, ctx string) ([]byte, error) {
	if nonce != ctx {
		return nil, errors.New("wrong context")
	}
	return hex.DecodeString(ct)
}

type fakeMail struct {
	mu   sync.Mutex
	sent []string
	fail bool
}

func (m *fakeMail) SendPlain(to, subject, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return errors.New("smtp down")
	}
	m.sent = append(m.sent, to+"|"+subject)
	return nil
}

// receiver is a webhook endpoint that records requests and answers with
// the configured status.
type receiver struct {
	mu     sync.Mutex
	status int
	bodies [][]byte
	heads  []http.Header
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	rc.bodies = append(rc.bodies, b)
	rc.heads = append(rc.heads, r.Header.Clone())
	st := rc.status
	rc.mu.Unlock()
	if st == 0 {
		st = 204
	}
	if st == 302 {
		http.Redirect(w, r, "http://203.0.113.9/elsewhere", http.StatusFound)
		return
	}
	w.WriteHeader(st)
}

func (rc *receiver) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.bodies)
}

type deliveryEnv struct {
	*env
	channels *ChannelStore
	deliv    *Deliverer
	mail     *fakeMail
	now      time.Time
	chMux    *http.ServeMux
}

func newDeliveryEnv(t *testing.T) *deliveryEnv {
	e := newEnv(t)
	d := &deliveryEnv{env: e, mail: &fakeMail{}, now: time.Now().UTC().Truncate(time.Second)}
	d.channels = NewChannelStore(e.store, testSealer{}, URLPolicy{AllowPrivate: true})
	d.deliv = &Deliverer{
		Store: e.store, Channels: d.channels,
		HTTP: auditexport.NewClient(auditexport.ClientOptions{AllowPrivate: true}),
		Mail: d.mail, ConsoleURL: "https://console.example.test/",
		Policy: DefaultRetryPolicy(),
		Now:    func() time.Time { return d.now },
	}
	h := &ChannelHandler{Channels: d.channels, Deliverer: d.deliv, Authz: provider.NewAuthz(), ProviderStore: provider.NewStore(e.pool)}
	d.chMux = http.NewServeMux()
	d.chMux.HandleFunc("GET /provider/alert-channels", h.List)
	d.chMux.HandleFunc("POST /provider/alert-channels", h.Create)
	d.chMux.HandleFunc("PATCH /provider/alert-channels/{id}", h.Update)
	d.chMux.HandleFunc("DELETE /provider/alert-channels/{id}", h.Delete)
	d.chMux.HandleFunc("POST /provider/alert-channels/{id}/test", h.Test)
	d.chMux.HandleFunc("POST /provider/alert-channels/{id}/rotate-secret", h.Rotate)
	d.chMux.HandleFunc("GET /provider/alert-events/{id}/deliveries", h.Deliveries)
	e.mux = d.chMux
	return d
}

type pgxTx = pgx.Tx

// fireOne records one event for rule through Apply (so it is queued exactly
// as in production) and returns it.
func (d *deliveryEnv) fireOne(t *testing.T, ruleID string, o Observation) Event {
	t.Helper()
	ctx := context.Background()
	d.exec(t, `UPDATE alert_rules SET for_seconds = 0, enabled = TRUE WHERE id = $1`, ruleID)
	r, err := d.store.GetRule(ctx, ruleID)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := d.store.Apply(ctx, r, []Observation{o}, d.now, "")
	if err != nil || len(ev) != 1 {
		t.Fatalf("fire: %v %v", ev, err)
	}
	return ev[0]
}

func verifySig(t *testing.T, secret string, h http.Header, body []byte) {
	t.Helper()
	sig := h.Get(HeaderSignature)
	parts := strings.Split(sig, ",")
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "t=") || !strings.HasPrefix(parts[1], "v1=") {
		t.Fatalf("signature header: %q", sig)
	}
	ts := strings.TrimPrefix(parts[0], "t=")
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + "."))
	m.Write(body)
	if want := hex.EncodeToString(m.Sum(nil)); strings.TrimPrefix(parts[1], "v1=") != want {
		t.Fatalf("signature doesn't verify with the channel secret")
	}
	unix, _ := strconv.ParseInt(ts, 10, 64)
	if time.Since(time.Unix(unix, 0)) > 24*time.Hour*3650 {
		t.Fatalf("timestamp nonsense: %s", ts)
	}
}

// D-52 payload, pinned field by field.
func TestPayload_Golden(t *testing.T) {
	tenant := "6b1f5f2e-0000-4000-8000-000000000001"
	e := Event{ID: "e-1", Kind: KindConnectorOffline, Scope: ScopeTenant, TenantID: &tenant, TargetType: "connector",
		TargetID: "c-1", TargetName: "conn-a", Status: "firing", Severity: SeverityWarning, Summary: "Connector conn-a of tenant acme is offline",
		StartedAt: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC), OccurredAt: time.Date(2026, 11, 20, 10, 5, 0, 0, time.UTC)}
	got := string(NewPayload(e, "https://console.example.test/").JSON())
	want := `{"version":1,"event_id":"e-1","event_type":"connector.offline","status":"firing","severity":"warning","scope":"tenant","tenant_id":"6b1f5f2e-0000-4000-8000-000000000001","target":{"type":"connector","id":"c-1","name":"conn-a"},"summary":"Connector conn-a of tenant acme is offline","started_at":"2026-11-20T10:00:00Z","timestamp":"2026-11-20T10:05:00Z","console_url":"https://console.example.test/alerts?event=e-1"}`
	if got != want {
		t.Fatalf("payload changed:\n got %s\nwant %s", got, want)
	}
	if p := NewPayload(Event{ID: "x"}, ""); p.ConsoleURL != nil || p.TenantID != nil {
		t.Fatalf("console_url / tenant_id must be null when unknown: %+v", p)
	}
	if s := NewPayload(Event{Summary: "a\r\nBcc: x", Severity: "info"}, "").EmailSubject(); strings.ContainsAny(s, "\r\n") {
		t.Fatalf("subject has a line break: %q", s)
	}
}

func TestURLPolicy(t *testing.T) {
	strict := URLPolicy{}
	for _, bad := range []string{
		"http://hooks.example.com/x", "https://127.0.0.1/x", "https://10.1.2.3/x", "https://[::1]/x",
		"https://169.254.169.254/latest", "https://localhost/x", "https://user:pw@hooks.example.com/x",
		"https://hooks.example.com/x#frag", "ftp://hooks.example.com", "https:///nohost", "https://0x7f000001/x",
	} {
		if err := strict.ValidateWebhookURL(bad); !errors.Is(err, ErrInvalidURL) {
			t.Errorf("strict %s: want ErrInvalidURL, got %v", bad, err)
		}
	}
	if err := strict.ValidateWebhookURL("https://hooks.example.com/services/T1/B2/abc"); err != nil {
		t.Errorf("public https: %v", err)
	}
	dev := URLPolicy{AllowPrivate: true}
	for _, ok := range []string{"http://127.0.0.1:9000/hook", "https://10.1.2.3/x"} {
		if err := dev.ValidateWebhookURL(ok); err != nil {
			t.Errorf("dev %s: %v", ok, err)
		}
	}
}

// The connect-time check: a name that resolves to loopback is refused when
// dialled even though the URL passed validation as a name (DNS rebinding);
// redirects are never followed.
func TestWebhook_DialTimeGuardAndRedirects(t *testing.T) {
	rc := &receiver{}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	strictD := &Deliverer{HTTP: auditexport.NewClient(auditexport.ClientOptions{})}
	if _, err := strictD.postWebhook(context.Background(), "http://localhost:"+port+"/hook", []byte("s"), "e", []byte("{}")); err == nil {
		t.Fatal("a name resolving to loopback was dialled")
	}
	if rc.count() != 0 {
		t.Fatal("the receiver was reached")
	}
	rc.status = 302
	devD := &Deliverer{HTTP: auditexport.NewClient(auditexport.ClientOptions{AllowPrivate: true})}
	code, err := devD.postWebhook(context.Background(), srv.URL+"/hook", []byte("s"), "e", []byte("{}"))
	if err == nil || code == nil || *code != 302 {
		t.Fatalf("redirect: code %v, err %v (want a failed delivery with 302)", code, err)
	}
}

func TestDeliveryDB_WebhookEndToEnd(t *testing.T) {
	d := newDeliveryEnv(t)
	ctx := context.Background()
	rc := &receiver{}
	srv := httptest.NewServer(rc)
	defer srv.Close()

	// Create a webhook through the API: the secret comes back once.
	r := d.do(t, provider.RoleSuperAdmin, "POST", "/provider/alert-channels", map[string]any{
		"name": "ops-hook", "type": "webhook", "url": srv.URL + "/services/T0/B0/topsecretpath", "reason": "lab"})
	if r.code != 201 {
		t.Fatalf("create: %d %s", r.code, r.body)
	}
	var created struct {
		Channel Channel `json:"channel"`
		Secret  string  `json:"secret"`
	}
	if err := json.Unmarshal([]byte(r.body), &created); err != nil || !strings.HasPrefix(created.Secret, "whsec_") {
		t.Fatalf("create body: %s", r.body)
	}
	if strings.Contains(r.body, "topsecretpath") {
		t.Fatalf("create echoed the URL path: %s", r.body)
	}
	// Never again: list, audit.
	l := d.do(t, provider.RoleSuperAdmin, "GET", "/provider/alert-channels", nil)
	if strings.Contains(l.body, created.Secret) || strings.Contains(l.body, "topsecretpath") || strings.Contains(l.body, "encrypted") {
		t.Fatalf("list leaks: %s", l.body)
	}
	var audit string
	if err := d.pool.QueryRow(ctx, `SELECT string_agg(details::text, ' ') FROM provider_audit_logs WHERE target_type = 'alert_channel'`).Scan(&audit); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(audit, created.Secret) || strings.Contains(audit, "topsecretpath") || !strings.Contains(audit, `"reason": "lab"`) {
		t.Fatalf("audit: %s", audit)
	}
	if r := d.do(t, provider.RoleRelayOps, "GET", "/provider/alert-channels", nil); r.code != 403 {
		t.Fatalf("relay-ops list: %d", r.code)
	}

	// A platform alert is delivered, signed, with the D-52 fields.
	ev := d.fireOne(t, ruleRelayOffline, Observation{TargetType: "relay", TargetID: "r-1", TargetName: "relay-a", Summary: "Relay relay-a is offline"})
	if n := d.deliv.RunOnce(ctx); n != 1 || rc.count() != 1 {
		t.Fatalf("delivered %d, receiver saw %d", n, rc.count())
	}
	verifySig(t, created.Secret, rc.heads[0], rc.bodies[0])
	if rc.heads[0].Get(HeaderEventID) != ev.ID {
		t.Fatalf("event id header: %q", rc.heads[0].Get(HeaderEventID))
	}
	var p Payload
	if err := json.Unmarshal(rc.bodies[0], &p); err != nil || p.Version != 1 || p.EventType != KindRelayOffline ||
		p.Scope != ScopePlatform || p.TenantID != nil || p.Status != "firing" || p.ConsoleURL == nil {
		t.Fatalf("payload: %s", rc.bodies[0])
	}
	if n := d.count(t, `SELECT count(*) FROM alert_delivery_queue`); n != 0 {
		t.Fatalf("queue after delivery: %d", n)
	}
	if n := d.count(t, `SELECT count(*) FROM alert_deliveries WHERE status = 'delivered' AND http_status = 204`); n != 1 {
		t.Fatalf("delivered rows: %d", n)
	}

	// Receiver down: retries with backoff, then a final failure.
	rc.status = 500
	d.fireOne(t, rulePool, Observation{TargetType: "pool", TargetID: "p-1", TargetName: "default", Summary: "Pool default has no eligible relay"})
	for i := 0; i < DefaultRetryPolicy().MaxRetries; i++ {
		if n := d.deliv.RunOnce(ctx); n != 1 {
			t.Fatalf("attempt %d: claimed %d", i+1, n)
		}
		if d.deliv.RunOnce(ctx) != 0 {
			t.Fatalf("attempt %d: retried before the backoff", i+1)
		}
		d.now = d.now.Add(outboxMax)
	}
	if n := d.count(t, `SELECT count(*) FROM alert_deliveries WHERE status = 'retrying'`); n != DefaultRetryPolicy().MaxRetries-1 {
		t.Fatalf("retrying rows: %d", n)
	}
	if n := d.count(t, `SELECT count(*) FROM alert_deliveries WHERE status = 'failed' AND http_status = 500`); n != 1 {
		t.Fatalf("final failure rows: %d", n)
	}
	if n := d.count(t, `SELECT count(*) FROM alert_delivery_queue`); n != 0 {
		t.Fatalf("queue after the final failure: %d", n)
	}

	// Rotate: new secret, old one no longer signs.
	rot := d.do(t, provider.RoleSuperAdmin, "POST", "/provider/alert-channels/"+created.Channel.ID+"/rotate-secret", map[string]any{"reason": "rotate"})
	var rotated struct {
		Secret string `json:"secret"`
	}
	if rot.code != 200 || json.Unmarshal([]byte(rot.body), &rotated) != nil || rotated.Secret == created.Secret || !strings.HasPrefix(rotated.Secret, "whsec_") {
		t.Fatalf("rotate: %d %s", rot.code, rot.body)
	}
	rc.status = 200
	tr := d.do(t, provider.RoleSuperAdmin, "POST", "/provider/alert-channels/"+created.Channel.ID+"/test", map[string]any{"reason": "check"})
	if tr.code != 200 || !strings.Contains(tr.body, `"delivered":true`) {
		t.Fatalf("test send: %d %s", tr.code, tr.body)
	}
	last := len(rc.bodies) - 1
	verifySig(t, rotated.Secret, rc.heads[last], rc.bodies[last])
	if n := d.count(t, `SELECT count(*) FROM provider_audit_logs WHERE action = 'alert.channel.test' AND details->>'reason' = 'check'`); n != 1 {
		t.Fatalf("test audit rows: %d", n)
	}
	if d.do(t, provider.RoleSuperAdmin, "GET", "/provider/alert-events/"+ev.ID+"/deliveries", nil).code != 200 {
		t.Fatal("deliveries read")
	}
}

// outboxMax is longer than any backoff step, to make the next attempt due.
const outboxMax = 6 * time.Minute

// D-50: routing by scope filter and minimum severity.
func TestDeliveryDB_Routing(t *testing.T) {
	d := newDeliveryEnv(t)
	ctx := context.Background()
	mk := func(name, scope, min string) string {
		t.Helper()
		ch, _, err := d.channels.CreateChannel(ctx, ChannelInput{Name: &name, Type: ChannelEmail, ScopeFilter: &scope, MinSeverity: &min},
			func(context.Context, pgxTx, *Channel, *Channel) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		return ch.ID
	}
	platformOnly := mk("platform-only", FilterPlatform, SeverityInfo)
	tenantOnly := mk("tenant-only", FilterTenant, SeverityInfo)
	criticalAll := mk("critical-all", FilterAll, SeverityCritical)
	acme := d.workspace(t, "acme")
	tenantEv := d.fireOne(t, ruleConnectorOffline, Observation{TargetType: "connector", TargetID: "c-1", TargetName: "c1", TenantID: acme, Summary: "s"}) // warning
	platEv := d.fireOne(t, ruleRelayOffline, Observation{TargetType: "relay", TargetID: "r-1", TargetName: "r1", Summary: "s"})                           // critical
	queued := func(ev, ch string) bool {
		return d.count(t, `SELECT count(*) FROM alert_delivery_queue WHERE event_id = $1 AND channel_id = $2`, ev, ch) == 1
	}
	if queued(tenantEv.ID, platformOnly) {
		t.Fatal("a tenant alert was routed to a platform-only channel")
	}
	if !queued(tenantEv.ID, tenantOnly) || queued(tenantEv.ID, criticalAll) {
		t.Fatal("tenant alert routing (warning) wrong")
	}
	if !queued(platEv.ID, platformOnly) || queued(platEv.ID, tenantOnly) || !queued(platEv.ID, criticalAll) {
		t.Fatal("platform alert routing (critical) wrong")
	}
	// Email to every super-admin (recipients null), plain one-line subject.
	d.deliv.RunOnce(ctx)
	if len(d.mail.sent) != 3 || !strings.HasPrefix(d.mail.sent[0], "admin@provider.test|[Zecurity ") {
		t.Fatalf("emails: %v", d.mail.sent)
	}
	// A disabled channel gets no new deliveries.
	off := false
	if _, err := d.channels.UpdateChannel(ctx, platformOnly, ChannelInput{Enabled: &off}, func(context.Context, pgxTx, *Channel, *Channel) error { return nil }); err != nil {
		t.Fatal(err)
	}
	d.now = d.now.Add(time.Minute)
	d.exec(t, `DELETE FROM alert_states`)
	ev := d.fireOne(t, ruleRelayOffline, Observation{TargetType: "relay", TargetID: "r-2", TargetName: "r2", Summary: "s"})
	if queued(ev.ID, platformOnly) {
		t.Fatal("a disabled channel was queued")
	}
}

func TestDeliveryDB_ChannelValidation(t *testing.T) {
	d := newDeliveryEnv(t)
	strict := NewChannelStore(d.store, testSealer{}, URLPolicy{})
	ctx := context.Background()
	na := func(context.Context, pgxTx, *Channel, *Channel) error { return nil }
	s := func(v string) *string { return &v }
	cases := map[string]ChannelInput{
		"private url":          {Name: s("a"), Type: ChannelWebhook, URL: s("https://10.0.0.5/hook")},
		"plain http":           {Name: s("b"), Type: ChannelWebhook, URL: s("http://hooks.example.com/x")},
		"webhook without url":  {Name: s("c"), Type: ChannelWebhook},
		"email with url":       {Name: s("d"), Type: ChannelEmail, URL: s("https://hooks.example.com/x")},
		"bad recipient":        {Name: s("e"), Type: ChannelEmail, Recipients: &[]string{"Bob <bob@x.test>"}},
		"header injection":     {Name: s("f"), Type: ChannelEmail, Recipients: &[]string{"a@x.test\r\nBcc: z@y.test"}},
		"empty recipient list": {Name: s("g"), Type: ChannelEmail, Recipients: &[]string{}},
		"unknown type":         {Name: s("h"), Type: "slack"},
		"bad scope filter":     {Name: s("i"), Type: ChannelEmail, ScopeFilter: s("everyone")},
		"padded name":          {Name: s(" j"), Type: ChannelEmail},
	}
	for name, in := range cases {
		if _, _, err := strict.CreateChannel(ctx, in, na); !errors.Is(err, ErrInvalidChannel) && !errors.Is(err, ErrInvalidURL) {
			t.Errorf("%s: want invalid, got %v", name, err)
		}
	}
	if n := d.count(t, `SELECT count(*) FROM alert_channels`); n != 0 {
		t.Fatalf("invalid channels stored: %d", n)
	}
	if r := d.do(t, provider.RoleSuperAdmin, "POST", "/provider/alert-channels", map[string]any{"name": "x", "type": "email"}); r.code != 400 || !strings.Contains(r.body, "reason_required") {
		t.Fatalf("create without reason: %d %s", r.code, r.body)
	}
	if r := d.do(t, provider.RoleRelayOps, "POST", "/provider/alert-channels", map[string]any{"name": "x", "type": "email", "reason": "r"}); r.code != 403 {
		t.Fatalf("relay-ops create: %d", r.code)
	}
}

// Review fixes (PR #235).
func TestDeliveryDB_ReviewFixes(t *testing.T) {
	d := newDeliveryEnv(t)
	ctx := context.Background()
	na := func(context.Context, pgxTx, *Channel, *Channel) error { return nil }
	s := func(v string) *string { return &v }

	// A queued tenant delivery isn't sent after the channel became
	// platform-only.
	ch, _, err := d.channels.CreateChannel(ctx, ChannelInput{Name: s("shared"), Type: ChannelEmail, MinSeverity: s(SeverityInfo)}, na)
	if err != nil {
		t.Fatal(err)
	}
	acme := d.workspace(t, "acme")
	d.fireOne(t, ruleConnectorOffline, Observation{TargetType: "connector", TargetID: "c-1", TargetName: "c1", TenantID: acme, Summary: "s"})
	if _, err := d.channels.UpdateChannel(ctx, ch.ID, ChannelInput{ScopeFilter: s(FilterPlatform)}, na); err != nil {
		t.Fatal(err)
	}
	d.deliv.RunOnce(ctx)
	if len(d.mail.sent) != 0 {
		t.Fatalf("tenant alert sent to a now platform-only channel: %v", d.mail.sent)
	}
	if n := d.count(t, `SELECT count(*) FROM alert_deliveries WHERE status = 'failed' AND error = 'channel filters no longer match'`); n != 1 {
		t.Fatalf("filter-mismatch record: %d", n)
	}

	// No mailer configured: email fails (never "delivered").
	d.exec(t, `UPDATE alert_channels SET scope_filter = 'all'`)
	d.exec(t, `DELETE FROM alert_states`)
	d.now = d.now.Add(time.Minute)
	d.deliv.Mail = nil
	d.fireOne(t, ruleConnectorOffline, Observation{TargetType: "connector", TargetID: "c-2", TargetName: "c2", TenantID: acme, Summary: "s"})
	d.deliv.RunOnce(ctx)
	if n := d.count(t, `SELECT count(*) FROM alert_deliveries WHERE status = 'retrying' AND error = 'email not configured'`); n != 1 {
		t.Fatalf("unconfigured mailer recorded as: %d retrying rows", n)
	}

	// A stalled mail server is abandoned after the timeout.
	d.exec(t, `DELETE FROM alert_delivery_queue`)
	d.deliv.Mail = stallMail{}
	d.deliv.MailTimeout = 200 * time.Millisecond
	start := time.Now()
	if err := d.deliv.sendEmail(ctx, []string{"a@x.test"}, "s", "b"); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("stalled SMTP: err %v after %s", err, time.Since(start))
	}

	// Partial email failure: delivered, with the shortfall recorded.
	pm := &partialMail{bad: "b@x.test"}
	d.deliv.Mail = pm
	err = d.deliv.sendEmail(ctx, []string{"a@x.test", "b@x.test"}, "s", "b")
	var partial *partialEmailError
	if !errors.As(err, &partial) || partial.failed != 1 || partial.total != 2 {
		t.Fatalf("partial email: %v", err)
	}

	// A channel whose stored URL fails today's policy can still be disabled.
	dev := NewChannelStore(d.store, testSealer{}, URLPolicy{AllowPrivate: true})
	hook, _, err := dev.CreateChannel(ctx, ChannelInput{Name: s("lab-hook"), Type: ChannelWebhook, URL: s("http://10.0.0.5/hook")}, na)
	if err != nil {
		t.Fatal(err)
	}
	strict := NewChannelStore(d.store, testSealer{}, URLPolicy{})
	off := false
	if _, err := strict.UpdateChannel(ctx, hook.ID, ChannelInput{Enabled: &off}, na); err != nil {
		t.Fatalf("disable a channel with a now-refused URL: %v", err)
	}
	if _, err := strict.UpdateChannel(ctx, hook.ID, ChannelInput{URL: s("http://10.0.0.6/hook")}, na); !errors.Is(err, ErrInvalidURL) {
		t.Fatalf("a changed URL is still checked: %v", err)
	}

	// Concurrent Notify and Run share one wake channel (run with -race).
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			d.deliv.Notify(ctx, nil)
		}
		close(done)
	}()
	_ = d.deliv.wakeCh()
	<-done
}

type stallMail struct{}

func (stallMail) SendPlain(string, string, string) error { time.Sleep(3 * time.Second); return nil }

type partialMail struct{ bad string }

func (m *partialMail) SendPlain(to, _, _ string) error {
	if to == m.bad {
		return errors.New("mailbox full")
	}
	return nil
}
