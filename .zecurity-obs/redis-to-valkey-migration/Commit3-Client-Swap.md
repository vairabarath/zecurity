# Commit 3 — Client Library Swap (go-redis → valkey-go)

The `valkeycompat` adapter means existing business logic in `redis.go`, `token.go` etc. does not change.
Only the client construction changes.

---

## Step 1 — Update go.mod

```bash
cd controller

# Add valkey-go
go get github.com/valkey-io/valkey-go

# Remove go-redis
go get github.com/redis/go-redis/v9@none

# Tidy
go mod tidy
```

---

## Step 2 — internal/auth/redis.go → valkey.go

Rename the file first:

```bash
git mv controller/internal/auth/redis.go controller/internal/auth/valkey.go
```

Replace the client construction (the methods below it stay identical):

```go
// BEFORE
package auth

import (
    "context"
    "fmt"
    "time"
    "github.com/redis/go-redis/v9"
)

type redisClient struct {
    rdb *redis.Client
}

func newRedisClient(url string) (*redisClient, error) {
    opts, err := redis.ParseURL(url)
    if err != nil {
        return nil, fmt.Errorf("parse valkey URL: %w", err)
    }
    rdb := redis.NewClient(opts)

    ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
    defer cancel()
    if err := rdb.Ping(ctx).Err(); err != nil {
        return nil, fmt.Errorf("ping valkey: %w", err)
    }
    return &redisClient{rdb: rdb}, nil
}

// AFTER
package auth

import (
    "context"
    "fmt"
    "strings"
    "time"
    "github.com/valkey-io/valkey-go"
    "github.com/valkey-io/valkey-go/valkeycompat"
)

// valkeyClient wraps the valkey-go client behind the valkeycompat adapter.
// The adapter exposes the same interface as go-redis so all callers
// (SetPKCEState, GetAndDeletePKCEState, etc.) are unchanged.
type valkeyClient struct {
    rdb valkeycompat.Cmdable  // same interface as go-redis *redis.Client
}

func newValkeyClient(url string) (*valkeyClient, error) {
    addr, err := parseValkeyAddr(url)
    if err != nil {
        return nil, fmt.Errorf("parse valkey URL: %w", err)
    }

    client, err := valkey.NewClient(valkey.ClientOption{
        InitAddress: []string{addr},
    })
    if err != nil {
        return nil, fmt.Errorf("create valkey client: %w", err)
    }

    ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
    defer cancel()
    if err := client.Do(ctx, client.B().Ping().Build()).Error(); err != nil {
        return nil, fmt.Errorf("ping valkey: %w", err)
    }

    rdb := valkeycompat.NewAdapter(client)
    return &valkeyClient{rdb: rdb}, nil
}

// parseValkeyAddr extracts "host:port" from a "redis://host:port" URL.
// valkey-go uses InitAddress []string, not a URL string.
func parseValkeyAddr(rawURL string) (string, error) {
    after, found := strings.CutPrefix(rawURL, "redis://")
    if !found {
        return "", fmt.Errorf("expected redis:// URL, got: %s", rawURL)
    }
    if idx := strings.LastIndex(after, "@"); idx != -1 {
        after = after[idx+1:]
    }
    return after, nil
}
```

Methods on `valkeyClient` that use `rdb` stay exactly the same — `valkeycompat.Cmdable` has the same method signatures as go-redis:

```go
// These methods DO NOT CHANGE AT ALL

func (r *valkeyClient) SetPKCEState(ctx context.Context, state, verifier string) error {
    return r.rdb.Set(ctx, pkceKey(state), verifier, 5*time.Minute).Err()
}

func (r *valkeyClient) GetAndDeletePKCEState(ctx context.Context, state string) (string, bool, error) {
    val, err := r.rdb.GetDel(ctx, pkceKey(state)).Result()
    // ... identical to before
}
```

---

## Step 3 — cmd/server/main.go client construction

```go
// BEFORE
redisClient, err := auth.NewRedisClient(valkeyURL)

// AFTER
valkeyClient, err := auth.NewValkeyClient(valkeyURL)
```

---

## Step 4 — Update version check function

```go
// BEFORE
func checkRedisVersion(ctx context.Context, rdb *redis.Client) { ... }

// AFTER
func checkCacheVersion(ctx context.Context, client valkey.Client) {
    resp := client.Do(ctx, client.B().Info().Section("server").Build())
    info, err := resp.ToString()
    if err != nil {
        log.Fatalf("valkey: cannot get server info: %v", err)
    }

    if strings.Contains(info, "valkey_version:") {
        version := parseVersionFromInfo(info, "valkey_version")
        log.Printf("✓ Valkey %s connected", version)
        return
    }

    if strings.Contains(info, "redis_version:") {
        version := parseVersionFromInfo(info, "redis_version")
        log.Printf("✓ Redis %s connected (Valkey recommended)", version)
        if !versionAtLeast(version, 6, 2) {
            log.Fatalf("Redis %s too old. Requires 6.2+ for GETDEL. "+
                "Switch to Valkey 7.2+", version)
        }
        return
    }

    log.Fatal("cache: unrecognized server. Expected Valkey 7.2+ or Redis 6.2+")
}

func parseVersionFromInfo(info, key string) string {
    for _, line := range strings.Split(info, "\r\n") {
        if strings.HasPrefix(line, key+":") {
            return strings.TrimSpace(strings.TrimPrefix(line, key+":"))
        }
    }
    return "unknown"
}

func versionAtLeast(version string, major, minor int) bool {
    parts := strings.Split(version, ".")
    if len(parts) < 2 {
        return false
    }
    maj, err1 := strconv.Atoi(parts[0])
    min, err2 := strconv.Atoi(parts[1])
    if err1 != nil || err2 != nil {
        return false
    }
    return maj > major || (maj == major && min >= minor)
}
```

---

## Step 5 — Update comments in token files

Logic unchanged — comments only:

`internal/connector/token.go`:
```go
// BEFORE: // BurnEnrollmentJTI atomically reads and deletes the JTI from Redis.
// AFTER:  // BurnEnrollmentJTI atomically reads and deletes the JTI from Valkey.
```

`internal/shield/token.go`:
```go
// BEFORE: // BurnShieldJTI atomically reads and deletes the JTI from Redis.
// AFTER:  // BurnShieldJTI atomically reads and deletes the JTI from Valkey.
```

---

## Verify Commit 3

```bash
cd controller

# Build must be clean
go build ./...

# Tests must pass
go test ./...

# No go-redis import remaining
grep -r "go-redis" . --include="*.go"
# Must return zero results

# Verify valkey-go is in go.mod
grep "valkey-io/valkey-go" go.mod
# Must show the dependency
```
