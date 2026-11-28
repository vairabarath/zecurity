package billing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yourorg/ztna/controller/internal/providerquery"
)

// Sprint 26 P2 (D-63 … D-67, D-69): invoice arithmetic, month close and the
// lifecycle.

// ── Compute (pure) ──────────────────────────────────────────────────────────

func TestCompute(t *testing.T) {
	gb := int64(1_000_000_000)
	price := Price{Currency: "INR", BaseFeeMinor: 49900, Meters: map[string]MeterPrice{
		"relay_bytes_to_clients": {Included: 50 * gb, Unit: gb, UnitPriceMinor: 200},
		"active_users":           {Included: 5, Unit: 1, UnitPriceMinor: 300},
	}}
	q := func(bytes, users int64) func(string) int64 {
		return func(m string) int64 {
			switch m {
			case "relay_bytes_to_clients":
				return bytes
			case "active_users":
				return users
			}
			return 0
		}
	}
	cases := []struct {
		name                  string
		bytes, users          int64
		tax                   int
		bytesUnits, userUnits int64
		subtotal, taxMinor    int64
	}{
		{"included not reached", 10 * gb, 3, 0, 0, 0, 49900, 0},
		{"exactly reached", 50 * gb, 5, 0, 0, 0, 49900, 0},
		{"one byte over", 50*gb + 1, 6, 0, 1, 1, 49900 + 200 + 300, 0},
		{"tax 18%", 52 * gb, 5, 1800, 2, 0, 50300, 9054},
	}
	for _, c := range cases {
		got, err := Compute(price, q(c.bytes, c.users), c.tax)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(got.Lines) != 3 || got.Lines[0].Kind != "base" || got.Lines[1].Meter != "relay_bytes_to_clients" || got.Lines[2].Meter != "active_users" {
			t.Fatalf("%s: lines not in D-57 order: %+v", c.name, got.Lines)
		}
		if *got.Lines[1].BillableUnits != c.bytesUnits || *got.Lines[2].BillableUnits != c.userUnits ||
			got.Subtotal != c.subtotal || got.Tax != c.taxMinor || got.Total != c.subtotal+c.taxMinor {
			t.Errorf("%s: units %d/%d subtotal %d tax %d total %d", c.name, *got.Lines[1].BillableUnits, *got.Lines[2].BillableUnits, got.Subtotal, got.Tax, got.Total)
		}
	}
	// Tax half up at the boundary: 50 × 1000 bp = 5 exactly; 5 × 1000 = 0.5 → 1;
	// 4 × 1000 = 0.4 → 0; 15 × 1000 = 1.5 → 2.
	for sub, want := range map[int64]int64{50: 5, 5: 1, 4: 0, 15: 2, 25: 3} {
		if got, _ := Tax(sub, 1000); got != want {
			t.Errorf("Tax(%d, 10%%) = %d, want %d", sub, got, want)
		}
	}
	// Empty meters: the base fee only.
	got, err := Compute(Price{Currency: "USD", BaseFeeMinor: 1000, Meters: map[string]MeterPrice{}}, q(1, 1), 0)
	if err != nil || len(got.Lines) != 1 || got.Total != 1000 {
		t.Fatalf("base only: %+v %v", got, err)
	}
	// Near int64 max: an error, never a wrapped amount.
	big := Price{Currency: "USD", BaseFeeMinor: math.MaxInt64 - 10, Meters: map[string]MeterPrice{"active_users": {Included: 0, Unit: 1, UnitPriceMinor: 100}}}
	if _, err := Compute(big, q(0, 1), 0); !errors.Is(err, ErrOverflow) {
		t.Fatalf("overflow in sum: %v", err)
	}
	if _, err := Compute(Price{BaseFeeMinor: 0, Meters: map[string]MeterPrice{"active_users": {Included: 0, Unit: 1, UnitPriceMinor: math.MaxInt64 / 2}}}, q(0, 3), 0); !errors.Is(err, ErrOverflow) {
		t.Fatalf("overflow in units × price: %v", err)
	}
	if _, err := Tax(math.MaxInt64/2, 10000); !errors.Is(err, ErrOverflow) {
		t.Fatalf("overflow in tax: %v", err)
	}
}

// ── DB fixtures ─────────────────────────────────────────────────────────────

var (
	oct     = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	nov     = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	afterOk = nov.Add(DefaultSettleDelay) // Nov 1 06:00 UTC
)

