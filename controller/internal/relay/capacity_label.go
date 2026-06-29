package relay

import "time"

// Capacity tier labels published in LabelledRelayList. Persisted as TEXT
// in relays.capacity_label and relays.pending_capacity_label (see migration
// 023_relay_capacity_label.sql).
const (
	CapacityLabelHigh   = "high"   // fill_ratio enter < 0.45 / exit >= 0.50 — Tier 1
	CapacityLabelMedium = "medium" // fill_ratio enter < 0.75 / exit >= 0.80 — Tier 2
	CapacityLabelLow    = "low"    // fill_ratio >= 0.80 — Exhausted; dropped from snapshot
)

// Dead-band thresholds from ADR-016 §"Capacity Tiers". Enter is the value
// fill_ratio must fall below to gain a tier; exit is the value it must reach
// to leave one. The asymmetry between enter and exit is the dead-band that
// prevents oscillation on the boundary.
const (
	tier1EnterFill = 0.45
	tier1ExitFill  = 0.50
	tier2EnterFill = 0.75
	tier2ExitFill  = 0.80
)

// hysteresisDecision is the pure output of the hold-down state machine for
// a single heartbeat. It is consumed by EvaluateCapacityLabel to write the
// resulting state back to the relays row.
type hysteresisDecision struct {
	NewLabel        string
	NewPending      *string
	NewPendingSince *time.Time
	Promoted        bool
}

// decideHysteresis runs the ADR-016 hold-down state machine for one heartbeat
// observation. It is pure (no DB, no clock) so callers pass in `now`. The
// rules are:
//
//   - candidate matches current: clear any in-flight pending fields.
//   - candidate is new (no in-flight pending, or pending differs): start a
//     new hold-down window stamped at `now`.
//   - candidate matches the in-flight pending and `now - pendingSince` >=
//     holdDown: promote — clear pending, return Promoted=true.
//   - otherwise (still inside hold-down): preserve the existing pending.
func decideHysteresis(current string, pending *string, pendingSince *time.Time, candidate string, now time.Time, holdDown time.Duration) hysteresisDecision {
	if candidate == current {
		return hysteresisDecision{NewLabel: current}
	}
	if pending == nil || *pending != candidate {
		c := candidate
		t := now
		return hysteresisDecision{NewLabel: current, NewPending: &c, NewPendingSince: &t}
	}
	if pendingSince != nil && now.Sub(*pendingSince) >= holdDown {
		return hysteresisDecision{NewLabel: candidate, Promoted: true}
	}
	return hysteresisDecision{NewLabel: current, NewPending: pending, NewPendingSince: pendingSince}
}

// computeCandidateLabel returns the tier label a relay should hold given its
// currently published label and its latest reported capacity. The result is
// the candidate — the actual published capacity_label only changes after
// the hold-down window has elapsed (see EvaluateCapacityLabel).
//
// max == 0 means the relay has not reported its configured ceiling
// (RELAY_MAX_CONNECTIONS unset). Treat the relay as fully available so it
// remains eligible until real capacity data arrives.
func computeCandidateLabel(current string, count, max uint32) string {
	if max == 0 {
		return CapacityLabelHigh
	}
	fill := float64(count) / float64(max)

	switch current {
	case CapacityLabelHigh:
		if fill >= tier2ExitFill {
			return CapacityLabelLow
		}
		if fill >= tier1ExitFill {
			return CapacityLabelMedium
		}
		return CapacityLabelHigh
	case CapacityLabelMedium:
		if fill >= tier2ExitFill {
			return CapacityLabelLow
		}
		if fill < tier1EnterFill {
			return CapacityLabelHigh
		}
		return CapacityLabelMedium
	case CapacityLabelLow:
		if fill < tier1EnterFill {
			return CapacityLabelHigh
		}
		if fill < tier2EnterFill {
			return CapacityLabelMedium
		}
		return CapacityLabelLow
	default:
		// Unset / unrecognised current label — treat as cold start.
		if fill < tier1EnterFill {
			return CapacityLabelHigh
		}
		if fill < tier2EnterFill {
			return CapacityLabelMedium
		}
		return CapacityLabelLow
	}
}
