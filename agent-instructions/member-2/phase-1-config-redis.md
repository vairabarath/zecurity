# Phase 1 — Config + Redis + Constructor

This is the foundation everything else uses. Nothing else can be built until this lands.

---

## Member 2 Role & Ownership

Member 2 owns the entire authentication flow inside Go.
No Express. No proxy. Go handles OIDC directly.

```
Member 2 owns:
  internal/auth/config.go        ← Config struct + NewService constructor
  internal/auth/redis.go         ← Redis client setup (PKCE state + refresh tokens)
  internal/auth/oidc.go          ← PKCE + Google token exchange
  internal/auth/idtoken.go       ← id_token verification (JWKS)
  internal/auth/session.go       ← JWT sign/verify + refresh token
  internal/auth/callback.go      ← HTTP handler for /auth/callback
  internal/auth/refresh.go       ← HTTP handler for /auth/refresh
  internal/auth/exchange.go      ← Google token exchange (server-to-server)
```

Member 2 does NOT touch:
- `internal/bootstrap/` — that is Member 3
- `internal/pki/` — that is Member 3
- `graph/` — that is Member 4
- `internal/middleware/` — that is Member 4

---

## Hard Dependencies — What Member 2 Waits For

### Must wait for Member 4 Phase 1 before starting anything:

**`internal/auth/service.go`** — the interface file Member 4 writes.
Member 2's entire implementation is the concrete struct that satisfies
this interface. Without it, Member 2 does not know the method signatures.

```go
// This is what Member 4 commits. Member 2 implements against it.
type Service interface {
    InitiateAuth(ctx context.Context, provider string) (*model.AuthInitPayload, error)
    CallbackHandler() http.Handler
    RefreshHandler() http.Handler
}
```

**`docker-compose.yml`** — Member 2 needs Redis running to test
PKCE state storage. Without Docker running, nothing can be tested.

**`internal/db/pool.go`** — Member 2 calls `db.Pool` in `config.go`
to pass it to the bootstrap service.

**`migrations/001_schema.sql`** — Member 2 needs to understand the
`users` table structure, specifically `provider_sub` and `tenant_id`,
because the bootstrap result shapes what goes into the JWT.

### Contract agreement with Member 4 (must align before implementing):

Member 4's `internal/middleware/auth.go` reads JWTs that Member 2 signs.
Both files must use identical field names. Agree on this before writing:

```go
// This Claims struct is defined in Member 4's middleware/auth.go
// Member 2's session.go must produce JWTs with these exact fields:
type Claims struct {
    TenantID string `json:"tenant_id"`  // must be "tenant_id" not "tenantId"
    Role     string `json:"role"`
    jwt.RegisteredClaims
    // sub (user_id) lives in RegisteredClaims.Subject
    // iss must be "ztna-controller"
    // exp must be set
}
```

If Member 2 writes `"tenantId"` and Member 4 reads `"tenant_id"`,
JWT verification will succeed but `TenantID` will be empty string.
The workspace guard will then reject every request with a 403.
This is a silent bug that is hard to trace. Agree on field names first.

---

## File 1: `internal/auth/config.go`

**Path:** `internal/auth/config.go`

```go
package auth

import (
    "fmt"
    "os"

    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/yourorg/ztna/controller/internal/pki"
)

// Config holds all dependencies and settings for the auth service.
// Called by: main.go (Member 4 instantiates this using env vars)
// Member 2 defines what fields are needed here.
type Config struct {
    Pool               *pgxpool.Pool // for passing to bootstrap
    PKIService         pki.Service   // not used by auth directly, passed to bootstrap
    JWTSecret          string
    JWTIssuer          string        // must be "ztna-controller"
    JWTAccessTTL       string        // e.g. "15m"
    JWTRefreshTTL      string        // e.g. "168h"
    GoogleClientID     string
    GoogleClientSecret string
    RedirectURI        string        // https://<domain>/auth/callback
    RedisURL           string
    AllowedOrigin      string        // for CORS on callback redirect
}

// serviceImpl is the concrete implementation of auth.Service.
// Unexported — callers use the Service interface from service.go (Member 4).
// Called by: NewService() below
type serviceImpl struct {
    cfg         Config
    redisClient *redisClient
}

// NewService constructs the auth service.
// Called by: main.go (once at startup)
// Panics if required config is missing.
func NewService(cfg Config) (Service, error) {
    if cfg.JWTSecret == "" {
        return nil, fmt.Errorf("auth: JWTSecret is required")
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
    if cfg.JWTIssuer == "" {
        cfg.JWTIssuer = "ztna-controller"
    }
    if cfg.JWTAccessTTL == "" {
        cfg.JWTAccessTTL = "15m"
    }
    if cfg.JWTRefreshTTL == "" {
        cfg.JWTRefreshTTL = "168h"
    }

    rc, err := newRedisClient(cfg.RedisURL)
    if err != nil {
        return nil, fmt.Errorf("auth: redis init: %w", err)
    }

    return &serviceImpl{
        cfg:         cfg,
        redisClient: rc,
    }, nil
}
```

