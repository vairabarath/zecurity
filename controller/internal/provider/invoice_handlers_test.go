package provider_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/billing"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// Sprint 26 P3 (D-66, D-68, D-70): the invoice API over a throwaway DB.

type invEnv struct {
	pool  *pgxpool.Pool
	mux   *http.ServeMux
	super provider.Actor
	ops   provider.Actor
	now   time.Time
}

func newInvEnv(t *testing.T) *invEnv {
	t.Helper()
	pool := newReadTestDB(t)
	e := &invEnv{pool: pool, now: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)}
	e.super = provider.Actor{UserID: scalar[string](t, pool, `INSERT INTO provider_users (email, role) VALUES ('admin@provider.test', 'super-admin') RETURNING id::text`),
		Email: "admin@provider.test", Role: provider.RoleSuperAdmin}
	e.ops = provider.Actor{UserID: scalar[string](t, pool, `INSERT INTO provider_users (email, role) VALUES ('ops@provider.test', 'relay-ops') RETURNING id::text`),
		Email: "ops@provider.test", Role: provider.RoleRelayOps}
	nowFn := func() time.Time { return e.now }
	h := &provider.InvoiceHandler{DB: pool, Authz: provider.NewAuthz(), Store: provider.NewStore(pool),
		Generator: &billing.Generator{DB: pool, Now: nowFn}, ProviderName: "Acme Cloud", Now: nowFn}
	e.mux = http.NewServeMux()
	e.mux.HandleFunc("GET /provider/invoices", h.List)
	e.mux.HandleFunc("GET /provider/invoices/{id}", h.Get)
	e.mux.HandleFunc("POST /provider/invoices/regenerate", h.Regenerate)
	e.mux.HandleFunc("POST /provider/invoices/{id}/issue", h.Issue)
	e.mux.HandleFunc("POST /provider/invoices/{id}/mark-paid", h.MarkPaid)
	e.mux.HandleFunc("POST /provider/invoices/{id}/void", h.Void)
	e.mux.HandleFunc("POST /provider/invoices/{id}/resend", h.Resend)
	return e
}

type invResp struct {
	code int
	body string
	hdr  http.Header
}

func (e *invEnv) do(t *testing.T, actor provider.Actor, method, path string, body any) invResp {
	t.Helper()
	raw := ""
	if body != nil {
		b, _ := json.Marshal(body)
		raw = string(b)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(raw))
	req.RemoteAddr = "192.0.2.1:1"
	req = req.WithContext(provider.WithActor(req.Context(), actor))
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return invResp{code: rec.Code, body: rec.Body.String(), hdr: rec.Header()}
}

func invErr(r invResp) string {
	var v struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal([]byte(r.body), &v)
	return v.Error
}

// seed: a priced card, tenants alpha and bravo (October usage), one purged
// tenant, and the October drafts.
func (e *invEnv) seed(t *testing.T) (alpha, bravo, gone string) {
	t.Helper()
	plan := scalar[string](t, e.pool, `INSERT INTO plans (name, limits) VALUES ('Starter',
		'{"users": null, "resources": null, "remote_networks": null, "connectors": null, "shields": null, "client_devices": null}') RETURNING id::text`)
	card, _ := json.Marshal(billing.Card{Name: "Starter", Limits: json.RawMessage(`{}`), Price: &billing.Price{Currency: "USD", BaseFeeMinor: 1000,
		Meters: map[string]billing.MeterPrice{"active_users": {Included: 1, Unit: 1, UnitPriceMinor: 250}}}})
	exec(t, e.pool, `INSERT INTO plan_card_revisions (plan_id, revision, card, changed_at, changed_by, reason) VALUES ($1, 1, $2, '2026-09-01', 'a', 'r')`, plan, card)
	mk := func(slug string) string {
		return scalar[string](t, e.pool, `INSERT INTO workspaces (slug, name, status, trust_domain, plan_id, created_at)
			VALUES ($1, $1 || ' Inc', 'active', $1 || '.zecurity.test', $2, '2026-10-01') RETURNING id::text`, slug, plan)
	}
	alpha, bravo = mk("alpha"), mk("bravo")
	exec(t, e.pool, `INSERT INTO users (tenant_id, email, provider, provider_sub, role, status) VALUES
		($1, 'admin1@alpha.example', 'google', 'a1', 'admin', 'active'),
		($2, 'admin1@bravo.example', 'google', 'b1', 'admin', 'active')`, alpha, bravo)
	gone = "eeeeeeee-0000-0000-0000-0000000000e9"
	exec(t, e.pool, `INSERT INTO usage_tenants (tenant_id, name, slug, plan_name, plan_id, purged_at) VALUES ($1, 'Gone Ltd', 'gone', 'Starter', $2, '2026-10-20')`, gone, plan)
	for i, ws := range []string{alpha, bravo} {
		for u := 0; u <= i+1; u++ { // alpha 2 users, bravo 3
			exec(t, e.pool, `INSERT INTO usage_active_users VALUES ($1, '2026-10-01', gen_random_uuid(), now())`, ws)
		}
	}
	g := &billing.Generator{DB: e.pool, Now: func() time.Time { return e.now }}
	if n, err := g.CloseDueMonths(t.Context()); err != nil || n != 3 {
		t.Fatalf("seed drafts: %d %v", n, err)
	}
	return alpha, bravo, gone
}

