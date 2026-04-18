# Redis → Valkey Migration — Overview

## What's Happening

```
1. Infrastructure swap    redis image → valkey/valkey image
2. Env var rename         REDIS_URL → VALKEY_URL
3. Client library swap    go-redis → valkey-go (with valkeycompat adapter)
```

Done as three separate commits so each can be verified independently.

---

## Why valkey-go Instead of go-redis

```
go-redis          → protocol-compatible with Valkey 7.2 but won't track
                    Valkey 8.0+ features (multi-threaded I/O, new commands)

valkey-go         → official Valkey GitHub org (valkey-io/valkey-go)
                    formerly rueidis — now the official community client
                    auto-pipelining built in (better throughput under load)
                    valkeycompat package = drop-in interface for go-redis code
                    tracks Valkey 8.0+ features as they land

Valkey GLIDE      → skip — Rust CGo core, complex build chain,
                    designed for managed cloud services (ElastiCache, Memorystore)
                    overkill for self-hosted ZTNA platform
```

---

## What Changes vs What Stays the Same

```
CHANGES
  docker-compose.yml              redis image → valkey/valkey:7.2-alpine
  docker-compose.yml              container name ztna_redis → ztna_valkey
  docker-compose.yml              healthcheck redis-cli → valkey-cli
  .env + .env.example             REDIS_URL → VALKEY_URL
  cmd/server/main.go              env var name + version check + client init
  internal/auth/redis.go          constructor + struct names + comments
  internal/auth/config.go         RedisURL field → ValkeyURL field
  go.mod / go.sum                 remove go-redis, add valkey-go
  README / docs                   Redis → Valkey mentions
  Obsidian vault                  stack section updated

DOES NOT CHANGE
  URL value in .env               still "redis://" — wire protocol, not product name
  Port                            still 6379
  All GETDEL calls                unchanged — same command, same behaviour
  All SET/GET/DEL/TTL calls       unchanged — valkeycompat mirrors go-redis interface
  proto files                     no cache layer references
  Rust connector + shield         no cache layer references
  migrations/                     no cache layer references
  frontend                        no cache layer references
  internal/connector/token.go     business logic unchanged (only comments)
  internal/shield/token.go        business logic unchanged (only comments)
```

---

## Cache Layer Stack (post-migration)

```
Product:    Valkey 7.2 (Linux Foundation open-source fork of Redis 7.2)
            Redis changed to SSPL license in 7.4+ — Valkey stays MIT/BSD
Client:     valkey-go (github.com/valkey-io/valkey-go)
            official Valkey GitHub org, formerly rueidis
            valkeycompat adapter used — same interface as go-redis
URL scheme: redis:// (RESP wire protocol name — correct for Valkey)
Env var:    VALKEY_URL=redis://localhost:6379
Port:       6379
Operations: GETDEL (atomic single-use burn), SET with TTL, GET

TTL-based keys (no persistent data):
  PKCE state:        5 minutes
  Refresh tokens:    7 days
  Connector JTIs:    24 hours
  Shield JTIs:       24 hours
```

---

## Data Migration Note

No data migration needed. All cache data is TTL-based and disposable.

When you bring down the old container and bring up Valkey:
- In-progress OAuth logins need to restart (PKCE state lost — 5 min TTL)
- Active refresh tokens need re-login (7 day TTL)
- Pending enrollment tokens need regeneration (24h TTL)

In development: non-issue.
In production: maintenance window, notify users to re-login after upgrade.
