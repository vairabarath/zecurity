package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Plan cards (D-63): a plan (062_, D-43) plus an optional price. Every edit
// raises plans.revision and appends the whole card to plan_card_revisions.
// The revision in force at the start of a month prices that month
// (CardForMonth). The plan fields themselves are written by internal/plans
// (B1); it calls CreateCard / SaveCard in the same transaction.

// Meters are the billable meters, in the D-57 order invoices list them.
var Meters = []string{
	"relay_bytes_to_clients",
	"relay_bytes_to_connectors",
	"session_minutes",
	"connector_minutes",
	"shield_minutes",
	"active_users",
}

func knownMeter(m string) bool {
	for _, k := range Meters {
		if k == m {
			return true
		}
	}
	return false
}

// MeterPrice prices one meter: usage above Included costs UnitPriceMinor for
// every started Unit.
type MeterPrice struct {
	Included       int64 `json:"included"`
	Unit           int64 `json:"unit"`
	UnitPriceMinor int64 `json:"unit_price_minor"`
}

// Price is a card's price part (D-63). Money is minor units of Currency.
type Price struct {
	Currency     string                `json:"currency"`
	BaseFeeMinor int64                 `json:"base_fee_minor"`
	Meters       map[string]MeterPrice `json:"meters"`
}

// Card is a plan card as the history keeps it.
type Card struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Limits      json.RawMessage `json:"limits"`
	Price       *Price          `json:"price"`
}

// CardRevision is one card as it was in force from ChangedAt.
type CardRevision struct {
	PlanID    string
	Revision  int
	Card      Card
	ChangedAt time.Time
}

var (
	// ErrInvalidPrice: the price object is malformed (unknown meter, missing
	// field, negative or non-integer value, unit < 1).
	ErrInvalidPrice = errors.New("invalid price")
	// ErrCurrencyMismatch: the price is not in the settings currency (D-64).
	ErrCurrencyMismatch = errors.New("price currency differs from the billing currency")
	// ErrPriceOverflow: a value doesn't fit the stored range (18 digits).
	ErrPriceOverflow = errors.New("price value too large")
	// ErrPlanNotFound: no such plan.
	ErrPlanNotFound = errors.New("plan not found")
)

// Querier is satisfied by *pgxpool.Pool and pgx.Tx.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// maxValue is the largest amount or quantity a card may carry: 18 decimal
// digits, as the DB check (plan_price_meters_valid). It keeps every invoice
// line's arithmetic far from int64 wrap-around; Compute still checks.
const maxValue int64 = 999_999_999_999_999_999

var digitsRe = regexp.MustCompile(`^[0-9]+$`)

// DecodePrice parses a price object strictly: integers only (no fraction or
// exponent), known meters, exactly the three meter fields. Values above 18
// digits are ErrPriceOverflow.
func DecodePrice(raw json.RawMessage) (*Price, error) {
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil || top == nil {
		return nil, ErrInvalidPrice
	}
	for k := range top {
		if k != "currency" && k != "base_fee_minor" && k != "meters" {
			return nil, ErrInvalidPrice
		}
	}
	p := &Price{Meters: map[string]MeterPrice{}}
	if err := json.Unmarshal(top["currency"], &p.Currency); err != nil {
		return nil, ErrInvalidPrice
	}
	var err error
	if p.BaseFeeMinor, err = decodeInt(top["base_fee_minor"]); err != nil {
		return nil, err
	}
	if m, ok := top["meters"]; ok && !bytes.Equal(bytes.TrimSpace(m), []byte("null")) {
		var meters map[string]map[string]json.RawMessage
		if err := json.Unmarshal(m, &meters); err != nil {
			return nil, ErrInvalidPrice
		}
		for name, f := range meters {
			if !knownMeter(name) || len(f) != 3 {
				return nil, ErrInvalidPrice
			}
			var mp MeterPrice
			if mp.Included, err = decodeInt(f["included"]); err != nil {
				return nil, err
			}
			if mp.Unit, err = decodeInt(f["unit"]); err != nil {
				return nil, err
			}
			if mp.UnitPriceMinor, err = decodeInt(f["unit_price_minor"]); err != nil {
				return nil, err
			}
			if mp.Unit < 1 {
				return nil, ErrInvalidPrice
			}
			p.Meters[name] = mp
		}
	}
	return p, nil
}

