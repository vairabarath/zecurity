package plans

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/yourorg/ztna/controller/internal/billing"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// Sprint 26 P1b (D-63, D-64, D-65, D-69, D-70): plan cards, billing settings
// and the month-start plan, through the HTTP handlers and the store.

func (e *env) withBilling() *env {
	h := &provider.BillingSettingsHandler{DB: e.pool, Authz: provider.NewAuthz(), Store: provider.NewStore(e.pool)}
	e.mux.HandleFunc("GET /provider/billing/settings", h.Get)
	e.mux.HandleFunc("PATCH /provider/billing/settings", h.Update)
	return e
}

func scalarOf[T any](t *testing.T, e *env, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return v
}

var cardLimits = map[string]any{"users": 10, "resources": 100, "remote_networks": 5, "connectors": 10, "shields": 10, "client_devices": 50}

func priceUSD(base int64) map[string]any {
	return map[string]any{"currency": "USD", "base_fee_minor": base, "meters": map[string]any{
		"relay_bytes_to_clients": map[string]any{"included": 50_000_000_000, "unit": 1_000_000_000, "unit_price_minor": 200},
	}}
}

func (e *env) createCard(t *testing.T, name string, price any) string {
	t.Helper()
	body := map[string]any{"name": name, "limits": cardLimits, "reason": "new card"}
	if price != nil {
		body["price"] = price
	}
	r := e.do(t, provider.RoleSuperAdmin, http.MethodPost, "/provider/plans", body)
	if r.code != http.StatusCreated {
		t.Fatalf("create %s: %d %s", name, r.code, r.body)
	}
	var p PlanJSON
	if err := json.Unmarshal([]byte(r.body), &p); err != nil {
		t.Fatal(err)
	}
	return p.ID
}

func (e *env) revision(t *testing.T, planID string) int {
	return scalarOf[int](t, e, `SELECT revision FROM plans WHERE id = $1`, planID)
}

func (e *env) revisions(t *testing.T, planID string) int {
	return scalarOf[int](t, e, `SELECT count(*) FROM plan_card_revisions WHERE plan_id = $1`, planID)
}

