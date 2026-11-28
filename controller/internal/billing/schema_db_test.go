package billing

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sprint 26 P1a (D-63 … D-69): the billing schema's constraints, triggers and
// grants, over a throwaway database with every migration
// (PKI_TEST_DATABASE_URL). A skipped run is a failed gate.

func schemaTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("billing_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	u, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
		admin.Close()
	})
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("migration %s: %v", filepath.Base(f), err)
		}
	}
	return pool
}

const (
	tenantA  = "bbbbbbbb-0000-0000-0000-0000000000a1"
	tenantB  = "bbbbbbbb-0000-0000-0000-0000000000a2"
	planFree = "00000000-0000-0000-0001-000000000001"
)

// insertDraft adds a draft invoice for (tenant, month) and returns its id.
func insertDraft(t *testing.T, db *pgxpool.Pool, tenant, month string) string {
	t.Helper()
	var id string
	err := db.QueryRow(context.Background(), `
		INSERT INTO invoices (tenant_id, tenant_name, tenant_slug, month, currency, plan_id, plan_name, plan_revision,
		                      subtotal_minor, tax_rate_bp, tax_label, tax_minor, total_minor,
		                      collected_minutes, month_minutes, incomplete_hours)
		VALUES ($1, 'Acme', 'acme', $2, 'USD', $3, 'Free', 1, 1000, 1800, 'GST', 180, 1180, 43200, 43200, 0)
		RETURNING id::text`, tenant, month, planFree).Scan(&id)
	if err != nil {
		t.Fatalf("insert draft: %v", err)
	}
	if _, err := db.Exec(context.Background(), `
		INSERT INTO invoice_lines (invoice_id, position, kind, amount_minor) VALUES ($1, 0, 'base', 500);
		`, id); err != nil {
		t.Fatalf("insert base line: %v", err)
	}
	if _, err := db.Exec(context.Background(), `
		INSERT INTO invoice_lines (invoice_id, position, kind, meter, quantity, included, unit, billable_units, unit_price_minor, amount_minor)
		VALUES ($1, 1, 'meter', 'relay_bytes_to_clients', 2500000001, 1000000000, 1000000000, 2, 250, 500)`, id); err != nil {
		t.Fatalf("insert meter line: %v", err)
	}
	return id
}

func issue(t *testing.T, db *pgxpool.Pool, id, number string) {
	t.Helper()
	if _, err := db.Exec(context.Background(), `
		UPDATE invoices SET status = 'issued', number = $2, issued_at = now(), issued_by = 'admin@example.test'
		 WHERE id = $1`, id, number); err != nil {
		t.Fatalf("issue: %v", err)
	}
}

func wantRefused(t *testing.T, name string, err error, contains string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: accepted, want refused", name)
		return
	}
	if contains != "" && !strings.Contains(err.Error(), contains) {
		t.Errorf("%s: refused with %v, want %q", name, err, contains)
	}
}

func TestBillingSchema_SettingsDefaults(t *testing.T) {
	db := schemaTestDB(t)
	var (
		country, currency, label string
		taxBP                    int
		years                    int16
		email                    bool
	)
	if err := db.QueryRow(context.Background(), `
		SELECT country, currency, tax_rate_bp, tax_label, invoice_retention_years, email_tenants FROM billing_settings`).
		Scan(&country, &currency, &taxBP, &label, &years, &email); err != nil {
		t.Fatal(err)
	}
	if country != "US" || currency != "USD" || taxBP != 0 || label != "" || years != 8 || !email {
		t.Fatalf("defaults = %s %s %d %q %d %v, want US USD 0 \"\" 8 true", country, currency, taxBP, label, years, email)
	}
	// One row only.
	wantRefused(t, "second settings row", execErr(db, `INSERT INTO billing_settings (id) VALUES (false)`), "check constraint")
	wantRefused(t, "duplicate settings row", execErr(db, `INSERT INTO billing_settings DEFAULT VALUES`), "duplicate key")
	// The seeded plans start at revision 1, unpriced, with a history row.
	var n int
	if err := db.QueryRow(context.Background(), `
		SELECT count(*) FROM plans p JOIN plan_card_revisions r ON r.plan_id = p.id AND r.revision = 1
		 WHERE p.revision = 1 AND r.card -> 'price' = 'null'::jsonb`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("seeded plans with revision 1 history: %d, want 2", n)
	}
}

