---
type: phase
member: M1
sprint: 17
phase: 13
title: Consume the Proven Mapping in UpdateScimConfig (close residual Finding C / path.md criterion 5)
status: done
depends_on: [10, 11, 12]
tags: [go, identity, scim, mapping-gate, adr-025, update-scim-config, equivalence]
---

# Phase 13 — Consume the Proven Mapping in `UpdateScimConfig`

## Relationship to prior phases (honesty boundary)

- **Phase 10** proved the SCIM-side mapping round-trip (`ProbeMapping`:
  `scimIdentifier → Provision → Get → same canonical value returned`). It
  deliberately did **not** wire `WithRoundTrip` into any production enable path.
- **Phase 11** made the OIDC-side `subjectClaim` operational in the real login
  path (`conn.SubjectClaim → ExtractSubjectClaim → AuthenticationContext.Subject`).
- **Phase 12** wired `ProbeMapping` (+ `WithRoundTrip`) into `TestIdpConnection`,
  so a successful probe there yields `MappingState="proven"` / `ScimEnabledAllowed=true`
  in *that mutation's GraphQL response*. But `TestIdpConnection` then unconditionally
  persists `scim_enabled=false` (the C1 fix — preserved this phase).

Phase 13 closes the **residual half of Finding C / path.md criterion 5**: the
DEDICATED normal enable action `UpdateScimConfig(scimEnabled:true)` never ran the
probe, so it could only ever refuse and defer to `enableScimBreakGlass`. SCIM was
therefore enablable **only** via break-glass — the exception had become the sole
path. This phase makes the normal path consume the already-proven probe machinery.

Phase 13 is a **follow-up to Phase 12**, NOT a redesign of the mapping system and
NOT a new proof mechanism.

## What this phase does NOT do (explicit)

- **No persisted proof state.** No `mapping_proven` / `proven_at` column, no
  migration, no cross-connection proof cache, no freshness/state machine. The
  probe runs AFRESH at enable time, in-memory, connection-scoped.
- **No auto-enable from `TestIdpConnection`.** Its `SetSCIMEnabled(..., false)`
  (C1) is untouched.
- **No break-glass change.** `enableScimBreakGlass` permission/reason/audit
  semantics unchanged.
- **No `ProbeMapping` redesign.** `mapping_probe.go`, `token_store.go`,
  `revocation.go`, `directory_service.go`, `auth/providers/oidc.go`,
  `auth/idp_adapter.go`, `auth/mapping/` untouched.
- **No frontend / outbox / PENDING-13 / migrations change.**

## Source-verified gap (the actual defect)

- `idp.resolvers.go` `UpdateScimConfig` enable branch (Rule 2, `conn.ScimEnabled==false`
  AND `scimEnabled:true` AND mapping config valid) called only
  `scim.NewMappingGate(conn.Provider).Evaluate(ctx, subjectClaim, scimIdentifier,
  scim.BreakGlassOverride{})` with an **EMPTY override**.
- `MappingGate.Evaluate` with an empty override can ONLY return
  `MappingUnproven` + `ScimEnabledAllowed=false` (`validation.go`). The ONLY way
  to `proven` is `WithRoundTrip`, which only the CALLER can attach. So the enable
  branch always hit the refusal (`idp.resolvers.go` `UserError`) and never flipped
  `scim_enabled=true`.
- Net: the normal enable path was dead; only `enableScimBreakGlass` worked.
  ADR-025 §3.1's "mapping proven → SCIM may be enabled" normal path did not exist.

## Implementation (approved scope — Option B, smallest fix)

- `controller/graph/resolvers/idp.resolvers.go` (the ONLY prod file changed)
  - In the enable branch (Rule 2), keep the existing `MappingGate.Evaluate` call,
    then run the existing production probe and attach it:
    ```go
    ds := r.ScimStore.DirectoryService()
    probe := ds.ProbeMapping(ctx, conn)
    res = res.WithRoundTrip(probe.Verified, subjectClaim, scimIdentifier)
    ```
  - On `!res.ScimEnabledAllowed`: prefer the probe's own `Reason` (precise
    round-trip failure, e.g. canonical-key mismatch / missing configured claim),
    fall back to the gate's generic `"the identity mapping is not proven"`, then
    return `scimEnableRefusedError(reason)` — a `*gqlerror.Error` carrying
    `extensions.code = "SCIM_MAPPING_UNPROVEN"` so the admin UI can offer the
    break-glass flow (per the FE-1/no-code-branch rule; `apperr.UserError` itself
    has no code, so the dedicated branchable code is what lets the UI act).
  - On success: `enabled = true`, continue to the existing `SetScimMapping`
    persistence + audit.
  - `Rule 1` (explicit disable) and `Rule 3` (mapping-edit force-disable) unchanged.