func TestCards_CreateAndEdit(t *testing.T) {
	e := newEnv(t)
	priced := e.createCard(t, "Starter", priceUSD(49900))
	unpriced := e.createCard(t, "Internal", nil)

	if got := e.revision(t, priced); got != 1 || e.revisions(t, priced) != 1 {
		t.Fatalf("new card: revision %d with %d history rows, want 1 and 1", got, e.revisions(t, priced))
	}
	if n := scalarOf[int](t, e, `SELECT count(*) FROM plan_prices WHERE plan_id = $1`, unpriced); n != 0 {
		t.Fatal("unpriced card has a price row")
	}
	if n := len(e.audits(t, provider.ActionPlanCreate)); n != 2 {
		t.Fatalf("create audit rows %d, want 2", n)
	}

	edits := []struct {
		name string
		body map[string]any
	}{
		{"price only", map[string]any{"price": priceUSD(59900), "reason": "price up"}},
		{"included only", map[string]any{"price": map[string]any{"currency": "USD", "base_fee_minor": 59900, "meters": map[string]any{
			"relay_bytes_to_clients": map[string]any{"included": 100_000_000_000, "unit": 1_000_000_000, "unit_price_minor": 200}}}, "reason": "more included"}},
		{"limits and price", map[string]any{"limits": map[string]any{"users": 20, "resources": 100, "remote_networks": 5, "connectors": 10, "shields": 10, "client_devices": 50},
			"price": priceUSD(69900), "reason": "bigger"}},
		{"name only", map[string]any{"name": "Starter Plus", "reason": "rename"}},
	}
	for i, ed := range edits {
		r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, "/provider/plans/"+priced, ed.body)
		if r.code != http.StatusNoContent {
			t.Fatalf("%s: %d %s", ed.name, r.code, r.body)
		}
		if got, want := e.revision(t, priced), i+2; got != want || e.revisions(t, priced) != want {
			t.Fatalf("%s: revision %d (%d rows), want %d", ed.name, got, e.revisions(t, priced), want)
		}
		if n := len(e.audits(t, provider.ActionPlanUpdate)); n != i+1 {
			t.Fatalf("%s: %d update audit rows, want %d", ed.name, n, i+1)
		}
	}
	// The last audit row carries the reason and before/after with the price.
	last := e.audits(t, provider.ActionPlanUpdate)[3]
	if last["reason"] != "rename" || last["before"] == nil || last["after"] == nil {
		t.Fatalf("audit details %v", last)
	}

	// price omitted keeps it; an identical edit is no change (no revision).
	before := e.revision(t, priced)
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, "/provider/plans/"+priced, map[string]any{"description": "", "reason": "noop"}); r.code != http.StatusNoContent {
		t.Fatalf("noop edit: %d", r.code)
	}
	if e.revision(t, priced) != before {
		t.Fatal("an edit that changes nothing raised the revision")
	}
	if n := scalarOf[int64](t, e, `SELECT base_fee_minor FROM plan_prices WHERE plan_id = $1`, priced); n != 69900 {
		t.Fatalf("price after omitted price: %d", n)
	}
	// price null removes it: one revision, card unbillable.
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, "/provider/plans/"+priced, `{"price": null, "reason": "free now"}`); r.code != http.StatusNoContent {
		t.Fatalf("remove price: %d %s", r.code, r.body)
	}
	if n := scalarOf[int](t, e, `SELECT count(*) FROM plan_prices WHERE plan_id = $1`, priced); n != 0 || e.revision(t, priced) != before+1 {
		t.Fatalf("price removed: rows %d, revision %d", n, e.revision(t, priced))
	}
	var card billing.Card
	if err := json.Unmarshal(scalarOf[[]byte](t, e, `SELECT card FROM plan_card_revisions WHERE plan_id = $1 ORDER BY revision DESC LIMIT 1`, priced), &card); err != nil {
		t.Fatal(err)
	}
	if card.Price != nil || card.Name != "Starter Plus" {
		t.Fatalf("history card %+v", card)
	}
}

