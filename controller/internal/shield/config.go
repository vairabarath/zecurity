package shield

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go/valkeycompat"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/pki"
)

// Service is the exported interface for the shield subsystem.
// Used by connector/heartbeat.go and graph resolvers without depending on the concrete type.
type Service interface {
	GenerateShieldToken(ctx context.Context, remoteNetworkID, workspaceID, tenantID, shieldID, shieldName string) (tokenString string, installCommand string, err error)
	UpdateShieldHealth(ctx context.Context, shieldID, connectorID, status, version string, lastHeartbeatAt int64) error
	RunDisconnectWatcher(ctx context.Context)
}

type Config struct {
	CertTTL             time.Duration
	RenewalWindow       time.Duration
	EnrollmentTokenTTL  time.Duration
	DisconnectThreshold time.Duration
	JWTSecret           string
}

type service struct {
	shieldpb.UnimplementedShieldServiceServer

	cfg   Config
	db    *pgxpool.Pool
	pki   pki.Service
	redis valkeycompat.Cmdable
}

func NewService(cfg Config, db *pgxpool.Pool, pkiSvc pki.Service, redis valkeycompat.Cmdable) *service {
	return &service{
		cfg:   cfg,
		db:    db,
		pki:   pkiSvc,
		redis: redis,
	}
}

var _ shieldpb.ShieldServiceServer = (*service)(nil)
var _ Service = (*service)(nil)
