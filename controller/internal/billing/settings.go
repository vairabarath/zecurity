package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Billing settings (D-64, D-67, D-68, D-69): one provider-wide row.

// Settings is the billing_settings row.
type Settings struct {
	Country               string    `json:"country"`
	Currency              string    `json:"currency"`
	TaxRateBP             int       `json:"tax_rate_bp"`
	TaxLabel              string    `json:"tax_label"`
	InvoiceRetentionYears int       `json:"invoice_retention_years"`
	EmailTenants          bool      `json:"email_tenants"`
	UpdatedAt             time.Time `json:"updated_at"`
	UpdatedBy             string    `json:"updated_by"`
}

// AuditView is the settings as written into audit details (before/after).
func (s Settings) AuditView() map[string]any {
	return map[string]any{
		"country":                 s.Country,
		"currency":                s.Currency,
		"tax_rate_bp":             s.TaxRateBP,
		"tax_label":               s.TaxLabel,
		"invoice_retention_years": s.InvoiceRetentionYears,
		"email_tenants":           s.EmailTenants,
	}
}

// SettingsPatch is a partial update; nil fields are unchanged.
type SettingsPatch struct {
	Country               *string
	Currency              *string
	TaxRateBP             *int
	TaxLabel              *string
	InvoiceRetentionYears *int
	EmailTenants          *bool
}

var (
	// ErrInvalidSettings: a value is out of range (tax, label, retention).
	ErrInvalidSettings = errors.New("invalid billing settings")
	// ErrInvalidCountry: not an ISO 3166-1 alpha-2 code.
	ErrInvalidCountry = errors.New("invalid country")
	// ErrInvalidCurrency: not an active ISO 4217 code, or a country outside
	// the built-in table without an explicit currency.
	ErrInvalidCurrency = errors.New("invalid currency")
)

const settingsColumns = `country, currency, tax_rate_bp, tax_label, invoice_retention_years, email_tenants, updated_at, updated_by`

func scanSettings(row pgx.Row) (Settings, error) {
	var (
		s     Settings
		years int16
	)
	if err := row.Scan(&s.Country, &s.Currency, &s.TaxRateBP, &s.TaxLabel, &years, &s.EmailTenants, &s.UpdatedAt, &s.UpdatedBy); err != nil {
		return Settings{}, fmt.Errorf("read billing settings: %w", err)
	}
	s.InvoiceRetentionYears = int(years)
	s.UpdatedAt = s.UpdatedAt.UTC()
	return s, nil
}

// GetSettings reads the billing settings.
func GetSettings(ctx context.Context, q Querier) (Settings, error) {
	return scanSettings(q.QueryRow(ctx, `SELECT `+settingsColumns+` FROM billing_settings`))
}

// CardSwitch is one plan card a currency change switched (D-64).
type CardSwitch struct {
	PlanID   string `json:"plan_id"`
	PlanName string `json:"plan_name"`
	Revision int    `json:"revision"`
}

// apply validates a patch against the current settings and returns the
// result. A country in the built-in table fills in its currency unless the
// patch sets one; a country outside it needs an explicit currency.
func (p SettingsPatch) apply(s Settings) (Settings, error) {
	out := s
	if p.Country != nil {
		c := strings.TrimSpace(*p.Country)
		if !ValidCountry(c) {
			return Settings{}, ErrInvalidCountry
		}
		out.Country = c
		if p.Currency == nil && c != s.Country {
			cur, ok := DefaultCurrency(c)
			if !ok {
				return Settings{}, ErrInvalidCurrency
			}
			out.Currency = cur
		}
	}
	if p.Currency != nil {
		c := strings.TrimSpace(*p.Currency)
		if !ValidCurrency(c) {
			return Settings{}, ErrInvalidCurrency
		}
		out.Currency = c
	}
	if p.TaxRateBP != nil {
		if *p.TaxRateBP < 0 || *p.TaxRateBP > 10000 {
			return Settings{}, ErrInvalidSettings
		}
		out.TaxRateBP = *p.TaxRateBP
	}
	if p.TaxLabel != nil {
		l := strings.TrimSpace(*p.TaxLabel)
		if utf8.RuneCountInString(l) > 32 || strings.ContainsFunc(l, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return Settings{}, ErrInvalidSettings
		}
		out.TaxLabel = l
	}
	if p.InvoiceRetentionYears != nil {
		if *p.InvoiceRetentionYears < 1 || *p.InvoiceRetentionYears > 30 {
			return Settings{}, ErrInvalidSettings
		}
		out.InvoiceRetentionYears = *p.InvoiceRetentionYears
	}
	if p.EmailTenants != nil {
		out.EmailTenants = *p.EmailTenants
	}
	return out, nil
}