func TestCards_Refusals(t *testing.T) {
	e := newEnv(t)
	id := e.createCard(t, "Starter", priceUSD(100))
	rev := e.revision(t, id)
	cases := []struct {
		name, code string
		body       any
	}{
		{"other currency", "currency_mismatch", map[string]any{"price": map[string]any{"currency": "INR", "base_fee_minor": 1, "meters": map[string]any{}}, "reason": "r"}},
		{"unknown currency shape", "invalid_price", map[string]any{"price": map[string]any{"currency": "usd", "base_fee_minor": 1, "meters": map[string]any{}}, "reason": "r"}},
		{"fraction", "invalid_price", `{"price": {"currency": "USD", "base_fee_minor": 1.5, "meters": {}}, "reason": "r"}`},
		{"negative", "invalid_price", `{"price": {"currency": "USD", "base_fee_minor": -1, "meters": {}}, "reason": "r"}`},
		{"unknown meter", "invalid_price", `{"price": {"currency": "USD", "base_fee_minor": 1, "meters": {"bytes": {"included": 0, "unit": 1, "unit_price_minor": 1}}}, "reason": "r"}`},
		{"unit 0", "invalid_price", `{"price": {"currency": "USD", "base_fee_minor": 1, "meters": {"active_users": {"included": 0, "unit": 0, "unit_price_minor": 1}}}, "reason": "r"}`},
		{"extra field", "invalid_price", `{"price": {"currency": "USD", "base_fee_minor": 1, "meters": {}, "tax": 1}, "reason": "r"}`},
		{"overflow", "price_overflow", `{"price": {"currency": "USD", "base_fee_minor": 9999999999999999999, "meters": {}}, "reason": "r"}`},
	}
	for _, c := range cases {
		r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, "/provider/plans/"+id, c.body)
		if r.code != http.StatusBadRequest || errCode(t, r) != c.code {
			t.Errorf("%s: %d %s, want 400 %s", c.name, r.code, r.body, c.code)
		}
	}
	// A failed edit (name taken, with a new price) changes nothing.
	e.createCard(t, "Taken", nil)
	r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, "/provider/plans/"+id, map[string]any{"name": "Taken", "price": priceUSD(999), "reason": "r"})
	if r.code != http.StatusConflict {
		t.Fatalf("name taken: %d", r.code)
	}
	if e.revision(t, id) != rev || scalarOf[int64](t, e, `SELECT base_fee_minor FROM plan_prices WHERE plan_id = $1`, id) != 100 {
		t.Fatal("a failed edit changed the card")
	}
	// Create in another currency is refused too; nothing is created.
	r = e.do(t, provider.RoleSuperAdmin, http.MethodPost, "/provider/plans", map[string]any{"name": "Euro", "limits": cardLimits,
		"price": map[string]any{"currency": "EUR", "base_fee_minor": 1, "meters": map[string]any{}}})
	if r.code != http.StatusBadRequest || errCode(t, r) != "currency_mismatch" {
		t.Fatalf("create in EUR: %d %s", r.code, r.body)
	}
	if n := scalarOf[int](t, e, `SELECT count(*) FROM plans WHERE name = 'Euro'`); n != 0 {
		t.Fatal("refused create left a plan")
	}
	// Relay-ops: 403 on card writes.
	if r := e.do(t, provider.RoleRelayOps, http.MethodPatch, "/provider/plans/"+id, map[string]any{"price": nil, "reason": "r"}); r.code != http.StatusForbidden {
		t.Fatalf("relay-ops card edit: %d", r.code)
	}
}

func TestBillingSettings_API(t *testing.T) {
	e := newEnv(t).withBilling()
	r := e.do(t, provider.RoleSuperAdmin, http.MethodGet, "/provider/billing/settings", nil)
	var s struct {
		billing.Settings
		Countries []billing.Country `json:"countries"`
	}
	if r.code != http.StatusOK || json.Unmarshal([]byte(r.body), &s) != nil {
		t.Fatalf("get: %d %s", r.code, r.body)
	}
	if s.Country != "US" || s.Currency != "USD" || s.TaxRateBP != 0 || s.InvoiceRetentionYears != 8 || !s.EmailTenants || len(s.Countries) == 0 {
		t.Fatalf("defaults %+v", s)
	}
	// Relay-ops: 403 on read and write (D-70).
	for _, m := range []string{http.MethodGet, http.MethodPatch} {
		if r := e.do(t, provider.RoleRelayOps, m, "/provider/billing/settings", map[string]any{"tax_rate_bp": 1, "reason": "r"}); r.code != http.StatusForbidden {
			t.Errorf("relay-ops %s: %d", m, r.code)
		}
	}
	bad := []struct {
		name, code string
		body       any
	}{
		{"no reason", "reason_required", map[string]any{"tax_rate_bp": 1}},
		{"tax over 100%", "invalid_settings", map[string]any{"tax_rate_bp": 10001, "reason": "r"}},
		{"negative tax", "invalid_settings", map[string]any{"tax_rate_bp": -1, "reason": "r"}},
		{"label too long", "invalid_settings", map[string]any{"tax_label": "abcdefghijabcdefghijabcdefghijabc", "reason": "r"}},
		{"retention 0", "invalid_settings", map[string]any{"invoice_retention_years": 0, "reason": "r"}},
		{"retention 31", "invalid_settings", map[string]any{"invoice_retention_years": 31, "reason": "r"}},
		{"bad country", "invalid_country", map[string]any{"country": "usa", "reason": "r"}},
		{"unknown country no currency", "invalid_currency", map[string]any{"country": "ZZ", "reason": "r"}},
		{"bad currency", "invalid_currency", map[string]any{"currency": "ABC", "reason": "r"}},
	}
	for _, c := range bad {
		if r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, "/provider/billing/settings", c.body); r.code != http.StatusBadRequest || errCode(t, r) != c.code {
			t.Errorf("%s: %d %s, want 400 %s", c.name, r.code, r.body, c.code)
		}
	}
	// Tax 18 % GST, audited with reason and before/after.
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, "/provider/billing/settings", map[string]any{"tax_rate_bp": 1800, "tax_label": "GST", "reason": "GST registered"}); r.code != http.StatusOK {
		t.Fatalf("tax: %d %s", r.code, r.body)
	}
	a := e.audits(t, provider.ActionBillingSettingsUpdate)
	if len(a) != 1 || a[0]["reason"] != "GST registered" || a[0]["before"].(map[string]any)["tax_rate_bp"] != float64(0) || a[0]["after"].(map[string]any)["tax_label"] != "GST" {
		t.Fatalf("settings audit %v", a)
	}
	// An unknown country with an explicit currency is fine.
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, "/provider/billing/settings", map[string]any{"country": "ZZ", "currency": "USD", "reason": "r"}); r.code != http.StatusOK {
		t.Fatalf("unknown country with currency: %d %s", r.code, r.body)
	}
	// Unchanged patch: no audit row.
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, "/provider/billing/settings", map[string]any{"tax_rate_bp": 1800, "reason": "r"}); r.code != http.StatusOK {
		t.Fatalf("noop: %d", r.code)
	}
	if n := len(e.audits(t, provider.ActionBillingSettingsUpdate)); n != 2 {
		t.Fatalf("audit rows %d, want 2 (noop not audited)", n)
	}
}

