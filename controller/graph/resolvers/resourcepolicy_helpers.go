package resolvers

// resourcepolicy_helpers.go — shared conversion helpers for the Resource Policy
// resolvers (PENDING-16 Phase 3).
//
// Lives in a separate file from resourcepolicy.resolvers.go so gqlgen does NOT
// move this code around or wrap it in deletion-warning comments when
// regenerating resolvers. gqlgen only touches files it generates.

import (
	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/posture"
)

// resourcePolicyToGQL converts a stored resource policy to its GraphQL shape.
//
// DeviceProfiles and Resources are deliberately left nil: both are
// forceResolver fields, so the field resolvers fetch them with a targeted
// lookup only when the query actually selects them.
func resourcePolicyToGQL(policy *posture.ResourcePolicy) *graph.ResourcePolicy {
	return &graph.ResourcePolicy{
		ID:        policy.ID.String(),
		Name:      policy.Name,
		CreatedAt: fmtTime(policy.CreatedAt),
		UpdatedAt: fmtTime(policy.UpdatedAt),
	}
}
