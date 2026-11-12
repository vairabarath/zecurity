---
type: adr
status: implemented
id: PENDING-09
domain: zero-trust
priority: P2
created: 2026-07-03
related:
  - ADR-001-Sprint8-ACL-Snapshot-Caching
  - PENDING-08-Device-Posture-Health
  - PENDING-06-MFA-Step-Up-Auth
updated: 2026-11-12
tags: [pending, finished, adr, zero-trust, authorization]
---

# Pending ADR 09 — Continuous / Re-evaluated Authorization

> **Implemented (2026-11-12), PR #198** `[schema: reset DB]`. Options A and B are in place (built over earlier sprints and verified here); C-1 … C-5 were decided and C-1 and D-8 built. **Option C (risk scoring, step-up) is deferred to PENDING-06.** What shipped, where it lives and how it was verified: **Implementation summary** at the end. The sections below keep the original discussion and the build history.

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

## Build (2026-11-12, merged in PR #198 as c1609588)

- **a4528303, C-4:** SCIM's six post-commit `NotifyPolicyChange` calls go through one helper that logs errors. Note: `NotifyPolicyChange` only fails on an empty workspace id. The push is asynchronous and never reports back, so for every caller a failed push is recovered by the 15 s heartbeat compile.
- **C-1 build** `[schema: reset DB]`: `056_client_device_session.sql`, `internal/devicesession` (manager and 60 s sweep), `auth.SessionObserver` (`Config.Sessions`), `AccessTokenClaims.SessionID`, `auth.Service.SessionDeadlines`; `EnrollDevice` binds the device to its session; client `logout` refreshes an expired token first.

**What ends a device (as built):**

