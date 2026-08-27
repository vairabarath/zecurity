---
type: phase
member: M1-Frontend
sprint: 17
phase: 6
title: Guided IdP Setup Wizard (OIDC Connection + SCIM Token in One Flow)
status: planned
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
| File | Change |
| --- | --- |
| `admin/src/components/idp/CreateIdpConnectionDialog.tsx` | Reused as Step 1 of the wizard, or refactored into a step component if the wizard wraps it. |
| `admin/src/components/idp/*` (FE-1's SCIM config + token-mint components) | Reused as Steps 2–3. |
| `admin/src/components/idp/GuidedIdpSetupWizard.tsx` | **new** — thin orchestration shell (step state, back/next, skip-SCIM option) around the existing FE-0/FE-1 components. No new GraphQL operations — this phase does not add backend surface. |

## Out of Scope
- Any backend change — `createIdpConnection`, `updateScimConfig`, and the token-mint mutation already
  exist and are reused as-is.
- Making the guided wizard mandatory — a direct path to FE-0's dialog and FE-1's page should remain
  available; this is additive UX, not a replacement flow.
- Anything covered by `Member1-Go/Phase10-Reference-Integration-Hardening.md` (deprovision
  session-kill, mapping round-trip probe) — that work is backend-only and unrelated to this wizard.

## Priority note
This phase is explicitly **not scheduled**. It is recorded here so the UX idea isn't lost, not because
any acceptance criterion or defect requires it. Confirm with product before picking it up — FE-0 and
FE-1 already deliver the full functional capability without it.

## Build gate
`cd admin && npm run codegen && npm run build` green; `npx vitest run` passes without regression;
`npx eslint` across touched files exits 0. (Same gate shape as FE-0/FE-1 — no new backend build gate,
since no backend files change.)
