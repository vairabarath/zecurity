package resolvers

// posture_helpers.go — shared conversion helpers used by posture resolvers.
//
// Lives in a separate file from posture.resolvers.go so gqlgen does NOT move this
// code around or wrap it in deletion-warning comments when regenerating resolvers.
// gqlgen only touches files it generates; hand-written files here are untouched.

import (
	"strings"

	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/posture"
)

func postureProfileToGQL(profile *posture.Profile) *graph.DeviceProfile {
	return &graph.DeviceProfile{
		ID:   profile.ID.String(),
		Name: profile.Name,
		Mode: graph.DeviceProfileMode(strings.ToUpper(profile.Mode)),
	}
}
