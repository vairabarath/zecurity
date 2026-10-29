---
type: phase
member: M2
person: Barath
sprint: 22
phase: 4
execution: X
title: Relay Revoke and Delete in the Console
status: implemented   # 2026-10-28 on sprint22/phase-x-relay-revoke-delete, not committed; live AT-X2…X5 PASS on isolated stacks (AT-X4 with a real connector)
depends_on: [2]   # A merged (server-side D-17 and D-31)
schema_change: false
tags:
  - react
  - provider-console
  - relay
  - provider-dashboard
---

# Phase 4 (X) — Relay Revoke and Delete in the Console

> **Decision Record:** **D-30** (console may revoke/delete relays), **D-31** (super-admin only), **D-17** (reason + typed confirmation), **D-20** (replace a broken relay).

## Problem

After Phase A the API enforces everything, but operators still need `curl` and a token to revoke or delete a relay.

## Goal

A super-admin revokes or deletes a relay from Relay detail, with a required reason and the relay name typed to confirm. Nobody else sees the controls.

## Console

- **`ConfirmDangerDialog`** (new shared component, reused in Phase D): title, consequence text, required **reason** (1–500 characters, counter), "Type **<relay name>** to confirm" input, destructive button enabled only when both are valid, pending state, error area.
- **Relay detail → "Danger zone" card**, visible only to super-admin and only for relays not `revoked` / `deleted`:
  - **Revoke relay:** "Its certificates go on the CRL and connectors move to other relays. The relay stays listed as revoked."
  - **Delete relay:** "Revokes it and removes it from the fleet. To replace it, add a new relay."
- After success: toast, detail refreshed; the audit log shows the reason.
- Error mapping: `already_revoked` / `already_deleted` → refresh and explain; `confirmation_mismatch` → field error; `forbidden` → Forbidden page.

## Tests (write first)

- Card hidden for relay-ops, unknown roles, and revoked/deleted relays.
- Button disabled until the reason is non-blank and the typed name matches exactly; over-long reason blocked.
- Sends `{reason, confirm_name}` in the body (never the URL); exactly one request per confirm.
- Each error code maps to the right message; the page refreshes after success.
- API-surface pin includes revoke and delete.
- Read-only guards on other pages still pass (no new controls elsewhere).

## Acceptance

[[Sprint22/Acceptance-Test-Plan]] → **AT-X1 … AT-X5** (incl. the live revoke on the lab).

## Test gate

Console gate from [[Sprint22/path]].

## Implementation record (2026-10-28)

**Console:**
- `src/components/ConfirmDangerDialog.tsx` (new, reused by Phase D): title, consequence text, required reason with a live counter, "Type <name> to confirm", destructive button enabled only when both are valid, pending state, field errors (`aria-describedby`) and an alert for other errors. The reason is trimmed and counted in code points, like the server (`utf8.RuneCountInString`, 1–500); the name is compared exactly (no trimming, case matters), like the server. It hands back the trimmed reason and the name as typed.
- `src/components/RelayDangerZone.tsx` (new): super-admin only (`hasRole`, fail-closed for unknown/empty roles), hidden for deleted relays. Revoke while the relay isn't revoked; **Delete for pending, active, inactive and revoked relays** (a revoked relay may still be removed; deleting a pending relay is the D-20 path for an expired token). One request per confirm (a ref guards a second submit before the pending re-render).
- `src/pages/RelayDetail.tsx`: the outcome notice lives in the page component, tied to the relay id, because the reload after an action replaces the view; a manual Refresh clears it.
- `src/api/relays.ts`: `revokeRelay` (`POST /provider/relays/{id}/revoke`) and `deleteRelay` (`DELETE /provider/relays/{id}`), body `{reason, confirm_name}`, never in the URL. `src/api/client.ts`: `DELETE` added to the method type.
- `src/lib/relay-errors.ts` `relayActionFailure`: `confirmation_mismatch` → name field; `reason_required` / `reason_too_long` → reason field; `forbidden` → error inside the dialog (no redirect); `already_revoked` / `already_deleted` / `not_found` → close, reload, explain; network and unknown errors → a generic message. An ended session is left to the session store.
- `src/api/types.ts`: `already_revoked`, `already_deleted`, `confirmation_mismatch` codes. `api-surface.test.ts` pins both writes.

**Controller (one line, found while checking the browser path):** `internal/middleware/provider_cors.go` allowed `GET, POST, PATCH, OPTIONS` only, so in production (console on its own origin) the browser's preflight for `DELETE /provider/relays/{id}` was refused and Delete would have failed; the Vite proxy hides this in development. Now `GET, POST, PATCH, DELETE, OPTIONS` (still the exact console origin only). Test: `TestProviderCORS_PreflightAllowsEveryConsoleMethod` (failed before).

**Deviations from the plan above (deliberate):**
- Delete stays visible on a revoked relay (the plan hid the whole card); the server allows it, and otherwise removing a revoked relay needs `curl`.
- `forbidden` is shown inside the dialog rather than navigating to the Forbidden page.
- Unknown roles can't open relay pages at all (route guard); the danger zone also checks the role itself.

**Tests (written first, confirmed failing):** `ConfirmDangerDialog.test.tsx` (9) and `RelayDetail.test.tsx` (+24: visibility by role and status incl. a direct render for unknown roles, request method/path/body with no query string, double click → one request, cancel → none, every error code, refresh after success, a revoked relay can still be deleted). The old super-admin "read-only" test became a relay-ops read-only test plus an exact super-admin button list.

**Gates:** console 469 pass (exit 0), lint exit 0, build exit 0; `go test ./internal/middleware/` pass.

**Live (2026-10-28), isolated stack:** see [[Sprint22/Acceptance-Test-Plan]] → "Live run — Phase X".

## Post-Phase Fixes

*(None yet.)*