func execErr(db *pgxpool.Pool, sql string, args ...any) error {
	_, err := db.Exec(context.Background(), sql, args...)
	return err
}

func TestBillingSchema_Checks(t *testing.T) {
	db := schemaTestDB(t)
	price := func(meters string) error {
		return execErr(db, `INSERT INTO plan_prices (plan_id, currency, base_fee_minor, meters, updated_by)
			VALUES ($1, 'USD', 100, $2::jsonb, 'a@example.test')
			ON CONFLICT (plan_id) DO UPDATE SET meters = EXCLUDED.meters`, planFree, meters)
	}
	refused := []struct {
		name string
		err  error
	}{
		{"country lower case", execErr(db, `UPDATE billing_settings SET country = 'us'`)},
		{"country three letters", execErr(db, `UPDATE billing_settings SET country = 'USA'`)},
		{"currency two letters", execErr(db, `UPDATE billing_settings SET currency = 'US'`)},
		{"tax over 10000", execErr(db, `UPDATE billing_settings SET tax_rate_bp = 10001`)},
		{"negative tax", execErr(db, `UPDATE billing_settings SET tax_rate_bp = -1`)},
		{"tax label too long", execErr(db, `UPDATE billing_settings SET tax_label = repeat('x', 33)`)},
		{"retention 0", execErr(db, `UPDATE billing_settings SET invoice_retention_years = 0`)},
		{"retention 31", execErr(db, `UPDATE billing_settings SET invoice_retention_years = 31`)},
		{"price currency", execErr(db, `INSERT INTO plan_prices (plan_id, currency, base_fee_minor, meters, updated_by) VALUES ($1, 'usd', 0, '{}', 'a')`, planFree)},
		{"negative base fee", execErr(db, `INSERT INTO plan_prices (plan_id, currency, base_fee_minor, meters, updated_by) VALUES ($1, 'USD', -1, '{}', 'a')`, planFree)},
		{"meters not an object", price(`[]`)},
		{"unknown meter", price(`{"bytes": {"included": 0, "unit": 1, "unit_price_minor": 1}}`)},
		{"meter missing a key", price(`{"session_minutes": {"included": 0, "unit": 1}}`)},
		{"meter extra key", price(`{"session_minutes": {"included": 0, "unit": 1, "unit_price_minor": 1, "x": 1}}`)},
		{"meter unit 0", price(`{"session_minutes": {"included": 0, "unit": 0, "unit_price_minor": 1}}`)},
		{"meter negative included", price(`{"session_minutes": {"included": -1, "unit": 1, "unit_price_minor": 1}}`)},
		{"meter fractional price", price(`{"session_minutes": {"included": 0, "unit": 1, "unit_price_minor": 1.5}}`)},
		{"meter decimal point", price(`{"session_minutes": {"included": 1.0, "unit": 1, "unit_price_minor": 1}}`)},
		{"meter string", price(`{"session_minutes": {"included": "1", "unit": 1, "unit_price_minor": 1}}`)},
		{"meter over bigint", price(`{"session_minutes": {"included": 9999999999999999999, "unit": 1, "unit_price_minor": 1}}`)},
		{"plan revision 0", execErr(db, `UPDATE plans SET revision = 0 WHERE id = $1`, planFree)},
		{"billing plan month not day 1", execErr(db, `INSERT INTO tenant_billing_plans (tenant_id, month, plan_id) VALUES ($1, '2026-11-15', $2)`, tenantA, planFree)},
		{"revision card missing price", execErr(db, `INSERT INTO plan_card_revisions (plan_id, revision, card, changed_by, reason) VALUES ($1, 9, '{"name":"x","description":"","limits":{}}', 'a', 'r')`, planFree)},
		{"revision empty reason", execErr(db, `INSERT INTO plan_card_revisions (plan_id, revision, card, changed_by, reason) VALUES ($1, 9, '{"name":"x","description":"","limits":{},"price":null}', 'a', '')`, planFree)},
	}
	for _, c := range refused {
		wantRefused(t, c.name, c.err, "")
	}
	// Valid prices: base fee only, and every meter.
	if err := price(`{}`); err != nil {
		t.Fatalf("base-fee-only price refused: %v", err)
	}
	all := `{"relay_bytes_to_clients": {"included": 50000000000, "unit": 1000000000, "unit_price_minor": 200},
	         "relay_bytes_to_connectors": {"included": 0, "unit": 1000000000, "unit_price_minor": 0},
	         "session_minutes": {"included": 0, "unit": 60, "unit_price_minor": 1},
	         "connector_minutes": {"included": 0, "unit": 1, "unit_price_minor": 0},
	         "shield_minutes": {"included": 0, "unit": 1, "unit_price_minor": 0},
	         "active_users": {"included": 5, "unit": 1, "unit_price_minor": 300}}`
	if err := price(all); err != nil {
		t.Fatalf("all-meter price refused: %v", err)
	}
}

