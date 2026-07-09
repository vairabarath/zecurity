---
type: adr
status: pending
id: PENDING-01
domain: security/relay
priority: P0
created: 2026-07-03
related:
  - ADR-014-Relay-Stabilization
  - Relay-E2E-Flow-and-Security-Review (F1)
tags: [pending, adr, relay, security, pki]
---

# Pending ADR 01 — Authenticated Relay Provisioning

> **Status: PENDING — for team discussion.** On adoption, promote to the next free `ADR-0NN`.

## Context / Current State

`RelayService/Provision` is **unauthenticated**. The handler ignores `provisioning_token`
(`controller/internal/relay/provision.go:82-84`, comment: *"reserved for a future
authenticated operator flow and is ignored"*), and it is in the SPIFFE interceptor skip list
(`spiffe.go:160-164`). The single-use-JWT machinery already exists in
`controller/internal/relay/token.go` (`IssueProvisioningToken` / `VerifyProvisioningToken` /
`StoreProvisioningJTI` / `BurnProvisioningJTI`) and the admin API already mints these tokens —
but **`Verify`/`Burn` have zero callers**. The relay client even sends an empty token
(`relay/src/provision.rs:157`).

**Impact:** anyone who can reach the controller gRPC port can submit a CSR + arbitrary UUID and
receive a Platform-Intermediate-CA-signed relay leaf (`spiffe://zecurity.in/relay/<uuid>`,
ServerAuth+ClientAuth), self-insert an `active` relays row, heartbeat, and appear in the
`LabelledRelayList` every workspace's connectors trust → DoS, metadata exposure, relay-identity
takeover. Tunnel payload stays E2E-encrypted, so this is not a data-confidentiality break, but it
is anonymous CA signing + fleet poisoning.

## Problem — Decision Needed

How do we authenticate relay provisioning, and how strict should the bootstrap trust model be?

## Options

### Option A — Wire the existing single-use token (recommended baseline)
Require `provisioning_token`; `VerifyProvisioningToken` → assert `claims.RelayID == req.RelayId`
→ `BurnProvisioningJTI` atomically; drop the self-insert fallback (require a pre-created
`pending` row from `POST /api/relays`).
- **Pros:** code already exists; minimal effort; closes the hole; ties SANs to an operator-created row (see PENDING, F8).
- **Cons:** operator must pre-register each relay + distribute a token (ops friction).

### Option B — mTLS bootstrap identity
Issue a short-lived bootstrap credential out-of-band; require client-cert on Provision.
- **Pros:** no bearer token to leak. **Cons:** chicken-and-egg cert distribution; more moving parts.

### Option C — Network-layer restriction only
Keep Provision tokenless but firewall it to an operator network / mTLS-fronted admin path.
- **Pros:** zero code. **Cons:** brittle; fails if the port is ever exposed; not defense-in-depth.

## Recommendation (non-binding)
Option A now (it's nearly free), optionally hardened with C at the network layer. Revisit B only
if token distribution proves painful.

## Dependency on the Provider Plane
Closing this hole is **backend-only and does not require the provider dashboard** — ship it now.
But relay creation + token issuance is a *provider* action, not a tenant-admin one, and today the
issuing endpoint `POST /api/relays` is `RequireRole("admin")` with no workspace guard (any tenant
admin can mint relay tokens). Interim: put a `RequireProvider` gate on that endpoint. Long-term:
re-home it under the provider tier ([[PENDING-07a-Provider-Identity-and-Authorization]]).

## Open Questions
- Token TTL (admin currently issues 24h) and delivery (env/file/installer)?
- Should re-provision / renewal reuse this path or get its own RPC? (see PENDING-13 pattern for relay itself)
- Rate-limit Provision regardless (unbounded anonymous CA signing today).

## Rough Effort / Priority
**S–M, P0.** Highest value-per-effort fix in the relay subsystem.
