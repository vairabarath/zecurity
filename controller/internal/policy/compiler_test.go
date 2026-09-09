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

// TestApplyPosture_AuditOnlyProfilesDoNotGate documents applyPosture's contract:
// it gates only on the profiles it is handed, so an empty list means Any Device.
//
// The contract still holds after PENDING-16, but the reason a profile is absent
// changed: it used to be "the profile's mode is audit", filtered out by the
// compiler; it is now "the resource's Resource Policy does not reference it".
// The test passes nil profiles either way, which is why it needed no edit.
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

// TestApplyPosture_PolicyProfileMatrix is the Phase 5 verification matrix: the
// six outcomes the Resource Policy path must produce.
//
// The compiler now hands applyPosture the profiles a resource's Resource Policy
// references (PENDING-16), so "how many profiles" here means "how many profiles
// are attached to the policy". Profile mode is irrelevant -- attachment is what
// gates -- but the literals keep ModeEnforce because the compiler no longer
// filters on it and applyPosture never reads it.
func TestApplyPosture_PolicyProfileMatrix(t *testing.T) {
	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	spiffe := "spiffe://example/client/device-1"

	profileA := posture.Profile{ID: uuid.New(), Revision: 2, Mode: posture.ModeEnforce}
	profileB := posture.Profile{ID: uuid.New(), Revision: 5, Mode: posture.ModeEnforce}

	// satisfied builds a fresh, current-revision evaluation for one profile.
	satisfied := func(deviceID uuid.UUID, profile posture.Profile, ok bool, receivedAt *time.Time) posture.Evaluation {
		return posture.Evaluation{
			DeviceID:         deviceID,
			ProfileID:        profile.ID,
			Satisfied:        ok,
			ProfileRevision:  profile.Revision,
			ReportReceivedAt: receivedAt,
		}
	}

	tests := []struct {
		name        string
		profiles    []posture.Profile
		profileAOK  bool
		profileBOK  bool
		wantAllowed bool
		wantGated   bool
	}{
		{
			// The row that must never regress into empty-deny: a policy with no
			// profiles is "Any Device", not "no devices".
			name:        "zero profiles is Any Device",
			profiles:    nil,
			wantAllowed: true,
			wantGated:   false,
		},
		{
			name:        "one profile satisfied allows",
			profiles:    []posture.Profile{profileA},
			profileAOK:  true,
			wantAllowed: true,
			wantGated:   true,
		},
		{
			name:        "one profile unsatisfied denies",
			profiles:    []posture.Profile{profileA},
			profileAOK:  false,
			wantAllowed: false,
			wantGated:   true,
		},
		{
			name:        "two profiles first satisfied allows (OR)",
			profiles:    []posture.Profile{profileA, profileB},
			profileAOK:  true,
			profileBOK:  false,
			wantAllowed: true,
			wantGated:   true,
		},
		{
			name:        "two profiles second satisfied allows (OR)",
			profiles:    []posture.Profile{profileA, profileB},
			profileAOK:  false,
			profileBOK:  true,
			wantAllowed: true,
			wantGated:   true,
		},
		{
			name:        "two profiles both unsatisfied denies",
			profiles:    []posture.Profile{profileA, profileB},
			profileAOK:  false,
			profileBOK:  false,
			wantAllowed: false,
			wantGated:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deviceID := uuid.New()
			receivedAt := now.Add(-time.Minute)

			evaluations := map[uuid.UUID][]posture.Evaluation{}
			for _, profile := range tt.profiles {
				ok := tt.profileAOK
				if profile.ID == profileB.ID {
					ok = tt.profileBOK
				}
				evaluations[deviceID] = append(
					evaluations[deviceID],
					satisfied(deviceID, profile, ok, &receivedAt),
				)
			}

			allowed, _, gated := applyPosture(
				now,
				map[uuid.UUID]string{deviceID: spiffe},
				tt.profiles,
				evaluations,
			)

			if gated != tt.wantGated {
				t.Fatalf("gated = %v, want %v", gated, tt.wantGated)
			}
			if tt.wantAllowed {
				if len(allowed) != 1 || allowed[0] != spiffe {
					t.Fatalf("allowed = %v, want the device allowed", allowed)
				}
			} else if len(allowed) != 0 {
				t.Fatalf("allowed = %v, want the device denied", allowed)
			}
		})
	}
}
