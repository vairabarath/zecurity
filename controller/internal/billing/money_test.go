package billing

import (
	"math"
	"testing"
)

func TestFormatMoney(t *testing.T) {
	for _, c := range []struct {
		minor int64
		cur   string
		want  string
	}{
		{0, "USD", "USD 0.00"},
		{5, "USD", "USD 0.05"},
		{123456, "USD", "USD 1,234.56"},
		{118000, "INR", "INR 1,180.00"},
		{1000, "JPY", "JPY 1,000"},
		{1234, "KWD", "KWD 1.234"},
		{-250, "EUR", "EUR -2.50"},
		{math.MaxInt64, "USD", "USD 92,233,720,368,547,758.07"},
		{math.MinInt64, "USD", "USD -92,233,720,368,547,758.08"},
	} {
		if got := FormatMoney(c.minor, c.cur); got != c.want {
			t.Errorf("FormatMoney(%d, %s) = %q, want %q", c.minor, c.cur, got, c.want)
		}
	}
}

func TestFormatRate(t *testing.T) {
	for bp, want := range map[int]string{0: "0%", 1800: "18%", 725: "7.25%", 750: "7.5%", 5: "0.05%", 10000: "100%"} {
		if got := FormatRate(bp); got != want {
			t.Errorf("FormatRate(%d) = %q, want %q", bp, got, want)
		}
	}
}

func TestFormatQuantityAndUnit(t *testing.T) {
	for _, c := range []struct {
		meter string
		q     int64
		want  string
	}{
		{"relay_bytes_to_clients", 123_456_789_012, "123.45 GB"}, // rounded down
		{"relay_bytes_to_connectors", 9_999_999, "0.00 GB"},
		{"session_minutes", 12345, "12,345 min"},
		{"active_users", 1, "1 user"},
		{"active_users", 7, "7 users"},
	} {
		if got := FormatQuantity(c.meter, c.q); got != c.want {
			t.Errorf("FormatQuantity(%s, %d) = %q, want %q", c.meter, c.q, got, c.want)
		}
	}
	for _, c := range []struct {
		meter string
		unit  int64
		want  string
	}{
		{"relay_bytes_to_clients", 1_000_000_000, "GB"},
		{"relay_bytes_to_clients", 5, "5 bytes"},
		{"connector_minutes", 60, "hour"},
		{"session_minutes", 1000, "1,000 min"},
		{"active_users", 1, "user"},
	} {
		if got := FormatUnit(c.meter, c.unit); got != c.want {
			t.Errorf("FormatUnit(%s, %d) = %q, want %q", c.meter, c.unit, got, c.want)
		}
	}
}