// D-64: choosing India fills in INR and switches every priced card, each with
// a revision; unpriced cards are untouched; amounts are not converted.
func TestBillingSettings_CurrencySwitch(t *testing.T) {
	e := newEnv(t).withBilling()
	a := e.createCard(t, "A", priceUSD(1000))
	b := e.createCard(t, "B", priceUSD(2000))
	c := e.createCard(t, "Unpriced", nil)

	r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, "/provider/billing/settings", map[string]any{"country": "IN", "reason": "moved to India"})
	if r.code != http.StatusOK {
		t.Fatalf("switch: %d %s", r.code, r.body)
	}
	var out struct {
		Currency      string               `json:"currency"`
		SwitchedCards []billing.CardSwitch `json:"switched_cards"`
	}
	_ = json.Unmarshal([]byte(r.body), &out)
	if out.Currency != "INR" || len(out.SwitchedCards) != 2 {
		t.Fatalf("switch response %+v", out)
	}
	for id, base := range map[string]int64{a: 1000, b: 2000} {
		var (
			cur string
			fee int64
		)
		if err := e.pool.QueryRow(context.Background(), `SELECT currency, base_fee_minor FROM plan_prices WHERE plan_id = $1`, id).Scan(&cur, &fee); err != nil {
			t.Fatal(err)
		}
		if cur != "INR" || fee != base || e.revision(t, id) != 2 {
			t.Errorf("card %s: %s %d rev %d, want INR %d rev 2", id, cur, fee, e.revision(t, id), base)
		}
		if reason := scalarOf[string](t, e, `SELECT reason FROM plan_card_revisions WHERE plan_id = $1 AND revision = 2`, id); reason != "moved to India" {
			t.Errorf("revision reason %q", reason)
		}
	}
	if e.revision(t, c) != 1 {
		t.Error("unpriced card got a revision")
	}
	au := e.audits(t, provider.ActionBillingSettingsUpdate)
	if len(au) != 1 || len(au[0]["switched_cards"].([]any)) != 2 {
		t.Fatalf("audit %v", au)
	}
	// A USD price is now refused; INR accepted.
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, "/provider/plans/"+a, map[string]any{"price": priceUSD(1), "reason": "r"}); errCode(t, r) != "currency_mismatch" {
		t.Fatalf("USD after switch: %d %s", r.code, r.body)
	}
}

