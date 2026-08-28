---
type: phase
member: M1
sprint: 17
phase: 10
title: Reference-Integration Hardening (Mapping Round-Trip Probe)
status: done
depends_on: [4]
tags: [go, identity, scim, oidc, mapping-gate, hardening, pending-05, pending-13]
---

# Phase 10 — Reference-Integration Hardening

> Depends on Phase 4 (mapping validation). Derived from a field-by-field comparison against a
> reference enterprise IdP integration (generic OIDC login + SCIM provisioning against Okta) — see
> `.zecurity-obs/Sprint17/pending.md`. The comparison confirmed Zecurity's existing
> `auth`/`idp`/`identity`/`scim` stack already covers the same ground the reference integration
> required.
>
> **Revision note (verified against the current codebase, superseding this file's original
> version):** this phase originally targeted two defects, `path.md`'s Findings A and C. Re-checking
> both directly against source turned up that **Finding A is already fixed** — `path.md`'s checklist
> was never updated after the fix landed. Only **Finding C** (the mapping round-trip probe) is still
> genuinely open. See "Verified status" below for the evidence trail. **Do not re-implement Finding
> A** — the code already does it.

## Goal
Close the one confirmed-open defect blocking Sprint 17's own acceptance criteria: SCIM mapping must
be provable by an active round-trip probe, so `scimEnabledAllowed` can become `true` without
requiring break-glass every time.

## Verified status (checked directly against source, not against `path.md`'s notes)

### Finding A — ALREADY FIXED, no work needed here
`path.md` (last updated 2026-08-26) still lists this as open: *"sessions are never proactively killed
and no audit event is emitted... `DELETE` does not remove `group_members`."* That is **no longer
true**. Checked directly:

- `controller/internal/identity/revocation.go` has an **exported** `Revoker.AfterBump(ctx, tenantID,
  userID, actorEmail, gen)` (the private `afterBump` from the original finding was exported precisely
  so transactional callers could invoke it post-commit).
- `controller/internal/scim/directory_service.go`, `Deprovision` (~line 344): commits the tx
  (`tx.Commit(ctx)`, ~line 411), and **only then** calls `s.revoker.AfterBump(...)` (~line 415) —
  exactly the post-commit shape ADR-025/Finding A required. `hard`-delete also already purges
  `group_members`, scoped to the workspace (~lines 389–398): `DELETE FROM group_members WHERE
  user_id = $1 AND group_id IN (SELECT id FROM groups WHERE workspace_id = $2)`.
- A regression test already exists and passes:
  `TestDeprovision_Integration/invalidation_is_best-effort_post-commit_and_does_not_fail_deprovision`
  (`controller/internal/scim/deprovision_integration_test.go:242`) arms a failing fake invalidator and
  asserts `fakeInvalidator.calls` still recorded exactly one call — i.e. `AfterBump` does fire, and its
  failure does not roll back the deprovision.
- `git log -- controller/internal/scim/directory_service.go` traces this to commit `b5c1bce`
  ("feat(scim): expose SCIM config on GraphQL, close tenant-isolation gap, structure errors") — the
  **same commit** `path.md` already credits with fixing Finding B (cross-tenant generation bump).
  Finding A's fix landed in that same commit; `path.md`'s Finding-A writeup and its acceptance-criteria
  checklist just never got updated to say so.

**Action for this phase: none.** Nothing to implement. If anyone re-opens work on this, the first
step is re-reading `directory_service.go:344-424` directly, not `path.md`'s stale Finding A text.

### Finding C — STILL OPEN, this is the one real task
- `controller/internal/scim/validation.go`: `MappingGateResult.WithRoundTrip` (~line 137) exists and
  is correctly implemented (verified `true` → `MappingProven` + `ScimEnabledAllowed: true`; `false` →
  unchanged unproven result). It is called **only** from `validation_test.go` — confirmed via
  `grep -rn "WithRoundTrip" controller/internal/scim/ controller/graph/resolvers/`, which returns zero
  production call sites.
