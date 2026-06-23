package auth

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/bootstrap"
)

// Config holds all dependencies and settings for the auth service.
// Called by: main.go — Member 4 instantiates this using env vars and passes it to NewService().
// Member 2 defines what fields are needed here.
type Config struct {
	// Pool is the pgx connection pool, passed to the bootstrap service.
	// Provided by: db.Pool (internal/db/pool.go, Member 4)
	Pool *pgxpool.Pool

	// JWTSecret is the symmetric key used for HS256 signing and verification.
	// Must match the key used in Member 4's middleware/auth.go for JWT verification.
	JWTSecret string

	// JWTIssuer is the "iss" claim value. Must match appmeta.ControllerIssuer.
	// Must match the issuer check in Member 4's middleware/auth.go.
	JWTIssuer string

	// JWTAccessTTL is the access token lifetime, e.g. "15m".
	// Parsed by time.ParseDuration in session.go.
	JWTAccessTTL string

	// JWTRefreshTTL is the rolling idle TTL for the refresh token, e.g. "168h"
	// (7 days). After this window of inactivity, the Redis key expires.
	// Parsed by time.ParseDuration in session.go.
	JWTRefreshTTL string

	// JWTRefreshMaxLifetime is the absolute lifetime cap from initial issuance,
	// e.g. "720h" (30 days). Even with continuous refresh activity, the session
	// must end after this window. See ADR-006.
	// Parsed by time.ParseDuration in session.go and refresh.go.
	JWTRefreshMaxLifetime string

	// GoogleClientID is the OAuth 2.0 client ID from Google Cloud Console.
	// Used in: oidc.go (auth URL), idtoken.go (aud verification), exchange.go (token exchange).
	GoogleClientID string

	// GoogleClientSecret is the OAuth 2.0 client secret from Google Cloud Console.
	// Used in: exchange.go (server-to-server token exchange only — never sent to browser).
	GoogleClientSecret string

	// RedirectURI is the callback URL registered with Google, e.g. "https://<domain>/auth/callback".
	// Used in: oidc.go (auth URL) and exchange.go (token exchange).
	RedirectURI string

	// ValkeyURL is the connection string for Valkey, e.g. "redis://localhost:6379".
	// Note: URL scheme is "redis://" — that is the wire protocol name, not the product.
	// Used by: redis.go for PKCE state and refresh token storage.
	ValkeyURL string

	// AllowedOrigin is the allowed CORS origin for the callback redirect.
	// Used by: main.go for CORS middleware configuration.
	AllowedOrigin string

	// BootstrapService provisions or retrieves the user's workspace membership
	// during the auth callback flow.
	BootstrapService *bootstrap.Service
}

// serviceImpl is the concrete implementation of auth.Service (defined in service.go by Member 4).
// Unexported — callers use the Service interface.
// Created by: NewService() below.
// Methods implemented in: oidc.go, callback.go, refresh.go, session.go, exchange.go.
type serviceImpl struct {
	cfg          Config
	redisClient  *valkeyClient
	bootstrapSvc *bootstrap.Service
}

// minJWTSecretBytes is the floor on JWT_SECRET length, in bytes.
//
// 32 bytes = 256 bits, matching HS256's hash output. Below this, the HMAC
// becomes vulnerable to offline brute-force given enough observed JWTs.
// Keeping the check here prevents accidental deployments with weak secrets
// (e.g. JWT_SECRET=x, JWT_SECRET=changeme). See ADR-007.
const minJWTSecretBytes = 32

// NewService constructs the auth service and connects to Redis.
// Called by: main.go (once at startup, before HTTP server starts).
// Returns the Service interface so callers never see the concrete struct.
func NewService(cfg Config) (Service, error) {
	// Validate required fields — fail fast at startup, not at first request.
	if cfg.JWTSecret == "" {
		return nil, fmt.Errorf("auth: JWTSecret is required")
	}
	if len(cfg.JWTSecret) < minJWTSecretBytes {
		return nil, fmt.Errorf(
			"auth: JWTSecret must be at least %d bytes (got %d). "+
				"Generate with: openssl rand -base64 48",
			minJWTSecretBytes, len(cfg.JWTSecret),
		)
	}
	if cfg.GoogleClientID == "" {
		return nil, fmt.Errorf("auth: GoogleClientID is required")
	}
	if cfg.GoogleClientSecret == "" {
		return nil, fmt.Errorf("auth: GoogleClientSecret is required")
	}
	if cfg.RedirectURI == "" {
		return nil, fmt.Errorf("auth: RedirectURI is required")
	}
	if cfg.BootstrapService == nil {
		return nil, fmt.Errorf("auth: BootstrapService is required")
	}

	// Apply defaults for optional fields.
	if cfg.JWTIssuer == "" {
		cfg.JWTIssuer = appmeta.ControllerIssuer
	}
	if cfg.JWTAccessTTL == "" {
		cfg.JWTAccessTTL = "15m"
	}
	if cfg.JWTRefreshTTL == "" {
		cfg.JWTRefreshTTL = "168h"
	}
	if cfg.JWTRefreshMaxLifetime == "" {
		cfg.JWTRefreshMaxLifetime = "720h" // 30 days — ADR-006
	}

	// Connect to Valkey — verifies connectivity with a PING.
	// Called: valkey.go → newValkeyClient()
	rc, err := newValkeyClient(cfg.ValkeyURL)
	if err != nil {
		return nil, fmt.Errorf("auth: redis init: %w", err)
	}

	return &serviceImpl{
		cfg:          cfg,
		redisClient:  rc,
		bootstrapSvc: cfg.BootstrapService,
	}, nil
}
