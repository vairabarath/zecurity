---
type: adr
status: proposed
id: ADR-028
former_id: PENDING-13
domain: identity/ops
priority: P1
created: 2026-07-03
proposed: 2026-08-20
related:
  - ADR-002-Client-Daemon-Required
  - ADR-027-Certificate-Revocation-Enforcement
  - ADR-025-SCIM-Directory-Synchronization
  - PENDING-15-Durable-Outbox-Infrastructure
  - PENDING-05-Directory-Sync-SCIM
tags: [adr, client, device, pki, lifecycle, revocation, renewal]
---

# ADR-028 — Client Device Lifecycle, Cert Renewal & Trust-Revocation Execution

> **Status: PROPOSED (2026-08-20).** Promoted from `PENDING-13`. The original pending doc was written
> 2026-07-03 and self-flagged as "partly inferred — confirm actual client cert-renewal status in code."
> This ADR replaces that guesswork with **verified ground truth** (client + controller code surveyed
> 2026-08-20) and reshapes the scope around what actually shipped since: the durable outbox (Sprint 18),
> SCIM deprovision (PENDING-05, in progress), and CRL revocation enforcement (ADR-027).
>
> **Recommendations are baked in below but not yet ratified.** No `decided:` date until the team accepts.

---

## Context / Current State (verified, not inferred)

A client device gets an mTLS **device certificate** at enrollment; it is the device's proof of identity on
the network. Surveying the code (2026-08-20) shows the platform is **further along than the July doc
assumed** on management/revocation, but has **three real gaps** on the client lifecycle.

### What EXISTS

**Controller (Go):**
- `client_devices` is a first-class table — `id, user_id, workspace_id, name, os, cert_serial,
  cert_not_after, spiffe_id, last_seen_at, created_at, revoked_at`
  (`migrations/011_client.sql`, `014_connector_logs.sql`).
- Enrollment issues a workspace-CA-signed cert (**7-day TTL**, `clientCertTTL = 7*24h`) + controller-
  assigned SPIFFE `client/<deviceID>` — `internal/client/service.go:419`, `internal/pki/workspace.go:440`.
- **Client cert revocation already works**: `GenerateClientCRL` publishes every `client_devices` row with
  `revoked_at IS NOT NULL` onto the workspace CRL at `GET /ca.crl?workspace_id=<uuid>`; connectors poll it
  (~60s, per ADR-027) — `internal/pki/workspace.go:530`, `internal/connector/ca_endpoint.go:41`.
- **Device-management API exists**: `myDevices`, `clientDevices` (@ADMIN), `revokeDevice` (@ADMIN) —
  `graph/client.graphqls`; resolvers set `revoked_at` + `NotifyPolicyChange` (`graph/resolvers/client.resolvers.go:44`).
- The **durable outbox** (Sprint 18) is wired: registry + background processor run in
  `cmd/server/main.go`, `Enqueue(ctx, tx, evt)` is transactional (`internal/outbox/*`).

**Client (Rust):**
- Interactive enrollment (OAuth+PKCE, in-memory P-384 keypair + CSR proof-of-possession) — `client/src/login.rs`.
- Device cert + private key **encrypted at rest** (AES-256-GCM, per-workspace key, 0600) — ADR-002 satisfied — `client/src/state_store.rs`.
- **Client-initiated** revoke on logout (`RevokeDevice` RPC + local wipe) — `client/src/cmd/logout.rs`.

### What is MISSING (the three gaps)

