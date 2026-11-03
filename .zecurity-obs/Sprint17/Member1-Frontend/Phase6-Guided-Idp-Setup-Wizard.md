---
type: phase
member: M1-Frontend
sprint: 17
phase: 6
title: Guided IdP Setup Wizard (OIDC Connection + SCIM Token in One Flow)
status: implemented-unverified
depends_on: [0, 1]
tags: [react, admin, idp, oidc, scim, ux, nice-to-have, pending-05]
---

# Phase 6 (FE) — Guided IdP Setup Wizard

> Depends on FE-0 (connection creation) and FE-1 (SCIM config + token minting). Low priority,
> **not currently required by any acceptance criterion** — this is a UX nice-to-have surfaced while
> comparing Zecurity's onboarding flow against a reference enterprise IdP integration walkthrough
> (generic OIDC login + SCIM provisioning against Okta); see `.zecurity-obs/Sprint17/pending.md` §7–8.

## Goal
Optionally combine "create an OIDC connection" and "mint a SCIM token" into a single guided,
multi-step wizard, instead of two separate admin actions the operator must know to do in sequence.

## Context & Problem
The reference integration's own setup surfaced the IdP connection dialog and the SCIM
endpoint-plus-token dialog back-to-back, as two steps of **one** wizard: filling in an OIDC
issuer/client id/secret, verifying the login works, and only then generating and displaying a SCIM
endpoint + shown-once bearer token. Zecurity's current admin flow (FE-0 → FE-1) accomplishes the same
end state, but as two independently-triggered mutations (`createIdpConnection`, then a separate
token-mint action on the connection detail page) rather than one continuous flow. This is a sequencing
/ discoverability gap, not a functional one — every field Zecurity's dialogs already collect matches
what the reference flow required (see `pending.md` §7, "Requirement" rows already marked
**Supported**).

## Expected Flow (proposed)
```text
Identity Providers (/idp-connections)
    ↓
Click "Add Identity Provider"
    ↓
Step 1: OIDC connection fields (reuses FE-0's CreateIdpConnectionDialog fields)
    ↓ createIdpConnection
Step 2: (if SCIM desired) mapping fields + enable-SCIM (reuses FE-1's fields)
    ↓ updateScimConfig / enableScimBreakGlass
Step 3: mint SCIM token, shown once (reuses FE-1's token-mint panel)
    ↓
Wizard closes → connection detail page, fully configured
```

## Files (proposed — not yet built)

Verified against the current tree: FE-0 and FE-1 are both already implemented (`status:
implemented-unverified` in their own phase files), so the reuse targets below are real, existing
components, not placeholders.

| File | Change |
| --- | --- |
| `admin/src/components/idp/CreateIdpConnectionDialog.tsx` | Reused as Step 1 of the wizard, or refactored into a step component if the wizard wraps it. |
| `admin/src/components/scim/ScimConfigCard.tsx` | Reused as Step 2 (mapping fields / enable-SCIM). |
| `admin/src/components/scim/ScimBaseUrlBox.tsx`, `admin/src/components/scim/ScimTokenPanel.tsx` | Reused as Step 3 (SCIM base URL + mint/rotate/revoke token panel). |
| `admin/src/pages/IdpConnectionDetail.tsx` | Currently composes `ScimConfigCard` + `ScimBaseUrlBox` + `ScimTokenPanel` + `IdentityHealthBadge` directly on the detail page; the wizard would extract steps 2–3 from here rather than duplicate them. |
| `admin/src/components/idp/GuidedIdpSetupWizard.tsx` | **new** — thin orchestration shell (step state, back/next, skip-SCIM option) around the existing FE-0/FE-1 components. No new GraphQL operations — this phase does not add backend surface. |

## Out of Scope
- Any backend change — `createIdpConnection`, `updateScimConfig`, and the token-mint mutation already
  exist and are reused as-is.
- Making the guided wizard mandatory — a direct path to FE-0's dialog and FE-1's page should remain
  available; this is additive UX, not a replacement flow.
- Anything covered by `Member1-Go/Phase10-Reference-Integration-Hardening.md` (the mapping
  round-trip probe, now implemented), `Phase11-OIDC-SubjectClaim-Wiring.md` (OIDC login-path
  `subjectClaim` wiring), or `Phase12-OIDC-SCIM-Canonical-Key-Equivalence.md` (OIDC↔SCIM equivalence
  proof) — all backend-only and unrelated to this wizard.
- Anything related to PENDING-13/ADR-028 (device-trust outbox handlers, client daemon changes) —
  unrelated to this admin-frontend wizard; owned by a different team member.

## Priority note
This phase is explicitly **not scheduled**. It is recorded here so the UX idea isn't lost, not because
any acceptance criterion or defect requires it. Confirm with product before picking it up — FE-0 and
FE-1 already deliver the full functional capability without it.

