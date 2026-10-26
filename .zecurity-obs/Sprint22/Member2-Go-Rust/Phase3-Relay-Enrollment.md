---
type: phase
member: M2
person: Barath
sprint: 22
phase: 3
execution: E
title: Relay Enrolment from the Console
status: planned
depends_on: [2]   # A merged
schema_change: false
tags:
  - go
  - react
  - provider
  - relay
  - provider-console
---

# Phase 3 (E) — Relay Enrolment from the Console

> **Decision Record:** **D-30** (console may create relays), **D-31** (relay-ops may create), **D-20** (expired token → replace the relay; no re-issue), **D-14** (audit).

## Problem (verified 2026-10-26)

- `POST /provider/relays` returns only `{relay_id, provisioning_token, expires_at}`.
- A relay also needs `CONTROLLER_ADDR`, `CONTROLLER_HTTP_ADDR` and `RELAY_CA_FINGERPRINT` (`relay/src/config.rs`). Operators compute the fingerprint by hand today (`docs/sprint20-live-acceptance-runbook.md` §6.3: `curl /ca.crt | openssl x509 -outform DER | sha256sum`).
- The console has no way to create a relay.

## Goal

From the Relays page, a super-admin or relay-ops operator adds a relay and gets, once, a provisioning token plus a complete install command to paste on the relay host.

## Contract change — `POST /provider/relays` → 201

```json
{
  "relay_id": "…",
  "provisioning_token": "…",
  "expires_at": "…",
  "ca_fingerprint": "<sha256 of the intermediate CA DER, lowercase hex>",
  "controller_grpc_addr": "<PROVIDER_RELAY_CONTROLLER_ADDR or empty>",
  "controller_http_addr": "<PROVIDER_RELAY_CONTROLLER_HTTP_ADDR or empty>"
}
```

- `Cache-Control: no-store`.
- The fingerprint comes from a helper shared with `shield/token.go` `loadCAFingerprint` (extract it; don't duplicate the SQL).
- The two settings are **optional**; `.env.example` documents them.

## Console

- **Add relay** button on Relays (both roles; hidden for unknown roles).
- **Form:** name (required), DNS names and IP addresses as chip inputs, validated with the same rules as the server; server errors mapped to field messages.
- **Result dialog**, shown once:
  - the token with **Copy**, its expiry ("valid for 24 hours");
  - an install block with every variable filled in (placeholders where a controller address setting is empty):
    ```bash
    RELAY_ID=… CONTROLLER_ADDR=… CONTROLLER_HTTP_ADDR=… \
    RELAY_CA_FINGERPRINT=… RELAY_PROVISIONING_TOKEN=… \
    RELAY_DNS_SANS=… RELAY_IP_SANS=… zecurity-relay
    ```
  - closing the dialog discards the token; the list refreshes and shows the new **pending** relay.
- **Pending relays** (list and detail): "Waiting for enrolment"; hint per D-20: "If the token has expired, delete this relay and add a new one."

## Tests (write first)

**Go:** response fields; fingerprint equals the SHA-256 of `/ca.crt` DER; settings empty vs set; `no-store` header; audit unchanged (`relay.create`, no token in details).

**Console:**
- Form validation mirrors the server; server errors map to fields.
- Token dialog: shown once; Copy works; token never in `sessionStorage`, the URL, or the DOM after close.
- Install block contains every variable; placeholders when settings are empty.
- relay-ops sees **Add relay**; API-surface pin includes `POST /provider/relays`; clipboard static guard allows exactly the token dialog and the temporary-password dialog.

## Acceptance

[[Sprint22/Acceptance-Test-Plan]] → **AT-E1 … AT-E5** (incl. the live enrolment on the lab).

## Test gate

Go gate and console gate from [[Sprint22/path]].

## Post-Phase Fixes

*(None yet.)*
