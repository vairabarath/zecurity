package relay

import (
	"testing"
	"time"
)

// TestComputeCandidateLabel covers the ADR-016 dead-band tier rules. The
// table fixes the boundary cases: enter < 0.45 / exit >= 0.50 for Tier 1,
// enter < 0.75 / exit >= 0.80 for Tier 2. Boundaries are deliberately
// asymmetric — at fill=0.49 a Tier 1 relay stays in Tier 1; at fill=0.50
// it exits.
func TestComputeCandidateLabel(t *testing.T) {
	tests := []struct {
		name    string
		current string
		count   uint32
		max     uint32
		want    string
	}{
		// max == 0 → unknown capacity, treat as available.
		{"unknown_capacity_high", CapacityLabelHigh, 1000, 0, CapacityLabelHigh},
		{"unknown_capacity_low", CapacityLabelLow, 1000, 0, CapacityLabelHigh},

		// From High — dead-band keeps it in High until fill >= 0.50.
		{"high_stays_at_0", CapacityLabelHigh, 0, 100, CapacityLabelHigh},
		{"high_stays_at_44_pct", CapacityLabelHigh, 44, 100, CapacityLabelHigh},
		{"high_stays_at_49_pct", CapacityLabelHigh, 49, 100, CapacityLabelHigh},
		{"high_exits_at_50_pct", CapacityLabelHigh, 50, 100, CapacityLabelMedium},
		{"high_to_low_at_80_pct", CapacityLabelHigh, 80, 100, CapacityLabelLow},

		// From Medium — must drop below 0.45 to enter High, must reach
		// 0.80 to exit to Low.
		{"medium_at_45_pct_holds", CapacityLabelMedium, 45, 100, CapacityLabelMedium},
		{"medium_at_44_pct_returns_high", CapacityLabelMedium, 44, 100, CapacityLabelHigh},
		{"medium_at_79_pct_holds", CapacityLabelMedium, 79, 100, CapacityLabelMedium},
		{"medium_at_80_pct_to_low", CapacityLabelMedium, 80, 100, CapacityLabelLow},

		// From Low — must drop below 0.75 to leave Low.
		{"low_at_80_pct_stays", CapacityLabelLow, 80, 100, CapacityLabelLow},
		{"low_at_75_pct_stays", CapacityLabelLow, 75, 100, CapacityLabelLow},
		{"low_at_74_pct_to_medium", CapacityLabelLow, 74, 100, CapacityLabelMedium},
		{"low_at_44_pct_to_high", CapacityLabelLow, 44, 100, CapacityLabelHigh},

		// Unknown / cold-start current label uses the enter thresholds only.
		{"unset_at_0_to_high", "", 0, 100, CapacityLabelHigh},
		{"unset_at_50_to_medium", "", 50, 100, CapacityLabelMedium},
		{"unset_at_80_to_low", "", 80, 100, CapacityLabelLow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeCandidateLabel(tt.current, tt.count, tt.max)
			if got != tt.want {
				t.Fatalf("computeCandidateLabel(%q, %d, %d) = %q, want %q",
					tt.current, tt.count, tt.max, got, tt.want)
			}
		})
	}
}

// TestDecideHysteresis_StableNoPending — when the candidate matches the
// currently published label, any in-flight pending fields must be cleared
// (a transient candidate that didn't survive the hold-down window).
func TestDecideHysteresis_StableNoPending(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-30 * time.Second)
	pending := CapacityLabelMedium

	d := decideHysteresis(CapacityLabelHigh, &pending, &since, CapacityLabelHigh, now, 60*time.Second)

	if d.Promoted {
		t.Fatalf("expected no promotion, got Promoted=true")
	}
	if d.NewLabel != CapacityLabelHigh {
		t.Fatalf("expected NewLabel=high, got %q", d.NewLabel)
	}
	if d.NewPending != nil || d.NewPendingSince != nil {
		t.Fatalf("expected pending cleared, got pending=%v since=%v", d.NewPending, d.NewPendingSince)
	}
}

