package bootstrap

import (
	"context"

	"github.com/yourorg/ztna/controller/internal/identity"
)

// RunInvitedJoin exposes the invited-join transaction to the external tests,
// so each single-use guard of the first-admin invitation can be hit directly
// (the race that needs them is otherwise timing-dependent).
func (s *Service) RunInvitedJoin(ctx context.Context, email string, in identity.ProvisionInput, workspaceID, role string, firstAdmin bool) (*identity.PrincipalCore, error) {
	return s.runInvitedUserTransaction(ctx, email, in, workspaceID, role, firstAdmin)
}
