# ADR-019: Revocation Enforcement & Propagation

**Status:** Proposed
**Track:** B - Architecture / Security
**Author:** Zecurity Engineering
**Reviewed:** 2026-07-06
**Depends on:** ADR-014 (Relay Stabilization), ADR-016 (Labelled Relay List)
**Related:** Relay E2E Flow & Security Review (F2, F12)

---

## Purpose

Define how certificate/identity revocation is propagated and enforced across the
platform, and close the gaps where a revoked entity is still trusted. Today
revocation is enforced at a single choke point (connector rejects revoked
clients); the relay enforces nothing, and principal-level ("suspend the user")
revocation is not enforced at login. This ADR proposes a coherent model and a
concrete first step (Flow A: revocation list to the relay).

---

## Current State (as of 2026-07-06)

- **Only client devices are in a CRL.** The controller serves one CRL,
  `GET /ca.crl?workspace_id=<uuid>` (`GenerateClientCRL`), listing
  `client_devices` rows with `revoked_at` set, signed by the **workspace CA**.
  Connectors and shields are **not** in any CRL. Relay certs are
  Intermediate-CA-signed and have **no** revocation state at all.
- **Only the connector consumes a CRL.** It fetches `/ca.crl` over plain HTTP
  every 5 min (`CrlManager`) and rejects revoked client certs on the inner
  tunnel (`device_tunnel.rs`, `relay_handler.rs`). The client, relay, and shield
  fetch no CRL.
- **Connector/shield "revocation" is a status check, not cert revocation.**
  `status='revoked'` is checked at enrollment, control-stream open, and
  renewal — but the cert stays cryptographically valid and is in no CRL.
- **Relay (F2):** authenticates connectors/clients by chain + SPIFFE only, with
  **no revocation check** — a revoked connector can still register and be
  bridged; a revoked client is only stopped downstream at the connector.
- **Controller control plane (fixed):** `GetACLSnapshot` now rejects a device
  whose `revoked_at` is set (previously it served revoked devices on a
  still-valid, user-scoped token). Shipped separately.

---

## The Revocation Model (two layers)

Credential revocation and principal revocation are different concerns and must
be enforced at different layers:

| Layer | Revokes | Mechanism | Stops re-enrollment? |
|---|---|---|---|
| **Credential** | one cert / identity instance | CRL / SPIFFE deny-list | No — re-enrollment mints a fresh cert + SPIFFE |
| **Principal** | the user / workspace | status check at **login (Bootstrap)** | Yes — no token issued → no new enrollment |

Key consequence: **revoking a certificate (by SPIFFE *or* serial) never stops a
re-login.** A new enrollment produces a new `device_id` → new SPIFFE **and** new
serial, so neither revocation key blocks it. Blocking the *principal* is a
login-layer concern (see F12 below), not a revocation-list concern.

---

## Decision

### 1. Flow A — Controller pushes a revocation list to the relay

The controller maintains a platform-wide **revoked-identity set** (revoked
clients + revoked connectors) and delivers it to each relay **piggybacked on the
relay heartbeat response** (`RelayService/Heartbeat`, mTLS, ~30s). The relay
caches the set and rejects a peer whose identity is revoked at **Register**
(connector) and **Lookup** (client), before bridging.

- **Delivery:** heartbeat response, not HTTP pull. Reuses the relay's existing
  authenticated periodic channel; no new endpoint; ~30s freshness; matches the
  "relay gets its instructions via heartbeat" convention.
- **Versioning:** include a `revocation_version` (content fingerprint, same
  technique as ADR-016 / the F11 LabelledRelayList version). The full set is sent
  only when the version changes; steady-state heartbeats stay small.
