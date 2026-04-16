---
type: task
status: pending
sprint: 4
member: M2
phase: 2
depends_on:
  - Phase1-Proto-appmeta (appmeta constants, buf generate done)
  - "M3 Phase 1: 003_shield_schema.sql (DB table must exist for DB calls)"
unlocks:
  - Phase3-PKI-SignShieldCert
  - Phase4-Main-Wiring
  - M4 Phase 3 (Enrollment) — M4 needs Enroll handler live in dev
tags:
  - go
  - shield
  - grpc
  - jwt
---

# M2 · Phase 2 — internal/shield/ Package

**Depends on: appmeta constants committed + DB migration committed.**

---

## Goal

Implement the entire `internal/shield/` package: config, JWT token generation, gRPC enrollment handler, disconnect watcher, SPIFFE helper.

---

## Files to Create

All new files in `controller/internal/shield/`:

| File | Purpose |
|------|---------|
| `config.go` | `ShieldConfig` struct |
| `token.go` | JWT generation, Redis JTI burn, connector selection, IP assignment |
| `enrollment.go` | gRPC `Enroll` handler (12-step flow) |
| `heartbeat.go` | Disconnect watcher goroutine |
| `spiffe.go` | Thin SPIFFE wrapper |

---

## Checklist

### config.go

- [ ] Package `shield`
- [ ] `ShieldConfig` struct:
  ```go
  type Config struct {
      CertTTL             time.Duration  // SHIELD_CERT_TTL, default 168h
      RenewalWindow       time.Duration  // SHIELD_RENEWAL_WINDOW, default 48h
      EnrollmentTokenTTL  time.Duration  // SHIELD_ENROLLMENT_TOKEN_TTL, default 24h
      DisconnectThreshold time.Duration  // SHIELD_DISCONNECT_THRESHOLD, default 120s
      JWTSecret           string         // JWT_SECRET (reused from auth)
  }
  ```

### token.go

- [ ] `GenerateShieldToken(ctx, remoteNetworkID, workspaceID, tenantID, shieldID, shieldName string) (jwt string, installCommand string, err error)`
- [ ] JWT payload includes: `jti`, `shield_id`, `remote_network_id`, `workspace_id`, `trust_domain`, `ca_fingerprint`, `connector_id`, `connector_addr`, `interface_addr`, `iss`, `exp`
- [ ] Store JTI in Redis with TTL = `EnrollmentTokenTTL`
- [ ] Connector selection: `selectConnector()` — query ACTIVE connectors for remote_network, sort by fewest shields (tiebreaker: most recent heartbeat)
- [ ] IP assignment: `assignInterfaceAddr()` — find next unused /32 from 100.64.0.0/10 for this tenant
- [ ] `installCommand` format: `curl -fsSL https://<controller>/shield-install.sh | sudo ENROLLMENT_TOKEN=<jwt> bash`
- [ ] `BurnShieldJTI(ctx, jti)` — atomic GET+DEL from Redis

### enrollment.go

- [ ] Implement `Enroll(ctx, req *shieldpb.EnrollRequest) (*shieldpb.EnrollResponse, error)`
- [ ] 12-step flow (see sprint4-shield-plan.md Enrollment section):
  1. Verify JWT signature + exp + iss
  2. Extract all claims (jti, shield_id, workspace_id, trust_domain, connector_id, interface_addr)
  3. GET+DEL JTI from Redis (atomic) — return `PERMISSION_DENIED` if not found
  4. Load shield row — verify `status='pending'`, tenant matches
  5. Verify workspace `status='active'`
  6. Verify connector exists and is `ACTIVE`
  7. Parse CSR from `req.csr_der`
  8. `csr.CheckSignature()` — verify self-signature
  9. Verify CSR SPIFFE SAN matches `appmeta.ShieldSPIFFEID(trust_domain, shield_id)`
  10. Call `pki.SignShieldCert(...)`
  11. `UPDATE shields SET status='active', cert_serial=..., cert_not_after=..., hostname=..., version=..., last_heartbeat_at=NOW(), enrollment_token_jti=NULL`
  12. Return `EnrollResponse` with cert chain + interface_addr + connector_addr + connector_id
- [ ] Return proper gRPC status codes: `PERMISSION_DENIED`, `FAILED_PRECONDITION`, `INTERNAL`

### heartbeat.go (disconnect watcher only)

- [ ] `RunDisconnectWatcher(ctx context.Context)` — exported method on service
- [ ] Ticker: fires every `DisconnectThreshold / 2`
- [ ] SQL: `UPDATE shields SET status='disconnected' WHERE status='active' AND last_heartbeat_at < NOW() - $1 AND tenant_id IN (SELECT id FROM workspaces WHERE status='active')`
- [ ] Logs disconnected count each tick at `info` level

### spiffe.go

- [ ] `ParseShieldSPIFFEID(uri string) (trustDomain, shieldID string, err error)`
- [ ] Validates format: `spiffe://<trust_domain>/shield/<id>`
- [ ] Returns `PERMISSION_DENIED` if role != "shield"

### Service struct

- [ ] `service` struct with: `cfg Config`, `db *pgxpool.Pool`, `pki pki.Service`, `redis *redis.Client`
- [ ] `NewService(cfg Config, db, pki, redis) *service`
- [ ] Implement `shieldpb.ShieldServiceServer` interface

---

## Build Check

```bash
cd controller && go build ./...
# internal/shield package must compile cleanly
# All imports resolved (gen/go/proto/shield/v1, appmeta, pki, etc.)
```

---

## Related

- [[Sprint4/Member2-Go-Proto-Shield/Phase3-PKI-SignShieldCert]] — pki.SignShieldCert called from enrollment.go
- [[Sprint4/Member2-Go-Proto-Shield/Phase4-Main-Wiring]] — registers this service
- [[Sprint4/Member3-Go-DB-GraphQL/Phase2-Shield-Resolvers]] — calls token.go GenerateShieldToken
