---
type: decision
status: accepted
date: 2026-08-29
related:
  - "[[Decisions/ADR-022-Shield-LAN-IP-Resource-Host-Sync]]"
  - "[[Decisions/ADR-023-Privileged-OS-DNS-Integration]]"
  - "[[Sprint16/path]]"
  - "[[Sprint16/Member3-Go-Rust/Phase10-Admin-UI-FQDN-Resources]]"
  - "[[Sprint16/Member3-Go-Rust/Phase12-OS-DNS-Integration]]"
tags:
  - adr
  - reliability
  - recovery
  - pki
  - shield
  - connector
  - architecture
---

# ADR-024 — A Recovery Path Must Not Run Through The Component It Recovers

## Context

Sprint 16's end-to-end verification hit **three independent failures that share one shape**. None was
found by design review; each was found by a machine sitting in an unrecoverable state, and each cost
most of a session to diagnose because the symptom named the wrong thing.

> **The shape:** component A can only be repaired over a channel that A itself provides. When A breaks,
> the repair channel breaks with it, and the system has no way back that does not involve a human with
> shell access.

### The three instances

**1. Orphaned shield (Gate 2).** Revoking a connector cascade-deleted its shields but left resources
`status='protected'` with a dangling `shield_id`. That row is unreachable by any UI action — the only
thing that could clear it was the resource editor, which could not load a resource whose shield no longer
existed. Compounded by a second defect (`CompileACLSnapshot` aborting the whole workspace on one bad
row), so a routine revoke became a total outage diagnosed as `unknown_resource` on resources that were
perfectly fine.

**2. Certificate expiry (Gate 3).** The connector's cert expired and it could not recover: `renewal.rs`
calls the `RenewCert` RPC, which travels over **the authenticated channel that expiry removes**. Renewal
is possible only *before* expiry. The root cause turned out to be worse than the symptom — renewal had
never fired at all (`ReEnrollSignal` was constructed nowhere in the controller; `Cfg.RenewalWindow` was
read by nothing), so with a 7-day TTL every connector reached this state on schedule.

**3. Orphaned shields on a connector IP change (Gate 2/3).** A shield learns peer coordinates only from
`PeerConnectorList` pushes over its Control stream. When its connector's address changed, the only
channel carrying the new address was **the peer whose address just changed**. `connectors.lan_addr` in the
database was already correct; the shield had no way to read it, and retried the dead address forever.
Recovery required temporarily re-adding the old IP to an interface so it could reconnect once.

### Why this is a design gap, not three accidents

Each instance arose from a rule that is individually correct:

| Rule | Why it is right | What it costs |
|---|---|---|
| Renewal requires an authenticated channel | An unauthenticated renewal endpoint is a certificate-issuing oracle | No way back after expiry |
| Shield → Connector only, never Controller | Keeps the controller out of the data path and shrinks the shield's attack surface | Shield cannot learn its connector moved |
| A protected resource requires a live shield | Fail-closed, correctly | Dangling state no UI can clear |

The gap is not in any one rule. It is that **no rule was paired with a bounded exception for its own
failure mode**, and nothing in review asked for one.

## Decision

**Every mechanism whose failure is self-sustaining must ship with a recovery path that does not depend on
the failed component.** Concretely, three requirements.

### 1. The design-review question

Any change that adds a dependency of the form *"A is repaired over a channel A provides"* must answer, in
the phase file or ADR:

> **If this component is in its worst state, what repairs it, and does that thing depend on it?**

An answer of "manual re-enrolment" or "an operator with shell access" is permitted, but it must be
**written down as the answer**, not left implicit. Two of the three instances above would have been caught
by asking the question out loud.

### 2. Prefer a narrow out-of-band path over a broad one

Where a recovery path is added it should be:

- **Reachable when the primary is not** — a different peer, a different credential, or a different
  transport. A path that fails in the same conditions is not a recovery path.
- **Narrow** — one operation, not general access. `GetPeerConnectors` returns coordinates and nothing
  else.
- **Authenticated by something that survives the failure** — and identified by that credential *alone*,
  never by a caller-supplied field.
- **Off the happy path** — invoked only after the primary has failed, so it cannot mask a regression in
  the primary.
- **Failure-distinguishable** — an "empty" result must be an error, not an empty success, wherever the
  caller treats empty as "keep what I have."

### 3. Prove the path by executing it

A recovery path is exercised, by definition, only when something is already broken — so it will not be
covered by ordinary use, and a latent bug in it is invisible until the day it is needed. Two rules follow:

- **A recovery path needs a test that runs it**, not a test of the arithmetic near it.
- **Wiring up a dormant mechanism activates code that has never executed.** That code must be executed
  before the wiring is called done. When the connector renewal trigger was added, it switched on
  `renewal.rs` and `RenewConnectorCert` — both of which had never run once, and the latter had zero
  tests.