func TestBillingSchema_InvoiceChecks(t *testing.T) {
	db := schemaTestDB(t)
	ins := func(cols, vals string) error {
		return execErr(db, `INSERT INTO invoices (tenant_id, tenant_name, tenant_slug, month, currency, plan_id, plan_name, plan_revision,
			subtotal_minor, tax_rate_bp, tax_minor, total_minor, collected_minutes, month_minutes, incomplete_hours`+cols+`)
			VALUES ('`+tenantA+`', 'Acme', 'acme', '2026-10-01', 'USD', '`+planFree+`', 'Free', 1, 1000, 0, 0, 1000, 10, 44640, 0`+vals+`)`)
	}
	refused := []struct {
		name string
		err  error
	}{
		{"total != subtotal + tax", execErr(db, `INSERT INTO invoices (tenant_id, tenant_name, tenant_slug, month, currency, plan_id, plan_name, plan_revision,
			subtotal_minor, tax_rate_bp, tax_minor, total_minor, collected_minutes, month_minutes, incomplete_hours)
			VALUES ($1, 'Acme', 'acme', '2026-10-01', 'USD', $2, 'Free', 1, 1000, 1800, 180, 1100, 10, 44640, 0)`, tenantA, planFree)},
		{"month not day 1", execErr(db, `INSERT INTO invoices (tenant_id, tenant_name, tenant_slug, month, currency, plan_id, plan_name, plan_revision,
			subtotal_minor, tax_rate_bp, tax_minor, total_minor, collected_minutes, month_minutes, incomplete_hours)
			VALUES ($1, 'Acme', 'acme', '2026-10-02', 'USD', $2, 'Free', 1, 0, 0, 0, 0, 10, 44640, 0)`, tenantA, planFree)},
		{"negative subtotal", execErr(db, `INSERT INTO invoices (tenant_id, tenant_name, tenant_slug, month, currency, plan_id, plan_name, plan_revision,
			subtotal_minor, tax_rate_bp, tax_minor, total_minor, collected_minutes, month_minutes, incomplete_hours)
			VALUES ($1, 'Acme', 'acme', '2026-09-01', 'USD', $2, 'Free', 1, -1, 0, 0, -1, 10, 44640, 0)`, tenantB, planFree)},
		{"draft with a number", ins(", number", ", 'INV-2026-000001'")},
		{"bad number format", ins(", status, number, issued_at, issued_by", ", 'issued', 'INV-26-1', now(), 'a'")},
		{"issued without number", ins(", status, issued_at, issued_by", ", 'issued', now(), 'a'")},
		{"paid without paid_at", ins(", status, number, issued_at, issued_by, paid_by", ", 'paid', 'INV-2026-000002', now(), 'a', 'a'")},
		{"void without a reason", ins(", status, number, issued_at, issued_by, voided_at, voided_by", ", 'void', 'INV-2026-000003', now(), 'a', now(), 'a'")},
		{"void with a blank reason", ins(", status, number, issued_at, issued_by, voided_at, voided_by, void_reason", ", 'void', 'INV-2026-000004', now(), 'a', now(), 'a', '  '")},
		{"reference over 200", ins(", status, number, issued_at, issued_by, paid_at, paid_by, paid_reference", ", 'paid', 'INV-2026-000005', now(), 'a', '2026-11-02', 'a', repeat('r', 201)")},
		{"unknown status", ins(", status", ", 'sent'")},
		{"collected over month", execErr(db, `INSERT INTO invoices (tenant_id, tenant_name, tenant_slug, month, currency, plan_id, plan_name, plan_revision,
			subtotal_minor, tax_rate_bp, tax_minor, total_minor, collected_minutes, month_minutes, incomplete_hours)
			VALUES ($1, 'Acme', 'acme', '2026-10-01', 'USD', $2, 'Free', 1, 0, 0, 0, 0, 44641, 44640, 0)`, tenantA, planFree)},
	}
	for _, c := range refused {
		wantRefused(t, c.name, c.err, "")
	}

	// Lines: kinds and arithmetic.
	id := insertDraft(t, db, tenantA, "2026-10-01")
	line := func(pos int, sql string) error {
		return execErr(db, `INSERT INTO invoice_lines (invoice_id, position, kind, meter, quantity, included, unit, billable_units, unit_price_minor, amount_minor)
			VALUES ($1, $2, `+sql+`)`, id, pos)
	}
	lineRefused := []struct {
		name string
		err  error
	}{
		{"base with a meter", line(10, `'base', 'session_minutes', NULL, NULL, NULL, NULL, NULL, 5`)},
		{"meter without quantity", line(11, `'meter', 'session_minutes', NULL, 0, 1, 0, 1, 0`)},
		{"billable rounded down", line(12, `'meter', 'session_minutes', 121, 0, 60, 2, 10, 20`)},
		{"billable over the included not reached", line(13, `'meter', 'session_minutes', 50, 60, 60, 1, 10, 10`)},
		{"amount wrong", line(14, `'meter', 'session_minutes', 121, 0, 60, 3, 10, 31`)},
		{"unknown meter", line(15, `'meter', 'bytes', 1, 0, 1, 1, 1, 1`)},
		{"unknown kind", line(16, `'tax', NULL, NULL, NULL, NULL, NULL, NULL, 1`)},
		{"duplicate position", line(1, `'base', NULL, NULL, NULL, NULL, NULL, NULL, 1`)},
		{"overflow", line(17, `'meter', 'session_minutes', 9223372036854775807, 0, 1, 9223372036854775807, 2, 0`)},
	}
	for _, c := range lineRefused {
		wantRefused(t, c.name, c.err, "")
	}
	// Exactly reached → 0 units; one over → 1 unit (rounded up).
	if err := line(20, `'meter', 'session_minutes', 60, 60, 60, 0, 10, 0`); err != nil {
		t.Errorf("included exactly reached: %v", err)
	}
	if err := line(21, `'meter', 'active_users', 6, 5, 1, 1, 300, 300`); err != nil {
		t.Errorf("one over: %v", err)
	}
	if err := line(22, `'meter', 'session_minutes', 61, 0, 60, 2, 10, 20`); err != nil {
		t.Errorf("rounded up: %v", err)
	}
}