func (e *invEnv) idOf(t *testing.T, tenant string) string {
	return scalar[string](t, e.pool, `SELECT id::text FROM invoices WHERE tenant_id = $1 AND status <> 'void'`, tenant)
}

func (e *invEnv) audits(t *testing.T, action string) []map[string]any {
	t.Helper()
	rows, err := e.pool.Query(t.Context(), `SELECT details FROM provider_audit_logs WHERE action = $1 ORDER BY created_at`, action)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var d map[string]any
		_ = rows.Scan(&d)
		out = append(out, d)
	}
	return out
}

func TestInvoiceAPI_RelayOpsForbidden(t *testing.T) {
	e := newInvEnv(t)
	id := "00000000-0000-0000-0000-000000000001"
	for _, c := range []struct{ m, p string }{
		{"GET", "/provider/invoices"}, {"GET", "/provider/invoices/" + id},
		{"POST", "/provider/invoices/regenerate"}, {"POST", "/provider/invoices/" + id + "/issue"},
		{"POST", "/provider/invoices/" + id + "/mark-paid"}, {"POST", "/provider/invoices/" + id + "/void"},
		{"POST", "/provider/invoices/" + id + "/resend"},
	} {
		if r := e.do(t, e.ops, c.m, c.p, map[string]any{"reason": "r", "month": "2026-10"}); r.code != http.StatusForbidden {
			t.Errorf("relay-ops %s %s: %d", c.m, c.p, r.code)
		}
	}
	if n := scalar[int](t, e.pool, `SELECT count(*) FROM provider_audit_logs`); n != 0 {
		t.Fatalf("refused requests audited: %d", n)
	}
}