1. **No cert auto-renewal.** There is no `RenewCert` RPC on `ClientService` and no daemon renewal
   scheduler (connectors/shields/relays all have one; clients don't). `cert_expires_at` is captured and
   only *displayed* in `status`. **With a 7-day cert and zero renewal, every device breaks weekly** and
   the only remedy is a full interactive browser re-login. No re-enroll path for offline-past-expiry.
2. **No server→client trust signal.** Every `ClientService` RPC is unary; ACL freshness is a **60s
   client pull-poll** (`GetACLSnapshot`); the client consumes only the *relay* CRL, never one about
   itself. So when a device is revoked server-side, the client never learns — mTLS just fails silently,
   and the encrypted key material stays on disk. **The `device.trust.re_enrollment_required` event SCIM
   will emit has nowhere to land on the client.**
3. **The outbox loop is open.** The outbox registry is **empty at runtime** — no `RegisterHandler` in
   `cmd/`, and **zero `Enqueue` producers** anywhere. SCIM (PENDING-05) will be the first producer;
   **PENDING-13 is the first — and currently missing — consumer.** Also `client_devices.last_seen_at`
   exists in schema + GraphQL but is **never written** (no data source).

---

## Problem — Decision Needed

How do client device certs renew, how does a device learn it has been revoked or must re-enroll, and who
executes the `device.trust.*` events that SCIM deprovisioning enqueues?

---

## Reshaped scope — three tracks

The July doc's Options A/B/C map onto today's reality like this: **Option B (device management) is ~80%
already built**; **Option A (auto-renew) is fully missing**; and a **new pillar the July doc predates** —
the outbox consumer + the server→client trust signal — is the keystone that connects SCIM deprovision
(PENDING-05) and CRL enforcement (ADR-027) to the actual device.

```text
Track 1  CLOSE THE LOOP     controller-only, small, highest value now
         SCIM enqueues device.trust.* → PENDING-13 handler executes cert revocation

Track 2  THE TRUST SIGNAL   controller + client + proto — the linchpin
         a server→client "device directive" channel + device lifecycle status + last_seen

Track 3  RENEW / RE-ENROLL  controller + client + proto — fixes weekly breakage
         RenewCert RPC + daemon renewal scheduler + offline re-enroll fallback (rides on Track 2)
```

**Dependency shape:** Track 1 ships independently. Track 2 is the linchpin — Track 1's re-enroll signal
and Track 3's revoke-reaction both need it. Track 3 rides on Track 2's channel for the offline case.
Device management (Option B) is *finished off* by Tracks 2–3 (last-seen + lifecycle status), not a
separate track.

---

## Decisions

### D1 — Renewal mechanism → **Option A (auto-renew), with re-enroll as the offline fallback**

The daemon proactively renews the device cert before expiry via a new `RenewCert` RPC: a
proof-of-possession CSR **signed by the existing device private key** (the daemon is already resident,
ADR-002), so no user interaction. If a device is offline past expiry and renewal is no longer possible,
it falls back to the interactive re-enroll flow (Option C) — but that is the exception, not the norm.
Rejected: Option C as the *primary* mechanism (interactive re-auth every 7 days is unacceptable UX).

- *Rationale:* parity with connector/shield/relay renewal; the daemon already holds the key; kills the
  weekly-breakage problem outright.

### D2 — Server→client signal → **piggyback a "device directive" on the existing 60s ACL poll**

Rather than build a new streaming plane, extend the response of the poll the daemon already makes every
60s (`GetACLSnapshot`, or a sibling lightweight field) to carry a small **device directive**:
`{ directive: none | renew_soon | revoked | re_enroll_required, reason }`. The daemon acts on it each
cycle. This mirrors the established platform pattern ("connector/shield receive instructions via
heartbeat piggyback — no new RPCs") and keeps everything unary + fail-closed.

- *Alternatives considered:* a dedicated `GetDeviceState` unary RPC (acceptable, slightly cleaner
  separation, one more round-trip); true server-push streaming (rejected this sprint — heavy, new
  connection-management surface, inconsistent with the pull-poll architecture).
- *Latency note:* worst-case reaction time = one poll interval (≤60s). Acceptable because the
  **authoritative** enforcement is still server-side (revoked SPIFFE drops from the ACL and connectors
  reject the device immediately via ADR-027 CRL) — the directive is for **client-side UX + local secret
  hygiene**, not the security boundary.

### D3 — Client reaction to revocation → **graceful shutdown + wipe local key material**

On a `revoked` directive the daemon: stops tunnels, shows a clear "your access was revoked" message, and
**wipes the encrypted device key + session state locally** (reusing `clear_workspace_state`, already used
on logout). On `re_enroll_required` it wipes the now-useless cert and prompts re-enrollment.

- *Rationale:* today a revoked device leaves usable secrets on disk indefinitely. Server-side enforcement
  already blocks network access; this closes the **local secret-hygiene** gap and gives honest UX.
- *Security invariant:* the wipe is a client-side hygiene action, never the enforcement of record. A
  device that never checks in is still fully blocked server-side.

### D4 — Cert TTL & renewal window → **keep 7-day TTL; renew at ~60% of life; 3-day pre-expiry warn**

Keep `clientCertTTL = 7d`. The daemon attempts renewal once elapsed life crosses **~60%** (≈day 4), with
jittered retries. If renewal keeps failing and the cert enters its last ~24h, surface a warning in
`status`. Past expiry with no successful renewal → re-enroll.

- *Open sub-question for review:* is 7 days the right TTL once renewal exists? A shorter TTL (e.g. 24–48h)
  tightens the revocation-vs-CRL race but increases renewal traffic. Left as an open question (see below).

### D5 — Device lifecycle status → **add a first-class `status` to `client_devices`**

Replace the bare `revoked_at` semantics with an explicit status the whole system can reason about:

```text
active            normal
renew_pending     renewal window entered (informational)
revoked           access cut (revoked_at set; on CRL; SPIFFE dropped from ACL)
re_enroll_required directory reactivated the user; old cert dead, device must re-enroll
```

`revoked_at` stays (it is the CRL source of truth); `status` is the richer state that drives the D2
directive and the admin UI. Migration adds `status` (default `active`) + backfills `revoked` where
`revoked_at IS NOT NULL`.

### D6 — Manual admin/user revoke path → **stays direct/synchronous; SCIM bulk path goes through the outbox**

The existing `revokeDevice` mutation and self-service `RevokeDevice` RPC keep their **direct** behavior
(set `revoked_at` + `NotifyPolicyChange` now). The **outbox** is specifically for the async,
durable, SCIM-driven bulk case (`device.trust.revoke.requested` covering *all* of a user's devices). Both
paths converge on the same store mutation and CRL, so enforcement is identical.

- *Rationale:* don't add async indirection to an already-correct synchronous admin action; use the outbox
  where durability across a bulk/cross-service operation actually matters.

---

## The outbox event contract (owned by ADR-025 §5.1; executed here)

```text
device.trust.revoke.requested
    payload: { workspace_id, user_id, reason: "suspended" | "deleted", correlation_id }
    → PENDING-13 handler: mark ALL of (workspace_id, user_id)'s client_devices revoked
      (revoked_at = NOW(), status = 'revoked') + NotifyPolicyChange; idempotent.

device.trust.re_enrollment_required
    payload: { workspace_id, user_id, correlation_id }
    → PENDING-13 handler: mark the user's devices status = 're_enroll_required'
      (their certs were revoked on the prior suspend); surfaced to the client via the D2 directive.
```

**Handler rules:** idempotent (safe to re-run — the outbox is at-least-once); returns `nil` only on
success; a transient failure returns an error so the outbox retries with backoff; registered via
`registry.RegisterHandler(eventType, handler)` in `cmd/server/main.go` (the first such registration in the
codebase). Execution is device/cert-layer only — the identity-layer effects (suspend, generation bump,
session kill) are already done synchronously by SCIM/PENDING-05 before the event is enqueued.

---

## Audit

Every lifecycle transition writes an `audit_logs` row (tenant-scoped), dotted verbs:
`device.cert.renewed`, `device.cert.renew_failed`, `device.revoked` (with `source: admin | self | scim`
and `correlation_id` when outbox-driven), `device.re_enroll_required`, `device.wiped_local`. The
`correlation_id` threads a SCIM deprovision → outbox event → device revocation into one traceable chain.

---

## Relationship to access/session revocation

This ADR is the **device/cert layer**. It composes with, and does not replace, the layers above it:

- **Session layer (PENDING-04):** `identity_generation` bump kills JWTs/refresh at `/auth/refresh`.
- **Access/ACL layer (ADR-027):** a revoked SPIFFE drops from the compiled ACL; connectors reject via CRL.
- **Device layer (this ADR):** the cert is revoked (→ CRL) and the client is told, so it stops and wipes.

SCIM deprovision fires all three: identity/session synchronously in its own tx, and the device layer
asynchronously-but-durably via the outbox. Defense in depth — no single layer is the sole gate.

---

## Security considerations

- **Fail-closed everywhere:** an unreachable server, an unreadable directive, or an expired cert all
  result in *no access*, never default-allow.
- **Directive is UX, enforcement is server-side:** a compromised/offline client that ignores the directive
  is still blocked by the ACL drop + CRL. The directive can only make the client *more* restrictive.
- **Renewal binds to the existing key:** `RenewCert` proves possession of the current device private key;
  it cannot be used to mint a cert for a different key/device.
- **Local secret hygiene:** revoked/re-enroll directives wipe on-disk key material so a lost device that
  later comes online sheds its secrets.
- **Idempotency:** at-least-once outbox delivery means every handler must tolerate replays.

---

## Consequences

**Positive:** weekly device breakage ends; offboarding actually reaches the device (cert revoked + local
wipe); the SCIM→device loop closes; admins get real `last_seen` + lifecycle status; the first outbox
consumer establishes the handler-registration pattern for future events.

**Costs / trade-offs:** a new `RenewCert` RPC + proto change (client rebuild); up-to-60s client reaction
latency (mitigated by server-side enforcement being immediate); a schema migration on `client_devices`;
PENDING-13 becomes the reference implementation others copy, so the handler-registration and directive
patterns must be clean.

---

## Open Questions

1. **TTL after renewal exists (D4):** keep 7d, or shorten to 24–48h to tighten the revoke race now that
   renewal is automatic? Trade-off: CRL-race tightness vs renewal traffic.
2. **Directive transport (D2):** piggyback on `GetACLSnapshot` vs a dedicated `GetDeviceState` RPC —
   which does the team prefer for long-term clarity?
3. **Re-enroll UX:** for a headless/CI device (if any exist later), interactive re-enroll is impossible —
   do we need a non-interactive enrollment token path? (Likely defer; no headless client today.)
4. **Renewal offline grace:** exact window/behavior when a laptop is asleep across the renewal window but
   wakes before expiry vs after.
5. **Should `renew_soon`/`renew_pending` be surfaced to admins**, or is it purely a client-internal state?

---

## Deferred (out of scope)

- Non-interactive / service-account device enrollment (no headless client exists today).
- OCSP for client certs (ADR-027 chose CRL as the baseline; unchanged here).
- Device posture *gating* of renewal (posture is PENDING-08/16; renewal here is identity/cert only).
- Any change to relay/connector renewal (already shipped).

---

## Relationship to other decisions

Builds on **ADR-002** (resident daemon, encrypted-at-rest state — enables silent renewal + local wipe),
**ADR-027** (CRL enforcement — the device-revocation *enforcement* this ADR *triggers*), **ADR-025 §5.1**
(the `device.trust.*` event contract this ADR *executes*), and **PENDING-15** (the durable outbox that
carries those events). Consumes what **PENDING-05** (SCIM) produces. Feeds the device-management surface
that partially exists in `graph/client.graphqls`.

---

## Rough effort / phasing

- **Track 1 (close the loop):** S — controller-only; register two handlers over existing revoke machinery.
  **P1** (unblocks the SCIM offboarding chain end-to-end).
- **Track 2 (trust signal):** M — proto + controller directive + client daemon reaction + `last_seen` +
  `status` migration. **P1** (linchpin).
- **Track 3 (renew/re-enroll):** M — `RenewCert` RPC + PKI signer + daemon scheduler + offline re-enroll.
  **P1 for renewal** (weekly-breakage), re-enroll fallback P2.
