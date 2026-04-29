---
type: phase
status: planned
sprint: 7
member: M3
phase: Phase3-Role-Enforcement
depends_on:
  - M3-Phase1 (ClientService gRPC)
  - M3-Phase2 (Invitation API)
tags:
  - controller
  - auth
  - middleware
  - rbac
---

# M3 Phase 3 — Role Enforcement Middleware

---

## What You're Building

An HTTP middleware `RequireRole` that checks the caller's role from the JWT and rejects non-matching requests with `403 Forbidden`. Apply it to admin-only routes.

The GraphQL `createInvitation` resolver already has an inline role check from Phase 2 — this phase adds the HTTP layer protection.

---

## Files to Modify

### `controller/internal/auth/middleware.go` (ADD to existing or NEW file)

Check if `middleware.go` already exists. If it does, add `RequireRole` to it. If not, create it.

```go
package auth

import (
    "net/http"
    "strings"
)

// RequireRole returns a middleware that allows only users with one of the given roles.
// roles: "admin", "member", "viewer"
func RequireRole(roles ...string) func(http.Handler) http.Handler {
    allowed := make(map[string]bool, len(roles))
    for _, r := range roles {
        allowed[strings.ToLower(r)] = true
    }

    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            user := UserFromContext(r.Context())
            if user == nil {
                http.Error(w, `{"error":"unauthenticated"}`, http.StatusUnauthorized)
                return
            }
            if !allowed[strings.ToLower(user.Role)] {
                http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
                return
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

> `UserFromContext` must already exist (used in GraphQL resolvers). If it's in a different package, import accordingly.

---

## Files to Modify

### `cmd/server/main.go` — apply middleware to invitation create route

Change the plain route registration from Phase 2:
```go
// BEFORE (Phase 2):
r.Post("/api/invitations", inviteHandler.Create)

// AFTER (Phase 3):
r.With(authMiddleware.RequireJWT, authMiddleware.RequireRole("admin")).
    Post("/api/invitations", inviteHandler.Create)
```

> `RequireJWT` should already exist (used on other `/api/` routes). If the pattern is different (e.g., a global middleware group), follow the same pattern already used for `/api/connectors/{id}/token`.

---

## No GraphQL Changes Needed

The `createInvitation` resolver already checks `user.Role != "admin"` and returns `ErrForbidden` (added in Phase 2). The HTTP middleware adds defence-in-depth for direct API calls.

---

## Build Check

```bash
cd controller && go build ./...
```

---

## Post-Phase Fixes

_None yet._
