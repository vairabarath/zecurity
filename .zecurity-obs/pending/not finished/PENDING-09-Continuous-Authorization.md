---
type: adr
status: approved-pending
id: PENDING-09
domain: zero-trust
priority: P2
created: 2026-07-03
related:
  - ADR-001-Sprint8-ACL-Snapshot-Caching
  - PENDING-08-Device-Posture-Health
  - PENDING-06-MFA-Step-Up-Auth
updated: 2026-11-12
tags: [pending, adr, zero-trust, authorization]
---

# Pending ADR 09 — Continuous / Re-evaluated Authorization

> **Status: PENDING — for team discussion.** On adoption, promote to the next free `ADR-0NN`.
>
> **Partial implementation note (Sprint 15):** a bounded variant of **Option B only** shipped as a
> side effect of PENDING-08's connector session-registry work — no new revocation RPC; the connector
> diffs `(spiffe_id, resource_id)` between the previous and newly-applied ACL snapshot in
> `control_stream.rs`'s `AclSnapshot` handler and cancels tunnels for pairs that dropped out, bounded
> by the existing heartbeat/snapshot-expiry interval, not immediate. **Options A (push-based
> immediate revocation) and C (risk-scored step-up) are still fully open**, and the team explicitly
> deferred promoting this to an ADR until the bounded-latency approach is validated in practice —
> see `.zecurity-obs/Sprint15/path.md` line ~362. Status stays `pending` because the actual decision
> (which option to standardize on) has not been made; Sprint 15 is a tactical interim measure, not a
> ratified choice.

## Context / Current State

Authorization is effectively **connect-time**. The connector/client enforce the local ACL
snapshot, which refreshes on a ~60s TTL and on policy-change notifications (ADR-001 caching +
epoch/CAS ADR-013). But there is no explicit model for **re-evaluating an in-flight session** when
something changes mid-session: group membership revoked, device posture degrades (PENDING-08),
risk rises, or a token is revoked. Long-lived flows (SSH/RDP) can outlive the authorization that
permitted them.

## Problem — Decision Needed

How aggressively do we re-evaluate live sessions, and on which triggers?

## Options

### Option A — Snapshot-TTL re-evaluation (mostly already true)
Rely on the 60s ACL refresh: new flows use the new snapshot; formalize + tighten the interval and
ensure existing flows are re-checked, not just new ones.
- **Pros:** builds on what exists. **Cons:** up-to-TTL lag; doesn't kill established connections.

### Option B — Event-driven session revocation
On revocation/policy-change/posture-drop, actively tear down affected live tunnels (client daemon
+ connector cooperate).
- **Pros:** true continuous authz; bounded exposure. **Cons:** needs a revocation-signal path to
  data plane + connection kill logic.

### Option C — Risk-scored step-up
Continuous signals feed a risk score; threshold crossing forces step-up (PENDING-06) or drop.
- **Pros:** strongest posture. **Cons:** most complex; needs posture + step-up first.

## Recommendation (non-binding)
Formalize A now (guarantee existing flows are re-checked each refresh, not just new ones), add B
for the high-value triggers (deprovision / device revoke), and treat C as the long-term goal once
PENDING-08 posture exists.

## Open Questions
- Which triggers must kill live connections vs. only block new ones?
- Acceptable revocation-to-teardown latency? Interaction with relay drain semantics (ADR-017)?

## Rough Effort / Priority
**A: S · B: M · C: L**, P2.

---

## Current state (verified against `main` ba97f259, 2026-11-12)

The Context above is out of date: connectors have no 60 s TTL any more, and Option B is built for every trigger that changes the compiled ACL.

- **Push path.** `NotifyPolicyChange` (`internal/policy/notifier.go:65`) bumps the version and fires the push hook, which compiles once per burst and sends `AclSnapshot` to every connected connector (`internal/connector/acl_push.go`). The connector's 15 s health report is the fallback when a push fails (`control_stream.go:1186`). The push is asynchronous: callers never see a push failure.
- **Teardown.** The connector diffs `(spiffe_id, resource_id)` between snapshots (`connector/src/policy/mod.rs:55`) and aborts live sessions (`session_registry.rs:103`). This covers direct, QUIC, relay and shield paths. FU-15 makes the cut a stream reset rather than a graceful finish.
- **Client.** The client drops the resource and RSTs its sockets on the FU-8 wake hint. The 60 s poll remains as the fallback (`client/src/daemon.rs:38`).
- **Measured (Sprint 22):** member removal to connector cancel 44 ms; device revoke refused 85 ms.

