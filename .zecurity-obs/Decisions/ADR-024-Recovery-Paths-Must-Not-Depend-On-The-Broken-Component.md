---
type: decision
status: proposed
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
| 2 | Certificate expiry | **Partially fixed.** The controller now prompts renewal 48h before expiry, so a connector should never reach expiry. **An already-expired cert is still unrecoverable** — see Open Question. |
| 3 | Shield stranded by an IP change | **Fixed, one level up.** `ShieldService.GetPeerConnectors` gives the shield an out-of-band path. It does not close the pattern: a shield whose **own certificate** has expired cannot open that channel either, and still needs manual re-enrolment. |

Instance 3's fix is itself an example of the limit: **an out-of-band path authenticated by a credential
that can also expire inherits the same shape one level up.** That is acceptable — the window shrinks by
orders of magnitude — but it should be stated rather than claimed as closed.

## Open Question — a grace window for `RenewCert`

Closing instance 2 properly means letting a component renew with an **expired but otherwise valid**
certificate, inside a bounded window. That is a real trust-model change and is deliberately **not** decided
here. The shape it would take:

- Accept an expired leaf **only** for `RenewCert`, never for any other RPC.
- Bound the window (e.g. cert TTL again, so a 7-day cert is renewable for 7 days past expiry) — long
  enough to survive an outage, short enough that a long-dead device cannot resurrect itself.
- Require everything else to still hold: chain to the workspace CA, correct SPIFFE role and entity, and
  **not revoked** — revocation must continue to beat expiry, or this becomes a way to un-revoke.
- Audit-log every renewal that used the grace window.

The counter-argument is real: an expired certificate is the one signal that a device has been out of
contact, and honouring it weakens that signal. The alternative — accept manual re-enrolment as the answer
and make it *easy* (a one-click re-issue in the admin UI) — satisfies requirement 1 above without
weakening the trust model, and may be the better trade.

**This needs a decision before either instance 2 or instance 3 can be called closed.**

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
