---
type: phase
member: M2
person: Barath
sprint: 22
phase: 4
execution: X
title: Relay Revoke and Delete in the Console
status: planned
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

## Post-Phase Fixes

*(None yet.)*
