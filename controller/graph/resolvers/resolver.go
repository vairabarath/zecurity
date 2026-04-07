package resolvers

import (
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/db"
)

// Resolver holds shared dependencies for all resolvers.
type Resolver struct {
	TenantDB    *db.TenantDB
	AuthService auth.Service
}