// decodeInt reads a JSON non-negative integer literal of at most 18 digits.
func decodeInt(raw json.RawMessage) (int64, error) {
	s := strings.TrimSpace(string(raw))
	if !digitsRe.MatchString(s) {
		return 0, ErrInvalidPrice
	}
	if len(strings.TrimLeft(s, "0")) > 18 {
		return 0, ErrPriceOverflow
	}
	var v int64
	for _, c := range s {
		v = v*10 + int64(c-'0')
	}
	return v, nil
}

// ValidatePrice checks a decoded price against the settings currency (D-64)
// and the stored ranges.
func ValidatePrice(p *Price, currency string) error {
	if p == nil {
		return nil
	}
	if !currencyRe.MatchString(p.Currency) {
		return ErrInvalidPrice
	}
	if p.Currency != currency {
		return ErrCurrencyMismatch
	}
	if p.BaseFeeMinor < 0 {
		return ErrInvalidPrice
	}
	if p.BaseFeeMinor > maxValue {
		return ErrPriceOverflow
	}
	for name, m := range p.Meters {
		if !knownMeter(name) || m.Included < 0 || m.Unit < 1 || m.UnitPriceMinor < 0 {
			return ErrInvalidPrice
		}
		if m.Included > maxValue || m.Unit > maxValue || m.UnitPriceMinor > maxValue {
			return ErrPriceOverflow
		}
	}
	return nil
}