func TestInvoiceAPI_ListAndDetail(t *testing.T) {
	e := newInvEnv(t)
	alpha, _, gone := e.seed(t)
	r := e.do(t, e.super, "GET", "/provider/invoices?month=2026-10", nil)
	if r.code != 200 || r.hdr.Get("Cache-Control") != "no-store" {
		t.Fatalf("list: %d %v", r.code, r.hdr)
	}
	var page struct {
		Items []struct {
			ID, TenantName, Status string
			Purged                 bool
			TotalMinor             int64 `json:"total_minor"`
		} `json:"items"`
		Totals     map[string]struct{ Count, TotalMinor int64 } `json:"totals"`
		Currency   string                                       `json:"currency"`
		NextCursor *string                                      `json:"next_cursor"`
	}
	if err := json.Unmarshal([]byte(r.body), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 3 || page.Currency != "USD" || page.Totals["draft"].Count != 3 {
		t.Fatalf("list %s", r.body)
	}
	// Keyset paging, 1 per page: 3 pages, no repeats or gaps.
	seen := map[string]bool{}
	cursor := ""
	for i := 0; i < 5; i++ {
		r := e.do(t, e.super, "GET", "/provider/invoices?month=2026-10&limit=1&cursor="+cursor, nil)
		var p struct {
			Items      []struct{ ID string } `json:"items"`
			NextCursor *string               `json:"next_cursor"`
		}
		_ = json.Unmarshal([]byte(r.body), &p)
		for _, it := range p.Items {
			if seen[it.ID] {
				t.Fatalf("repeat %s", it.ID)
			}
			seen[it.ID] = true
		}
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
	}
	if len(seen) != 3 {
		t.Fatalf("paged %d, want 3", len(seen))
	}
	// Filters; a bad status is 400.
	if r := e.do(t, e.super, "GET", "/provider/invoices?tenant_id="+alpha, nil); !strings.Contains(r.body, alpha) || strings.Count(r.body, `"id"`) != 1 {
		t.Fatalf("tenant filter %s", r.body)
	}
	if r := e.do(t, e.super, "GET", "/provider/invoices?status=sent", nil); r.code != 400 {
		t.Fatalf("bad status: %d", r.code)
	}
	if r := e.do(t, e.super, "GET", "/provider/invoices?month=Oct", nil); r.code != 400 || invErr(r) != "invalid_month" {
		t.Fatalf("bad month: %d %s", r.code, r.body)
	}
	// One invoice.fleet.read row per list request (4 successful lists so far + paging).
	if n := len(e.audits(t, provider.ActionInvoiceFleetRead)); n < 4 {
		t.Fatalf("fleet read audits %d", n)
	}
	// The purged tenant is named from its snapshot and marked purged.
	gid := e.idOf(t, gone)
	r = e.do(t, e.super, "GET", "/provider/invoices/"+gid, nil)
	var d struct {
		TenantName   string `json:"tenant_name"`
		Purged       bool   `json:"purged"`
		ProviderName string `json:"provider_name"`
		EmailTenants bool   `json:"email_tenants"`
		Lines        []struct {
			Position int    `json:"position"`
			Kind     string `json:"kind"`
		} `json:"lines"`
		History []struct{ Status string } `json:"history"`
	}
	if err := json.Unmarshal([]byte(r.body), &d); err != nil || r.code != 200 {
		t.Fatalf("detail %d %s", r.code, r.body)
	}
	if d.TenantName != "Gone Ltd" || !d.Purged || d.ProviderName != "Acme Cloud" || !d.EmailTenants || len(d.Lines) != 2 ||
		d.Lines[0].Kind != "base" || d.Lines[1].Position != 1 || len(d.History) != 1 {
		t.Fatalf("purged detail %+v", d)
	}
	if n := len(e.audits(t, provider.ActionInvoiceRead)); n != 1 {
		t.Fatalf("invoice.read audits %d", n)
	}
	if r := e.do(t, e.super, "GET", "/provider/invoices/00000000-0000-0000-0000-000000000009", nil); r.code != 404 {
		t.Fatalf("missing: %d", r.code)
	}
}

func TestInvoiceAPI_Writes(t *testing.T) {
	e := newInvEnv(t)
	alpha, bravo, gone := e.seed(t)
	a := e.idOf(t, alpha)
	post := func(id, verb string, body any) invResp {
		return e.do(t, e.super, "POST", "/provider/invoices/"+id+"/"+verb, body)
	}
	// Wrong transitions → 409; missing reasons → 400.
	for _, c := range []struct {
		verb, code string
		status     int
		body       any
	}{
		{"mark-paid", "invoice_not_issued", 409, map[string]any{"paid_at": "2026-11-01", "reason": "r"}},
		{"void", "invoice_not_issued", 409, map[string]any{"reason": "r"}},
		{"resend", "invoice_not_issued", 409, map[string]any{"reason": "r"}},
		{"void", "reason_required", 400, map[string]any{}},
		{"mark-paid", "reason_required", 400, map[string]any{"paid_at": "2026-11-01"}},
		{"mark-paid", "invalid_paid_at", 400, map[string]any{"paid_at": "1 Nov", "reason": "r"}},
		{"mark-paid", "reference_too_long", 400, map[string]any{"paid_at": "2026-11-01", "reason": "r", "reference": strings.Repeat("x", 201)}},
	} {
		if r := post(a, c.verb, c.body); r.code != c.status || invErr(r) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.verb, r.code, r.body, c.status, c.code)
		}
	}
	// Issue → 204, number, one email row; the audit has before/after and no address.
	if r := post(a, "issue", map[string]any{}); r.code != 204 {
		t.Fatalf("issue: %d %s", r.code, r.body)
	}
	if r := post(a, "issue", map[string]any{}); r.code != 409 || invErr(r) != "invoice_not_draft" {
		t.Fatalf("issue twice: %d %s", r.code, r.body)
	}
	if n := scalar[int](t, e.pool, `SELECT count(*) FROM invoice_emails WHERE invoice_id = $1`, a); n != 1 {
		t.Fatalf("email rows after issue: %d", n)
	}
	au := e.audits(t, provider.ActionInvoiceIssue)
	if len(au) != 1 || au[0]["before"].(map[string]any)["status"] != "draft" || au[0]["after"].(map[string]any)["number"] != "INV-2026-000001" {
		t.Fatalf("issue audit %v", au)
	}
	// Resend twice → two more rows; issue alone sent once.
	for i := 0; i < 2; i++ {
		if r := post(a, "resend", map[string]any{"reason": "lost it"}); r.code != 204 {
			t.Fatalf("resend: %d %s", r.code, r.body)
		}
	}
	if n := scalar[int](t, e.pool, `SELECT count(*) FROM invoice_emails WHERE invoice_id = $1 AND kind = 'resend'`, a); n != 2 {
		t.Fatalf("resend rows %d", n)
	}
	// Mark paid with a reference and reason.
	if r := post(a, "mark-paid", map[string]any{"paid_at": "2026-11-01", "reference": "bank 42", "reason": "received"}); r.code != 204 {
		t.Fatalf("mark paid: %d %s", r.code, r.body)
	}
	if mp := e.audits(t, provider.ActionInvoiceMarkPaid); len(mp) != 1 || mp[0]["reason"] != "received" {
		t.Fatalf("mark paid audit %v", mp)
	}
	// Void bravo after issuing; regenerate then creates a new draft for it.
	b := e.idOf(t, bravo)
	if r := post(b, "issue", map[string]any{"reason": "ok"}); r.code != 204 {
		t.Fatal(r.body)
	}
	if r := post(b, "void", map[string]any{"reason": "wrong plan"}); r.code != 204 {
		t.Fatalf("void: %d %s", r.code, r.body)
	}
	r := e.do(t, e.super, "POST", "/provider/invoices/regenerate", map[string]any{"month": "2026-10", "reason": "rebuild"})
	if r.code != 200 || !strings.Contains(r.body, `"regenerated":2`) { // bravo new, gone's draft rewritten
		t.Fatalf("regenerate: %d %s", r.code, r.body)
	}
	if n := scalar[int](t, e.pool, `SELECT count(*) FROM invoices WHERE tenant_id = $1`, bravo); n != 2 {
		t.Fatalf("bravo invoices after void + regenerate: %d", n)
	}
	if rg := e.audits(t, provider.ActionInvoiceRegenerate); len(rg) != 1 || rg[0]["reason"] != "rebuild" || len(rg[0]["drafts"].([]any)) != 2 {
		t.Fatalf("regenerate audit %v", rg)
	}
	if r := e.do(t, e.super, "POST", "/provider/invoices/regenerate", map[string]any{"month": "2026-11", "reason": "r"}); r.code != 409 || invErr(r) != "month_open" {
		t.Fatalf("regenerate open month: %d %s", r.code, r.body)
	}
	if r := e.do(t, e.super, "POST", "/provider/invoices/regenerate", map[string]any{"month": "2026-10"}); r.code != 400 || invErr(r) != "reason_required" {
		t.Fatalf("regenerate without reason: %d %s", r.code, r.body)
	}
	// No audit row anywhere carries a tenant admin address.
	if n := scalar[int](t, e.pool, `SELECT count(*) FROM provider_audit_logs WHERE details::text LIKE '%@alpha.example%' OR details::text LIKE '%@bravo.example%'`); n != 0 {
		t.Fatalf("%d audit rows carry a recipient address", n)
	}
	_ = gone
}