func sameSettings(a, b Settings) bool {
	return a.Country == b.Country && a.Currency == b.Currency && a.TaxRateBP == b.TaxRateBP &&
		a.TaxLabel == b.TaxLabel && a.InvoiceRetentionYears == b.InvoiceRetentionYears && a.EmailTenants == b.EmailTenants
}

// UpdateSettings applies a patch inside tx (the caller commits it with its
// audit row). When the currency changes, every priced card is switched to it
// in the same transaction: its price currency set (amounts are NOT
// converted), its revision raised and a history row written with reason
// (D-64). changed=false when nothing differs.
func UpdateSettings(ctx context.Context, tx pgx.Tx, patch SettingsPatch, actor, reason string) (before, after Settings, switched []CardSwitch, changed bool, err error) {
	before, err = scanSettings(tx.QueryRow(ctx, `SELECT `+settingsColumns+` FROM billing_settings FOR UPDATE`))
	if err != nil {
		return
	}
	after, err = patch.apply(before)
	if err != nil {
		return
	}
	if sameSettings(before, after) {
		after = before
		return
	}
	after, err = scanSettings(tx.QueryRow(ctx, `
UPDATE billing_settings
   SET country = $1, currency = $2, tax_rate_bp = $3, tax_label = $4, invoice_retention_years = $5,
       email_tenants = $6, updated_at = now(), updated_by = $7
RETURNING `+settingsColumns,
		after.Country, after.Currency, after.TaxRateBP, after.TaxLabel, after.InvoiceRetentionYears, after.EmailTenants, actor))
	if err != nil {
		return
	}
	changed = true
	if after.Currency != before.Currency {
		switched, err = switchCardCurrency(ctx, tx, after.Currency, actor, reason)
	}
	return
}

// switchCardCurrency moves every priced card to currency (D-64). Plan rows
// are locked in id order (as B1's set_default) so a concurrent card edit
// serialises with the switch.
func switchCardCurrency(ctx context.Context, tx pgx.Tx, currency, actor, reason string) ([]CardSwitch, error) {
	rows, err := tx.Query(ctx, `
SELECT p.id::text, p.name FROM plans p JOIN plan_prices pp ON pp.plan_id = p.id
 ORDER BY p.id FOR UPDATE OF p`)
	if err != nil {
		return nil, fmt.Errorf("lock priced cards: %w", err)
	}
	var cards []CardSwitch
	for rows.Next() {
		var c CardSwitch
		if err := rows.Scan(&c.PlanID, &c.PlanName); err != nil {
			rows.Close()
			return nil, err
		}
		cards = append(cards, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lock priced cards: %w", err)
	}
	for i, c := range cards {
		if _, err := tx.Exec(ctx, `UPDATE plan_prices SET currency = $2, updated_at = now(), updated_by = $3 WHERE plan_id = $1`,
			c.PlanID, currency, actor); err != nil {
			return nil, fmt.Errorf("switch card %s: %w", c.PlanID, err)
		}
		_, rev, err := SaveCard(ctx, tx, c.PlanID, PriceChange{}, actor, reason)
		if err != nil {
			return nil, err
		}
		cards[i].Revision = rev
	}
	return cards, nil
}
