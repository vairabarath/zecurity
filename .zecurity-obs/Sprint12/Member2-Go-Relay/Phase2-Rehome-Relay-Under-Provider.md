---
type: phase
member: M2
sprint: 12
phase: 2
title: Re-home Relay Management under /provider
status: planned
depends_on:
  - Sprint12/Member1-Go/Phase2-Provider-Session-Middleware
tags:
  - go
  - relay
  - provider
  - authz
  - audit
  - pending-07a
  - pending-01
---

# Phase 2 — Re-home Relay Management under /provider

> Depends on M1 Phase 2 (`RequireProvider` + `provider.Authz` must exist).
> This is the seam where 07a and 01 meet: relay-token issuance becomes a provider action.

## Goal

Move relay creation + provisioning-token issuance off the tenant authorization model and onto the
provider tier:

1. `POST /api/relays` (today `RequireRole("admin")`, no `WorkspaceGuard`) is **removed**.
2. `POST /provider/relays` is added behind `AuthMiddleware(provider) → RequireProvider`.
3. The handler funnels through the authz chokepoint (`Authz.CanIssueProvisioningToken`) and writes a
   `provider_audit_logs` row for every relay created.

> Conflict zone: `main.go` — M1 lands the `/provider` route group first (Phase 2 B3); M2 rebases and
> hangs the relay routes off it. Do not re-declare the group.

## Files

| File | Change |
|------|--------|
| `controller/cmd/server/main.go` | remove `POST /api/relays`; register `POST /provider/relays` in the group |
| `controller/internal/relay/admin_handler.go` | inject `Authz` + `ProviderStore`; authz check + audit write |

## admin_handler.go

```go
// AdminHandler gains dependencies (set in main.go):
//   Authz         provider.Authz
//   ProviderStore *provider.ProviderStore   // for InsertAudit
// The old comment "middleware stack (AuthMiddleware → RequireRole("admin"))" is replaced by
// "AuthMiddleware(provider) → RequireProvider".

func (h *AdminHandler) Create(w http.ResponseWriter, r *http.Request) {
    actor := provider.ActorFromContext(r.Context())   // injected by RequireProvider

    if err := h.Authz.CanIssueProvisioningToken(actor, provider.Target{Type: "relay"}); err != nil {
        // 403
        return
    }

    // ... existing create + IssueProvisioningToken + StoreProvisioningJTI + AttachJTI ...

    _ = h.ProviderStore.InsertAudit(ctx, provider.AuditEntry{
        ProviderUserID: &actor.UserID,
        ProviderEmail:  actor.Email,
        Action:         "relay.create",
        TargetType:     "relay",
        TargetID:       relayID,
        Details:        map[string]any{"name": req.Name, "ttl": ProvisioningTokenTTL.String(), "dns": dnsAllowlist, "ip": ipAllowlist},
        IPAddress:      clientIP(r),
    })
}
```

- Audit is best-effort-logged but the create still succeeds; log a warning on audit failure. (Decide
  during implementation whether audit failure should fail the request — for alpha, log-and-continue.)

## main.go

```go
// REMOVE the old route:
//   mux.Handle("POST /api/relays", relayCreateRoute)   // delete (or return 410 Gone)

// ADD under the /provider group (created in M1 Phase 2):
//   provider group: AuthMiddleware(providerAudience) → RequireProvider(...)
//   POST /provider/relays → relayAdminHandler.Create
relayAdminHandler.Authz = providerAuthz
relayAdminHandler.ProviderStore = providerStore
```

## Optional — PENDING-02 seam

- Stub `DELETE /provider/relays/{id}` guarded by `Authz.CanDeleteRelay` + audit (`relay.revoke`).
  Mark clearly **not wired to CRL** — enforcement lands in PENDING-02. Skip if it risks scope creep.

## Tests

- Tenant admin JWT → `POST /provider/relays` rejected (401/403 via `RequireProvider`).
- Provider `relay-ops`/`super-admin` → succeeds; `provider_audit_logs` gains a `relay.create` row.
- `POST /api/relays` returns 404 / 410 (route gone).

## Build Check

```bash
cd controller && go build ./...
cd controller && go test ./internal/relay/...
```

## Implementation Checklist

- [ ] **M2-D1** `main.go` — remove `POST /api/relays`; add `POST /provider/relays` behind `RequireProvider`
- [ ] **M2-D2** `admin_handler.go` — `CanIssueProvisioningToken` check + `provider_audit_logs` write
- [ ] **M2-D3** (optional) `DELETE /provider/relays/{id}` seam for PENDING-02
- [ ] **Build gate:** `cd controller && go build ./...`

## Post-Phase Fixes

_None yet._