// The SQL email store with the worker: the tenant's own active admins only,
// once, never a second time; a purged tenant is not delivered; a row a
// restart left in sending fails without a resend.
func TestInvoiceEmail_SQLStore(t *testing.T) {
	e := newInvEnv(t)
	alpha, bravo, gone := e.seed(t)
	ctx := t.Context()
	exec(t, e.pool, `INSERT INTO users (tenant_id, email, provider, provider_sub, role, status) VALUES
		($1, 'admin2@alpha.example', 'google', 'a2', 'admin', 'active'),
		($1, 'member@alpha.example', 'google', 'a3', 'member', 'active'),
		($1, 'old@alpha.example', 'google', 'a4', 'admin', 'suspended')`, alpha)
	for _, ws := range []string{alpha, gone} {
		if r := e.do(t, e.super, "POST", "/provider/invoices/"+e.idOf(t, ws)+"/issue", map[string]any{}); r.code != 204 {
			t.Fatalf("issue: %d %s", r.code, r.body)
		}
	}
	mail := &capture{}
	store := &billing.SQLEmailStore{DB: e.pool}
	// Queue rows are stamped with the DB clock: the worker runs on real time.
	w := &billing.EmailWorker{Store: store, Mail: mail, ProviderName: "Acme Cloud"}
	if n, err := w.Tick(ctx); err != nil || n != 2 {
		t.Fatalf("tick: %d %v", n, err)
	}
	if got := strings.Join(mail.to, ","); got != "admin1@alpha.example,admin2@alpha.example" {
		t.Fatalf("recipients %q (own tenant's active admins only)", got)
	}
	if !strings.Contains(mail.text[0], "INV-2026-000001") || strings.Contains(mail.html[0], "/provider/") {
		t.Fatalf("body: %q", mail.text[0])
	}
	st := func(ws string) (string, int) {
		var s string
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT status, recipients_count FROM invoice_emails WHERE invoice_id = $1`, e.idOf(t, ws)).Scan(&s, &n); err != nil {
			t.Fatal(err)
		}
		return s, n
	}
	if s, n := st(alpha); s != "sent" || n != 2 {
		t.Fatalf("alpha delivery %s %d", s, n)
	}
	if s, _ := st(gone); s != "not_delivered" {
		t.Fatalf("purged tenant delivery %s", s)
	}
	// Nothing left: a second tick sends nothing.
	if n, _ := w.Tick(ctx); n != 0 || len(mail.to) != 2 {
		t.Fatalf("second tick: %d, %d sends", n, len(mail.to))
	}
	// A row claimed and never finished (a restart) → failed, never re-sent.
	b := e.idOf(t, bravo)
	if r := e.do(t, e.super, "POST", "/provider/invoices/"+b+"/issue", map[string]any{}); r.code != 204 {
		t.Fatal(r.body)
	}
	if _, err := store.ClaimDue(ctx, time.Now(), 10); err != nil {
		t.Fatal(err)
	}
	if n, err := store.FailInterrupted(ctx, time.Now().Add(time.Second)); err != nil || n != 1 {
		t.Fatalf("fail interrupted: %d %v", n, err)
	}
	if n, _ := w.Tick(ctx); n != 0 {
		t.Fatalf("interrupted row re-sent: %d", n)
	}
	if fmt.Sprint(mail.to) != "[admin1@alpha.example admin2@alpha.example]" {
		t.Fatalf("sends %v", mail.to)
	}
}

type capture struct{ to, text, html []string }

func (c *capture) SendMultipart(to, subject, text, html string) error {
	c.to = append(c.to, to)
	c.text = append(c.text, text)
	c.html = append(c.html, html)
	return nil
}
