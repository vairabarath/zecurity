package resolvers

import (
	"context"
	"fmt"
	"strings"

	"github.com/99designs/gqlgen/graphql"
	"github.com/yourorg/ztna/controller/graph"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// HasRole implements the @hasRole GraphQL directive. It runs before the field's
// resolver and allows the call only if the caller's JWT role (from TenantContext,
// injected by AuthMiddleware) matches one of the supplied roles.
//
// This is the GraphQL-layer analogue of middleware.RequireRole: route-level HTTP
// middleware cannot gate individual operations on the single /graphql endpoint,
// but a field directive can. Authentication itself is still enforced upstream by
// AuthMiddleware — this only checks authorization. We use tenant.Get (not MustGet)
// so a directive accidentally placed on a public field returns a clean error
// instead of panicking.
func HasRole(ctx context.Context, _ any, next graphql.Resolver, roles []graph.Role) (any, error) {
	tc, ok := tenant.Get(ctx)
	if !ok {
		return nil, fmt.Errorf("unauthenticated")
	}
	for _, r := range roles {
		if strings.EqualFold(string(r), tc.Role) { // graph.Role is "ADMIN"; tc.Role is "admin"
			return next(ctx)
		}
	}
	return nil, fmt.Errorf("forbidden: requires one of roles %v", roles)
}
