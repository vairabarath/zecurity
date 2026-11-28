package billing

import (
	"errors"
	"math"
)

// Invoice arithmetic (D-64, D-69), pure and integer only:
//   - base line = the card's monthly fee;
//   - per priced meter, in D-57 order: over = max(0, quantity − included),
//     billable_units = ceil(over / unit), amount = billable_units × unit price;
//   - tax = (subtotal × rate_bp + 5000) / 10000 (half up);
//   - total = subtotal + tax.
//
// Every addition and multiplication is overflow-checked; an overflow is an
// error (the tenant is skipped and retried), never a wrapped amount.

// ErrOverflow: an amount doesn't fit int64.
var ErrOverflow = errors.New("billing amount overflow")

// Line is one invoice line. The meter fields are nil on the base line.
type Line struct {
	Position       int
	Kind           string // "base" | "meter"
	Meter          string
	Quantity       *int64
	Included       *int64
	Unit           *int64
	BillableUnits  *int64
	UnitPriceMinor *int64
	AmountMinor    int64
}

// Computed is an invoice's lines and totals.
type Computed struct {
	Lines    []Line
	Subtotal int64
	Tax      int64
	Total    int64
}

func addChecked(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, ErrOverflow
	}
	return a + b, nil
}

func mulChecked(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a > math.MaxInt64/b {
		return 0, ErrOverflow
	}
	return a * b, nil
}

// BillableUnits is ceil(max(0, quantity − included) / unit): usage above the
// included quantity, rounded up to whole units (D-64).
func BillableUnits(quantity, included, unit int64) int64 {
	if unit < 1 || quantity <= included {
		return 0
	}
	over := quantity - included
	units := over / unit
	if over%unit != 0 {
		units++
	}
	return units
}

// Tax is subtotal × rateBP / 10000, rounded half up, in integers (D-69).
func Tax(subtotal int64, rateBP int) (int64, error) {
	if rateBP <= 0 {
		return 0, nil
	}
	p, err := mulChecked(subtotal, int64(rateBP))
	if err != nil {
		return 0, err
	}
	p, err = addChecked(p, 5000)
	if err != nil {
		return 0, err
	}
	return p / 10000, nil
}

// Compute prices one tenant's month. quantity returns the counted quantity
// of a meter (from U2's ReadMonthUsage only, D-59).
func Compute(price Price, quantity func(meter string) int64, taxRateBP int) (Computed, error) {
	c := Computed{}
	c.Lines = append(c.Lines, Line{Position: 0, Kind: "base", AmountMinor: price.BaseFeeMinor})
	sub := price.BaseFeeMinor
	pos := 1
	for _, meter := range Meters {
		mp, ok := price.Meters[meter]
		if !ok {
			continue
		}
		q := quantity(meter)
		units := BillableUnits(q, mp.Included, mp.Unit)
		amount, err := mulChecked(units, mp.UnitPriceMinor)
		if err != nil {
			return Computed{}, err
		}
		if sub, err = addChecked(sub, amount); err != nil {
			return Computed{}, err
		}
		qq, inc, unit, up := q, mp.Included, mp.Unit, mp.UnitPriceMinor
		c.Lines = append(c.Lines, Line{Position: pos, Kind: "meter", Meter: meter,
			Quantity: &qq, Included: &inc, Unit: &unit, BillableUnits: &units, UnitPriceMinor: &up, AmountMinor: amount})
		pos++
	}
	tax, err := Tax(sub, taxRateBP)
	if err != nil {
		return Computed{}, err
	}
	total, err := addChecked(sub, tax)
	if err != nil {
		return Computed{}, err
	}
	c.Subtotal, c.Tax, c.Total = sub, tax, total
	return c, nil
}
