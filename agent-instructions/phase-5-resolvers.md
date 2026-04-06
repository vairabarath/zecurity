# Phase 5 — Resolvers

Thin resolvers that delegate to services. All business logic lives outside these files.

---

## File 1: `controller/graph/resolvers/query.resolvers.go`

**Path:** `controller/graph/resolvers/query.resolvers.go`

This file implements the `me` and `workspace` queries.

```go
package resolvers

import (
	"context"
	"fmt"

	"github.com/yourorg/ztna/controller/internal/models"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

func (r *queryResolver) Me(ctx context.Context) (*models.User, error) {
	tc := tenant.MustGet(ctx)

	var u models.User
	err := r.TenantDB.QueryRow(ctx,
		`SELECT id, tenant_id, email, provider, provider_sub,
		        role, status, last_login_at, created_at, updated_at
		 FROM users
		 WHERE id        = $1
		   AND tenant_id = $2
		   AND status    = 'active'`,
		tc.UserID, tc.TenantID,
	).Scan(
		&u.ID, &u.TenantID, &u.Email,
		&u.Provider, &u.ProviderSub,
		&u.Role, &u.Status, &u.LastLoginAt,
		&u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("me: %w", err)
	}
	return &u, nil
}

func (r *queryResolver) Workspace(ctx context.Context) (*models.Workspace, error) {
	tc := tenant.MustGet(ctx)

	var ws models.Workspace
	err := r.TenantDB.QueryRow(ctx,
		`SELECT id, slug, name, status, ca_cert_pem, created_at, updated_at
		 FROM workspaces
		 WHERE id = $1`,
		tc.TenantID,
	).Scan(
		&ws.ID, &ws.Slug, &ws.Name,
		&ws.Status, &ws.CACertPEM,
		&ws.CreatedAt, &ws.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("workspace: %w", err)
	}
	return &ws, nil
}
```

**Note:** `Workspace` query does not add `AND tenant_id = $x` because
`id` in the workspaces table IS the tenant_id — it is the root table.
`Me` query adds `AND tenant_id = $2` as defence-in-depth even though
`id` alone uniquely identifies the user.

---

## File 2: `controller/graph/resolvers/auth.resolvers.go`

**Path:** `controller/graph/resolvers/auth.resolvers.go`

This file implements the `initiateAuth` mutation. Intentionally thin — delegates to `AuthService`.

```go
package resolvers

import (
	"context"

	"github.com/yourorg/ztna/controller/graph/model"
)

// InitiateAuth is intentionally thin.
// All logic lives in internal/auth/ (Member 2's territory).
// This file is just the GraphQL entry point.
func (r *mutationResolver) InitiateAuth(
	ctx context.Context, provider string,
) (*model.AuthInitPayload, error) {
	return r.AuthService.InitiateAuth(ctx, provider)
}
```

---

## Verification Checklist

```
[ ] me resolver returns user scoped to tenant_id from JWT
[ ] workspace resolver returns workspace scoped to tenant_id from JWT
[ ] initiateAuth resolver delegates to AuthService without error
```
