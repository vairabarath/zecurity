---
type: adr
status: pending
id: PENDING-13
domain: identity/ops
priority: P2
created: 2026-07-03
related:
  - ADR-002-Client-Daemon-Required
  - PENDING-02-Certificate-Revocation-Enforcement
tags: [pending, adr, client, device, pki, lifecycle]
---

# Pending ADR 13 — Client Device Lifecycle & Cert Renewal

> **Status: DRAFTED as [[ADR-028-Client-Device-Lifecycle-and-Cert-Renewal]] (PROPOSED, 2026-08-20).**
> The scope below was written 2026-07-03 and partly inferred. It has since been **verified against the
> code** and reshaped into three tracks (close-the-SCIM-loop / server→client trust signal / auto-renew +
> re-enroll). **Read ADR-028 for the authoritative, grounded design** — this file is retained as the
> original problem statement. On team ratification, ADR-028 flips to `accepted`.

## Context / Current State

Connectors and shields have automatic cert renewal (ADR-003 pattern). For **client devices**,
Sprint 11.3 delivered **access-token refresh & session management**, but that is the bearer/session
layer — **device certificate renewal** appears to still be unaddressed (the original roadmap listed
"Client cert renewal — same pattern, future sprint" as deferred). There is also no evident
**device management UI**: an admin/user can't list a user's enrolled devices, see last-seen, or
revoke a specific lost/stolen device. Client SPIFFE revocation on logout exists
(recent commit "revoke device SPIFFE on logout"), which is a good start but not full lifecycle.

## Problem — Decision Needed

How do client device certs renew, and what device-lifecycle management do admins/users get?

## Options

### Option A — Mirror the connector renewal pattern for clients
Daemon auto-renews the device cert (proof-of-possession CSR) before expiry; no user action.
- **Pros:** consistent with connector/shield; daemon already resident (ADR-002). **Cons:** must
  handle offline-past-expiry (re-enroll flow).

### Option B — Device management surface
Admin/user UI + API: list devices (last-seen, posture), revoke a device (→ CRL, PENDING-02),
rename, see which are active.
- **Pros:** essential for lost/stolen response + audit. **Cons:** new UI + API + revocation wiring.

### Option C — Short-lived certs, re-enroll instead of renew
Skip renewal; re-run enrollment on expiry.
- **Pros:** simplest. **Cons:** worse UX (interactive re-auth); not automatic.

## Recommendation (non-binding)
Option A (auto-renew) for UX parity with connectors, plus **Option B** device management (the
lost/stolen-device story is a security must-have and pairs with PENDING-02 revocation and
PENDING-05 deprovisioning).

## Open Questions
- Confirm actual client device-cert renewal status in code (this ADR assumes it's absent).
- Renewal window/TTL for client certs; behavior when a device is offline past expiry?
- Where does device management live — tenant admin, end-user self-service, or both?

## Rough Effort / Priority
**M, P2** (B rises to P1 if customers demand lost-device revocation).
