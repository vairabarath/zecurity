package billing

import (
	"errors"
	"testing"
	"time"
)

func TestTestClock(t *testing.T) {
	if c, err := TestClock("", false, time.Now()); c != nil || err != nil {
		t.Fatalf("unset: clock %v, err %v", c != nil, err)
	}
	if _, err := TestClock("2026-12-01T06:05:00Z", false, time.Now()); !errors.Is(err, ErrTestClockNotDev) {
		t.Fatalf("outside development: %v", err)
	}
	if _, err := TestClock("1 Dec", true, time.Now()); err == nil {
		t.Fatal("bad time accepted")
	}
	started := time.Now()
	c, err := TestClock("2026-12-01T06:05:00Z", true, started)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 12, 1, 6, 5, 0, 0, time.UTC).Add(time.Since(started))
	if d := c().Sub(want); d < -time.Second || d > time.Second {
		t.Fatalf("clock %v, want about %v", c(), want)
	}
	// It ticks.
	a := c()
	time.Sleep(20 * time.Millisecond)
	if !c().After(a) {
		t.Fatal("test clock doesn't advance")
	}
	// November is due at the test time with the default delay.
	g := &Generator{Now: c}
	if m := g.DueMonths(); len(m) == 0 || m[len(m)-1].Month() != time.November {
		t.Fatalf("due months at the test time: %v", m)
	}
}
