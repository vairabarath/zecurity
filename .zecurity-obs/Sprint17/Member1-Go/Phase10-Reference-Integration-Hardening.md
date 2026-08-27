---
type: phase
member: M1
sprint: 17
phase: 10
title: Reference-Integration Hardening (Deprovision Session-Kill + Mapping Round-Trip Probe)
status: planned
depends_on: [4, 6]
tags: [go, identity, scim, oidc, deprovision, mapping-gate, hardening, pending-05, pending-13]
---

# Phase 10 — Reference-Integration Hardening

> Depends on Phase 4 (mapping validation) and Phase 6 (deprovision + SideEffectSink). Derived from a
> field-by-field comparison against a reference enterprise IdP integration (generic OIDC login + SCIM
> provisioning against Okta) — see `.zecurity-obs/Sprint17/pending.md`. The comparison confirmed
> Zecurity's existing `auth`/`idp`/`identity`/`scim` stack already covers the same ground the reference
> integration required; the two concrete gaps below are not new discoveries — they are the same
> defects Sprint 17's own `path.md` verification pass already flagged as open (Findings A and C).
> This phase exists to close them, not to redesign anything.

## Goal
Close the two confirmed-open defects blocking Sprint 17's own acceptance criteria:
1. SCIM deprovision must actually kill live sessions and emit an audit event, atomically with the
   status change.
2. SCIM mapping must be provable by an active round-trip probe, so `scimEnabledAllowed` can become
   `true` without requiring break-glass every time.

## Context
`path.md`'s 2026-08-26 verification pass found both defects already, with root cause identified:

- **Finding A** — `Revoker.BumpGenerationTx` (`internal/identity/revocation.go:80`) is a bare
  `return r.bump(ctx, tx, ...)` and never calls `afterBump`, despite its own doc comment claiming
  session invalidation + audit "still happen via `afterBump` ... post-commit." That comment is false.
  `DirectoryService.Deprovision` uses `BumpGenerationTx`, so on the SCIM path: live sessions are not
  proactively revoked (access dies only at next token refresh), and no `ActionGenerationBump` audit
  event is published — deprovision via SCIM is currently unaudited. Also, ADR-025 §5 requires `DELETE`
  to remove `group_members`; `Deprovision` never touches that table today.
- **Finding C** — `MappingGateResult.WithRoundTrip` (`internal/scim/validation.go:137`) has no
  production caller. The only non-test `MappingGate.Evaluate` call site is
  `mutationResolver.TestIdpConnection` (`graph/resolvers/idp.resolvers.go:236`), and it passes a bare
  `scim.BreakGlassOverride{}` without ever attaching a round-trip result. M1-4b deferred the literal
  `POST→GET→verify→DELETE` probe-user round-trip to Phase 5; Phase 5 never wired it. Consequence:
  `MappingState` can never become `proven`, so SCIM is only enablable via `enableScimBreakGlass` —
  break-glass is the sole path, not the exception ADR-025 §3.1 intends.

Both were independently reconfirmed as still open while comparing Zecurity's SCIM engine against the
reference Okta integration walkthrough — the reference flow's own "Sign in with Okta" step (a live,
active login round-trip, not a passive check) is exactly the shape of verification ADR-025 §3.1
already specifies for the mapping-probe gate, reinforcing that Finding C is the right thing to close
before treating any connection's mapping as "proven."

## Relationship to PENDING-13 / ADR-028 (read before starting — owned by another teammate, NOT a task here)

> **Do not implement any part of PENDING-13/ADR-028 in this phase.** It is owned and being worked by a
> different team member. The paragraphs below exist only so M1 understands why the outbox events this
> phase's fixes still enqueue don't yet have an effect on device certs — not as an invitation to build
> the consumer. If PENDING-13's handler needs to change, that's a conversation with its owner, not a
> file to touch from Phase 10.

Sprint 17's own goal statement claims deprovision "durably triggers device-trust revocation (outbox →
PENDING-13)." That promise is **not yet end-to-end true in the running system**, independently of
anything this phase fixes:

- `SideEffectSink.Enqueue` (Phase 6) does correctly write `device.trust.revoke.requested` /
  `device.trust.re_enrollment_required` rows into the durable outbox, inside the identity tx. That part
  works today.