- `controller/graph/resolvers/idp.resolvers.go`, `TestIdpConnection` (~line 205): runs an OIDC
  discovery probe (`OIDCProvider.Probe`), then calls `gate.Evaluate(ctx, conn.SubjectClaim,
  conn.ScimIdentifier, scim.BreakGlassOverride{})` (~line 239) with a **bare, empty**
  `BreakGlassOverride{}` — it never attaches a round-trip result. `MappingState` can therefore never
  become `proven` through this path; `enableScimBreakGlass` remains the only way to turn SCIM on,
  which is exactly what ADR-025 §3.1 says should be the exception, not the rule.

The reference integration's own "Sign in with Okta" step (a live, active login round-trip, not a
passive discovery check) is the same shape of verification ADR-025 §3.1 already specifies for this
gate — independent confirmation that Finding C is the right thing to close.

## Relationship to PENDING-13 / ADR-028 (context only — owned by another teammate, NOT a task here)

> **Do not implement, stub, or modify any part of PENDING-13/ADR-028 in this phase.** It is owned and
> implemented by a different team member. The paragraphs below record the current, verified state —
> not an invitation to touch that code from here.

**Status update:** as of this phase's latest revision, `origin/fixed-pendings` (PRs #78 and #79,
"feat(identity): add device-trust outbox contract for SCIM<->client loop" and "feat(client):
PENDING-13 Track 2 — server→client device trust directive") has been merged into this branch. PENDING-13
Track 1 **and** Track 2 are both present in the working tree now:

- `controller/internal/client/device_trust_handler.go` — `DeviceTrustRevokeHandler` and
  `DeviceTrustReEnrollHandler`, both implementing `outbox.EventHandler`, registered via
  `outboxRegistry.RegisterHandler(identity.EventDeviceTrustRevokeRequested, ...)` /
  `...EventDeviceTrustReEnrollmentRequired, ...)` in `controller/cmd/server/main.go`.
- Track 2 (server→client device directive over the existing 60s ACL poll) landed in `client/` —
  `daemon.rs`, `ipc.rs`, `runtime.rs`, `state_store.rs`, plus the proto/migration changes
  (`controller/migrations/034_device_status.sql`, `proto/client/v1/client.proto`).

**Practical consequence — this is now good news, not an open gap:** the `device.trust.revoke.requested`
/ `device.trust.re_enrollment_required` events that Phase 6's `SideEffectSink.Enqueue` writes are no
longer enqueued into a void. Sprint 17's "offboarding is automatic" goal is now much closer to
end-to-end true than earlier revisions of this phase file described. Two loose ends, **neither of which
is this phase's or PENDING-13's owner's job to fix from here**, worth flagging separately:

1. **Migration-number collision, not a code conflict:** `controller/migrations/034_scim_directory_sync.sql`
   (this branch) and `controller/migrations/034_device_status.sql` (merged from `fixed-pendings`) both
   claim `034`. The incoming file's own header comment already flags this and says whichever branch
   merges second must renumber — needs a manual decision by the team on which number(s) to use; not an
   edit to make casually or unilaterally.
2. Full end-to-end verification of the merged Track 1/2 chain (SCIM deprovision → outbox → device
   revoked → client daemon reacts) has not been re-run against this merged tree as part of writing this
   phase file — the individual pieces were checked to exist and be wired, but no integration test
   spanning both `scim` and `client` packages together was executed here.