type billingFixture struct {
	t  *testing.T
	db *pgxpool.Pool
}

func newBillingFixture(t *testing.T) *billingFixture {
	return &billingFixture{t: t, db: schemaTestDB(t)}
}

func (f *billingFixture) exec(sql string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(context.Background(), sql, args...); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
}

func (f *billingFixture) scalarInt(sql string, args ...any) int64 {
	f.t.Helper()
	var v int64
	if err := f.db.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		f.t.Fatalf("%s: %v", sql, err)
	}
	return v
}

// card makes a plan whose card revision 1 (in force from changedAt) has price
// (nil = unpriced).
func (f *billingFixture) card(name string, price *Price, changedAt time.Time) string {
	f.t.Helper()
	var id string
	limits := `{"users": null, "resources": null, "remote_networks": null, "connectors": null, "shields": null, "client_devices": null}`
	if err := f.db.QueryRow(context.Background(), `INSERT INTO plans (name, limits) VALUES ($1, $2) RETURNING id::text`, name, limits).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	f.revise(id, 1, name, price, changedAt)
	if price != nil {
		meters, _ := json.Marshal(price.Meters)
		f.exec(`INSERT INTO plan_prices (plan_id, currency, base_fee_minor, meters, updated_by) VALUES ($1, $2, $3, $4, 'a')`, id, price.Currency, price.BaseFeeMinor, meters)
	}
	return id
}

func (f *billingFixture) revise(planID string, rev int, name string, price *Price, at time.Time) {
	f.t.Helper()
	c, _ := json.Marshal(Card{Name: name, Limits: json.RawMessage(`{}`), Price: price})
	f.exec(`INSERT INTO plan_card_revisions (plan_id, revision, card, changed_at, changed_by, reason) VALUES ($1, $2, $3, $4, 'a', 'r')`, planID, rev, c, at)
}

func (f *billingFixture) tenant(slug, planID string, created time.Time) string {
	f.t.Helper()
	var id string
	if err := f.db.QueryRow(context.Background(), `INSERT INTO workspaces (slug, name, status, trust_domain, plan_id, created_at)
		VALUES ($1, $1 || ' Inc', 'active', $1 || '.zecurity.test', $2, $3) RETURNING id::text`, slug, planID, created).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *billingFixture) usage(tenant, meter string, hour time.Time, q int64) {
	f.exec(`INSERT INTO usage_hourly VALUES ($1, $2, $3, $4)`, tenant, meter, hour, q)
}

// collectAll marks every minute of the month collected except the skipped
// half-open ranges.
func (f *billingFixture) collectAll(month time.Time, skip ...[2]time.Time) {
	f.exec(`INSERT INTO usage_collector_minutes SELECT generate_series($1::timestamptz, $2::timestamptz - interval '1 minute', interval '1 minute')`,
		month, month.AddDate(0, 1, 0))
	for _, s := range skip {
		f.exec(`DELETE FROM usage_collector_minutes WHERE minute >= $1 AND minute < $2`, s[0], s[1])
	}
}

func (f *billingFixture) gen(now time.Time) *Generator {
	return &Generator{DB: f.db, Now: func() time.Time { return now }}
}

