---
type: planning
status: active
sprint: 19
tags:
  - sprint19
  - pending-13
  - client-device-lifecycle
  - device-trust
  - outbox-consumer
---

# Sprint 19 — Client Device Lifecycle (PENDING-13)

> **Read this before writing a single line of code.**
> Source of truth: [[PENDING-13-Client-Device-Lifecycle]] and its promotion
> [[ADR-028-Client-Device-Lifecycle-and-Cert-Renewal]]. This sprint closes the
> SCIM→device outbox loop and the server→client trust signal for client devices.

## Scope
- Track 1 (DONE, merged contract + handler branch `feat/pending-13-device-revoke-handler`):
  first outbox consumers revoke a user's devices on `device.trust.revoke.requested`,
  and record `device.trust.re_enrollment_required`.
- Track 2 (DONE): server→client **device directive** on the 60s ACL poll —
  `REVOKED` / `RE_ENROLL_REQUIRED` / `RENEW_SOON` / `NONE`, with the client daemon
  reacting (wipe key, stop tunnels, surface the right message). See
  `Member2-Go/Track2-Device-Trust-Directive.md` (all acceptance criteria checked).
- Track 3 (later): `RenewCert` RPC + daemon renewal scheduler, riding the
  `RENEW_SOON` channel defined in Track 2.

## Dependency shape
- Track 1 ships independently; its branch is stacked on the merged
  `feat/identity-device-trust-contract` (now in `fixed-pendings`).
- Track 2 stacks on the Track 1 branch and upgrades `ReEnrollHandler` to set
  `client_devices.status`.
- The SCIM producer (Sathiya, Sprint 17 Phase 6) enqueues the events via the shared
  `identity` contract; until it lands, the loop can't fire end-to-end but the
  consumer/handler is correct and tested.

## Roles
- Member2-Go: controller + client (Rust) implementation of PENDING-13.
- Member1-Go (Sathiya): SCIM producer (PENDING-05 / Sprint 17 Phase 6) — owns the
  enqueue side, not the consumer.
