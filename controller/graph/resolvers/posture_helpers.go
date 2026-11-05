package resolvers

// posture_helpers.go — shared conversion helpers used by posture resolvers.
//
// Lives in a separate file from posture.resolvers.go so gqlgen does NOT move this
// code around or wrap it in deletion-warning comments when regenerating resolvers.
// gqlgen only touches files it generates; hand-written files here are untouched.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/posture"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

func postureProfileToGQL(profile *posture.Profile) *graph.DeviceProfile {
	return &graph.DeviceProfile{
		ID:          profile.ID.String(),
		Name:        profile.Name,
		ManualTrust: profile.ManualTrustEnabled,
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

// policyRefNames lists policy names for audit details.
func policyRefNames(refs []posture.PolicyRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Name
	}
	return out
}

// toResourcePolicyRefs maps the store's policy references to the API's,
// never nil so an empty list serializes as [].
func toResourcePolicyRefs(refs []posture.PolicyRef) []*graph.ResourcePolicyRef {
	out := make([]*graph.ResourcePolicyRef, len(refs))
	for i, r := range refs {
		out[i] = &graph.ResourcePolicyRef{ID: r.ID.String(), Name: r.Name}
	}
	return out
}

// PENDING-16 helpers.

func policyWorkspace(ctx context.Context) (uuid.UUID, error) {
	ws, err := uuid.Parse(tenant.MustGet(ctx).TenantID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid workspace id: %w", err)
	}
	return ws, nil
}

// parsePolicyID parses an id from the API; a malformed one is answered like a
// missing one by the caller.
func parsePolicyID(id string) (uuid.UUID, bool) {
	u, err := uuid.Parse(id)
	return u, err == nil
}

func parseIDs(ids []string) ([]uuid.UUID, bool) {
	out := make([]uuid.UUID, 0, len(ids))
	for _, s := range ids {
		u, err := uuid.Parse(s)
		if err != nil {
			return nil, false
		}
		out = append(out, u)
	}
	return out, true
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

func resourcePolicyToGQL(p *posture.ResourcePolicy) *graph.ResourcePolicy {
	return &graph.ResourcePolicy{
		ID:            p.ID.String(),
		Name:          p.Name,
		Description:   p.Description,
		IsDefault:     p.IsDefault,
		ResourceCount: p.ResourceCount,
	}
}
