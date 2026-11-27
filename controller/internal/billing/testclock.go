package billing

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Dev-only billing test clock (Sprint 26 acceptance; path.md allows "a test
// clock" for the month close). BILLING_TEST_NOW=<RFC 3339 time> moves the
// billing clock, the month close, regenerate, the open-month check of issue
// and the paid-date check, so that it reads that time at startup and keeps
// ticking from there (a fixed offset). The email queue and the retention
// prune keep real time. It is refused outside ENV=development, so it can
// never change billing in a real deployment.

// ErrTestClockNotDev: BILLING_TEST_NOW set without ENV=development.
var ErrTestClockNotDev = errors.New("BILLING_TEST_NOW is only allowed with ENV=development")

// TestClock returns the billing clock for the BILLING_TEST_NOW value raw.
// Empty raw → nil (callers use time.Now). started is the process start
// (normally time.Now()); the returned clock is real time + (test − started).
func TestClock(raw string, dev bool, started time.Time) (func() time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if !dev {
		return nil, ErrTestClockNotDev
	}
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, fmt.Errorf("BILLING_TEST_NOW: %w", err)
	}
	offset := at.Sub(started)
	return func() time.Time { return time.Now().Add(offset).UTC() }, nil
}