## Build gate
`cd admin && npm run codegen && npm run build` green; `npx vitest run` passes without regression;
`npx eslint` across touched files exits 0. (Same gate shape as FE-0/FE-1 — no new backend build gate,
since no backend files change.)

## Status — implemented-unverified (2026-08-28)

Built as a NEW additive component per decisions: the wizard composes the existing
FE-0 (`CreateIdpConnectionDialog`) and FE-1 (`ScimConfigCard` / `ScimBaseUrlBox` /
`ScimTokenPanel`) components into one guided 3-step shell. It does NOT replace or
remove them, and the direct "Add Identity Provider" dialog + connection detail page
remain fully available.

### Files shipped
- `admin/src/components/idp/GuidedIdpSetupWizard.tsx` — new orchestration shell
  (step state 1→2→3, Back/Next, "Skip SCIM", Finish → navigates to the new
  connection's detail page). Reuses the existing step components verbatim; does
  not duplicate their GraphQL or business logic. SCIM enablement stays
  server-authoritative — Step 2's `ScimConfigCard` already attempts
  `updateScimConfig` and surfaces `BreakGlassDialog` on the server's refusal; the
  wizard never re-implements the MappingGate.
- `admin/src/components/idp/GuidedIdpSetupWizard.test.tsx` — new (3 tests): Step 1
  renders the embedded create dialog; Cancel calls `onClose`; successful create
  advances to Step 2 exposing the "Skip SCIM" option. Uses `vi.mock('@apollo/client/react')`
  + `MemoryRouter` (matching the FE-4 Apollo-v4 pattern; no `MockedProvider`).
- `admin/src/pages/IdpConnections.tsx` — ADDITIVE only: a second "Set up with SCIM"
  button opens the wizard alongside the existing "Add Identity Provider" button.
  The direct dialog and detail-page paths are untouched.

### Build gate results
- `npm run codegen` — green (idempotent; no new GraphQL operations).
- `npm run build` (`tsc -b` + vite) — green.
- `npx vitest run` — 11 files / 53 tests pass (this phase adds 1 file; suite
  delta = +1 file / +13 tests vs the pre-phase 8-file / 40-test baseline).
- `npx eslint` — 18 problems (12 err / 6 warn): byte-identical to the pre-existing
  `admin/` baseline; zero problems introduced by this phase. `GuidedIdpSetupWizard.tsx`
  itself reports 0 problems after an unused-param fix.

### Update (2026-08-28) — Twingate-exact "popup provider picker → per-provider dialog" interaction
The original "Add Identity Provider" button opened a single dialog with the provider chosen via an
inline `<select>` alongside all the other fields — functionally equivalent to Twingate's dialog fields,
but a different *interaction sequence* (Twingate: click → a small provider popup → pick Okta → the
"Connect Okta" dialog opens). Changed to match exactly:
- `admin/src/components/idp/providers.ts` (**new**) — `PROVIDER_OPTIONS`/`ProviderKey`/`providerLabel`
  extracted out of `CreateIdpConnectionDialog.tsx` (constants can't be co-exported with a component in
  the same file under this repo's `react-refresh/only-export-components` lint rule).
- `admin/src/components/idp/AddIdentityProviderMenu.tsx` (**new**) — the popup step: a `DropdownMenu`
  listing Okta / Entra ID / JumpCloud / Keycloak / Generic OIDC. Selecting one is the only thing it
  does; it collects no connection details itself.
- `admin/src/components/idp/CreateIdpConnectionDialog.tsx` — gained an optional `initialProvider`
  prop. When set (picker step already ran): dialog title becomes `Connect Okta` (per-provider, not
  generic), the provider `<select>` is replaced by a static, non-editable label, and the field set is
  otherwise identical. When omitted, prior behavior (inline editable selector, generic title) is
  unchanged — kept for robustness/direct usage.
- `admin/src/pages/IdpConnections.tsx` — both the header button and the empty-state CTA now open
  `AddIdentityProviderMenu` first; selecting a provider opens `CreateIdpConnectionDialog` with
  `initialProvider` set.
- Tests: 5 new (`AddIdentityProviderMenu.test.tsx` ×3: renders closed, popup lists all 5 providers,
  selecting Okta calls `onSelect('okta')`; `CreateIdpConnectionDialog.test.tsx` +2: `initialProvider`
  shows `Connect Okta` + no editable selector; omitted `initialProvider` preserves the original
  behavior). Full suite: 12 files / 58 tests pass (was 11/53). `tsc -b`, `eslint` on all touched files:
  clean.

### Update (2026-08-28, second pass) — merged into a single Twingate-parity entry point
Explicit follow-up request: "I want it like Twingate." Twingate has exactly **one** entry point
("Add Identity Provider") that flows straight through connect → SCIM, not a choice between a plain
create and a separately-labeled "guided" flow. The page previously had two buttons ("Add Identity
Provider" doing a plain create, "Set up with SCIM" opening the 3-step wizard) — removed the split:
- `GuidedIdpSetupWizard.tsx` gained an `initialProvider?: ProviderKey` prop, threaded into its Step 1
  `CreateIdpConnectionDialog`. Omitted, it falls back to the original generic-form behavior (still
  covered by the pre-existing tests), so nothing about the wizard's own contract broke.
- `IdpConnections.tsx`: the standalone `CreateIdpConnectionDialog` mount and the separate "Set up with
  SCIM" button are **removed**. There is now exactly one button — `AddIdentityProviderMenu` — at both
  the header and the empty-state CTA. Selecting a provider opens `GuidedIdpSetupWizard` directly with
  `initialProvider` set: popup → "Connect Okta" → SCIM mapping → token mint, one continuous flow,
  matching Twingate's own sequence (§1 of `findings.md`) exactly.
- New test: `GuidedIdpSetupWizard.test.tsx` — `initialProvider="okta"` renders the "Connect Okta" step,
  not the generic form, proving the popup selection threads all the way through. Full suite: 12 files /
  59 tests pass. `tsc -b`, `eslint` on all touched files: clean.
- The wizard's own "Skip SCIM" option (Step 2) is intentionally kept, even though Twingate's real flow
  doesn't offer a skip — a deliberate, minor Zecurity-side improvement, not a departure from what was
  asked (the *entry sequence* is what had to match Twingate; an extra safety valve mid-flow doesn't
  contradict that).

### Audit notes
- No backend, migration, outbox/PENDING-13, or unrelated file changed. `git diff
  --stat` confirms only `admin/src/**` + this phase doc are touched.
- `ScimTokenPanel`'s secret-handling (`fetchPolicy: 'no-cache'` on mint/rotate,
  show-once plaintext dialog) is reused as-is — the wizard does not strip it.
- Manual gate NOT run: without a live IdP/Okta/Entra tenant the end-to-end flow
  (mint a token, paste base URL into the IdP, real break-glass refusal) is
  unexercised. Automated gates pass; hence `implemented-unverified`, not `done`.
- Per the spec's own note, this phase is a UX nice-to-have, not a required
  acceptance criterion; the wizard is optional and does not become the sole or
  mandatory configuration path.

---

## Post-Phase Fixes

### Fix: the wizard presented an unverified connection as a completed setup step (2026-08-31)

**Issue:** This phase made the guided wizard the **single entry point** for IdP setup
(`AddIdentityProviderMenu` → `GuidedIdpSetupWizard` → `CreateIdpConnectionDialog`), which raised the
stakes on a pre-existing gap: Step 1 never contacted the IdP. `createIdpConnection` only persisted the
row, so Step 1 could "succeed" against a mistyped Okta domain and the wizard would advance to Step 2
(SCIM mapping) and Step 3 (token mint) on top of a connection that had never been reached.

Note this phase's own **Context & Problem** section describes the reference flow as "filling in an
OIDC issuer/client id/secret, **verifying the login works**, and only then generating … a SCIM
endpoint + token" — the verification beat was specified here but was never actually implemented on
either side.

**Root cause:** see FE Phase 0's Post-Phase Fixes — `createIdpConnection` performed no network call,
and the existing `OIDCProvider.Probe` was unreachable from the UI.

**Fix applied:** Step 1 now genuinely gates the wizard. `createIdpConnection` validates the issuer's
OIDC discovery document before persisting and refuses to save an unreachable/invalid one, so the
wizard cannot advance past Step 1 on an unverified issuer. Implemented entirely in the reused FE-0
dialog + backend — **no wizard-shell logic changed** and the step sequence is unchanged.

**Do not confuse the two probes in this wizard:**

| Step | Probe | Contacts the IdP? |
|------|-------|-------------------|
| 1 (create) | `validateOIDCDiscovery` → `OIDCProvider.ProbeFresh` | **Yes** — fetches `.well-known/openid-configuration` |
| 2 (SCIM enable) | `ScimStore.DirectoryService().ProbeMapping` | **No** — internal Zecurity mapping-consistency round-trip through our own SCIM engine, using a synthetic probe person |

Step 2's probe proves the OIDC↔SCIM canonical keys agree; it is **not** an Okta connectivity test and
must never be described as one.

**Still not verified anywhere in this wizard:** OAuth client ID, client secret, redirect URI, and
whether a user can actually sign in. The wizard finishes without ever having proven those; the first
real proof is a sign-in. Full write-up: `Sprint17/path.md` → **Finding D** (including why
`testIdpConnection` must not be wired to a "Test Connection" button as-is).
