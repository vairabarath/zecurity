---
title: Connector Revocation — Phase F check: connector delete vs shields (two findings)
date: 2026-10-23
type: live check + findings (no code change)
design: Connector-Revocation-Implementation-Design-2026-10-14.md (§10, §12, O14)
branch: feat/sprint20-m1-phase2
status: FINDINGS OPEN — fixes need the user's choice
---

# Phase F check — what deleting a revoked connector does to its shields

## Background

The user chose "2a": forbid deleting a revoked connector while its certificates are valid. On inspection, the CRL part was **already done** by `52c16fa` (migration 039). `connector_certificates` has no foreign key, outlives the row, and feeds the CRL, so the design doc §12 and O14 were corrected (`2151541`). The same inspection showed that `DeleteConnector` cascades to `shields` (`003_shield_schema.sql`: `shields.connector_id … ON DELETE CASCADE`). This check verifies that live.

## Setup

- controller `56e0496` (`k2-controller`);
- new network `pf-net` (`218a40ea-…`) with resource `pf-res` (192.168.1.164:18098, nothing listening) in an empty group `pf-group`;
- `pf-c1` (`856e95b6-…`): HEAD connector `d3a26e8f`, user-level on this PC;
- `pf-shield` (`038e8e87-…`): HEAD shield `5ba9d7d4`, user-level on this PC, attached to `pf-c1`;
- `pf-c2` (`1130a22b-…`): HEAD connector, user-level on udaya;
- the shield's `state.json` = [`pf-c1`, `pf-c2`].

Tokens went through 0600 files or the hidden prompt and were shredded.

## Run

1. 06:28:05 UTC: the shield was stopped (graceful), simulating an offline shield.
2. 06:28:06: `revokeConnector(pf-c1)`, then `deleteConnector(pf-c1)`. Both returned true.
   - The shield row is **gone**, `shield_revocations` has **0** rows for it, and `pf-c1`'s certificate stays in `connector_certificates`, revoked (the CRL is fine).
3. 06:28:17: the shield was restarted.
   - **Finding B:** it retried `pf-c1` every ~2.4 s ("connector is not serving yet; retrying") for 2 min and never tried `pf-c2`.
4. 06:30:12: the `pf-c1` process was stopped.
   - 06:30:14.59: the shield failed over and `pf-c2` **accepted** it ("Control stream established").
   - Phase E then pruned `pf-c1` from its list.
5. The controller handled the orphan as follows:
   - its heartbeats update nothing (the row is absent, so `UPDATE … RETURNING` yields no rows and is silently ignored);
   - its discovery report failed on the `shield_discovered_services` foreign key, so nothing was written;
   - no resource references it.
6. All processes were stopped afterwards.

## Finding A — deleting a connector orphans its shields (cascade without tombstone)

- `deleteConnector` deletes every `shields` row whose `connector_id` is that connector and does **not** record them in `shield_revocations`.
- The shield's certificate stays valid and unrevoked, and any **peer connector in the same network accepts it** (verified live).
- Bounds:
  - certificate renewal is refused ("shield not found"), so it ends at certificate expiry (`SHIELD_CERT_TTL`, 7 d by default);
  - it is invisible in the admin UI and can't be revoked there, because it has no row;
  - nothing is written for it, and no resource routes to it.
- Severity: low to medium. It is an untracked, unrevocable identity for up to 7 days, not a data path.
- It happens whenever the shield's row still points to the deleted connector, i.e. the shield had not failed over (offline, or stuck as in Finding B).
- **Fix options** (code only, no schema change):
  - (a) refuse `deleteConnector` while shields are attached;
  - (b) tombstone the attached shields (`shield_revocations` + outbox) in the same transaction, then delete.

## Finding B — a shield never fails over away from a revoked connector

- The connector refuses the shield with `Unavailable("connector is not authorized to serve")` in **every** non-serving state (`connector/src/agent_server.rs:902`, and the close path at `:989`).
- That includes the **terminal** `Revoked` and `UnknownIdentity` states.
- The shield treats that message as "the connector will be back soon" (`CONNECTOR_NOT_SERVING_MESSAGES`, B2 live finding 1). It retries the same head every 2 s and resets the backoff. `build_client` always picks the first connector that completes TLS, so it **never fails over** while the revoked connector's process runs.
- Verified live on restart (step 3). For a shield attached when the revocation happens, the stream ends with the same message, so the same loop follows. That is from reading the code, not run live.
- This contradicts design §10 ("Channel closed → existing failover"). It is fail-closed (no security impact) but an **availability** bug: a revoked connector strands its shields even when a healthy peer exists.
- **Fix option:** the connector sends a distinct refusal for the terminal states (for example `connector is revoked` / `connector identity unknown`). The shield does **not** treat it as "not serving yet", so its normal failover walks to the next peer. The shield could also move that connector to the tail. No proto change is needed (a status message string, like the existing pair).

## Lab state left

- DB: `pf-net`, `pf-res`, `pf-group`, `pf-c2` (active row, process stopped).
- `pf-c1` and `pf-shield` rows are deleted; that is the finding.
- Files: `~/.cache/zecurity-f` on this PC; `~/pf-c2.sh`, `~/pf-c2/`, `~/connector-pf` on udaya.

Nothing is pushed (user rule).
