# Commit 2 — Env Var Rename

## .env and .env.example

```env
# BEFORE
# Redis
REDIS_URL=redis://localhost:6379

# AFTER
# Valkey (open-source Redis-compatible cache, Linux Foundation fork of Redis 7.2)
# Note: URL scheme is "redis://" — that is the wire protocol name, not the product.
#       The value redis://localhost:6379 is correct even when using Valkey.
VALKEY_URL=redis://localhost:6379
```

---

## cmd/server/main.go

Find every `os.Getenv("REDIS_URL")` or `mustEnv("REDIS_URL")`:

```go
// BEFORE
redisURL := mustEnv("REDIS_URL")

// AFTER
valkeyURL := mustEnv("VALKEY_URL")
```

---

## internal/auth/config.go

```go
// BEFORE
type Config struct {
    RedisURL  string
    // ...
}

// AFTER
type Config struct {
    ValkeyURL string  // Valkey URL — scheme is redis://, port 6379
    // ...
}
```

Find every `cfg.RedisURL` reference in `internal/auth/` and rename to `cfg.ValkeyURL`.

---

## Verify Commit 2

```bash
# No REDIS_URL references should remain
grep -r "REDIS_URL" . --include="*.go" --include="*.env" \
     --include="*.example" --include="*.yml" --include="*.yaml"
# Must return zero results

cd controller && go build ./...
go test ./...
# All must pass — behaviour unchanged, only variable names changed
```
