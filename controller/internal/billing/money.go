// Package billing is Sprint 26 F34: plan cards, invoices and their delivery
// (Decision Record amendment 2026-11-23, billing, D-62 … D-70).
//
// This file formats money and quantities for people. Money is always whole
// minor units (D-64); formatting never uses floating point.
package billing

import (
	"strconv"
	"strings"
)

// currencyExponent: ISO 4217 minor-unit digits that differ from 2.
var currencyExponent = map[string]int{
	"BIF": 0, "CLP": 0, "DJF": 0, "GNF": 0, "ISK": 0, "JPY": 0, "KMF": 0, "KRW": 0,
	"PYG": 0, "RWF": 0, "UGX": 0, "UYI": 0, "VND": 0, "VUV": 0, "XAF": 0, "XOF": 0, "XPF": 0,
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3, "OMR": 3, "TND": 3,
}

// CurrencyExponent returns the number of minor-unit digits of an ISO 4217
// code (2 unless listed).
func CurrencyExponent(currency string) int {
	if e, ok := currencyExponent[currency]; ok {
		return e
	}
	return 2
}

// FormatMoney renders minor units as "USD 1,234.56" (integer arithmetic).
func FormatMoney(minor int64, currency string) string {
	neg := minor < 0
	u := uint64(minor)
	if neg {
		u = uint64(-(minor + 1)) + 1 // safe for math.MinInt64
	}
	exp := CurrencyExponent(currency)
	pow := uint64(1)
	for i := 0; i < exp; i++ {
		pow *= 10
	}
	whole, frac := u/pow, u%pow
	s := group(strconv.FormatUint(whole, 10))
	if exp > 0 {
		f := strconv.FormatUint(frac, 10)
		s += "." + strings.Repeat("0", exp-len(f)) + f
	}
	if neg {
		s = "-" + s
	}
	return currency + " " + s
}

// FormatRate renders basis points as a percentage: 1800 → "18%", 725 → "7.25%".
func FormatRate(bp int) string {
	s := strconv.Itoa(bp / 100)
	if r := bp % 100; r != 0 {
		f := strconv.Itoa(r)
		if len(f) == 1 {
			f = "0" + f
		}
		s += "." + strings.TrimRight(f, "0")
	}
	return s + "%"
}

// group inserts thousands separators into a run of digits.
func group(d string) string {
	if len(d) <= 3 {
		return d
	}
	var b strings.Builder
	head := len(d) % 3
	if head > 0 {
		b.WriteString(d[:head])
	}
	for i := head; i < len(d); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(d[i : i+3])
	}
	return b.String()
}

// Meter labels and units (the D-57 meters).
var meterLabel = map[string]string{
	"relay_bytes_to_clients":    "Relay traffic to clients",
	"relay_bytes_to_connectors": "Relay traffic to connectors",
	"session_minutes":           "Client session time",
	"connector_minutes":         "Connector time",
	"shield_minutes":            "Shield time",
	"active_users":              "Active users",
}

// MeterLabel is the human name of a meter (the meter id if unknown).
func MeterLabel(meter string) string {
	if l, ok := meterLabel[meter]; ok {
		return l
	}
	return meter
}

func isByteMeter(m string) bool   { return strings.HasPrefix(m, "relay_bytes_") }
func isMinuteMeter(m string) bool { return strings.HasSuffix(m, "_minutes") }

// FormatQuantity renders a meter quantity: bytes as GB with two decimals
// (rounded down, integer arithmetic), minutes as "1,234 min", users as a count.
func FormatQuantity(meter string, q int64) string {
	switch {
	case isByteMeter(meter):
		if q < 0 {
			q = 0
		}
		const gb = 1_000_000_000
		return group(strconv.FormatInt(q/gb, 10)) + "." + pad2(q%gb/10_000_000) + " GB"
	case isMinuteMeter(meter):
		return group(strconv.FormatInt(q, 10)) + " min"
	case meter == "active_users":
		if q == 1 {
			return "1 user"
		}
		return group(strconv.FormatInt(q, 10)) + " users"
	}
	return group(strconv.FormatInt(q, 10))
}

// FormatUnit renders a price unit: "GB", "hour", "user", "1,000 min".
func FormatUnit(meter string, unit int64) string {
	switch {
	case isByteMeter(meter):
		switch unit {
		case 1_000_000:
			return "MB"
		case 1_000_000_000:
			return "GB"
		case 1_000_000_000_000:
			return "TB"
		}
		return group(strconv.FormatInt(unit, 10)) + " bytes"
	case isMinuteMeter(meter):
		switch unit {
		case 1:
			return "minute"
		case 60:
			return "hour"
		}
		return group(strconv.FormatInt(unit, 10)) + " min"
	case meter == "active_users" && unit == 1:
		return "user"
	}
	return group(strconv.FormatInt(unit, 10))
}

func pad2(n int64) string {
	if n < 10 {
		return "0" + strconv.FormatInt(n, 10)
	}
	return strconv.FormatInt(n, 10)
}
