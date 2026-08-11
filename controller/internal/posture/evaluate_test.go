package posture

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestEvaluateProfileStatusMatrix(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		status           ObservationStatus
		allowUnsupported bool
		want             bool
	}{
		{ObservationStatusPass, false, true},
		{ObservationStatusPass, true, true},
		{ObservationStatusFail, false, false},
		{ObservationStatusFail, true, false},
		{ObservationStatusUnsupported, false, false},
		{ObservationStatusUnsupported, true, true},
		{ObservationStatusUnknown, false, false},
		{ObservationStatusUnknown, true, false},
		{ObservationStatusError, false, false},
		{ObservationStatusError, true, false},
	}

	for _, tt := range tests {
		name := fmt.Sprintf("%s/allow=%d", tt.status, boolInt(tt.allowUnsupported))
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
			observations: map[string]Observation{CheckLUKS: {CheckID: CheckLUKS, Status: ObservationStatusPass}},
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
				CheckLUKS:     {CheckID: CheckLUKS, Status: ObservationStatusPass},
				CheckFirewall: {CheckID: CheckFirewall, Status: ObservationStatusFail},
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

type recordingNotifier struct {
	calls int
	err   error
}

func (n *recordingNotifier) NotifyPolicyChange(
	context.Context,
	string,
) error {
	n.calls++
	return n.err
}
func TestReevaluateDevicesContinuesAfterPartialFailure(t *testing.T) {
	workspaceID := uuid.New()

	deviceA := uuid.New()
	deviceB := uuid.New()
	deviceC := uuid.New()
	deviceD := uuid.New()

	notifier := &recordingNotifier{}
	evaluator := &Evaluator{
		notifier: notifier,
	}

	var evaluated []uuid.UUID

	err := evaluator.reevaluateDevices(
		context.Background(),
		workspaceID,
		[]uuid.UUID{
			deviceA,
			deviceB,
			deviceC,
			deviceD,
		},
		func(
			_ context.Context,
			_ uuid.UUID,
			deviceID uuid.UUID,
		) (bool, error) {
			evaluated = append(evaluated, deviceID)

			switch deviceID {
			case deviceC:
				return false, errors.New("database unavailable")
			case deviceD:
				return true, nil
			default:
				return false, nil
			}
		},
	)

	want := []uuid.UUID{
		deviceA,
		deviceB,
		deviceC,
		deviceD,
	}

	if !reflect.DeepEqual(evaluated, want) {
		t.Fatalf("evaluated = %v, want %v", evaluated, want)
	}

	if err == nil {
		t.Fatal("expected aggregated partial-failure error")
	}

	if !strings.Contains(err.Error(), deviceC.String()) {
		t.Fatalf(
			"error %q does not contain failed device ID %s",
			err,
			deviceC,
		)
	}

	if !strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf(
			"error %q does not contain database failure",
			err,
		)
	}

	if notifier.calls != 1 {
		t.Fatalf(
			"notification calls = %d, want 1",
			notifier.calls,
		)
	}

}

func TestReevaluateDevicesNoPolicyChangeDoesNotNotify(t *testing.T) {
	workspaceID := uuid.New()

	deviceA := uuid.New()
	deviceB := uuid.New()
	deviceC := uuid.New()

	notifier := &recordingNotifier{}
	evaluator := &Evaluator{
		notifier: notifier,
	}

	err := evaluator.reevaluateDevices(
		context.Background(),
		workspaceID,
		[]uuid.UUID{
			deviceA,
			deviceB,
			deviceC,
		},
		func(
			_ context.Context,
			_ uuid.UUID,
			_ uuid.UUID,
		) (bool, error) {
			return false, nil
		},
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if notifier.calls != 0 {
		t.Fatalf(
			"notification calls = %d, want 0",
			notifier.calls,
		)
	}
}

func TestReevaluateDevicesAggregatesNotificationFailure(t *testing.T) {
	workspaceID := uuid.New()

	deviceA := uuid.New()
	deviceB := uuid.New()

	notifier := &recordingNotifier{
		err: errors.New("notification failed"),
	}

	evaluator := &Evaluator{
		notifier: notifier,
	}

	err := evaluator.reevaluateDevices(
		context.Background(),
		workspaceID,
		[]uuid.UUID{
			deviceA,
			deviceB,
		},
		func(
			_ context.Context,
			_ uuid.UUID,
			deviceID uuid.UUID,
		) (bool, error) {
			switch deviceID {
			case deviceA:
				return true, nil
			case deviceB:
				return false, errors.New("database unavailable")
			default:
				return false, nil
			}
		},
	)

	if err == nil {
		t.Fatal("expected aggregated error")
	}

	if notifier.calls != 1 {
		t.Fatalf(
			"notification calls = %d, want 1",
			notifier.calls,
		)
	}

	if !strings.Contains(err.Error(), "database unavailable") {
		t.Fatalf(
			"error %q does not contain device failure",
			err,
		)
	}

	if !strings.Contains(err.Error(), "notification failed") {
		t.Fatalf(
			"error %q does not contain notification failure",
			err,
		)
	}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