// TestDecideHysteresis_NewCandidateStartsTimer — first observation of a
// candidate distinct from both current and pending starts a new hold-down
// window stamped at `now`. The label itself does not change yet.
func TestDecideHysteresis_NewCandidateStartsTimer(t *testing.T) {
	now := time.Now().UTC()

	d := decideHysteresis(CapacityLabelHigh, nil, nil, CapacityLabelMedium, now, 60*time.Second)

	if d.Promoted {
		t.Fatalf("expected no promotion on first candidate observation")
	}
	if d.NewLabel != CapacityLabelHigh {
		t.Fatalf("expected NewLabel still high, got %q", d.NewLabel)
	}
	if d.NewPending == nil || *d.NewPending != CapacityLabelMedium {
		t.Fatalf("expected pending=medium, got %v", d.NewPending)
	}
	if d.NewPendingSince == nil || !d.NewPendingSince.Equal(now) {
		t.Fatalf("expected pending_since=%v, got %v", now, d.NewPendingSince)
	}
}

// TestDecideHysteresis_CandidateChangesResetsTimer — if an in-flight pending
// candidate is replaced by a different candidate before the hold-down elapses,
// the timer restarts at the new observation. The previous pending is discarded.
func TestDecideHysteresis_CandidateChangesResetsTimer(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-30 * time.Second)
	pending := CapacityLabelMedium

	d := decideHysteresis(CapacityLabelHigh, &pending, &since, CapacityLabelLow, now, 60*time.Second)

	if d.Promoted {
		t.Fatalf("expected no promotion on candidate change")
	}
	if d.NewPending == nil || *d.NewPending != CapacityLabelLow {
		t.Fatalf("expected pending=low, got %v", d.NewPending)
	}
	if d.NewPendingSince == nil || !d.NewPendingSince.Equal(now) {
		t.Fatalf("expected pending_since reset to %v, got %v", now, d.NewPendingSince)
	}
}

// TestDecideHysteresis_BelowHoldDownNoPromotion — same candidate observed
// repeatedly inside the hold-down window must NOT promote. The existing
// pending fields are preserved verbatim so subsequent heartbeats can keep
// accumulating against the same timer.
func TestDecideHysteresis_BelowHoldDownNoPromotion(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-30 * time.Second) // 30s into a 60s hold-down
	pending := CapacityLabelMedium

	d := decideHysteresis(CapacityLabelHigh, &pending, &since, CapacityLabelMedium, now, 60*time.Second)

	if d.Promoted {
		t.Fatalf("expected no promotion at 30s into 60s hold-down")
	}
	if d.NewLabel != CapacityLabelHigh {
		t.Fatalf("expected NewLabel still high, got %q", d.NewLabel)
	}
	if d.NewPending == nil || *d.NewPending != CapacityLabelMedium {
		t.Fatalf("expected pending preserved, got %v", d.NewPending)
	}
	if d.NewPendingSince == nil || !d.NewPendingSince.Equal(since) {
		t.Fatalf("expected pending_since preserved as %v, got %v", since, d.NewPendingSince)
	}
}

// TestDecideHysteresis_AtHoldDownPromotes — the exact moment `now -
// pendingSince == holdDown` is the boundary where promotion fires. Using
// >= for the comparison ensures the equality case is included.
func TestDecideHysteresis_AtHoldDownPromotes(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-60 * time.Second)
	pending := CapacityLabelMedium

	d := decideHysteresis(CapacityLabelHigh, &pending, &since, CapacityLabelMedium, now, 60*time.Second)

	if !d.Promoted {
		t.Fatalf("expected promotion at exact hold-down boundary")
	}
	if d.NewLabel != CapacityLabelMedium {
		t.Fatalf("expected NewLabel=medium, got %q", d.NewLabel)
	}
	if d.NewPending != nil || d.NewPendingSince != nil {
		t.Fatalf("expected pending cleared post-promotion, got %v / %v", d.NewPending, d.NewPendingSince)
	}
}

// TestDecideHysteresis_BeyondHoldDownPromotes — same as the boundary case
// but with the candidate stable for well beyond the hold-down window.
func TestDecideHysteresis_BeyondHoldDownPromotes(t *testing.T) {
	now := time.Now().UTC()
	since := now.Add(-5 * time.Minute)
	pending := CapacityLabelLow

	d := decideHysteresis(CapacityLabelMedium, &pending, &since, CapacityLabelLow, now, 60*time.Second)

	if !d.Promoted {
		t.Fatalf("expected promotion long past hold-down")
	}
	if d.NewLabel != CapacityLabelLow {
		t.Fatalf("expected NewLabel=low, got %q", d.NewLabel)
	}
}