func TestBillingSchema_AppendOnly(t *testing.T) {
	db := schemaTestDB(t)
	ctx := context.Background()
	wantRefused(t, "revision update", execErr(db, `UPDATE plan_card_revisions SET reason = 'x'`), "append-only")
	wantRefused(t, "revision delete", execErr(db, `DELETE FROM plan_card_revisions`), "append-only")
	wantRefused(t, "revision truncate", execErr(db, `TRUNCATE plan_card_revisions`), "append-only")
	wantRefused(t, "duplicate revision", execErr(db, `INSERT INTO plan_card_revisions (plan_id, revision, card, changed_by, reason)
		VALUES ($1, 1, '{"name":"x","description":"","limits":{},"price":null}', 'a', 'r')`, planFree), "duplicate key")

	if _, err := db.Exec(ctx, `INSERT INTO tenant_billing_plans (tenant_id, month, plan_id) VALUES ($1, '2026-11-01', $2)`, tenantA, planFree); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, "billing plan update", execErr(db, `UPDATE tenant_billing_plans SET plan_id = gen_random_uuid()`), "append-only")
	wantRefused(t, "billing plan truncate", execErr(db, `TRUNCATE tenant_billing_plans`), "append-only")
	wantRefused(t, "duplicate (tenant, month)", execErr(db, `INSERT INTO tenant_billing_plans (tenant_id, month, plan_id) VALUES ($1, '2026-11-01', gen_random_uuid())`, tenantA), "duplicate key")
	// D-65: the first change in a month wins; later ones keep it.
	if _, err := db.Exec(ctx, `INSERT INTO tenant_billing_plans (tenant_id, month, plan_id) VALUES ($1, '2026-11-01', gen_random_uuid()) ON CONFLICT DO NOTHING`, tenantA); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := db.QueryRow(ctx, `SELECT plan_id::text FROM tenant_billing_plans WHERE tenant_id = $1`, tenantA).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != planFree {
		t.Fatalf("month-start plan %s, want %s", got, planFree)
	}
}

