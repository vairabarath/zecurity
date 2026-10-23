---
type: fix
sprint: 20
fix: 10
title: Admin UI can't protect a resource created before its shield
status: fixed and verified live (2026-10-22). Committed locally, not pushed.
source: Phase B2 live run 2026-10-21, finding 2
severity: low (UI only; workaround was delete + recreate the resource)
component: admin (src/pages/ResourceDetail.tsx, src/lib/protectShield.ts, src/graphql/queries.graphql)
approval: the user approved this admin/ change on 2026-10-22 (CLAUDE.md "Don't modify admin/" during Sprint 21)
branch: feat/sprint20-m1-phase2
date: 2026-10-22
tags:
  - admin
  - react
  - resource
  - shield
---

# Fix 10: the Protect gate mirrors the backend's shield choice

## Symptom

In B2 (2026-10-21), "Protect this resource" stayed disabled for a resource created before its shield. The workaround was to delete and recreate the resource.

## Root cause

- **Backend:**
  - `CreateResource` binds a shield only if one is **active** on the host at creation (`AutoMatchShield`, `internal/resource/store.go`);
  - `MarkProtecting` (same file) picks *any* active shield in the resource's remote network whose `lan_ip` equals the host, preferring the bound one, and binds it.
- **UI:** `admin/src/pages/ResourceDetail.tsx` gated the button on `resource.shield?.status === Active`, i.e. on the **bound** shield only.
- **Result:**
  - a resource created before its shield has `shield_id = NULL`, so the button could never be enabled, and the page said "No shield installed on this host" even with an active shield there;
  - the same applied to a resource whose bound shield was revoked after a new shield was installed on the host.

## Fix

- `admin/src/graphql/queries.graphql`: `GetShields` also selects `lanIp`. `npm run codegen` changed only that query.
- **New `admin/src/lib/protectShield.ts`:**
  - `protectTargetShield(host, bound, shields)` returns the active shield of the remote network with `lanIp === host`, the bound one preferred. This is exactly `MarkProtecting`.
  - `hostShield(...)` returns the shield to describe when none is protectable: the bound one, else any shield on the host.
- **`ResourceDetail.tsx`:**
  - loads `GetShields(resource.remoteNetwork.id)` (polled like the resource);
  - `canProtect = protectTarget !== null` drives the header button, the "Unprotected" panel and the shield card, and the panel names the target shield;
  - the shield card says "Protecting binds this resource to the shield on …" when the target isn't bound yet;
  - the header still shows only the **bound** shield.
- The backend is unchanged and remains the gate. A stale page still gets the backend's error toast.

## Tests

- **`src/lib/protectShield.test.ts`** (5):
  - nothing bound plus an active shield on the host → protectable;
  - an active bound shield is preferred;
  - a revoked bound shield falls back to an active one;
  - inactive, wrong-IP or no-IP shields → null;
  - `hostShield` fallbacks.
- **`src/pages/ResourceDetail.protect.test.tsx`** (3, MockedProvider + MemoryRouter):
  - created-before-shield → Protect **enabled**;
  - only a disconnected shield → disabled;
  - revoked bound shield plus a new active one → enabled.
- **On the old page:**
  - tests 1 and 3 FAIL on `toBeEnabled` (the bug);
  - test 2 fails only because the old page never named an unbound shield on the host; the button was disabled there too.
- **Gate:** `npx vitest run` gives **114 PASS** (19 files). `eslint` on the changed files is clean, and `npm run build` (`tsc -b` + vite) is clean.

## Live verification (2026-10-22)

- **Setup:** controller `071c0de`; `npm run dev` (vite, :5173); local test login in Orca's built-in browser.
- **Repro:**
  1. resource `s3-ui` (k2-net, `192.168.1.164:18080`) created while shield `s3-shield` (lan_ip `192.168.1.164`) was **disconnected** → `shield: null`;
  2. then `s3-shield` started → **active**.

  The client tunnel was taken down, and a local connector `s3-relaylist` was started for the shield.

| | Old `ResourceDetail.tsx` (HEAD) | Fixed |
|---|---|---|
| Header button | **"Protect this resource" disabled** | enabled |
| Panel | "Install a shield on 192.168.1.164 …" | "The shield **s3-shield** is available on 192.168.1.164" + Protect |
| Shield card | "No shield installed on this host" | `s3-shield` · **Ready** · "Protecting binds this resource to the shield on …" |
| Click Protect | not possible | accepted. DB: `s3-ui` **bound to `s3-shield`**, `pending_action=apply`. The shield got the apply and reported `failed to apply resource_protect chain`, because this test shield runs **without root** (no nftables). That's expected and not part of this fix |

- Screenshots: `~/.cache/zecurity-s3/ui-baseline.png`, `ui-fixed.png`.
- **Cleanup:** `s3-ui` was force-deleted; the local connector and shield were stopped; the client is back up.