## Files
| File | Change |
| --- | --- |
| `controller/internal/scim/mapping_probe.go` | **New.** `MappingProbeResult` + `DirectoryService.ProbeMapping` — the engine-direct probe. See "Implementation" below for the design and why it lives in its own file rather than `validation.go`. |
| `controller/internal/scim/mapping_probe_test.go` | **New.** Live-Postgres integration tests: successful round-trip → proven; wired-gate reaches proven; setup failure (unseeded connection) is unproven, not a panic; workspace-less connection short-circuits; **no `scim_tokens` row is ever created, revoked, or rotated by the probe, even with 2 pre-existing active tokens**; degenerate-mapping config is still rejected by the existing validator. |
| `controller/graph/resolvers/idp.resolvers.go` | `TestIdpConnection`: after the existing OIDC discovery probe and `gate.Evaluate` (unchanged), added a step 3 that runs `ds.ProbeMapping` only when the mapping config is valid, then calls the existing `res.WithRoundTrip(...)` and overwrites `res.Reason` with the probe's own reason on failure. |
| `controller/internal/scim/validation.go` | **One-line correction**, not new logic: `WithRoundTrip`'s hardcoded success `Reason` string previously claimed *"OIDC subjectClaim and SCIM scimIdentifier resolve to the same logical user"* — inaccurate, since subjectClaim is never exercised by this or any SCIM-engine path. Reworded to state only what's true (scimIdentifier verified; subjectClaim not exercised). No logic changed. |
| `controller/internal/scim/validation_test.go` | Added one assertion to the existing `TestResult_WithRoundTripSeam`: a proven result's `Reason` must never claim subjectClaim was verified — regression guard for the string correction above. |
| `controller/internal/scim/deprovision_integration_test.go` | Added one new subtest, `"hard-delete removes the user's group_members"` — a coverage gap fix only (Finding A/B's `group_members` purge already existed in production code; this file just never had a test proving it). No production code touched by this addition. |

## Steps
- [x] **M1-10a** Built `ProbeMapping` as an engine-direct probe (no `Store.Mint`, no `scim_tokens`
  touched, no `AuthMiddleware`/public-HTTP-router involvement) and wired its result into
  `TestIdpConnection` via the existing `WithRoundTrip` seam. See "Implementation" below for the full
  design and the explicit `subjectClaim` limitation.
- [x] **Build gate:** `go build ./...`, `go vet ./...` both clean. `go test ./internal/scim/...
  ./internal/identity/... ./graph/...` run against a live Postgres instance (local
  `ztna_postgres` docker container) — **all green**, including every new test. `path.md`'s two open
  acceptance criteria (deprovision session/audit kill; mapping-gate round-trip) can now both be flipped
  to met — deprovision was already correct in code (see "Verified status" above), and the round-trip
  probe closes the mapping-gate criterion.

## Implementation (2026-08-27)

### Why `Store.Mint` is never called
`Store.Mint` enforces the ≤2-active-token dual-rotation limit (`maxActiveTokens`, `token_store.go`)
and will **silently revoke the connection's oldest live token** via `applyThirdTokenRule` if two are
already active. A mapping-test probe minting a throwaway token would risk revoking a real customer's
live Okta/Entra SCIM credential as a side effect of an admin clicking "Test Connection" — unacceptable,
and the reason the original plan for this phase was rejected before any code was written. `ProbeMapping`
never calls `Mint`, `Rotate`, or `Revoke`, and never inserts a `scim_tokens` row. This is asserted by a
dedicated test (`probe_never_mints,_revokes,_or_rotates_any_scim_tokens`) that seeds exactly 2 active
tokens, runs the probe, and asserts both the count and each token's `revoked_at` are unchanged
afterward.

### How the probe gets its scope without a token
`resolveScope(ctx, workspaceID, connectionID)` is the only place a `*scope` is normally built — it
loads the connection, applies two fail-closed gates (`!conn.ScimEnabled → 403`,
`conn.Status != "active" → 403`), then constructs a `*scope` literal from the connection's fields.
`ProbeMapping` builds that exact same struct directly (same package, so the unexported fields are
reachable), using the `*idp.Connection` `TestIdpConnection` already has in hand from
`r.IdpStore.GetByID` — skipping only the two gate checks that exist specifically to block writes
*before* the mapping is proven, which is exactly the state a pre-enable probe runs in. No token, no
`AuthMiddleware`, no `Lookup`, no public `/scim/v2` HTTP router involved. This means the probe does
**not** exercise bearer-token authentication or the HTTP layer — only the mapping/CRUD engine
(`Provision`/`Get`/`Deprovision`) is under test, and the phase file and code comments say so explicitly
rather than overclaiming HTTP-router coverage.

### What is actually verified: scimIdentifier, not subjectClaim
Traced every read of `scope.subjectClaim` and every caller of `ExtractSubjectClaim` across
`internal/scim`, `internal/identity`, and `internal/auth`: **zero production call sites** for either.
`subjectClaim` is set into `*scope` by `resolveScope` but never read by `Provision`, `Get`, or
`Deprovision`; it's a login-side concept (per its own doc comment) that isn't wired into the login path
either. A `Provision → Get` round trip therefore cannot honestly verify it. Only `scimIdentifier` is
genuinely provable this way: `ExtractScimIdentifier(resource, sc.scimIdent)` extracts the canonical
key, `Provision` stores it as `users.provider_sub` via the identity `Linker`, and `Get` → `loadUser`
reads it back out as `User.ExternalID`. `ProbeMapping` sends a synthetic `probeExternalID` under the
connection's configured attribute name and asserts the value that comes back from `Get` matches
exactly. `MappingProbeResult.Reason` and `WithRoundTrip`'s corrected success string both say, in the
success case, that subjectClaim was **not** exercised — no caller can mistake a verified probe for
proof that subjectClaim is correct.

### Probe flow
```text
resolveScope-equivalent *scope (built directly, scim_enabled gate skipped)
  → EnsureSyncInstance
  → Provision(synthetic resource: userName + configured-scimIdentifier-attr = probeExternalID)
  → [deferred, guaranteed] Deprovision(hard=true)   ← runs even if Get or verification below fails
  → Get(provisioned userID)
  → compare Get().ExternalID == probeExternalID
  → Verified=true only on an exact match; Verified=false with a descriptive Reason otherwise
```
Cleanup is a `defer` registered immediately after `Provision` succeeds, so it runs on every exit path
(`Get` failure, mismatch, or success) — proven by the first probe test asserting zero non-tombstoned
users remain in the workspace after a successful probe.

### Confirmed test results (live Postgres, `PKI_TEST_DATABASE_URL` pointed at the local `ztna_postgres`
docker container)
```
go build ./...    clean
go vet ./...      clean
go test ./internal/scim/... ./internal/identity/... ./graph/...
  ok  internal/scim        (TestProbeMapping_Integration: 6/6 subtests pass;
                             TestDeprovision_Integration: 7/7 subtests pass, including the
                             new group_members regression; TestGate_* all pass)
  ok  internal/identity
  ok  graph
  ok  graph/resolvers
```
One pre-existing test isolation bug was found and fixed **in the new test file, not production code**:
the new `group_members` regression subtest originally reused the canonical key `"okta-finn-1"`, which
collided with the pre-existing `"forced enqueue failure aborts the whole deprovision tx"` subtest's
hardcoded use of the same key — `Provision` being idempotent-on-canonical-key meant the later subtest
silently received the first subtest's already-tombstoned user instead of a fresh one, breaking its
"must remain active" assertion. Renamed the new subtest's synthetic identity to `"okta-hollis-1"`;
no other file was touched to fix this.