func TestBillingSchema_InvoiceLifecycle(t *testing.T) {
	db := schemaTestDB(t)
	ctx := context.Background()

	id := insertDraft(t, db, tenantA, "2026-10-01")
	// One non-void invoice per (tenant, month).
	wantRefused(t, "second non-void invoice", execErr(db, `INSERT INTO invoices (tenant_id, tenant_name, tenant_slug, month, currency, plan_id, plan_name, plan_revision,
		subtotal_minor, tax_rate_bp, tax_minor, total_minor, collected_minutes, month_minutes, incomplete_hours)
		VALUES ($1, 'Acme', 'acme', '2026-10-01', 'USD', $2, 'Free', 1, 0, 0, 0, 0, 10, 44640, 0)`, tenantA, planFree), "invoices_one_per_month")

	// A draft changes freely; its lines can be replaced.
	if _, err := db.Exec(ctx, `UPDATE invoices SET plan_revision = 2, generated_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("draft update: %v", err)
	}
	if _, err := db.Exec(ctx, `DELETE FROM invoice_lines WHERE invoice_id = $1 AND position = 1`, id); err != nil {
		t.Fatalf("draft line delete: %v", err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO invoice_lines (invoice_id, position, kind, meter, quantity, included, unit, billable_units, unit_price_minor, amount_minor)
		VALUES ($1, 1, 'meter', 'active_users', 7, 5, 1, 2, 250, 500)`, id); err != nil {
		t.Fatalf("draft line insert: %v", err)
	}
	wantRefused(t, "line update (draft)", execErr(db, `UPDATE invoice_lines SET amount_minor = 0 WHERE invoice_id = $1`, id), "can't be updated")
	// A draft is deleted, never voided or paid.
	wantRefused(t, "draft -> void", execErr(db, `UPDATE invoices SET status = 'void', number = 'INV-2026-000009', issued_at = now(), issued_by = 'a',
		voided_at = now(), voided_by = 'a', void_reason = 'r' WHERE id = $1`, id), "change refused")
	wantRefused(t, "draft -> paid", execErr(db, `UPDATE invoices SET status = 'paid', number = 'INV-2026-000009', issued_at = now(), issued_by = 'a',
		paid_at = '2026-11-02', paid_by = 'a' WHERE id = $1`, id), "change refused")

	issue(t, db, id, "INV-2026-000001")

	// Issued: nothing changes but -> paid / -> void with their fields.
	issuedRefused := map[string]string{
		"amount":            `UPDATE invoices SET total_minor = total_minor + 1, subtotal_minor = subtotal_minor + 1 WHERE id = $1`,
		"number":            `UPDATE invoices SET number = 'INV-2026-000099' WHERE id = $1`,
		"tenant name":       `UPDATE invoices SET tenant_name = 'Renamed' WHERE id = $1`,
		"back to draft":     `UPDATE invoices SET status = 'draft', number = NULL, issued_at = NULL, issued_by = NULL WHERE id = $1`,
		"paid plus amount":  `UPDATE invoices SET status = 'paid', paid_at = '2026-11-02', paid_by = 'a', tax_label = 'VAT' WHERE id = $1`,
		"void plus regen":   `UPDATE invoices SET status = 'void', voided_at = now(), voided_by = 'a', void_reason = 'r', generated_at = now() WHERE id = $1`,
		"line update":       `UPDATE invoice_lines SET amount_minor = 0 WHERE invoice_id = $1`,
		"line delete":       `DELETE FROM invoice_lines WHERE invoice_id = $1`,
		"line insert":       `INSERT INTO invoice_lines (invoice_id, position, kind, amount_minor) VALUES ($1, 9, 'base', 1)`,
		"delete issued":     `DELETE FROM invoices WHERE id = $1`,
		"truncate invoices": `TRUNCATE invoices CASCADE`,
	}
	for name, sql := range issuedRefused {
		args := []any{id}
		if strings.HasPrefix(sql, "TRUNCATE") {
			args = nil
		}
		wantRefused(t, name, execErr(db, sql, args...), "")
	}
	wantRefused(t, "truncate lines", execErr(db, `TRUNCATE invoice_lines`), "truncated")
	// A no-op update is allowed (nothing changes).
	if _, err := db.Exec(ctx, `UPDATE invoices SET tenant_name = tenant_name WHERE id = $1`, id); err != nil {
		t.Errorf("no-op update of an issued invoice: %v", err)
	}

	// issued -> paid; then final.
	if _, err := db.Exec(ctx, `UPDATE invoices SET status = 'paid', paid_at = '2026-11-02', paid_reference = 'bank ref 42', paid_by = 'a' WHERE id = $1`, id); err != nil {
		t.Fatalf("issued -> paid: %v", err)
	}
	wantRefused(t, "paid -> void", execErr(db, `UPDATE invoices SET status = 'void', paid_at = NULL, paid_reference = NULL, paid_by = NULL,
		voided_at = now(), voided_by = 'a', void_reason = 'r' WHERE id = $1`, id), "change refused")
	wantRefused(t, "paid reference edit", execErr(db, `UPDATE invoices SET paid_reference = 'other' WHERE id = $1`, id), "change refused")

	// issued -> void; then a new draft for the same month is allowed.
	id2 := insertDraft(t, db, tenantB, "2026-10-01")
	issue(t, db, id2, "INV-2026-000002")
	if _, err := db.Exec(ctx, `UPDATE invoices SET status = 'void', voided_at = now(), voided_by = 'a', void_reason = 'wrong plan' WHERE id = $1`, id2); err != nil {
		t.Fatalf("issued -> void: %v", err)
	}
	wantRefused(t, "void -> issued", execErr(db, `UPDATE invoices SET status = 'issued', voided_at = NULL, voided_by = NULL, void_reason = NULL WHERE id = $1`, id2), "change refused")
	insertDraft(t, db, tenantB, "2026-10-01")

	// Duplicate numbers refused.
	id3 := insertDraft(t, db, tenantA, "2026-09-01")
	wantRefused(t, "duplicate number", execErr(db, `UPDATE invoices SET status = 'issued', number = 'INV-2026-000001', issued_at = now(), issued_by = 'a' WHERE id = $1`, id3), "duplicate key")

	// Deleting a draft cascades to its lines.
	if _, err := db.Exec(ctx, `DELETE FROM invoices WHERE id = $1`, id3); err != nil {
		t.Fatalf("draft delete: %v", err)
	}
	var lines int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM invoice_lines WHERE invoice_id = $1`, id3).Scan(&lines); err != nil {
		t.Fatal(err)
	}
	if lines != 0 {
		t.Fatalf("draft lines left after delete: %d", lines)
	}
}

func TestBillingSchema_EmailQueue(t *testing.T) {
	db := schemaTestDB(t)
	id := insertDraft(t, db, tenantA, "2026-10-01")
	issue(t, db, id, "INV-2026-000001")
	if err := execErr(db, `INSERT INTO invoice_emails (invoice_id, kind, seq) VALUES ($1, 'issued', 1)`, id); err != nil {
		t.Fatal(err)
	}
	wantRefused(t, "issued email twice", execErr(db, `INSERT INTO invoice_emails (invoice_id, kind, seq) VALUES ($1, 'issued', 1)`, id), "duplicate key")
	wantRefused(t, "issued email seq 2", execErr(db, `INSERT INTO invoice_emails (invoice_id, kind, seq) VALUES ($1, 'issued', 2)`, id), "invoice_emails_once")
	wantRefused(t, "unknown status", execErr(db, `INSERT INTO invoice_emails (invoice_id, kind, seq, status) VALUES ($1, 'resend', 1, 'queued')`, id), "")
	wantRefused(t, "sent without sent_at", execErr(db, `INSERT INTO invoice_emails (invoice_id, kind, seq, status) VALUES ($1, 'resend', 1, 'sent')`, id), "invoice_emails_sent")
	for seq := 1; seq <= 2; seq++ {
		if err := execErr(db, `INSERT INTO invoice_emails (invoice_id, kind, seq) VALUES ($1, 'resend', $2)`, id, seq); err != nil {
			t.Fatalf("resend %d: %v", seq, err)
		}
	}
	// The worker claims and finishes a row.
	if err := execErr(db, `UPDATE invoice_emails SET status = 'sent', sent_at = now(), recipients_count = 2, attempts = 1
		WHERE invoice_id = $1 AND kind = 'issued'`, id); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestBillingSchema_Prune(t *testing.T) {
	db := schemaTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	oldest := monthStart.AddDate(-8, 0, 0) // default retention 8 years
	old := oldest.AddDate(0, -1, 0)        // older than the retention
	kept := oldest                         // exactly at the edge: kept
	day := func(t time.Time) string { return t.Format("2006-01-02") }

	oldID := insertDraft(t, db, tenantA, day(old))
	issue(t, db, oldID, "INV-2018-000001")
	if err := execErr(db, `INSERT INTO invoice_emails (invoice_id, kind, seq) VALUES ($1, 'issued', 1)`, oldID); err != nil {
		t.Fatal(err)
	}
	keptID := insertDraft(t, db, tenantA, day(kept))
	issue(t, db, keptID, "INV-2018-000002")

	// Inside the retention: refused.
	wantRefused(t, "cutoff inside retention", execErr(db, `SELECT prune_invoices($1::date)`, day(oldest.AddDate(0, 1, 0))), "retention")
	wantRefused(t, "null cutoff", execErr(db, `SELECT prune_invoices(NULL)`), "retention")

	// The app role can't fake the prune: setting the flag itself doesn't
	// unlock the delete trigger.
	err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE ztna_app`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('zecurity.invoice_prune', 'on', true)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM invoices WHERE id = $1`, oldID)
		return err
	})
	wantRefused(t, "app role with the flag", err, "only prune_invoices")

	// As the app role, at the oldest allowed cutoff: the old invoice (lines and
	// emails) goes, the one at the edge stays.
	var n int64
	err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE ztna_app`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT prune_invoices($1::date)`, day(oldest)).Scan(&n); err != nil {
			return err
		}
		// The flag is off again after the prune, in the same transaction
		// (tried in a savepoint so the prune itself commits).
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := sp.Exec(ctx, `DELETE FROM invoices WHERE id = $1`, keptID); err == nil {
			return fmt.Errorf("delete after prune accepted")
		}
		return sp.Rollback(ctx)
	})
	if err != nil {
		t.Fatalf("prune as app role: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
	var left, lines, emails int
	if err := db.QueryRow(ctx, `SELECT (SELECT count(*) FROM invoices), (SELECT count(*) FROM invoice_lines WHERE invoice_id = $1),
		(SELECT count(*) FROM invoice_emails WHERE invoice_id = $1)`, oldID).Scan(&left, &lines, &emails); err != nil {
		t.Fatal(err)
	}
	if left != 1 || lines != 0 || emails != 0 {
		t.Fatalf("after prune: %d invoices, %d old lines, %d old emails; want 1, 0, 0", left, lines, emails)
	}

	// A shorter retention moves the cutoff: at 1 year the edge invoice can go.
	if err := execErr(db, `UPDATE billing_settings SET invoice_retention_years = 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `SELECT prune_invoices($1::date)`, day(monthStart.AddDate(-1, 0, 0))).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d after retention 1 year, want 1", n)
	}
}

