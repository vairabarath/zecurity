package resolvers

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/connector"
	"github.com/yourorg/ztna/controller/internal/db"
)

// Resolver holds shared dependencies for all resolvers.
type Resolver struct {
	TenantDB     *db.TenantDB
	AuthService  auth.Service
	ConnectorCfg connector.Config
	Redis        *redis.Client
	Pool         *pgxpool.Pool
}
