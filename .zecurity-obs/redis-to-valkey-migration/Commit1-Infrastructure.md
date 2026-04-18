# Commit 1 — Infrastructure Swap

## docker-compose.yml

```yaml
# BEFORE
services:
  redis:
    image: redis:7-alpine
    container_name: ztna_redis
    restart: unless-stopped
    ports:
      - "6379:6379"
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 3s
      retries: 5

# AFTER
services:
  valkey:
    image: valkey/valkey:7.2-alpine
    container_name: ztna_valkey
    restart: unless-stopped
    ports:
      - "6379:6379"
    healthcheck:
      test: ["CMD", "valkey-cli", "ping"]
      interval: 5s
      timeout: 3s
      retries: 5
```

If there is a named volume for Redis, rename it:

```yaml
# BEFORE
volumes:
  ztna_redis_data:

# AFTER
volumes:
  ztna_valkey_data:
```

---

## Verify Commit 1

```bash
docker compose down
docker compose up -d

docker exec ztna_valkey valkey-cli ping
# → PONG

docker exec ztna_valkey valkey-cli info server | grep valkey_version
# → valkey_version:7.2.x

# Controller still starts (go-redis still installed, REDIS_URL still in .env)
cd controller && go build ./...
```
