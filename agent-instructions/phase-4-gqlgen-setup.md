# Phase 4 — gqlgen Setup

Configure gqlgen, generate boilerplate, set up the base resolver struct.

---

## File 1: `controller/graph/gqlgen.yml`

**Path:** `controller/graph/gqlgen.yml`

```yaml
schema:
  - graph/schema.graphqls

exec:
  filename: graph/generated.go
  package: graph

model:
  filename: graph/models_gen.go
  package: graph

resolver:
  layout: follow-schema
  dir: graph/resolvers
  package: resolvers
  filename_template: "{name}.resolvers.go"

models:
  User:
    model: github.com/yourorg/ztna/controller/internal/models.User
  Workspace:
    model: github.com/yourorg/ztna/controller/internal/models.Workspace
```

Run this after any schema change:
```
go run github.com/99designs/gqlgen generate
```

---

## File 2: `controller/graph/resolver.go`

**Path:** `controller/graph/resolver.go`

Replace the existing empty stub with:

```go
package graph

import (
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/db"
)

// Resolver holds shared dependencies for all resolvers.
// Member 4 owns this struct.
// Add fields here when new services are needed by resolvers.
type Resolver struct {
	TenantDB    *db.TenantDB
	AuthService auth.Service
}
```

---

## Go Module Setup

These steps must be done before gqlgen can generate:

```bash
cd controller

# Initialize Go module (if not already done)
go mod init github.com/yourorg/ztna/controller

# Add dependencies
go get github.com/99designs/gqlgen@latest
go get github.com/vektah/gqlparser/v2@latest
go get github.com/jackc/pgx/v5@latest
go get github.com/golang-jwt/jwt/v5@latest
go get github.com/redis/go-redis/v9@latest
go get google.golang.org/grpc@latest

# Generate gqlgen boilerplate
go run github.com/99designs/gqlgen generate
```

After generation, these files will exist (DO NOT EDIT them):
- `graph/generated.go` — gqlgen generated executor
- `graph/models_gen.go` — generated model types for GraphQL types

---

## Verification Checklist

```
[ ] go run github.com/99designs/gqlgen generate completes with no errors
[ ] generated.go and models_gen.go exist and compile
[ ] graph/resolver.go has Resolver struct with TenantDB + AuthService fields
```