func (f *billingFixture) invoices(tenant string) []Invoice {
	f.t.Helper()
	rows, err := f.db.Query(context.Background(), `SELECT id::text FROM invoices WHERE tenant_id = $1 ORDER BY generated_at`, tenant)
	if err != nil {
		f.t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var out []Invoice
	for _, id := range ids {
		inv, err := GetInvoice(context.Background(), f.db, id)
		if err != nil {
			f.t.Fatal(err)
		}
		out = append(out, inv)
	}
	return out
}

func (f *billingFixture) tx(fn func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(context.Background(), f.db, fn)
}

var gb = int64(1_000_000_000)

func inr(base int64, included int64, perGB int64) *Price {
	return &Price{Currency: "INR", BaseFeeMinor: base, Meters: map[string]MeterPrice{
		"relay_bytes_to_clients": {Included: included * gb, Unit: gb, UnitPriceMinor: perGB},
	}}
}

// ── Month close ─────────────────────────────────────────────────────────────

func TestMonthClose_DraftsFromUsage(t *testing.T) {
	f := newBillingFixture(t)
	ctx := context.Background()
	starter := f.card("Starter", inr(49900, 50, 200), oct.AddDate(0, -1, 0))
	free := f.card("Free card", nil, oct.AddDate(0, -1, 0))
	a := f.tenant("alpha", starter, oct)
	b := f.tenant("bravo", free, oct)
	idle := f.tenant("idle", starter, oct)
	late := f.tenant("late", starter, nov.Add(time.Hour)) // created after October
	f.usage(a, "relay_bytes_to_clients", oct.Add(10*time.Hour), 52*gb+1)
	f.usage(b, "relay_bytes_to_clients", oct.Add(10*time.Hour), 99*gb)
	// The controller was stopped 5 minutes on Oct 15 (one incomplete hour).
	stop := oct.AddDate(0, 0, 14).Add(12 * time.Hour)
	f.collectAll(oct, [2]time.Time{stop, stop.Add(5 * time.Minute)})

	// Before the settling delay: nothing.
	if n, err := f.gen(afterOk.Add(-time.Second)).CloseDueMonths(ctx); err != nil || n != 0 {
		t.Fatalf("before settle delay: %d %v", n, err)
	}
	n, err := f.gen(afterOk).CloseDueMonths(ctx)
	if err != nil || n != 2 {
		t.Fatalf("month close: %d %v, want 2 (alpha, idle)", n, err)
	}
	inv := f.invoices(a)
	if len(inv) != 1 {
		t.Fatalf("alpha invoices %d", len(inv))
	}
	got := inv[0]
	// 52 GB + 1 byte over 50 GB included → 3 GB billed at ₹2.
	if got.Status != StatusDraft || got.Currency != "INR" || got.SubtotalMinor != 49900+600 || got.TaxMinor != 0 ||
		got.TotalMinor != 50500 || got.PlanName != "Starter" || got.PlanRevision != 1 || got.TenantName != "alpha Inc" {
		t.Fatalf("alpha draft %+v", got)
	}
	if got.IncompleteHours != 1 || got.CollectedMinutes != 44640-5 || got.MonthMinutes != 44640 {
		t.Fatalf("completeness: %d hours, %d/%d minutes", got.IncompleteHours, got.CollectedMinutes, got.MonthMinutes)
	}
	if len(got.Lines) != 2 || *got.Lines[1].BillableUnits != 3 {
		t.Fatalf("lines %+v", got.Lines)
	}
	// A priced tenant with no usage pays the base fee.
	if inv := f.invoices(idle); len(inv) != 1 || inv[0].TotalMinor != 49900 {
		t.Fatalf("idle tenant %+v", inv)
	}
	// Unpriced card: no invoice (D-62). Created after the month: none.
	if len(f.invoices(b)) != 0 || len(f.invoices(late)) != 0 {
		t.Fatal("unpriced or late tenant invoiced")
	}
	// A second run creates nothing.
	if n, err := f.gen(afterOk.Add(time.Hour)).CloseDueMonths(ctx); err != nil || n != 0 {
		t.Fatalf("second run: %d %v", n, err)
	}
	if c := f.scalarInt(`SELECT count(*) FROM invoices`); c != 2 {
		t.Fatalf("invoices after two runs: %d", c)
	}
}

func TestMonthClose_CatchUpWindowAndBoundaries(t *testing.T) {
	g := (&billingFixture{}).gen(time.Date(2027, 1, 1, 6, 0, 0, 0, time.UTC))
	got := g.DueMonths()
	want := []time.Time{time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("due months %v, want %v", got, want)
	}
	// 05:59 on Jan 1: December still settling.
	g.Now = func() time.Time { return time.Date(2027, 1, 1, 5, 59, 0, 0, time.UTC) }
	if got := g.DueMonths(); len(got) != 2 || got[1].Month() != time.November {
		t.Fatalf("before settle: %v", got)
	}
	// A short settle delay (the lab) and a time zone off UTC change nothing
	// about the UTC boundary.
	g.SettleDelay = time.Minute
	g.Now = func() time.Time { return time.Date(2027, 1, 1, 5, 31, 0, 0, time.FixedZone("IST", 5*3600+1800)) } // 00:01 UTC
	if got := g.DueMonths(); len(got) != 3 {
		t.Fatalf("short delay: %v", got)
	}
}

func TestMonthClose_MissedMonthOnStart(t *testing.T) {
	f := newBillingFixture(t)
	p := f.card("Starter", inr(100, 0, 1), oct.AddDate(0, -3, 0))
	a := f.tenant("alpha", p, oct.AddDate(0, -3, 0))
	// The controller was down from September to Dec 10: September, October
	// and November are generated on start; August is outside the window.
	n, err := f.gen(time.Date(2026, 12, 10, 0, 0, 0, 0, time.UTC)).CloseDueMonths(context.Background())
	if err != nil || n != 3 {
		t.Fatalf("catch up: %d %v", n, err)
	}
	months := map[string]bool{}
	for _, i := range f.invoices(a) {
		months[i.Month.Format("2006-01")] = true
	}
	if !months["2026-09"] || !months["2026-10"] || !months["2026-11"] || months["2026-08"] {
		t.Fatalf("months %v", months)
	}
}

// D-65 / D-63: a mid-month plan change bills the month-start plan; a card
// edited mid-month bills the revision in force on the 1st; the next month
// uses the new plan and terms.
func TestMonthClose_MonthStartPlanAndCard(t *testing.T) {
	f := newBillingFixture(t)
	ctx := context.Background()
	cardA := f.card("A", inr(49900, 50, 200), oct.AddDate(0, -1, 0))
	cardB := f.card("B", inr(99900, 0, 0), oct.AddDate(0, -1, 0))
	ws := f.tenant("alpha", cardA, oct)
	// Oct 15: card A edited (applies from November), tenant moved to B.
	f.revise(cardA, 2, "A", inr(59900, 100, 100), oct.AddDate(0, 0, 14))
	err := f.tx(func(tx pgx.Tx) error {
		if err := RecordMonthStartPlan(ctx, tx, ws, cardA, oct.AddDate(0, 0, 14)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE workspaces SET plan_id = $2 WHERE id = $1`, ws, cardB)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.gen(afterOk).CloseDueMonths(ctx); err != nil {
		t.Fatal(err)
	}
	inv := f.invoices(ws)
	if len(inv) != 1 || inv[0].PlanID != cardA || inv[0].PlanRevision != 1 || inv[0].TotalMinor != 49900 {
		t.Fatalf("October %+v, want card A revision 1 at 49900", inv)
	}
	// November: card B (the plan since Oct 15).
	dec := time.Date(2026, 12, 1, 6, 0, 0, 0, time.UTC)
	if _, err := f.gen(dec).CloseDueMonths(ctx); err != nil {
		t.Fatal(err)
	}
	all := f.invoices(ws)
	if len(all) != 2 || all[1].PlanID != cardB || all[1].TotalMinor != 99900 {
		t.Fatalf("November %+v, want card B", all)
	}
}

// Quantities come only through ReadMonthUsage (D-59): a replaced reader is
// what the invoice bills.
func TestMonthClose_ReadsOnlyThroughU2(t *testing.T) {
	f := newBillingFixture(t)
	p := f.card("Starter", inr(0, 0, 1), oct.AddDate(0, -1, 0))
	a := f.tenant("alpha", p, oct)
	f.usage(a, "relay_bytes_to_clients", oct.Add(time.Hour), 5*gb) // the DB says 5 GB
	g := f.gen(afterOk)
	calls := 0 // one read per due month (Aug, Sep, Oct)
	g.read = func(ctx context.Context, q providerquery.Querier, m time.Time) (providerquery.MonthUsage, error) {
		calls++
		u, err := providerquery.ReadMonthUsage(ctx, q, m)
		for i := range u.Tenants {
			if u.Tenants[i].Tenant.ID == a {
				u.Tenants[i].Meters["relay_bytes_to_clients"] = 7 * gb // the reader says 7
			}
		}
		return u, err
	}
	if _, err := g.CloseDueMonths(context.Background()); err != nil {
		t.Fatal(err)
	}
	if inv := f.invoices(a); calls != 3 || len(inv) != 1 || inv[0].SubtotalMinor != 7 {
		t.Fatalf("calls %d, invoice %+v: quantities not from the U2 reader", calls, inv)
	}
}

// Card, currency and tax changes reach drafts on regenerate, never an issued
// invoice. Regenerate twice → still one invoice; after a void it creates a
// new draft.
func TestRegenerateAndLifecycle(t *testing.T) {
	f := newBillingFixture(t)
	ctx := context.Background()
	p := f.card("Starter", inr(10000, 0, 0), oct.AddDate(0, -1, 0))
	a := f.tenant("alpha", p, oct)
	b := f.tenant("bravo", p, oct)
	g := f.gen(afterOk)
	if n, err := g.CloseDueMonths(ctx); err != nil || n != 2 {
		t.Fatalf("close: %d %v", n, err)
	}
	draftID := f.invoices(a)[0].ID

	// Tax set after the draft: used on regenerate (same id).
	f.exec(`UPDATE billing_settings SET tax_rate_bp = 1800, tax_label = 'GST'`)
	for i := 0; i < 2; i++ {
		res, err := g.Regenerate(ctx, oct, "")
		if err != nil || res.Regenerated != 2 || res.Created != 0 {
			t.Fatalf("regenerate %d: %+v %v", i, res, err)
		}
	}
	inv := f.invoices(a)
	if len(inv) != 1 || inv[0].ID != draftID || inv[0].TaxMinor != 1800 || inv[0].TaxLabel != "GST" || inv[0].TotalMinor != 11800 {
		t.Fatalf("after regenerate %+v", inv)
	}
	// Regenerating the open month is refused.
	if _, err := g.Regenerate(ctx, nov, ""); !errors.Is(err, ErrMonthOpen) {
		t.Fatalf("regenerate open month: %v", err)
	}

	// Issue: number, email row (setting on), SQL UPDATE refused.
	var issued Invoice
	if err := f.tx(func(tx pgx.Tx) error {
		_, after, err := Issue(ctx, tx, draftID, "admin@p.test", afterOk)
		issued = after
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if issued.Status != StatusIssued || issued.Number == nil || *issued.Number != "INV-2026-000001" {
		t.Fatalf("issued %+v", issued)
	}
	if n := f.scalarInt(`SELECT count(*) FROM invoice_emails WHERE invoice_id = $1 AND kind = 'issued'`, draftID); n != 1 {
		t.Fatalf("issued email rows %d", n)
	}
	if _, err := f.db.Exec(ctx, `UPDATE invoices SET total_minor = 1, subtotal_minor = 1, tax_minor = 0 WHERE id = $1`, draftID); err == nil {
		t.Fatal("SQL UPDATE of an issued invoice accepted")
	}
	// Issuing again: not a draft.
	if err := f.tx(func(tx pgx.Tx) error { _, _, err := Issue(ctx, tx, draftID, "a", afterOk); return err }); !errors.Is(err, ErrNotDraft) {
		t.Fatalf("issue twice: %v", err)
	}
	// A card edit, a currency switch and a tax change after issue: the issued
	// invoice is unchanged, even after regenerate.
	f.revise(p, 2, "Starter", &Price{Currency: "USD", BaseFeeMinor: 1, Meters: map[string]MeterPrice{}}, oct.AddDate(0, 0, -20))
	f.exec(`UPDATE billing_settings SET tax_rate_bp = 0, currency = 'USD'`)
	if _, err := g.Regenerate(ctx, oct, ""); err != nil {
		t.Fatal(err)
	}
	if now := f.invoices(a); now[0].TotalMinor != 11800 || now[0].Currency != "INR" || now[0].PlanRevision != 1 {
		t.Fatalf("issued invoice changed: %+v", now[0])
	}
	// ...while bravo's draft took the month-start revision (2, edited before
	// October) and the new tax.
	if bv := f.invoices(b); bv[0].PlanRevision != 2 || bv[0].Currency != "USD" || bv[0].TotalMinor != 1 {
		t.Fatalf("bravo draft %+v", bv[0])
	}

	// Void with a reason → void notice; regenerate creates a new draft.
	if err := f.tx(func(tx pgx.Tx) error { _, _, err := Void(ctx, tx, draftID, " ", "a", afterOk); return err }); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("void without reason: %v", err)
	}
	if err := f.tx(func(tx pgx.Tx) error { _, _, err := Void(ctx, tx, draftID, "wrong tax", "a", afterOk); return err }); err != nil {
		t.Fatal(err)
	}
	if n := f.scalarInt(`SELECT count(*) FROM invoice_emails WHERE invoice_id = $1 AND kind = 'voided'`, draftID); n != 1 {
		t.Fatalf("void notice rows %d", n)
	}
	res, err := g.Regenerate(ctx, oct, a)
	if err != nil || res.Created != 1 {
		t.Fatalf("regenerate after void: %+v %v", res, err)
	}
	if inv := f.invoices(a); len(inv) != 2 || inv[1].Status != StatusDraft {
		t.Fatalf("after void + regenerate %+v", inv)
	}
	// Resend: issued/paid/void only, a new row each time; a draft refused.
	for i := 1; i <= 2; i++ {
		if err := f.tx(func(tx pgx.Tx) error { _, err := Resend(ctx, tx, draftID); return err }); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.scalarInt(`SELECT max(seq) FROM invoice_emails WHERE invoice_id = $1 AND kind = 'resend'`, draftID); n != 2 {
		t.Fatalf("resend seq %d", n)
	}
	if err := f.tx(func(tx pgx.Tx) error { _, err := Resend(ctx, tx, f.invoices(a)[1].ID); return err }); !errors.Is(err, ErrNotSent) {
		t.Fatalf("resend a draft: %v", err)
	}

	// Mark paid: bravo issued then paid; dates checked; paid → void refused.
	bID := f.invoices(b)[0].ID
	if err := f.tx(func(tx pgx.Tx) error { _, _, err := Issue(ctx, tx, bID, "a", afterOk); return err }); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		at   time.Time
		ref  string
		want error
	}{
		{afterOk.Add(48 * time.Hour), "", ErrInvalidPaidAt}, // future
		{oct.AddDate(0, 0, -1), "", ErrInvalidPaidAt},       // before the month
		{afterOk, string(make([]byte, 201)), ErrReferenceTooLong},
	} {
		if err := f.tx(func(tx pgx.Tx) error { _, _, err := MarkPaid(ctx, tx, bID, c.at, c.ref, "a", afterOk); return err }); !errors.Is(err, c.want) {
			t.Errorf("mark paid %v: %v, want %v", c.at, err, c.want)
		}
	}
	if err := f.tx(func(tx pgx.Tx) error {
		_, _, err := MarkPaid(ctx, tx, bID, afterOk, "bank 42", "a", afterOk)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.tx(func(tx pgx.Tx) error { _, _, err := Void(ctx, tx, bID, "r", "a", afterOk); return err }); !errors.Is(err, ErrNotIssued) {
		t.Fatalf("void a paid invoice: %v", err)
	}
	// Issuing in the open month is refused.
	c := f.card("Now", inr(1, 0, 0), oct.AddDate(0, -1, 0))
	cur := f.tenant("current", c, oct)
	var openID string
	if err := f.tx(func(tx pgx.Tx) error {
		id, _, err := insertDraftInvoice(ctx, tx, Invoice{TenantID: cur, TenantName: "c", TenantSlug: "c", Month: nov, Currency: "INR", PlanID: c,
			PlanName: "Now", PlanRevision: 1, MonthMinutes: 43200, Lines: []Line{{Kind: "base"}}})
		openID = id
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.tx(func(tx pgx.Tx) error { _, _, err := Issue(ctx, tx, openID, "a", afterOk); return err }); !errors.Is(err, ErrMonthOpen) {
		t.Fatalf("issue the open month: %v", err)
	}
}

// Email off: issue and void enqueue nothing.
func TestIssue_EmailOff(t *testing.T) {
	f := newBillingFixture(t)
	ctx := context.Background()
	p := f.card("Starter", inr(100, 0, 0), oct.AddDate(0, -1, 0))
	a := f.tenant("alpha", p, oct)
	if _, err := f.gen(afterOk).CloseDueMonths(ctx); err != nil {
		t.Fatal(err)
	}
	f.exec(`UPDATE billing_settings SET email_tenants = false`)
	id := f.invoices(a)[0].ID
	if err := f.tx(func(tx pgx.Tx) error {
		if _, _, err := Issue(ctx, tx, id, "a", afterOk); err != nil {
			return err
		}
		_, _, err := Void(ctx, tx, id, "test", "a", afterOk)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := f.scalarInt(`SELECT count(*) FROM invoice_emails`); n != 0 {
		t.Fatalf("email rows with email off: %d", n)
	}
}

// 20 concurrent issues get 20 gap-free numbers.
func TestIssue_ConcurrentGapFree(t *testing.T) {
	f := newBillingFixture(t)
	ctx := context.Background()
	p := f.card("Starter", inr(100, 0, 0), oct.AddDate(0, -1, 0))
	for i := 0; i < 20; i++ {
		f.tenant(fmt.Sprintf("t%02d", i), p, oct)
	}
	if n, err := f.gen(afterOk).CloseDueMonths(ctx); err != nil || n != 20 {
		t.Fatalf("close: %d %v", n, err)
	}
	rows, _ := f.db.Query(ctx, `SELECT id::text FROM invoices`)
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	var wg sync.WaitGroup
	errs := make(chan error, len(ids))
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			errs <- f.tx(func(tx pgx.Tx) error { _, _, err := Issue(ctx, tx, id, "a", afterOk); return err })
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, _ = f.db.Query(ctx, `SELECT number FROM invoices ORDER BY number`)
	var got []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		got = append(got, n)
	}
	rows.Close()
	sort.Strings(got)
	for i, n := range got {
		if want := InvoiceNumber(2026, int64(i+1)); n != want {
			t.Fatalf("number %d = %s, want %s (all: %v)", i, n, want, got)
		}
	}
	// A rolled-back issue releases its number: the next one has no gap.
	c := f.card("Late", inr(1, 0, 0), oct.AddDate(0, -1, 0))
	late := f.tenant("late", c, oct)
	if _, err := f.gen(afterOk).Regenerate(ctx, oct, late); err != nil {
		t.Fatal(err)
	}
	id := f.invoices(late)[0].ID
	_ = f.tx(func(tx pgx.Tx) error {
		_, _, _ = Issue(ctx, tx, id, "a", afterOk)
		return errors.New("roll back")
	})
	if err := f.tx(func(tx pgx.Tx) error { _, _, err := Issue(ctx, tx, id, "a", afterOk); return err }); err != nil {
		t.Fatal(err)
	}
	if inv := f.invoices(late); *inv[0].Number != "INV-2026-000021" {
		t.Fatalf("after a rolled-back issue: %s", *inv[0].Number)
	}
}

// A purged tenant's final month is invoiced under its kept name, even with
// no usage; its invoices survive.
func TestMonthClose_PurgedTenant(t *testing.T) {
	f := newBillingFixture(t)
	ctx := context.Background()
	p := f.card("Starter", inr(100, 0, 0), oct.AddDate(0, -1, 0))
	gone := "eeeeeeee-0000-0000-0000-0000000000e1"
	f.exec(`INSERT INTO usage_tenants (tenant_id, name, slug, plan_name, plan_id, purged_at) VALUES ($1, 'Gone Ltd', 'gone', 'Starter', $2, $3)`,
		gone, p, oct.AddDate(0, 0, 20))
	if _, err := f.gen(afterOk).CloseDueMonths(ctx); err != nil {
		t.Fatal(err)
	}
	inv := f.invoices(gone)
	if len(inv) != 1 || inv[0].TenantName != "Gone Ltd" || inv[0].TotalMinor != 100 {
		t.Fatalf("final invoice %+v", inv)
	}
}

func TestPrune_FollowsSetting(t *testing.T) {
	f := newBillingFixture(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	p := f.card("Starter", inr(100, 0, 0), time.Date(2017, 1, 1, 0, 0, 0, 0, time.UTC))
	a := f.tenant("alpha", p, time.Date(2017, 1, 1, 0, 0, 0, 0, time.UTC))
	for _, m := range []time.Time{time.Date(2018, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2018, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if err := f.tx(func(tx pgx.Tx) error {
			id, _, err := insertDraftInvoice(ctx, tx, Invoice{TenantID: a, TenantName: "a", TenantSlug: "a", Month: m, Currency: "INR", PlanID: p,
				PlanName: "S", PlanRevision: 1, MonthMinutes: 43200, Lines: []Line{{Kind: "base"}}})
			if err != nil {
				return err
			}
			_, _, err = Issue(ctx, tx, id, "a", now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	audited := int64(0)
	n, err := Prune(ctx, f.db, now, func(ctx context.Context, tx pgx.Tx, cutoff time.Time, n int64) error {
		audited = n
		return nil
	})
	// 8 years: cutoff Nov 2018 → only October 2018 goes.
	if err != nil || n != 1 || audited != 1 {
		t.Fatalf("prune: %d (audited %d) %v", n, audited, err)
	}
	f.exec(`UPDATE billing_settings SET invoice_retention_years = 2`)
	if n, err := Prune(ctx, f.db, now, nil); err != nil || n != 1 {
		t.Fatalf("prune at 2 years: %d %v", n, err)
	}
	if left := f.scalarInt(`SELECT count(*) FROM invoices`); left != 1 {
		t.Fatalf("left %d", left)
	}
}