## Review fixes (2026-08-27, second pass)

A review of the first implementation found one real bug and two documentation gaps, all now fixed:

- **C1 — auto-enable bug (fixed).** `TestIdpConnection` was persisting
  `res.ScimEnabledAllowed` to `identity_connections.scim_enabled`. Once the round-trip probe could make
  that field `true`, `TestIdpConnection` — a verification/test operation — could enable SCIM directly,
  bypassing `UpdateScimConfig`, the one dedicated enable path ADR-025 §3.2 requires. Fixed by always
  persisting `false` from `TestIdpConnection`, regardless of the probe outcome; the GraphQL response
  still truthfully reports `ScimEnabledAllowed: true` for a proven mapping, so the admin sees the proof
  and then must take the separate `UpdateScimConfig(scimEnabled: true)` action to actually enable SCIM.
  Proven by `graph/resolvers/idp_test_connection_scim_enable_test.go` (new), which spins up a minimal
  in-process OIDC discovery fixture so the full flow — including a **genuinely proven** mapping via a
  real `ProbeMapping` round-trip — is exercised end-to-end, and asserts the database column stays
  `false` even though the response reports `MappingState: proven` and `ScimEnabledAllowed: true`.
  **Known consequence, not fixed here (out of scope per explicit review instruction not to touch
  `UpdateScimConfig`):** `UpdateScimConfig`'s own enable path calls `gate.Evaluate` without ever calling
  `WithRoundTrip`, so it does not itself re-run or remember the `TestIdpConnection` probe's proof —
  today, `ScimEnabledAllowed` will be `false` there too, and `enableScimBreakGlass` remains the only way
  to actually flip `scim_enabled` on. Wiring `UpdateScimConfig` to the round-trip proof (or re-running
  the probe there) is a real, separate follow-up, not part of this phase.
