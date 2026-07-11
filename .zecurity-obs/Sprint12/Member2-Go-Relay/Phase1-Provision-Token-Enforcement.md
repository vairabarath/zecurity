---
type: phase
member: M2
sprint: 12
phase: 1
title: Provision Token Enforcement + Drop Self-Provision Fallback
status: planned
depends_on: []
tags:
  - go
  - relay
  - pki
  - security
  - pending-01
---

# Phase 1 — Provision Token Enforcement + Drop Self-Provision Fallback

> Day-1 work — independent of M1. This is the security core of PENDING-01: it closes anonymous
> CA-signing. Can be hotfixed on its own if needed; the `/provider` re-home (M2 Phase 2) is what
> makes issuance authorization-correct.

## Goal

Make the `Provision` RPC actually authenticate:

1. Require a non-empty `provisioning_token`, **verify** it, assert its `relay_id` matches the request,
   and **atomically burn** the single-use JTI **before** signing.
2. Remove the self-provision fallback so an unknown UUID can no longer self-insert an `active` relay.

Reuse the existing (currently zero-caller) machinery in `controller/internal/relay/token.go`:
`VerifyProvisioningToken`, `BurnProvisioningJTI`. The `Service` already holds `redis` (via
`WithHeartbeatCache`); it needs the JWT secret injected.

## Files

| File | Change |
|------|--------|
| `controller/internal/relay/provision.go` | JWT-secret field + `WithProvisioningAuth`; verify+burn before sign; drop fallback |
| `controller/cmd/server/main.go` | wire `WithProvisioningAuth(JWT_SECRET)` on relay-service construction (~line 120) |
| `controller/internal/relay/provision_test.go` | token enforcement test cases |

## provision.go

```go
// Add to Service struct:
//   jwtSecret string
func (s *Service) WithProvisioningAuth(jwtSecret string) *Service {
    s.jwtSecret = jwtSecret
    return s
}

// In Provision(), BEFORE s.pki.SignRelayCert(...):
//   1. if req.ProvisioningToken == "" → codes.Unauthenticated "provisioning token required"
//   2. claims, err := VerifyProvisioningToken(s.jwtSecret, req.ProvisioningToken)
//        err → codes.PermissionDenied
//   3. if claims.RelayID != relayID → codes.PermissionDenied "token relay mismatch"
//   4. burnedRelayID, ok, err := BurnProvisioningJTI(ctx, s.redis, claims.ID)
//        err → codes.Internal
//        !ok → codes.PermissionDenied "token already used or unknown"
//        burnedRelayID != relayID → codes.PermissionDenied (defensive)
//   5. proceed to SignRelayCert
```

```go
// Drop the self-provision fallback (currently provision.go ~123-138):
// On MarkProvisioned → ErrRelayNotFound, DO NOT call InsertProvisionedRelay.
// Return codes.FailedPrecondition "relay not registered".
// Verify InsertProvisionedRelay has no other callers before deleting it from store.go.
```

- Keep `Provision` in the SPIFFE interceptor skip list (`internal/connector/spiffe.go:160-164`) — the
  relay still has no certificate at provisioning time; the **token** is now the auth. Update the
  stale comment there ("token authentication is deferred") to reflect that it is now enforced.

## main.go

```go
relaySvc := relay.NewService(pkiService, relayStore, mustDuration("RELAY_CERT_TTL", 30*24*time.Hour)).
    WithHeartbeatCache(valkeycompat.NewAdapter(connectorValkey), mustDuration("RELAY_HEARTBEAT_DB_WRITE_INTERVAL", 5*time.Minute)).
    WithProvisioningAuth(mustEnv("JWT_SECRET"))   // <-- add
```

## Tests (provision_test.go)

- valid token + pre-created `pending` row → signs, row flips `active`, JTI burned.
- empty token → `Unauthenticated`.
- token `relay_id` ≠ request `relay_id` → `PermissionDenied`.
- replayed token (JTI already burned) → `PermissionDenied`.
- unregistered relay (no row) → `FailedPrecondition` (no self-insert).

## Build Check

```bash
cd controller && go build ./...
cd controller && go test ./internal/relay/...
```

## Implementation Checklist

- [ ] **M2-C1** `provision.go` — JWT-secret field + `WithProvisioningAuth`
- [ ] **M2-C2** `provision.go` — verify → assert relay_id → `BurnProvisioningJTI` before sign
- [ ] **M2-C3** `provision.go` — drop self-provision fallback; `FailedPrecondition` on not-found
- [ ] **M2-C4** `main.go` — wire `WithProvisioningAuth(JWT_SECRET)`
- [ ] **M2-C5** `provision_test.go` — enforcement cases
- [ ] **Build gate:** `cd controller && go build ./...`

## Post-Phase Fixes

_None yet._