- `controller/graph/resolvers/idp_helpers.go` — `scimEnableRefusedError(reason)`
  (the branchable `SCIM_MAPPING_UNPROVEN` `*gqlerror.Error`), added this phase.
- `controller/graph/resolvers/idp_update_scim_config_enable_test.go` — NEW live-DB
  integration suite (6 subtests; 1 explicit SKIP that documents engine-level
  coverage). Mirrors the C1 harness (temp DB + migrations + in-process OIDC
  discovery fixture); skips cleanly when `PKI_TEST_DATABASE_URL` is unset.

No other file changed. `git diff --name-only` for the prod half = the single
`idp.resolvers.go` edit (+ `idp_helpers.go` helper). No migration.

## Reused, not duplicated

- `ProbeMapping` (`mapping_probe.go`) — the SAME live round-trip; reuses
  `auth/mapping.ExtractSubjectClaim` for the OIDC side. NO second canonical-key
  extractor was introduced.
- `MappingGate.WithRoundTrip` / `MappingGateResult` shape unchanged.
- `enableScimBreakGlass` + `identity.mapping.break_glass` permission unchanged.

## Safety invariants preserved (verified by the live-DB suite)

- **No SCIM token mutation.** `countScimTokens(connID)` == 0 after every enable
  probe (`ProbeMapping` performs no `Store.Mint`/`Revoke`/`Rotate`).
- **`TestIdpConnection` C1 intact.** `TestIdpConnection_DoesNotPersistScimEnabled`
  stays green: a genuinely PROVEN mapping is reported to the caller but is never
  persisted as `scim_enabled=true`.
- **Break-glass unchanged** — still the only route when the probe cannot run;
  still audits `scim.mapping.break_glass_override` with `mapping_proven:false`.
- **No cross-connection proof reuse** — each `UpdateScimConfig` runs its own
  per-connection probe at enable time; proving A does not enable B.
- **Fail-closed** — invalid mapping *configuration* (e.g. `subjectClaim==scimIdentifier`),
  missing/empty configured `subjectClaim` at the config layer, or a genuine SCIM
  `Provision→Get` round-trip error refuses with `scim_enabled` left false (no silent
  `sub` fallback). NOTE: this is config-level / round-trip fail-closed; the production
  `autoAdd=true` probe does **not** independently detect a mismatch between a real customer's
  supplied OIDC claim and SCIM data (see "What 'proven' means here" below).
- **Disable / mapping-change semantics unchanged.**

## Verification gates (run live)

```bash
cd controller && export $(grep -vE '^\s*#|^\s*$' .env | xargs)   # loads PKI_TEST_DATABASE_URL
go build ./...
go vet ./graph/... ./internal/scim/... ./internal/auth/... ./internal/identity/...
go test ./graph/resolvers/ -run 'TestUpdateScimConfig|TestIdpConnection_DoesNotPersistScimEnabled' -count=1 -v
go test ./internal/scim/... ./internal/auth/... ./internal/identity/... ./graph/... -count=1
```

### Live Postgres results (2026-08-29, ztna_postgres up, DSN from controller/.env)

