package policy

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yourorg/ztna/controller/internal/posture"
)

func TestApplyPosture_EnforcedProfilesUseOR(t *testing.T) {
	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	deviceID := uuid.New()
	profileOne := posture.Profile{ID: uuid.New(), Revision: 3, Mode: posture.ModeEnforce}
	profileTwo := posture.Profile{ID: uuid.New(), Revision: 7, Mode: posture.ModeEnforce}
	receivedAt := now.Add(-time.Minute)

	allowed, validUntil, gated := applyPosture(
		now,
		map[uuid.UUID]string{deviceID: "spiffe://example/client/device-1"},
		[]posture.Profile{profileOne, profileTwo},
		map[uuid.UUID][]posture.Evaluation{
			deviceID: {
				{
					DeviceID:         deviceID,
					ProfileID:        profileOne.ID,
					Satisfied:        false,
					ProfileRevision:  profileOne.Revision,
					ReportReceivedAt: &receivedAt,
				},
				{
					DeviceID:         deviceID,
					ProfileID:        profileTwo.ID,
					Satisfied:        true,
					ProfileRevision:  profileTwo.Revision,
					ReportReceivedAt: &receivedAt,
				},
			},
		},
	)

	if !gated {
		t.Fatal("expected posture gating to be enabled")
	}
	if len(allowed) != 1 || allowed[0] != "spiffe://example/client/device-1" {
		t.Fatalf("allowed SPIFFE IDs = %v, want the device allowed by profile two", allowed)
	}
	if !validUntil.Equal(receivedAt.Add(posture.MaxReportAge)) {
		t.Fatalf("valid until = %v, want %v", validUntil, receivedAt.Add(posture.MaxReportAge))
	}
}

func TestApplyPosture_FailsClosedForInvalidEvaluations(t *testing.T) {
	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	deviceID := uuid.New()
	profile := posture.Profile{ID: uuid.New(), Revision: 4, Mode: posture.ModeEnforce}
	receivedAt := now.Add(-time.Minute)
	spiffe := "spiffe://example/client/device-1"

	tests := []struct {
		name       string
		evaluation posture.Evaluation
	}{
		{
			name: "unsatisfied",
			evaluation: posture.Evaluation{
				Satisfied:        false,
				ProfileRevision:  profile.Revision,
				ReportReceivedAt: &receivedAt,
			},
		},
		{
			name: "revision mismatch",
			evaluation: posture.Evaluation{
				Satisfied:        true,
				ProfileRevision:  profile.Revision - 1,
				ReportReceivedAt: &receivedAt,
			},
		},
		{
			name: "missing report timestamp",
			evaluation: posture.Evaluation{
				Satisfied:       true,
				ProfileRevision: profile.Revision,
			},
		},
		{
			name: "expired report",
			evaluation: posture.Evaluation{
				Satisfied:        true,
				ProfileRevision:  profile.Revision,
				ReportReceivedAt: func() *time.Time { value := now.Add(-posture.MaxReportAge - time.Second); return &value }(),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.evaluation.DeviceID = deviceID
			test.evaluation.ProfileID = profile.ID

			allowed, validUntil, gated := applyPosture(
				now,
				map[uuid.UUID]string{deviceID: spiffe},
				[]posture.Profile{profile},
				map[uuid.UUID][]posture.Evaluation{deviceID: {test.evaluation}},
			)

			if !gated {
				t.Fatal("expected posture gating to be enabled")
			}
			if len(allowed) != 0 {
				t.Fatalf("allowed SPIFFE IDs = %v, want none", allowed)
			}
			if !validUntil.IsZero() {
				t.Fatalf("valid until = %v, want zero for denied device", validUntil)
			}
		})
	}
}

func TestApplyPosture_AuditOnlyProfilesDoNotGate(t *testing.T) {
	deviceID := uuid.New()
	allowed, validUntil, gated := applyPosture(
		time.Now(),
		map[uuid.UUID]string{deviceID: "spiffe://example/client/device-1"},
		nil,
		nil,
	)

	if gated {
		t.Fatal("audit-only/no-enforce profile set unexpectedly gated access")
	}
	if len(allowed) != 1 {
		t.Fatalf("allowed SPIFFE IDs = %v, want one", allowed)
	}
	if !validUntil.IsZero() {
		t.Fatalf("valid until = %v, want zero for ungated access", validUntil)
	}
}

func TestApplyPosture_UsesLatestPassingProfileExpiry(t *testing.T) {
	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	deviceID := uuid.New()
	profileOne := posture.Profile{ID: uuid.New(), Revision: 1, Mode: posture.ModeEnforce}
	profileTwo := posture.Profile{ID: uuid.New(), Revision: 1, Mode: posture.ModeEnforce}
	firstReport := now.Add(-2 * time.Minute)
	secondReport := now.Add(-time.Minute)

	_, validUntil, _ := applyPosture(
		now,
		map[uuid.UUID]string{deviceID: "spiffe://example/client/device-1"},
		[]posture.Profile{profileOne, profileTwo},
		map[uuid.UUID][]posture.Evaluation{deviceID: {
			{ProfileID: profileOne.ID, Satisfied: true, ProfileRevision: 1, ReportReceivedAt: &firstReport},
			{ProfileID: profileTwo.ID, Satisfied: true, ProfileRevision: 1, ReportReceivedAt: &secondReport},
		}},
	)

	want := secondReport.Add(posture.MaxReportAge)
	if !validUntil.Equal(want) {
		t.Fatalf("valid until = %v, want latest passing expiry %v", validUntil, want)
	}
}