// A currency switch that fails half way leaves settings and every card as
// they were (one transaction).
func TestBillingSettings_CurrencySwitchAtomic(t *testing.T) {
	e := newEnv(t).withBilling()
	a := e.createCard(t, "A", priceUSD(1000))
	b := e.createCard(t, "B", priceUSD(2000))
	// Inject a failure on the second card's switch.
	for _, sql := range []string{
		`CREATE FUNCTION fail_switch() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		   IF NEW.plan_id = '` + b + `' THEN RAISE EXCEPTION 'injected'; END IF; RETURN NEW; END $$`,
		`CREATE TRIGGER fail_switch BEFORE UPDATE ON plan_prices FOR EACH ROW EXECUTE FUNCTION fail_switch()`,
	} {
		if _, err := e.pool.Exec(context.Background(), sql); err != nil {
			t.Fatal(err)
		}
	}
	if r := e.do(t, provider.RoleSuperAdmin, http.MethodPatch, "/provider/billing/settings", map[string]any{"currency": "EUR", "reason": "r"}); r.code != http.StatusInternalServerError {
		t.Fatalf("injected failure: %d %s", r.code, r.body)
	}
	if cur := scalarOf[string](t, e, `SELECT currency FROM billing_settings`); cur != "USD" {
		t.Fatalf("settings currency %s after failed switch", cur)
	}
	for _, id := range []string{a, b} {
		if cur := scalarOf[string](t, e, `SELECT currency FROM plan_prices WHERE plan_id = $1`, id); cur != "USD" || e.revision(t, id) != 1 {
			t.Errorf("card %s: %s rev %d after failed switch", id, cur, e.revision(t, id))
		}
	}
	if n := len(e.audits(t, provider.ActionBillingSettingsUpdate)); n != 0 {
		t.Fatalf("failed switch audited: %d", n)
	}
}

// D-65: the first plan change in a month records the plan it replaces; later
// changes in the month keep it; the next month records its own start plan.
func TestMonthStartPlan(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	a := e.createCard(t, "A", priceUSD(1000))
	b := e.createCard(t, "B", priceUSD(2000))
	c := e.createCard(t, "C", nil)
	ws := e.workspace(t, "acme")
	if _, err := e.pool.Exec(ctx, `UPDATE workspaces SET plan_id = $2 WHERE id = $1`, ws, a); err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	e.store.Now = func() time.Time { return clock }
	nov := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	dec := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	oct := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	plan := func(m time.Time) billing.BillingPlan {
		t.Helper()
		bp, err := billing.PlanForMonth(ctx, e.pool, ws, m)
		if err != nil {
			t.Fatal(err)
		}
		return bp
	}
	// Never changed: the current plan.
	if bp := plan(nov); bp.PlanID != a || bp.Source != "current" {
		t.Fatalf("unchanged: %+v", bp)
	}
	assign := func(p string) {
		t.Helper()
		if _, err := e.store.Assign(ctx, ws, p, nil); err != nil {
			t.Fatal(err)
		}
	}
	assign(b) // Nov 10: A → B
	clock = clock.Add(5 * 24 * time.Hour)
	assign(c) // Nov 15: B → C
	if bp := plan(nov); bp.PlanID != a || bp.Source != "recorded" {
		t.Fatalf("November after two changes: %+v, want A recorded", bp)
	}
	if e.planOf(t, ws) != c {
		t.Fatal("limits plan didn't change at once")
	}
	// October (no change) regenerated after the November change: still A.
	if bp := plan(oct); bp.PlanID != a || bp.Source != "later_change" {
		t.Fatalf("October: %+v, want A from the later change", bp)
	}
	// December, no change yet: the current plan C.
	if bp := plan(dec); bp.PlanID != c {
		t.Fatalf("December: %+v, want C", bp)
	}
	clock = time.Date(2026, 12, 3, 0, 0, 0, 0, time.UTC)
	assign(a) // Dec 3: C → A
	if bp := plan(dec); bp.PlanID != c || bp.Source != "recorded" {
		t.Fatalf("December after change: %+v, want C", bp)
	}
	if bp := plan(nov); bp.PlanID != a {
		t.Fatalf("November unchanged by the December change: %+v", bp)
	}
	// Assigning the same plan again records nothing.
	if n := scalarOf[int](t, e, `SELECT count(*) FROM tenant_billing_plans WHERE tenant_id = $1`, ws); n != 2 {
		t.Fatalf("month rows %d, want 2", n)
	}

	// A purged tenant: the frozen snapshot plan.
	gone := "cccccccc-0000-0000-0000-0000000000c1"
	if _, err := e.pool.Exec(ctx, `INSERT INTO usage_tenants (tenant_id, name, slug, plan_name, plan_id, purged_at) VALUES ($1, 'gone', 'gone', 'B', $2, now())`, gone, b); err != nil {
		t.Fatal(err)
	}
	if bp := plan2(t, e, gone, nov); bp.PlanID != b || bp.Source != "frozen" {
		t.Fatalf("purged: %+v", bp)
	}
	if _, err := billing.PlanForMonth(ctx, e.pool, "cccccccc-0000-0000-0000-0000000000c2", nov); err != billing.ErrNoBillingPlan {
		t.Fatalf("unknown tenant: %v", err)
	}
}