- **C2 — cleanup limitation (documented, not redesigned).** `ProbeMapping`'s deferred cleanup was
  already best-effort/log-only; added an explicit doc comment (in `mapping_probe.go`) stating that a
  cleanup failure does not change the already-computed `Verified` result and can leave a residual probe
  user behind until the next successful probe or manual cleanup. No retry/GC mechanism was added —
  none was asked for, and adding one would have been a redesign.
- **C3 — unrelated file reverted.** `Member1-Frontend/Phase6-Guided-Idp-Setup-Wizard.md` was edited in
  an earlier, unrelated verification pass and had no business being in this phase's diff; reverted with
  `git checkout` back to its committed state. No frontend source file was ever touched by this phase.

### Remaining architectural gap (explicitly not addressed by this phase)
`subjectClaim` resolution has **no production implementation anywhere** — `ExtractSubjectClaim` is
dead code. Whether or how the OIDC login path should actually use `conn.SubjectClaim` to extract a
canonical key (mirroring what SCIM's `ExtractScimIdentifier` already does) is a separate, larger
question outside Finding C's scope and was not touched here, per explicit instruction. Flagging it here
so it isn't lost: proving the *full* ADR-025 §3.1 mapping equivalence (both subjectClaim AND
scimIdentifier resolve to the same person) is not possible until that gap is closed.

> **Clarification (2026-08-28, Phase 12):** this gap was subsequently closed.
> Phase 11 wired `conn.SubjectClaim` into the OIDC login path (making `ExtractSubjectClaim`
> production code), and Phase 12 extended `ProbeMapping` to assert OIDC↔SCIM canonical-key
> equivalence. Phase 10 itself remains complete and correct per its original SCIM-side scope;
> the note above is retained as accurate historical context, not as an open defect.

## Rules
- The mapping round-trip probe must be a real write/read/delete against the connection's SCIM
  endpoint — not a discovery-only check (that's already covered by `OIDCProvider.Probe()` and is
  insufficient per ADR-025 §3.1).
- Do not weaken the break-glass path while wiring the proven-mapping path — break-glass must remain
  available for connections where an active probe genuinely cannot run, still gated by
  `identity.mapping.break_glass` + mandatory reason + audit.
- Do not touch `controller/internal/identity/revocation.go` or the deprovision path in
  `controller/internal/scim/directory_service.go` as part of this phase — both are already correct
  (see "Verified status" above); re-touching them without a new, independently-verified defect risks
  regressing a working, tested implementation.

## Out of scope (tracked elsewhere, not duplicated here)
- Extending the `users` table with `name`/`displayName`/`title`/`department` — already tracked
  separately per ADR-025 §5 (Phase 5's known gap); not part of this phase.
- Any frontend work — see `Member1-Frontend/Phase6-Guided-Idp-Setup-Wizard.md` for the one
  UX item the reference-integration comparison surfaced on the frontend side.
- SCIM conformance suite / live Okta-Entra interop testing — no fixture or runner exists yet anywhere
  in the repo; out of scope for this phase, tracked as a standing gap in `path.md`.
- **PENDING-13 / ADR-028 (Track 1 and Track 2) itself** — **owned by another team member**, already
  implemented and merged from `fixed-pendings`. Do not implement, stub, or duplicate any part of it
  here, even temporarily. Do not renumber `034_device_status.sql` unilaterally either — that's a
  team decision, not something to resolve inside this phase.

## Build gate
`cd controller && go build ./... && go vet ./... && go test ./internal/scim/... ./graph/...` green on live Postgres.
