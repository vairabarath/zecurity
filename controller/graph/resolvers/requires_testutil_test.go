package resolvers

import (
	"context"

	"github.com/yourorg/ztna/controller/internal/rbac"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// ctxRoleSource is a test RoleSource that treats the TenantContext role as the
// live role, for tests that build a server without a database-backed Checker.
type ctxRoleSource struct{}

func (ctxRoleSource) Role(ctx context.Context, _, _ string) (string, error) {
	tc, ok := tenant.Get(ctx)
	if !ok || !rbac.ValidRole(tc.Role) {
		return "", rbac.ErrNoAccess
	}
	return tc.Role, nil
}

// testRequires is the @requires directive backed by ctxRoleSource.
var testRequires = NewRequires(ctxRoleSource{})