| Trigger | Cuts live sessions | Latency |
|---|---|---|
| Group member removed (console, SCIM); resource unassigned, deleted or retargeted | yes | ms |
| User suspended or deprovisioned (SCIM); IdP connection disabled (via outbox) | yes | ms |
| Device revoked (admin, self, SCIM) | yes | ~85 ms |
| `zecurity-client logout` | yes: it revokes the device first (`client/src/cmd/logout.rs:30`) | ms. Gap: an expired stored access token makes the revoke fail silently |
| Posture fails / report goes stale (PENDING-08/16) | yes | report cadence ≤ 300 s; stale after 10 min |
| Country change (PENDING-20) | yes | ≤ 60 s poll (≤ 300 s via report) |
| Login session ends on its own (idle 168 h, max 720 h), or all sessions ended (IdP disable, generation bump) | **no**: the device keeps tunnelling on its cert until it expires (≤ 7 days, since `RenewCert` needs a live session) | — |
| Console sign-out (browser) | no; it is a different session | — |
| Client cert on the CRL alone | only new tunnels (the CRL is checked at tunnel open) | 60–75 s |

## Decisions (2026-11-12, Sathiya)

| # | Decision |
|---|---|
| C-1 | **When a device's login session ends, its live tunnels end.** This covers `zecurity-client logout` (closing its expired-token gap), idle or max-lifetime expiry, and server-side "end all sessions" (IdP disable, generation bump, failed lifecycle check at refresh). It does not cover console sign-out: the browser session is not a device's session. |
| C-2 | **No CRL-driven live cut.** Every client-cert CRL entry comes from the `client_devices.revoked_at` trigger (`039_certificate_history.sql`), and the same statement drops the device from the compiled ACL, which already cuts its sessions. Cutting by SPIFFE ID on a CRL entry would also wrongly kill a renewed cert's sessions: renewal keeps the SPIFFE ID, and the session registry stores no serial. Revisit only if a path ever revokes a single serial without `revoked_at`. |
| C-3 | **Posture and geofence latency (≤ 300 s report, 10 min staleness, ≤ 60 s country) is acceptable.** |
| C-4 | **Log SCIM's ignored `NotifyPolicyChange` errors** (six post-commit sites). The change is already committed, so the request must not fail. |
| C-5 | **Option C (risk scoring, step-up) waits for PENDING-06.** `RequireBreakGlassMFA` is still a no-op. |

## Design for C-1 (proposed, for approval)

| # | Design |
|---|---|
| D-1 | **Device ↔ session link in Postgres**, migration `056_client_device_session.sql`. New columns on `client_devices`: `session_id TEXT`, `session_idle_until TIMESTAMPTZ` and `session_max_until TIMESTAMPTZ`. `EnrollDevice` stores the access token's `sid`, which is required: a token without one is refused. It also stores the session's `max_lifetime_at` (fixed from the first sign-in, ADR-006) and `now + JWTRefreshTTL`. The `sid` stays the same across refresh rotation (`refresh.go`, `newSID := sid`). |
| D-2 | **Deadlines live in Postgres, not Valkey.** A device is never revoked because a Valkey key is missing: a Valkey flush or restart must not revoke every device. Each successful client-session refresh moves `session_idle_until` forward for the devices with that `sid` (a hook from `auth` into the client store). |
| D-3 | **Immediate revoke on explicit ends:** - `EndSession` (it already accepts an expired but correctly signed access token) revokes the devices whose `session_id` is the token's `sid`. This closes the logout gap on the server. - "End all of the user's sessions" (`InvalidateUserSessions` and the two refresh-time lifecycle/generation failures) revokes all of that user's devices. - Console sign-out ends only the browser's `sid`, which no device carries. |
| D-4 | **Expiry sweeper.** An in-controller worker (single instance, D-02) runs every 60 s. It revokes active devices whose `session_idle_until` or `session_max_until` has passed. Latency after the deadline: ≤ 60 s plus the push. |
| D-5 | **Revoke, not suspend.** The existing terminal `revoked_at` path is used: CRL entry, ACL drop, live cut, audit. One `client.device.revoked` audit row per device, with system actor and `reason: session_ended` (or `signed_out` / `sessions_revoked`). This matches today's lifecycle: a dead session cannot renew the cert anyway, so the device already dies within ≤ 7 days. This change shortens that tail to seconds or minutes. **User-visible effect:** at the 30-day max lifetime the user signs in again, which enrols a new device. Under an approved-only resource policy, that device needs approval again; this already happens today, with up to 7 days of lag. |
| D-6 | **Client.** `zecurity-client logout` refreshes an expired access token before `RevokeDevice`. This is insurance only: D-3 already covers it on the server. |
| D-7 | **Tests:** - console sign-out leaves devices untouched; - `EndSession` for device A's `sid` revokes only A, and the same user's device B keeps working (as in the FU-15 two-device check); - idle and max-lifetime expiry each revoke within one sweep, with one audit row; - a Valkey flush revokes nothing; - an IdP disable revokes all of that user's devices; - a live run with a real tunnel cut on logout and on expiry. |
| D-8 | **Out of scope (follow-up).** A re-login leaves the previous device active until its own session ends (D-4 then revokes it). Revoking it at re-login is a separate change. |

**Schema:** `056_client_device_session.sql` `[schema: reset DB]`.
