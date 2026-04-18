package resolvers

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go/valkeycompat"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/connector"
	"github.com/yourorg/ztna/controller/internal/db"
	"github.com/yourorg/ztna/controller/internal/shield"
)

// Resolver holds shared dependencies for all resolvers.
type Resolver struct {
	TenantDB     *db.TenantDB
	AuthService  auth.Service
	ConnectorCfg connector.Config
	ShieldSvc    shield.Service
	Redis        valkeycompat.Cmdable
	Pool         *pgxpool.Pool
}
