package shield

import (
	"context"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go/valkeycompat"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/pki"
)

// Service is the exported interface for the shield subsystem.
// Used by connector/control_stream.go and graph resolvers without depending on the concrete type.
type Service interface {
	GenerateShieldToken(ctx context.Context, remoteNetworkID, workspaceID, tenantID, shieldID, shieldName string) (tokenString string, installCommand string, err error)
	UpdateShieldHealth(ctx context.Context, shieldID, connectorID, status, version, lanIP string, lastHeartbeatAt int64) (connectorChanged, lanIPChanged bool, err error)
	RunDisconnectWatcher(ctx context.Context)
	TokenHandler() http.Handler
}

type Config struct {
	CertTTL             time.Duration
	RenewalWindow       time.Duration
	EnrollmentTokenTTL  time.Duration
	DisconnectThreshold time.Duration
	JWTSecret           string
}

// EffectiveRenewalWindow is the window the Connector uses to ask a Shield to
// renew (Sprint 21 K2 / Sprint 20 KI-2): min(RenewalWindow, 2/5 × CertTTL).
//
// The clamp keeps a freshly issued certificate (NotAfter = now + CertTTL)
// outside the window, so a Shield is never asked to renew right after it
// renewed. It is computed from the configured TTL rather than the
// certificate's NotBefore, which is backdated by 1 h. Not positive when
// either setting is not positive.
func (c Config) EffectiveRenewalWindow() time.Duration {
	return min(c.RenewalWindow, c.CertTTL*2/5)
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