func plan2(t *testing.T, e *env, ws string, m time.Time) billing.BillingPlan {
	t.Helper()
	bp, err := billing.PlanForMonth(context.Background(), e.pool, ws, m)
	if err != nil {
		t.Fatal(err)
	}
	return bp
}

// D-63 (changed 2026-11-24): the revision in force at the start of the month
// prices it. Starter ₹499 / 50 GB / ₹2 edited on the 15th to ₹599 / 100 GB /
// ₹1 → that month uses the old terms, the next month the new ones; a card
// created during a month uses its first revision.
func TestCardForMonth(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	plan := "dddddddd-0000-0000-0000-0000000000d1"
	card := func(fee, included, price int64) string {
		c, _ := json.Marshal(billing.Card{Name: "Starter", Limits: json.RawMessage(`{}`), Price: &billing.Price{Currency: "INR", BaseFeeMinor: fee,
			Meters: map[string]billing.MeterPrice{"relay_bytes_to_clients": {Included: included, Unit: 1_000_000_000, UnitPriceMinor: price}}}})
		return string(c)
	}
	for _, r := range []struct {
		rev  int
		at   string
		card string
	}{
		{1, "2026-10-20T09:00:00Z", card(49900, 50_000_000_000, 200)},
		{2, "2026-11-15T09:00:00Z", card(59900, 100_000_000_000, 100)},
	} {
		if _, err := e.pool.Exec(ctx, `INSERT INTO plan_card_revisions (plan_id, revision, card, changed_at, changed_by, reason) VALUES ($1, $2, $3, $4, 'a', 'r')`,
			plan, r.rev, r.card, r.at); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		month   string
		rev     int
		baseFee int64
	}{
		{"2026-10-01", 1, 49900}, // created during October → first revision
		{"2026-11-01", 1, 49900}, // edited on Nov 15 → November still the old terms
		{"2026-12-01", 2, 59900}, // December the new terms
	} {
		m, _ := time.Parse("2006-01-02", c.month)
		got, err := billing.CardForMonth(ctx, e.pool, plan, m)
		if err != nil {
			t.Fatal(err)
		}
		if got.Revision != c.rev || got.Card.Price.BaseFeeMinor != c.baseFee {
			t.Errorf("%s: revision %d fee %d, want %d %d", c.month, got.Revision, got.Card.Price.BaseFeeMinor, c.rev, c.baseFee)
		}
	}
}