// The app role gets what P1b, P2 and P3 need, and no more.
func TestBillingSchema_Grants(t *testing.T) {
	db := schemaTestDB(t)
	ctx := context.Background()
	want := map[string][]string{
		"billing_settings":        {"SELECT", "UPDATE"},
		"plan_prices":             {"SELECT", "INSERT", "UPDATE", "DELETE"},
		"plan_card_revisions":     {"SELECT", "INSERT"},
		"tenant_billing_plans":    {"SELECT", "INSERT"},
		"invoices":                {"SELECT", "INSERT", "UPDATE", "DELETE"},
		"invoice_lines":           {"SELECT", "INSERT", "UPDATE", "DELETE"},
		"invoice_number_counters": {"SELECT", "INSERT", "UPDATE", "DELETE"},
		"invoice_emails":          {"SELECT", "INSERT", "UPDATE", "DELETE"},
		"usage_tenants":           {"SELECT", "INSERT", "UPDATE", "DELETE"},
	}
	for table, privs := range want {
		var got []string
		for _, p := range []string{"DELETE", "INSERT", "SELECT", "UPDATE"} {
			var ok bool
			if err := db.QueryRow(ctx, `SELECT has_table_privilege('ztna_app', $1, $2)`, table, p).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			if ok {
				got = append(got, p)
			}
		}
		sort.Strings(privs)
		if strings.Join(got, ",") != strings.Join(privs, ",") {
			t.Errorf("%s: ztna_app has %v, want %v", table, got, privs)
		}
	}
	var exec bool
	if err := db.QueryRow(ctx, `SELECT has_function_privilege('ztna_app', 'prune_invoices(date)', 'EXECUTE')`).Scan(&exec); err != nil {
		t.Fatal(err)
	}
	if !exec {
		t.Error("ztna_app can't run prune_invoices")
	}
	if err := db.QueryRow(ctx, `SELECT has_function_privilege('public', 'prune_invoices(date)', 'EXECUTE')`).Scan(&exec); err != nil {
		t.Fatal(err)
	}
	if exec {
		t.Error("PUBLIC can run prune_invoices")
	}
	// usage_tenants carries the plan snapshot.
	if err := execErr(db, `INSERT INTO usage_tenants (tenant_id, name, slug, plan_name, plan_id) VALUES ($1, 'a', 'a', 'Free', $2)`, tenantA, planFree); err != nil {
		t.Fatalf("usage_tenants.plan_id: %v", err)
	}
}