- **Growth bound:** prune a revoked entry once its cert's `not_after` has passed
  (an expired cert can't authenticate anyway). Bounds the set to
  `revocations x cert-lifetime`.
- **Scope:** clients + connectors only. Shields never connect to the relay, so
  they are excluded.
- **v1 = handshake-time rejection.** Tearing down already-open bridged sessions
  on revocation is deferred to v2.

### 2. Revocation key = SPIFFE identity, not certificate serial

The revoked set is keyed on **SPIFFE ID** (revoked client SPIFFEs + revoked
connector SPIFFEs), not X.509 serials. Rationale:

1. **Already in hand.** The relay already extracts and validates the peer SPIFFE
   on every handshake (`tls.rs`). A serial-based check would require additional
   X.509 serial extraction the relay does not do today — new code, new fail-open
   surface.
2. **No encoding fragility.** Serials are big integers; the controller stores
   `cert_serial` as hex TEXT while Rust compares raw bytes (`raw_serial`). A
   hex/bytes/endianness/leading-zero mismatch fails **open**. SPIFFE is a
   canonical string with parsing already shared across Go and Rust (`appmeta`).
3. **Survives connector renewal.** `RenewCert` keeps `connector_id` (stable
   SPIFFE) but mints a **new serial** every ~7 days. A serial entry goes stale on
   renewal; a SPIFFE entry revokes the identity across all its certs.
4. **One flat set across workspaces.** Client/connector certs are signed by many
   workspace CAs; a real X.509 CRL is single-issuer. SPIFFE IDs are globally
   namespaced, so one flat set works with no per-issuer bundling.
5. **Semantic clarity.** Revocation intent is "block this identity"; SPIFFE names
   it directly and the list is human-auditable.

Rejected counter-argument: *consistency* with the connector's existing
serial-based CRL. The relay is a new consumer over a different channel
(heartbeat, not HTTP `/ca.crl`) and will not reuse `crl.rs` regardless, so the
consistency benefit does not carry over.

### 3. Principal revocation must be enforced at login (F12)

To make "revoke the user so they cannot return" actually stick, `Bootstrap` must
require `users.status='active'` **and** `workspaces.status='active'` before
issuing a token. Today it filters on neither (documented in-code as F12,
`bootstrap.go`), so a suspended user can re-login, obtain a token, and enroll a
new device — regenerating a valid cert + SPIFFE. Closing F12 is the real
"make revocation stick" fix and is independent of the relay list.

---

## Scope & Non-Goals

- **In scope (this ADR):** Flow A (relay revocation list, SPIFFE-keyed,
  heartbeat delivery); the model; F12 as the principal-revocation fix.
- **Related, separate:** Flow B — the controller pushes revoked **shield + relay**
  identities to the **connector** (so a connector rejects a revoked shield on the
  `:9091` mTLS and a revoked relay when dialing it). Requires adding relay
  revocation state (relays have none today). Tracked separately.
- **Deferred:** mid-session teardown of active bridged sessions on revocation;
  CRL-over-HTTP hardening on the connector path (unsigned, 5-min refresh).

---

## Alternatives Considered

- **X.509 CRL to the relay (serial-based):** rejected as the relay key — see
  "Revocation key" above (multi-issuer, encoding fragility, renewal staleness,
  requires serial extraction).
- **HTTP pull (like the connector):** rejected — per-workspace endpoint doesn't
  fit a platform-level relay, unauthenticated, unsigned, and still needs a new
  aggregate endpoint.
- **OCSP-style online check (relay asks controller per handshake):** rejected —
  puts the controller in the connection hot path (violates "controller is not in
  the tunnel hot path") and couples relay availability to the controller.
- **Short-lived certs, revoke by not renewing:** noted as the long-term
  direction but insufficient now — connector certs are 7 days and relays 30 days
  with no renewal, so "stop renewing" is not a timely kill-switch. Complementary
  to, not a replacement for, the deny-list.

---

## Open Questions (for team discussion)

1. Confirm SPIFFE-keyed deny-list over serial for the relay (this ADR's
   recommendation).
2. Full-set vs delta on version change — is full-set-on-change acceptable given
   the prune-at-expiry bound, or do we want deltas?
3. Do we want v1 mid-session teardown, or is handshake-time rejection enough for
   the first cut?
4. Prioritization of F12 (principal revocation) relative to Flow A — F12 is what
   makes user revocation durable; Flow A is what makes connector/client
   credential revocation enforced at the relay.
5. Flow B ordering: does adding relay revocation state (so a connector can reject
   a revoked relay) belong in this effort or a follow-up?

---

## Implementation Touch Points (indicative, per component)

- **Proto:** `RevokedIdentityList` (repeated SPIFFE + version) on
  `HeartbeatResponse` (`proto/relay/v1/relay.proto`).
- **Controller (Go):** aggregate revoked client + connector SPIFFEs platform-wide
  (`client_devices.revoked_at` + `connectors` `status='revoked'`), prune at
  cert expiry, version, and attach to the heartbeat response
  (`internal/relay/heartbeat.go`, `store.go`). Separately, F12 in
  `internal/bootstrap/bootstrap.go`.
- **Relay (Rust):** cache the set from the heartbeat response
  (`heartbeat.rs`, new `revocation.rs`); reject revoked SPIFFEs at Register and
  Lookup (`session.rs`); serial/SPIFFE extraction helper (`tls.rs`).