## Status of the three instances

| # | Instance | State |
|---|---|---|
| 1 | Orphaned shield / resource | **Fixed.** `DeleteConnector`/`DeleteShield` demote bound resources in the same transaction; the compiler skips a bad row instead of failing the workspace. |
| 2 | Certificate expiry | **Closed.** The controller now prompts renewal 48h before expiry, so a connector should not reach expiry; and an expired one re-enrols cheaply via `reenrollConnector`, keeping its identity. Expiry remains a hard boundary by decision — see below. |
| 3 | Shield stranded by an IP change | **Closed.** `ShieldService.GetPeerConnectors` gives the shield an out-of-band path. A shield whose **own certificate** has expired cannot open that channel and re-enrols instead — which, after the decision below, is the intended behaviour rather than a residual gap. |

Instance 3's fix is itself an example of the limit: **an out-of-band path authenticated by a credential
that can also expire inherits the same shape one level up.** That is acceptable — the window shrinks by
orders of magnitude — but it should be stated rather than claimed as closed.

## Decided — expiry is a hard trust boundary; optimize re-enrolment instead

**Decision (2026-08-29): no grace window.** Renewal requires a **currently valid** certificate. An expired
certificate requires re-enrolment. The trade was considered and rejected: an expired certificate is the one
signal that a device has been out of contact, and honouring it — even briefly — weakens that signal for
every device, to buy convenience in a case that good renewal timing should prevent.

Requirement 1 of this ADR is therefore satisfied the other way round: the answer to *"what repairs an
expired component?"* is **"re-enrolment"**, and the obligation that follows is to make that answer *cheap*
rather than to widen the boundary.

### Why re-enrolment was not cheap

Re-enrolment was the documented answer and a **destructive** one. Three gates all required
`status = 'pending'`:

- `Enroll` (`internal/connector/enrollment.go`) — correct, and unchanged.
- `RegenerateTokenHandler` — issues a token only for a pending connector.
- and **nothing could return an existing connector to pending.**

So the only route was **revoke + create a new connector**, which mints a new `connector_id` — orphaning
every shield bound to the old one and demoting their resources. *The recovery from instance 2 caused
instance 1.* That is the pattern this ADR is about, closing on itself.

### The fix: a supported transition, not an exception

`reenrollConnector(id)` / `reenrollShield(id)` (admin-only) return an existing component to `pending`,
clearing exactly the credential columns `Enroll` rewrites (`cert_serial`, `cert_not_after`,
`enrollment_token_jti`) and **keeping the id** — so shields stay bound to their connector and resources
stay bound to their shield.

**No trust check changed.** `Enroll` still accepts only a pending component; the token endpoint still
issues only for a pending one. What was added is a supported way to *reach* pending without destroying
identity. Recovery becomes: re-enrol → fresh install command → paste on the host.

The status gate is load-bearing:

| Current status | Result | Why |
|---|---|---|
| `revoked` | **refused** | Revocation is absolute. This must never become a way to un-revoke. |
| `active` | **refused** | A pending connector is excluded from ACL snapshots (`policy/store.go`: `c.status = 'active'`), so flipping a *working* connector takes its resources offline — an outage disguised as a repair. Revoke first if replacement is genuinely intended. |
| `disconnected` | allowed | The expired-cert state: it cannot heartbeat, the watcher has marked it, and it is already out of the ACL snapshots — so this costs nothing. |
| `pending` | allowed | Idempotent; a second call just yields a fresh token. |

`active` being refused is the non-obvious half, and it is the reason this could not simply be "allow any
non-revoked component."

### What this does not do

- **No certificate is retired early.** A component's old cert remains cryptographically valid until it
  expires; re-enrolment issues a new one. Since re-enrolment is refused for an `active` component, the
  realistic case is a cert that has already expired, where nothing is left to retire. To kill a live
  identity immediately, revoke — that is what revocation is for.
- **Instance 3 is unchanged.** A shield whose own certificate has expired still cannot open the
  `GetPeerConnectors` channel, and re-enrols like any other expired component. That is now the *intended*
  behaviour rather than a gap, given this decision.

## Consequences

- A new question in design review, and a written answer in the phase file. Cheap.
- Recovery paths carry a test obligation that ordinary features do not — they must be executed, because
  nothing else will execute them.
- Some designs get an extra narrow endpoint they would not otherwise have. `GetPeerConnectors` is the
  precedent for what that should look like: one operation, cert-only identity, empty request, off the
  happy path.
- The pattern is not fully closed and this ADR does not claim it is. The bottom of every chain is a
  credential that can expire, and below that there is a human. The goal is to make that depth 3 or 4
  instead of 1.