| Event | Devices revoked | Reason in audit |
|---|---|---|
| `zecurity-client logout` (`Logout` RPC; checked: only `cmd/logout.rs` calls it, so `down`, daemon shutdown and re-login don't) | the devices of that session | `signed_out` |
| Refresh refused at the session's max lifetime | the devices of that session | `session_expired` |
| Refresh with a token whose user is no longer active, or whose generation is stale | **only the token's own session** (see the review fix below) | `sessions_revoked` |
| `InvalidateUserSessions`, reached only from a generation bump: IdP connection disabled (`setIdpConnectionStatus`) or deleted (`deleteIdpConnection`), and SCIM deprovision (`AfterBump`) | all of the user's devices | `sessions_revoked` |
| Sweep, every 60 s: `session_idle_until` or `session_max_until` passed | the devices whose deadline passed | `session_idle` / `session_expired` |
| Console sign-out | none (the browser's sid is on no device) | — |

**Review fixes before the live run:**
- **Stale-generation refresh** (advisor). The refresh-time lifecycle and generation checks run before the refresh token is compared, and they accept an expired access token. A user-wide revoke there would let a stale console tab (from before an IdP disable/re-enable) revoke the user's newer devices, on every refresh. They now end only the token's own session. Test `TestSessionObserver_StaleGenerationRefreshEndsOnlyItsOwnSession` fails on the user-wide version.
- **Enrollment race** (automated security review). A session ending between enrollment's session check and its insert would have left a device bound to a dead session, until the 7-day idle sweep. Two fixes:
  - every session-end path now deletes the session before revoking (sign-out used to revoke first);
  - `EnrollDevice` re-checks the session after inserting the device and refuses (row deleted, cert never returned) if it is gone.
  Either the re-check sees the session gone or the revoke sees the row. Test `TestEnrollDevice_SessionEndingDuringEnrollmentLeavesNoDevice`.

**Accepted:** on SCIM deprovision, `AfterBump` → `InvalidateUserSessions` revokes the devices synchronously, before the `device.trust.revoke.requested` outbox handler. The handler then finds 0 rows and skips its single `source: scim` row with the `correlation_id`. Each device still gets a `device.revoked` row (`source: session`, `reason: sessions_revoked`), and the generation bump is audited with actor `scim`.

**Tests:**
- `devicesession` 6;
- auth 6 (observer hooks incl. expired-token sign-out, stale-generation regression, `SessionDeadlines`);
- client enrollment 3;
- mutation checks: the session-scoping and stale-generation tests fail on the broken code.

Gate: 1795 pass / 2 known skips. One `auditexport` timing test (`TestExporterRun_CatchesUpWithinOnePoll`) failed once under full-suite load and passes alone: follow-up, unrelated. Client: 353 pass.

**Live run (2026-11-12): PASS.**

**Setup:**
- Shared local DB reset with 056; this branch's controller (as `ztna_app`) and connector, with the connector in its own container; workspace `p09live`.
- A sandbox client daemon (root, via the Orca terminal) tunnelling a rate-limited 200 MB download from a file server on the connector's Docker network.

| Test | Result |
|---|---|
| Enrolment binds the session | device row has `session_id`, idle deadline +168 h, max deadline +720 h |
| A: console sign-in, then `/auth/logout`, while the tunnel download runs | logout 204, then refresh 401; `auth.sign_out` audited; device **not** revoked; download kept running; connector cancelled nothing |
| B: idle expiry (deadline moved into the past as the owner role at 07:59:04.98; the 168 h TTL is not configurable) | sweep revoked at 07:59:25.89 (next tick); connector `revoked sessions torn down count=1` at 07:59:25.93; download aborted (curl exit 56 after 15.8 MB); audit `device.revoked`, `source: session`, `reason: session_idle`, system actor |
| C: `zecurity-client logout` during a tunnel download (after a new login, new device) | typed 07:59:59.30, device revoked 07:59:59.97, connector cut 07:59:59.99, download aborted (exit 56); `Logged out of p09live.` |
| Audit of C (found live, fixed) | `RevokeDevice` (the device's own revoke, called by `logout` before `Logout`) wrote no audit row, so only `auth.sign_out` showed. It now writes `device.revoked`, `source: self`, `reason: signed_out`, once per revocation. Re-checked live on a third device; test `TestRevokeDevice_AuditsTheDevicesOwnRevokeOnce`. |

**Not run live (covered by tests):** max-lifetime expiry (same sweep, other column), expired-token logout, the IdP-disable user-wide revoke, the stale-generation regression, and the enrolment race.

**Follow-ups, done 2026-11-12:**
- **D-8, re-login by a different user (decided by Sathiya: same user, leave as is; different user, end the previous session).** `zecurity-client login` compares the saved login's email with the new one, case-insensitively. If it belongs to another user, it ends that session over the pinned channel, and the controller then revokes the device enrolled under it, even with an expired token. This runs after `PostLoginState`, so the old device's revocation cannot shut the daemon down mid-switch. The same user signing in again keeps today's behaviour: the previous device ends when its own session does.
  - Unit test: `is_different_user`. The client suite passes (354).
  - Not checked live: the local test login has one identity. The server path it uses (sign-out revokes that session's device) passed live in test C.
- **`auditexport` timing flake.** `TestExporterRun_CatchesUpWithinOnePoll` asserted the 1200-event backlog drained within one 2 s poll interval, which failed under full-suite load. It now counts `catchUp` calls (one claimed catch-up versus three for one batch per poll), with the 30 s catch-up budget as a backstop. A mutant that delivers one batch per catch-up fails it ("drained in 3 catch-ups, want 1").

---

## Implementation summary (as merged, PR #198)

### What is in place

| Area | What | Where |
|---|---|---|
| Push and teardown (Options A/B, earlier sprints) | A policy change pushes a fresh ACL snapshot to every connector within milliseconds; the connector diffs `(spiffe_id, resource_id)` and aborts live sessions (stream reset, FU-15) on direct, QUIC, relay and shield paths. The 15 s connector health report is the fallback; the client drops routes on the FU-8 wake hint (60 s poll as fallback). | `internal/policy/notifier.go`, `internal/connector/acl_push.go`; `connector/src/policy/mod.rs`, `session_registry.rs`; `client/src/daemon.rs` |
| Session end ends the device (C-1) | Each device is bound to the login session it enrolled under and that session's idle and max deadlines, kept in Postgres. Sign-out revokes that session's devices (even with an expired token); "end all sessions" (IdP connection disabled/deleted, SCIM deprovision) revokes all of the user's devices; a 60 s sweep revokes devices past their idle (168 h) or max (720 h) deadline. Console sign-out touches no device. | `migrations/056_client_device_session.sql`; `internal/devicesession`; `internal/auth/device_sessions.go` (`SessionObserver`, `SessionDeadlines`); `internal/client/service.go` (`EnrollDevice`) |
| Revocation path | The existing terminal path: `revoked_at` → CRL trigger, ACL drop, connector cut; one `device.revoked` audit row per device with source and reason (`signed_out`, `session_idle`, `session_expired`, `sessions_revoked`; `source: self` for the device's own revoke). | `internal/devicesession`; `RevokeDevice` |
| Re-login by a different user (D-8) | `zecurity-client login` ends the previous user's session when the saved login belongs to someone else; the same user signing in again is unchanged. | `client/src/cmd/login.rs` |
| Client logout | Refreshes an expired access token before revoking (insurance; the server covers it). | `client/src/cmd/logout.rs` |
| SCIM (C-4) | Post-commit policy-notify errors are logged instead of dropped. | `internal/scim` |

### Decided not to build

- **C-2, no CRL-driven live cut:** every client-cert CRL entry already comes from the `revoked_at` trigger, which drops the device from the ACL; a renewed cert keeps its SPIFFE ID, so cutting by ID on a CRL entry would hit the wrong sessions.
- **C-3:** posture and geofence latency (report every ≤ 300 s, 10 min staleness, ≤ 60 s country check) is acceptable.
- **C-5 / Option C:** risk scoring and step-up wait for PENDING-06 (`RequireBreakGlassMFA` is still a no-op).

### Verification

- **Gate (2026-11-12):** controller 1799 pass / 0 fail / 2 skip (the known two), all CI `*_TEST_DATABASE_URL` set; client 354 pass; CI 8/8 on PR #198.
- **Live run, PASS:** console sign-out leaves the tunnel up; idle expiry revoked within one sweep and the connector cut the download 36 ms later; `zecurity-client logout` cut it 0.67 s after typing (details in the Build section).
- **Reviews:** two races found and fixed before the live run (stale-generation refresh, enrolment race), plus the missing `RevokeDevice` audit row found live; each has a test that fails on the old code.

### Known limits

- Covered by tests but not live: max-lifetime expiry, expired-token logout, IdP disable, the two races, and re-login by a different user (the local test login has one identity).
- On SCIM deprovision the devices are revoked by the session path first, so the SCIM handler's single summary row with its `correlation_id` is skipped (each device still has its own `device.revoked` row).
- The 168 h idle and 720 h max lifetimes are not configurable from the environment.