// SamePrice reports whether two prices are equal (nil = unpriced).
func SamePrice(a, b *Price) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	if a.Currency != b.Currency || a.BaseFeeMinor != b.BaseFeeMinor || len(a.Meters) != len(b.Meters) {
		return false
	}
	for k, v := range a.Meters {
		if w, ok := b.Meters[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// PriceChange is the price part of a card edit: Set=false leaves the price
// unchanged; Set=true with Price=nil removes it (the card becomes unbillable).
type PriceChange struct {
	Set   bool
	Price *Price
}

func readPrice(ctx context.Context, q Querier, planID string) (*Price, error) {
	var (
		p   Price
		raw []byte
	)
	err := q.QueryRow(ctx, `SELECT currency, base_fee_minor, meters FROM plan_prices WHERE plan_id = $1`, planID).
		Scan(&p.Currency, &p.BaseFeeMinor, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read price: %w", err)
	}
	if err := json.Unmarshal(raw, &p.Meters); err != nil {
		return nil, fmt.Errorf("decode price meters: %w", err)
	}
	if p.Meters == nil {
		p.Meters = map[string]MeterPrice{}
	}
	return &p, nil
}

// CardFor returns a plan's card as it reads now, with its revision.
func CardFor(ctx context.Context, q Querier, planID string) (Card, int, error) {
	var (
		c   Card
		rev int
	)
	err := q.QueryRow(ctx, `SELECT name, description, limits, revision FROM plans WHERE id = $1`, planID).
		Scan(&c.Name, &c.Description, &c.Limits, &rev)
	if errors.Is(err, pgx.ErrNoRows) {
		return Card{}, 0, ErrPlanNotFound
	}
	if err != nil {
		return Card{}, 0, fmt.Errorf("read plan: %w", err)
	}
	if c.Price, err = readPrice(ctx, q, planID); err != nil {
		return Card{}, 0, err
	}
	return c, rev, nil
}

func writePrice(ctx context.Context, tx pgx.Tx, planID string, p *Price, actor string) error {
	if p == nil {
		_, err := tx.Exec(ctx, `DELETE FROM plan_prices WHERE plan_id = $1`, planID)
		return err
	}
	meters := p.Meters
	if meters == nil {
		meters = map[string]MeterPrice{}
	}
	raw, err := json.Marshal(meters)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO plan_prices (plan_id, currency, base_fee_minor, meters, updated_at, updated_by)
VALUES ($1, $2, $3, $4, now(), $5)
ON CONFLICT (plan_id) DO UPDATE
   SET currency = EXCLUDED.currency, base_fee_minor = EXCLUDED.base_fee_minor, meters = EXCLUDED.meters,
       updated_at = EXCLUDED.updated_at, updated_by = EXCLUDED.updated_by`,
		planID, p.Currency, p.BaseFeeMinor, raw, actor)
	return err
}

func appendRevision(ctx context.Context, tx pgx.Tx, planID string, rev int, c Card, actor, reason string) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO plan_card_revisions (plan_id, revision, card, changed_at, changed_by, reason)
VALUES ($1, $2, $3, clock_timestamp(), $4, $5)`, planID, rev, raw, actor, reason); err != nil {
		return fmt.Errorf("append card revision: %w", err)
	}
	return nil
}

// CreateCard runs in B1's create transaction, after the plan row is
// inserted (revision 1): it stores the price (nil = unbillable) and the
// first history row. The price must already be validated (ValidatePrice).
func CreateCard(ctx context.Context, tx pgx.Tx, planID string, price *Price, actor, reason string) (Card, error) {
	if price != nil {
		if err := writePrice(ctx, tx, planID, price, actor); err != nil {
			return Card{}, fmt.Errorf("write price: %w", err)
		}
	}
	c, rev, err := CardFor(ctx, tx, planID)
	if err != nil {
		return Card{}, err
	}
	return c, appendRevision(ctx, tx, planID, rev, c, actor, reason)
}

// SaveCard runs in B1's update transaction, after the plan fields are
// written and with the plan row locked: it applies the price change, raises
// the revision and appends the card to the history. Every card edit calls it
// once (one edit = one revision).
func SaveCard(ctx context.Context, tx pgx.Tx, planID string, change PriceChange, actor, reason string) (Card, int, error) {
	if change.Set {
		if err := writePrice(ctx, tx, planID, change.Price, actor); err != nil {
			return Card{}, 0, fmt.Errorf("write price: %w", err)
		}
	}
	var rev int
	if err := tx.QueryRow(ctx, `UPDATE plans SET revision = revision + 1 WHERE id = $1 RETURNING revision`, planID).Scan(&rev); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Card{}, 0, ErrPlanNotFound
		}
		return Card{}, 0, fmt.Errorf("raise revision: %w", err)
	}
	c, _, err := CardFor(ctx, tx, planID)
	if err != nil {
		return Card{}, 0, err
	}
	return c, rev, appendRevision(ctx, tx, planID, rev, c, actor, reason)
}

// CardForMonth is the card revision in force at the start of month (D-63,
// changed 2026-11-24): the latest revision changed before the month began,
// else (a card created during the month) its first revision. Never the card
// as it reads now.
func CardForMonth(ctx context.Context, q Querier, planID string, month time.Time) (CardRevision, error) {
	start := MonthStart(month)
	r := CardRevision{PlanID: planID}
	var raw []byte
	err := q.QueryRow(ctx, `
SELECT revision, card, changed_at FROM plan_card_revisions
 WHERE plan_id = $1
 ORDER BY (changed_at < $2) DESC,
          CASE WHEN changed_at < $2 THEN revision END DESC,
          revision ASC
 LIMIT 1`, planID, start).Scan(&r.Revision, &raw, &r.ChangedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CardRevision{}, ErrPlanNotFound
	}
	if err != nil {
		return CardRevision{}, fmt.Errorf("card for month: %w", err)
	}
	if err := json.Unmarshal(raw, &r.Card); err != nil {
		return CardRevision{}, fmt.Errorf("decode card revision: %w", err)
	}
	r.ChangedAt = r.ChangedAt.UTC()
	return r, nil
}

// MonthStart is the first instant of t's UTC month.
func MonthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// CardPrice reads a plan's price (nil = not billable).
func CardPrice(ctx context.Context, q Querier, planID string) (*Price, error) {
	return readPrice(ctx, q, planID)
}