- But **nothing consumes them.** ADR-028 (the promoted, more-verified successor to PENDING-13, status
  `proposed`, not yet `accepted`) states plainly: *"the outbox registry is empty at runtime — no
  `RegisterHandler` in `cmd/`, and zero `Enqueue` producers anywhere [at ADR-028's time of writing]...
  SCIM (PENDING-05) will be the first producer; PENDING-13 is the first — and currently missing —
  consumer."* Confirmed again while writing this phase: `grep -rn "RegisterHandler" controller/cmd
  controller/internal` finds the registry mechanism (Sprint 18, `internal/outbox/handler.go`) and its
  *tests*, but no live registration of a `device.trust.*` handler anywhere in `cmd/server/main.go`.
- **Practical consequence:** even after this phase's Finding-A fix lands (session/JWT access does get
  cut, correctly), a deprovisioned user's **device certificates are not automatically revoked**. The
  device keeps a live mTLS credential until someone separately revokes it via the existing
  `revokeDevice` admin mutation (ADR-028 D6's synchronous path) or PENDING-13's Track 1 ("CLOSE THE
  LOOP") ships and registers the handler ADR-028 specifies:
  `device.trust.revoke.requested → mark all of (workspace_id,user_id)'s client_devices revoked +
  NotifyPolicyChange`, idempotent, registered via `registry.RegisterHandler` in `cmd/server/main.go`.

**This phase must not implement, stub, or pre-empt PENDING-13/ADR-028 Track 1** — it is a separate,
teammate-owned track per `path.md`'s own "one hard boundary" table (PENDING-05 decides + enqueues; a
separate track, owned by someone else, executes). Do not register an outbox handler, do not touch
`client_devices`, and do not add a "temporary" consumer here even as a stopgap — that would create a
second, conflicting implementation of work already assigned elsewhere. What this phase *does* do is
make sure the identity-layer half of deprovision (session kill + audit) is no longer silently broken,
so that once PENDING-13 Track 1 ships (from its owner), the full chain (identity cut → outbox → device
revoked) has no remaining gap on the SCIM/M1 side.

**Recommendation, not a task of this phase:** flag to PENDING-13's owner (outside this phase, e.g. in
standup or `path.md`'s ownership table) that Sprint 17's "offboarding is automatic" goal depends on
Track 1 shipping — either it should be scheduled alongside this phase, or `path.md`'s Sprint Goal /
acceptance criteria should be reworded to reflect that device-trust revocation is currently *enqueued*
reliably but not yet *executed* automatically.

## Files
| File | Change |
| --- | --- |
| `controller/internal/identity/revocation.go` | `BumpGenerationTx` (or its caller) must invoke `afterBump` **after** `tx.Commit()` — never inside the tx, since `afterBump` must not be able to fail the deprovision transaction. A returned closure the caller invokes post-commit is one viable shape; matching the existing `BumpGeneration` (non-Tx) behavior is the target. |
| `controller/internal/scim/directory_service.go` | `Deprovision`: call the post-commit hook from `revocation.go`; on hard-delete, purge `group_members` for the deleted user, scoped to the connection's workspace (mirroring the existing workspace-scoped hard-delete membership purge pattern already used elsewhere in `scim/groups.go`). |
| `controller/internal/scim/validation.go` | Implement the actual `POST→GET→verify→DELETE` probe-user round-trip that produces a `MappingGateResult.WithRoundTrip` result — this is the piece M1-4b deferred to Phase 5 and Phase 5 never delivered. |
| `controller/graph/resolvers/idp.resolvers.go` | `TestIdpConnection` (`~line 236`): attach the round-trip result to the `MappingGate.Evaluate` call instead of a bare `scim.BreakGlassOverride{}`, so a successful probe can move `MappingState` to `proven`. |
| `controller/internal/scim/deprovision_integration_test.go` | Extend with a regression test asserting `afterBump` side effects (session invalidation + audit event) actually fire on the SCIM deprovision path — the existing `TestDeprovision_Integration/cross-tenant_deprovision_is_refused_and_mutates_nothing` test wires a real `Revoker` and sink already and is the right home for this. |
| `controller/internal/scim/validation_test.go` | Add coverage for the wired round-trip probe: success → `proven`; probe failure → stays `unproven`, SCIM enable still gated. |

## Steps
- [ ] **M1-10a** Fix `Revoker.BumpGenerationTx` (or its caller) to run `afterBump` post-commit on the
  SCIM deprovision path. Confirm this does **not** allow `afterBump` failure to roll back the identity
  transaction — the existing non-Tx `BumpGeneration` path is the reference behavior.
- [ ] **M1-10b** Purge `group_members` on hard delete inside `Deprovision`, scoped to the target's
  workspace, per ADR-025 §5.
- [ ] **M1-10c** Wire the `POST→GET→verify→DELETE` probe-user round-trip in `internal/scim/validation.go`
  and attach its result to the `MappingGate.Evaluate` call inside `TestIdpConnection`, so a genuinely
  proven mapping can enable SCIM without break-glass.
- [ ] **Build gate:** `go build ./...` && `go vet ./...` && `go test ./internal/scim/... ./internal/identity/... ./graph/...` green on live Postgres; the new deprovision regression subtest and the new mapping round-trip tests both pass; re-run `path.md`'s two open acceptance criteria (deprovision session/audit kill; mapping-gate round-trip) and flip them to met once verified against code, not just against passing tests.

## Rules
- `afterBump` must run **after** commit, never inside the deprovision tx — a downstream session-kill
  or audit failure must never roll back the identity mutation itself (same invariant Phase 6 already
  established for `SideEffectSink.Enqueue`).
- The mapping round-trip probe must be a real write/read/delete against the connection's SCIM
  endpoint — not a discovery-only check (that's already covered by `OIDCProvider.Probe()` and is
  insufficient per ADR-025 §3.1).
- Do not weaken the break-glass path while wiring the proven-mapping path — break-glass must remain
  available for connections where an active probe genuinely cannot run, still gated by
  `identity.mapping.break_glass` + mandatory reason + audit.

## Out of scope (tracked elsewhere, not duplicated here)
- Extending the `users` table with `name`/`displayName`/`title`/`department` — already tracked
  separately per ADR-025 §5 (Phase 5's known gap); not part of this phase.
- Any frontend work — see `Member1-Frontend/Phase6-Guided-Idp-Setup-Wizard.md` for the one
  UX item the reference-integration comparison surfaced on the frontend side.
- SCIM conformance suite / live Okta-Entra interop testing — no fixture or runner exists yet anywhere
  in the repo; out of scope for this phase, tracked as a standing gap in `path.md`.
- **PENDING-13 / ADR-028 Track 1 itself** (registering the `device.trust.*` outbox handlers, marking
  `client_devices` revoked, `NotifyPolicyChange` on the device/cert layer) — **owned by another team
  member**, per `path.md`'s hard boundary table; see the dedicated section above. Do not implement,
  stub, or duplicate any part of it here, even temporarily.

## Build gate
`cd controller && go build ./... && go vet ./... && go test ./internal/scim/... ./internal/identity/... ./graph/...` green on live Postgres.
