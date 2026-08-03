package resolvers

// posture_helpers.go — shared conversion helpers used by posture resolvers.
//
// Lives in a separate file from posture.resolvers.go so gqlgen does NOT move this
// code around or wrap it in deletion-warning comments when regenerating resolvers.
// gqlgen only touches files it generates; hand-written files here are untouched.

import (
	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/posture"
	"strings"
	"time"
)

func postureProfileToGQL(profile *posture.Profile) *graph.DeviceProfile {
	return &graph.DeviceProfile{
		ID:   profile.ID.String(),
		Name: profile.Name,
		Mode: graph.DeviceProfileMode(strings.ToUpper(profile.Mode)),
	}
}

func postureObservationToGQL(obs posture.Observation) *graph.DevicePostureObservation {
	var collectorError *string
	if obs.Status == posture.ObservationStatusError {
		collectorError = obs.Detail
	}

	return &graph.DevicePostureObservation{
		CheckID:        obs.CheckID,
		Status:         graph.PostureCheckStatus(obs.Status),
		ObservedAt:     fmtTime(obs.CreatedAt),
		CollectorError: collectorError,
	}
}

func postureVisibilityListToGQL(
	items []posture.DevicePostureVisibility,
) []*graph.DevicePostureVisibility {
	result := make([]*graph.DevicePostureVisibility, 0, len(items))

	for _, item := range items {
		result = append(result, postureVisibilityToGQL(item))
	}

	return result
}

func postureVisibilityToGQL(
	item posture.DevicePostureVisibility,
) *graph.DevicePostureVisibility {
	var reportAge *int

	if item.ReportReceivedAt != nil {
		age := int(time.Since(*item.ReportReceivedAt).Seconds())
		if age < 0 {
			age = 0
		}
		reportAge = &age
	}

	observations := make([]*graph.DevicePostureObservation, 0, len(item.Observations))
	for _, obs := range item.Observations {
		observations = append(observations, postureObservationToGQL(obs))
	}

	return &graph.DevicePostureVisibility{
		DeviceID:         item.DeviceID.String(),
		DeviceName:       item.DeviceName,
		ProfileID:        item.ProfileID.String(),
		Satisfied:        item.Satisfied,
		Stale:            item.Stale,
		FailureReason:    item.FailureReason,
		EvaluatedAt:      fmtTime(item.EvaluatedAt),
		ReportReceivedAt: fmtTimePtr(item.ReportReceivedAt),
		ReportAgeSeconds: reportAge,
		Observations:     observations,
	}
}
