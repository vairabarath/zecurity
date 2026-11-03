package resolvers

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/posture"
)

func TestPostureObservationToGQL_ErrorCollectorError(t *testing.T) {
	now := time.Now().UTC()

	detail := "collector failed"

	obs := posture.Observation{
		CheckID:   posture.CheckFirewall,
		Status:    posture.ObservationStatusError,
		Detail:    &detail,
		CreatedAt: now,
	}

	got := postureObservationToGQL(obs)

	if got.CheckID != posture.CheckFirewall {
		t.Fatalf("unexpected check id: %q", got.CheckID)
	}

	if got.Status != graph.PostureCheckStatusError {
		t.Fatalf("unexpected status: %v", got.Status)
	}

	if got.CollectorError == nil {
		t.Fatal("expected collector error")
	}

	if *got.CollectorError != detail {
		t.Fatalf("unexpected collector error: %q", *got.CollectorError)
	}
}

func TestPostureObservationToGQL_NonErrorHidesCollectorError(t *testing.T) {
	now := time.Now().UTC()

	detail := "should not leak"

	obs := posture.Observation{
		CheckID:   posture.CheckFirewall,
		Status:    posture.ObservationStatusPass,
		Detail:    &detail,
		CreatedAt: now,
	}

	got := postureObservationToGQL(obs)

	if got.Status != graph.PostureCheckStatusPass {
		t.Fatalf("unexpected status: %v", got.Status)
	}

	if got.CollectorError != nil {
		t.Fatal("collector error should be nil")
	}
}

func TestPostureVisibilityToGQL(t *testing.T) {
	now := time.Now().UTC()
	reportTime := now.Add(-90 * time.Second)

	deviceID := uuid.New()
	profileID := uuid.New()

	item := posture.DevicePostureVisibility{
		DeviceID:         deviceID,
		DeviceName:       "laptop-01",
		ProfileID:        profileID,
		Satisfied:        true,
		Stale:            false,
		FailureReason:    nil,
		EvaluatedAt:      now,
		ReportReceivedAt: &reportTime,
		Observations: []posture.Observation{
			{
				CheckID:   posture.CheckFirewall,
				Status:    posture.ObservationStatusPass,
				CreatedAt: reportTime,
			},
		},
	}

	got := postureVisibilityToGQL(item)

	if got.DeviceID != deviceID.String() {
		t.Fatalf("unexpected device id: %q", got.DeviceID)
	}

	if got.ProfileID != profileID.String() {
		t.Fatalf("unexpected profile id: %q", got.ProfileID)
	}

	if got.DeviceName != "laptop-01" {
		t.Fatalf("unexpected device name: %q", got.DeviceName)
	}

	if !got.Satisfied {
		t.Fatal("expected satisfied=true")
	}

	if got.Stale {
		t.Fatal("expected stale=false")
	}

	if got.ReportReceivedAt == nil {
		t.Fatal("expected report received time")
	}

	if got.ReportAgeSeconds == nil {
		t.Fatal("expected report age")
	}

	if *got.ReportAgeSeconds < 80 || *got.ReportAgeSeconds > 100 {
		t.Fatalf("unexpected report age: %d", *got.ReportAgeSeconds)
	}

	if len(got.Observations) != 1 {
		t.Fatalf("expected 1 observation, got %d", len(got.Observations))
	}

	if got.Observations[0].Status != graph.PostureCheckStatusPass {
		t.Fatalf("unexpected observation status: %v", got.Observations[0].Status)
	}
}
