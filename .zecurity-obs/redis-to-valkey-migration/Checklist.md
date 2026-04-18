# Redis → Valkey Migration — Verification Checklist

## Three-Commit Summary

```
Commit 1 — Infrastructure
  docker-compose.yml: redis → valkey/valkey:7.2-alpine
  Verify: valkey-cli ping → PONG, go build clean

Commit 2 — Env var rename
  .env + .env.example: REDIS_URL → VALKEY_URL
  main.go + config.go: all references renamed
  Verify: grep REDIS_URL returns zero results, go build clean

Commit 3 — Client library
  go.mod: remove go-redis, add valkey-go
  internal/auth/redis.go → valkey.go: newValkeyClient + valkeycompat adapter
  main.go: updated client construction + version check
  Verify: go test ./... passes, grep go-redis returns zero results
```

---

## Final Checklist

- [x] `docker compose up` starts `ztna_valkey` (not `ztna_redis`)
- [x] `valkey-cli ping` → PONG
- [x] `valkey-cli info server` shows `valkey_version:7.2.x`
- [x] `go build ./...` clean
- [x] `go test ./...` all pass
- [x] `grep -r "REDIS_URL"` returns zero results
- [x] `grep -r "go-redis" --include="*.go"` returns zero results
- [x] `grep "valkey-io/valkey-go" go.mod` returns the dependency
- [x] Controller starts and logs `✓ Valkey 7.2.x connected`
- [ ] OAuth login flow works end to end
- [ ] Connector enrollment works (JTI burn via GETDEL)
- [ ] Shield enrollment works (JTI burn via GETDEL)
- [x] `.env.example` has `VALKEY_URL` with explanation comment
- [x] README updated
- [x] `internal/auth/redis.go` renamed to `valkey.go` / `redis_test.go` renamed to `valkey_test.go`
