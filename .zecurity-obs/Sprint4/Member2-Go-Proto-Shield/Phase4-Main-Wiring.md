---
type: task
status: pending
sprint: 4
member: M2
phase: 4
depends_on:
  - Phase2-Shield-Package (shield.NewService exists)
  - Phase3-PKI-SignShieldCert (pki.Service has SignShieldCert)
  - "M3 Phase 1: 003_shield_schema.sql committed"
unlocks:
  - Sprint 4 controller complete (go build ./... fully passes)
tags:
  - go
  - main
  - wiring
  - grpc
---

# M2 · Phase 4 — cmd/server/main.go Wiring

**Depends on: all M2 service phases done. This is the final integration step.**

---

## Goal

Wire `ShieldConfig`, `shield.NewService()`, and `ShieldServiceServer` registration into the controller's `main.go`. Also add the Shield env vars to `.env` files.

---

## Files to Modify

| File | Action |
|------|--------|
| `controller/cmd/server/main.go` | Add ShieldConfig + ShieldService + RegisterShieldServiceServer |
| `controller/.env` | Add Shield env vars |
| `.env.example` (if exists at repo root) | Add Shield env vars |

---

## Checklist

### main.go — Shield config

- [ ] Add `shieldCfg` construction alongside existing `connectorCfg`:
  ```go
  shieldCfg := shield.Config{
      CertTTL:             mustDuration("SHIELD_CERT_TTL",             7*24*time.Hour),
      RenewalWindow:       mustDuration("SHIELD_RENEWAL_WINDOW",       48*time.Hour),
      EnrollmentTokenTTL:  mustDuration("SHIELD_ENROLLMENT_TOKEN_TTL", 24*time.Hour),
      DisconnectThreshold: mustDuration("SHIELD_DISCONNECT_THRESHOLD", 120*time.Second),
      JWTSecret:           mustEnv("JWT_SECRET"),
  }
  ```
- [ ] Add `shieldSvc := shield.NewService(shieldCfg, db, pkiSvc, redisClient)`

### main.go — gRPC registration

- [ ] Register Shield service on same gRPC server as Connector:
  ```go
  shieldpb.RegisterShieldServiceServer(grpcServer, shieldSvc)
  ```
- [ ] Import path: `shieldpb "github.com/vairabarath/zecurity/gen/go/proto/shield/v1"`

### main.go — disconnect watcher

- [ ] Start disconnect watcher goroutine:
  ```go
  go shieldSvc.RunDisconnectWatcher(ctx)
  ```
- [ ] Place alongside existing `go connectorSvc.RunDisconnectWatcher(ctx)` call

### .env files

- [ ] Add to `controller/.env`:
  ```env
  # ── Shield (Sprint 4) ────────────────────────────────────────────────────────
  SHIELD_CERT_TTL=168h
  SHIELD_RENEWAL_WINDOW=48h
  SHIELD_ENROLLMENT_TOKEN_TTL=24h
  SHIELD_DISCONNECT_THRESHOLD=120s
  ```
- [ ] Mirror same additions to `.env.example`

---

## Build Check

```bash
cd controller && go build ./...
# Binary must compile cleanly with all new imports
# No unused imports, no missing implementations
```

---

## Final Verification

```bash
# Start controller locally (with dev DB + Redis)
cd controller && go run ./cmd/server/...
# Should start with no errors
# gRPC server should log both ConnectorService and ShieldService registered
```

---

## Notes

- The Shield gRPC service registers on `:9090` alongside Connector service. The SPIFFE interceptor applies to both automatically — it validates any valid SPIFFE cert from the correct trust domain.
- Role checking (`role == "shield"`) happens inside individual Shield handlers, not in the interceptor.
- `JWT_SECRET` is already required — Shield reuses the same secret for its enrollment JWTs.

---

## Related

- [[Services/Controller]] — update after this phase to document ShieldService
- [[Sprint4/Member2-Go-Proto-Shield/Phase2-Shield-Package]] — NewService implemented there
