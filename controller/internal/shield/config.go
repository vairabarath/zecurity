package shield

import (
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/pki"
)

// Config holds all tunable settings for the shield subsystem.
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
	redis *redis.Client
}

func NewService(cfg Config, db *pgxpool.Pool, pkiSvc pki.Service, redis *redis.Client) *service {
	return &service{
		cfg:   cfg,
		db:    db,
		pki:   pkiSvc,
		redis: redis,
	}
}

var _ shieldpb.ShieldServiceServer = (*service)(nil)