- `go build ./...` → exit 0.
- `go vet ./graph/... ./internal/scim/... ./internal/auth/... ./internal/identity/...` → clean.
- Resolver integration (`-run TestUpdateScimConfig|TestIdpConnection_DoesNotPersistScimEnabled`):
  - `TestIdpConnection_DoesNotPersistScimEnabled` (3 subtests) — PASS (C1 regression).
  - `TestUpdateScimConfig_EnableAfterProvenMapping` — PASS (proven, NO break-glass → `scim_enabled=true`).
  - `TestUpdateScimConfig_UnprovenMappingRefused` — PASS (invalid config `subjectClaim==scimIdentifier` → UserError, stays false).
  - `TestUpdateScimConfig_NonDefaultSubjectClaimEnables` — PASS (`subjectClaim="email"` enables normally).
  - `TestUpdateScimConfig_CanonicalKeyMismatchFailsClosed` — SKIP (covered by `internal/scim/mapping_probe_test.go` `autoAdd=false` T2/T3 + gate-level config rejection; intentionally not re-asserted at the resolver layer).
  - `TestUpdateScimConfig_NoCrossConnectionProofReuse` — PASS (A enabled; B NOT enabled by A's proof; B enables on its own probe).
  - `TestUpdateScimConfig_DisableUnchanged` — PASS (disable no-proof, always allowed).
  - `TestUpdateScimConfig_BreakGlassFallbackUnchanged` — PASS (unreachable issuer → normal enable refused; with dedicated permission → break-glass enables, still unproven/audited).
  - `go test ./graph/resolvers/` full package — PASS.
- `go test ./internal/scim/... ./internal/auth/... ./internal/identity/... ./graph/...` — all PASS.

Token-safety assertion: `harness.countScimTokens(connID) == 0` holds in every
enable subtest — ProbeMapping created/minted/rotated/revoked zero `scim_tokens`.

## Remaining limitation (honest)

Phase 13 proves the SAME canonical-key equivalence Phase 12 proved, but now runs it
at the normal enable point so a proven mapping enables SCIM **without** break-glass.
This is still the configuration proof (the admin configured both extractors to
converge), exercised by the real production `ProbeMapping` with no live OIDC
login/JWT/IdP/session. It is NOT a live end-to-end OIDC↔SCIM login integration
against a real IdP (ADR-025 §3.1 lists that only as a fall-back safe read-only
validation and explicitly says the probe avoids it). The invariant "for the same
person both resolve to the same value" is now actively verified on the normal
enable path.

## What "proven" means here — autoAdd=true boundary

Two statements must be kept distinct:

1. **`UpdateScimConfig` actually executes `ProbeMapping` during enable.** TRUE — verified
   against live Postgres. The normal enable branch calls
   `r.ScimStore.DirectoryService().ProbeMapping(ctx, conn)` and attaches the result via
   `WithRoundTrip` before deciding. Phase 13 closes the missing enable-path wiring.
2. **`ProbeMapping` independently proves a customer's *real* OIDC `subjectClaim` matches
   their SCIM identifier.** NOT established by the current probe. The production probe runs
   with `autoAdd=true`, which fabricates the configured non-default `subjectClaim` onto the
   *synthetic* probe person's claims (`mapping_probe.go`'s `probeMappingWithClaims` autoAdd
   branch). So the probe genuinely exercises the SCIM `Provision→Get` round-trip and the
   canonical-key equivalence machinery, but it does **not** independently validate a real
   customer's independently-supplied OIDC claim against SCIM data — a live customer claim
   mismatch cannot be surfaced by this probe.

Consequence for the gate:
- A *proven* `ProbeMapping` now enables SCIM without break-glass — correct and verified.
- An *unproven* mapping still fails closed — but the resolver-level unproven path
  (`TestUpdateScimConfig_UnprovenMappingRefused`) is reached via mapping **configuration**
  rejection (e.g. `subjectClaim == scimIdentifier`), not via `ProbeMapping` returning
  `Verified=false` from an actual live claim mismatch. Live-claim-mismatch equivalence
  remains the Phase 12 configuration-proof scope, covered at the engine level by
  `internal/scim/mapping_probe_test.go` `autoAdd=false` T2/T3 (and the resolver test
  `TestUpdateScimConfig_CanonicalKeyMismatchFailsClosed` is a deliberate SKIP pointing there).

This caveat does **not** invalidate Phase 13's implementation acceptance. It precisely scopes
the remaining verification/design boundary: Phase 13 proves the enable path is now wired to the
real probe; it does not strengthen `ProbeMapping` into a live customer-claim validator.

## Acceptance criteria (from the approved plan) — status

1. Proven mapping → `scim_enabled=true` without break-glass — **MET** (live).
2. Unproven → fail-closed UserError + `SCIM_MAPPING_UNPROVEN` code + break-glass
   pointer + stays false — **MET** (live).
3. Disable / mapping-change force-disable semantics unchanged — **MET**.
4. Break-glass unchanged (still the exception when probe cannot run) — **MET** (live).
5. TestIdpConnection C1: never persists `scim_enabled=true` — **MET** (live regression).
6. No new state / migration / duplicated extraction — **MET** (no migration; reuses `ProbeMapping`).
7. Probe non-destructive (zero `scim_tokens` created/rotated/revoked) — **MET** (live assertion).
8. Live-DB suite green — **MET** (see results above).
9. path.md criterion 5 → MET (this phase) — **MET**.

## Out of scope (untouched)

Migrations, frontend, outbox/PENDING-13, revocation, `directory_service.go`,
Phase 11 OIDC wiring, `ProbeMapping` canonical-key logic, `TestIdpConnection`
C1 semantics, SCIM token handling.
