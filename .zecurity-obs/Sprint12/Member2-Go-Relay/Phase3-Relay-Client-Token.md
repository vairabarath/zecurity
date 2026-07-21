---
type: phase
member: M2
sprint: 12
phase: 3
title: Relay Client Provisioning-Token Delivery
status: done
depends_on:
  - Sprint12/Member2-Go-Relay/Phase1-Provision-Token-Enforcement
tags:
  - rust
  - relay
  - provisioning
  - pending-01
---

# Phase 3 — Relay Client Provisioning-Token Delivery

> Depends on M2 Phase 1 (the server now *requires* the token). Client half of PENDING-01.

## Goal

Make the relay client actually send its single-use provisioning token instead of an empty string
(`relay/src/provision.rs` currently sends `provisioning_token: String::new()`):

1. Read the token from config (env / file / installer arg).
2. Populate `ProvisionRequest.provisioning_token`.
3. Fail fast with a clear operator error when the token is missing (before making the RPC).

## Files

| File | Change |
|------|--------|
| `relay/src/provision.rs` | send the real `provisioning_token` |
| `relay/src/config.rs` (or wherever `Config` lives) | add `provisioning_token` source (`RELAY_PROVISIONING_TOKEN` env / file) |
| `relay/README.md` (or ops doc) | document token delivery to operators |

## provision.rs

```rust
// BEFORE:
//   provisioning_token: String::new(),
// AFTER:
//   provisioning_token: cfg.provisioning_token.clone(),

// Before building the request, guard:
if cfg.provisioning_token.trim().is_empty() {
    anyhow::bail!(
        "no provisioning token: set RELAY_PROVISIONING_TOKEN (issued by an operator via \
         POST /provider/relays)"
    );
}
```

## config.rs

```rust
// Load provisioning_token from env RELAY_PROVISIONING_TOKEN, falling back to a
// file path (e.g. RELAY_PROVISIONING_TOKEN_FILE) if that is how the installer delivers it.
// Token is single-use and short-lived (controller default TTL 24h) — it is consumed on first
// successful Provision; a re-provision needs a fresh token.
```

## Config

```
RELAY_PROVISIONING_TOKEN        the single-use JWT issued by POST /provider/relays
RELAY_PROVISIONING_TOKEN_FILE   (optional) path to a file containing the token
```

## Tests / manual

- With a valid token → relay provisions, receives its cert (existing flow).
- With an empty/missing token → clear operator error, no RPC attempt.
- Re-run with the same (now-burned) token → server rejects (`PermissionDenied`); operator must
  request a fresh token.

## Build Check

```bash
cd relay && cargo build
```

## Implementation Checklist

- [x] **M2-E1** `relay/src/provision.rs` — send real `provisioning_token`
- [x] **M2-E2** relay config — `RELAY_PROVISIONING_TOKEN` (env/file) + README note
- [x] **M2-E3** fail fast with a clear error when the token is missing
- [x] **Build gate:** `cd relay && cargo build`

## Post-Phase Fixes

_None yet._