// A temporary table named like a billing table must not fool the guards: the
// functions list pg_temp last and qualify their tables (pg_temp is otherwise
// searched first for relations).
func TestBillingSchema_TempTableShadowing(t *testing.T) {
	db := schemaTestDB(t)
	ctx := context.Background()
	id := insertDraft(t, db, tenantA, "2026-10-01")
	issue(t, db, id, "INV-2026-000001")

	asApp := func(stmts ...string) error {
		return pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SET LOCAL ROLE ztna_app`); err != nil {
				return err
			}
			for _, s := range stmts {
				if _, err := tx.Exec(ctx, s); err != nil {
					return err
				}
			}
			return nil
		})
	}
	// A temp "invoices" that says the issued invoice is a draft.
	err := asApp(
		`CREATE TEMP TABLE invoices (id uuid, status text) ON COMMIT DROP`,
		`INSERT INTO invoices VALUES ('`+id+`', 'draft')`,
		`DELETE FROM public.invoice_lines WHERE invoice_id = '`+id+`'`,
	)
	wantRefused(t, "lines deleted through a temp invoices table", err, "can't change")

	// A temp "billing_settings" with a 1-year retention.
	now := time.Now().UTC()
	recent := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(-1, 0, 0).Format("2006-01-02")
	err = asApp(
		`CREATE TEMP TABLE billing_settings (invoice_retention_years smallint) ON COMMIT DROP`,
		`INSERT INTO billing_settings VALUES (1)`,
		`SELECT public.prune_invoices('`+recent+`'::date)`,
	)
	wantRefused(t, "prune through a temp billing_settings table", err, "retention")
}