---

## File 2: `internal/auth/redis.go`

**Path:** `internal/auth/redis.go`

```go
package auth

import (
    "context"
    "fmt"
    "time"

    "github.com/redis/go-redis/v9"
)

// redisClient wraps the Redis connection for PKCE state and refresh token storage.
// Called by: NewService() in config.go (constructed once at startup)
type redisClient struct {
    rdb *redis.Client
}

// newRedisClient connects to Redis and verifies connectivity.
// Called by: NewService() in config.go
func newRedisClient(url string) (*redisClient, error) {
    opts, err := redis.ParseURL(url)
    if err != nil {
        return nil, fmt.Errorf("parse redis URL: %w", err)
    }

    rdb := redis.NewClient(opts)

    // Verify connectivity
    ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
    defer cancel()

    if err := rdb.Ping(ctx).Err(); err != nil {
        return nil, fmt.Errorf("ping redis: %w", err)
    }

    return &redisClient{rdb: rdb}, nil
}

// SetPKCEState stores the code_verifier keyed by state value.
// TTL = 5 minutes. After that, the PKCE pair is unusable.
// The user must restart the login flow.
// Called by: InitiateAuth() in oidc.go
func (r *redisClient) SetPKCEState(ctx context.Context,
    state, codeVerifier string) error {
    return r.rdb.Set(ctx,
        pkceKey(state),
        codeVerifier,
        5*time.Minute,
    ).Err()
}

// GetAndDeletePKCEState retrieves the code_verifier and immediately
// deletes it. Single-use — cannot be replayed.
// Returns ("", false, nil) if the key does not exist (expired or already used).
// Called by: CallbackHandler() in callback.go (Step 3)
func (r *redisClient) GetAndDeletePKCEState(ctx context.Context,
    state string) (string, bool, error) {

    // Use a pipeline to GET + DEL atomically.
    // If we GET then DEL separately, a crash between the two
    // could leave a used verifier in Redis (replay risk).
    pipe := r.rdb.Pipeline()
    getCmd := pipe.Get(ctx, pkceKey(state))
    pipe.Del(ctx, pkceKey(state))
    _, err := pipe.Exec(ctx)

    if err != nil && err != redis.Nil {
        return "", false, fmt.Errorf("redis pipeline: %w", err)
    }

    val, err := getCmd.Result()
    if err == redis.Nil {
        return "", false, nil // expired or already used
    }
    if err != nil {
        return "", false, fmt.Errorf("get pkce state: %w", err)
    }

    return val, true, nil
}

// SetRefreshToken stores a refresh token keyed to user_id.
// TTL = 7 days (168 hours).
// Called by: issueRefreshToken() in session.go
func (r *redisClient) SetRefreshToken(ctx context.Context,
    userID, token string, ttl time.Duration) error {
    return r.rdb.Set(ctx,
        refreshKey(userID),
        token,
        ttl,
    ).Err()
}

// GetRefreshToken retrieves the stored refresh token for a user.
// Returns ("", false, nil) if not found or expired.
// Called by: RefreshHandler() in refresh.go (Step 3)
func (r *redisClient) GetRefreshToken(ctx context.Context,
    userID string) (string, bool, error) {

    val, err := r.rdb.Get(ctx, refreshKey(userID)).Result()
    if err == redis.Nil {
        return "", false, nil
    }
    if err != nil {
        return "", false, fmt.Errorf("get refresh token: %w", err)
    }
    return val, true, nil
}

// DeleteRefreshToken removes the refresh token.
// Called by: sign-out handler (future)
func (r *redisClient) DeleteRefreshToken(ctx context.Context,
    userID string) error {
    return r.rdb.Del(ctx, refreshKey(userID)).Err()
}

// pkceKey builds the Redis key for PKCE state storage.
// Called by: SetPKCEState(), GetAndDeletePKCEState()
func pkceKey(state string) string {
    return "pkce:" + state
}

// refreshKey builds the Redis key for refresh token storage.
// Called by: SetRefreshToken(), GetRefreshToken(), DeleteRefreshToken()
func refreshKey(userID string) string {
    return "refresh:" + userID
}
```

---

## Phase 1 Checklist

```
✓ NewService returns error on missing GoogleClientID
✓ NewService returns error on missing JWTSecret
✓ Redis connection verified on startup
✓ Redis ping fails gracefully with descriptive error
```
