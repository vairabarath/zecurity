package posture

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEvaluateProfileStatusMatrix(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		status           string
		allowUnsupported bool
		want             bool
	}{
		{StatusPass, false, true},
		{StatusPass, true, true},
		{StatusFail, false, false},
		{StatusFail, true, false},
		{StatusUnsupported, false, false},
		{StatusUnsupported, true, true},
		{StatusUnknown, false, false},
		{StatusUnknown, true, false},
		{StatusError, false, false},
		{StatusError, true, false},
	}

	for _, tt := range tests {
		name := tt.status + "/allow=" + strings.ToLower(string(rune('0'+boolInt(tt.allowUnsupported))))
		t.Run(name, func(t *testing.T) {
			result := EvaluateProfile(
				now,
				now,
				Profile{ID: uuid.New(), Mode: ModeEnforce, Revision: 4},
				[]Requirement{{CheckID: CheckLUKS, AllowUnsupported: tt.allowUnsupported}},
				map[string]Observation{CheckLUKS: {CheckID: CheckLUKS, Status: tt.status}},
			)
			if result.Satisfied != tt.want {
				t.Fatalf("Satisfied = %v, want %v; reason=%q", result.Satisfied, tt.want, result.Reason)
			}
			if result.Revision != 4 {
				t.Fatalf("Revision = %d, want 4", result.Revision)
			}
		})
	}
}

func TestEvaluateProfileFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	profile := Profile{ID: uuid.New(), Mode: ModeEnforce, Revision: 1}

	tests := []struct {
		name         string
		receivedAt   time.Time
		requirements []Requirement
		observations map[string]Observation
		reason       string
	}{
		{
			name:         "empty profile",
			receivedAt:   now,
			requirements: nil,
			observations: map[string]Observation{},
			reason:       "no requirements",
		},
		{
			name:         "stale pass",
			receivedAt:   now.Add(-MaxReportAge - time.Second),
			requirements: []Requirement{{CheckID: CheckLUKS}},
			observations: map[string]Observation{CheckLUKS: {CheckID: CheckLUKS, Status: StatusPass}},
			reason:       "stale",
		},
		{
			name:         "missing observation",
			receivedAt:   now,
			requirements: []Requirement{{CheckID: CheckLUKS}},
			observations: map[string]Observation{},
			reason:       "missing required check",
		},
		{
			name:       "and semantics",
			receivedAt: now,
			requirements: []Requirement{
				{CheckID: CheckLUKS},
				{CheckID: CheckFirewall},
			},
			observations: map[string]Observation{
				CheckLUKS:     {CheckID: CheckLUKS, Status: StatusPass},
				CheckFirewall: {CheckID: CheckFirewall, Status: StatusFail},
			},
			reason: CheckFirewall,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := EvaluateProfile(now, tt.receivedAt, profile, tt.requirements, tt.observations)
			if result.Satisfied {
				t.Fatalf("Satisfied = true, reason=%q", result.Reason)
			}
			if !strings.Contains(result.Reason, tt.reason) {
				t.Fatalf("reason = %q, want substring %q", result.Reason, tt.reason)
			}
		})
	}
}

func TestResourceSatisfiedUsesEnforceOnlyOR(t *testing.T) {
	tests := []struct {
		name    string
		results []ProfileResult
		want    bool
	}{
		{"no enforce profiles", nil, true},
		{"audit failure ignored", []ProfileResult{{Mode: ModeAudit, Satisfied: false}}, true},
		{"audit pass cannot mask enforce failure", []ProfileResult{{Mode: ModeAudit, Satisfied: true}, {Mode: ModeEnforce, Satisfied: false}}, false},
		{"one enforce winner", []ProfileResult{{Mode: ModeEnforce, Satisfied: false}, {Mode: ModeEnforce, Satisfied: true}}, true},
		{"all enforce fail", []ProfileResult{{Mode: ModeEnforce, Satisfied: false}, {Mode: ModeEnforce, Satisfied: false}}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResourceSatisfied(tt.results); got != tt.want {
				t.Fatalf("ResourceSatisfied() = %v, want %v", got, tt.want)
			}
		})
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
