---
type: phase
member: M1
sprint: 17
phase: 1
title: SCIM Schema
status: planned
depends_on: []
tags: [go, identity, scim, data-model, pending-05]
---

# Phase 1 — SCIM Schema

> Depends on nothing — Day 1. Additive DDL; no runtime path reads it yet → zero risk to live auth.
> Full spec: [[ADR-025-SCIM-Directory-Synchronization]] §12 · [[PENDING-05-SCIM-Implementation-Plan]] P1.

## Goal
Stand up every table/column ADR-025 §12 requires, so later phases have their storage. **Do not** create
`outbox_events` — that is M2 / PENDING-15.

## Files
| File | Change |
| --- | --- |
| `controller/migrations/<TBD>.sql` | **new** — all SCIM schema (number assigned at integration, after `030` + `033`) |

## Steps
- [ ] `identity_connections` +`subject_claim TEXT DEFAULT 'sub'`, +`scim_identifier TEXT DEFAULT 'externalId'`, +`scim_enabled BOOL DEFAULT FALSE`, +`last_sync_at TIMESTAMPTZ`, and `status` CHECK extended to `active|disabled|deleted`.
- [ ] `users` +`provisioned_by` (immutable: jit|manual|scim), +`provisioning_owner` (mutable: jit|manual|scim|unmanaged), +`sync_instance_id`.
- [ ] `groups` +`origin` (manual|scim|system), +`external_id`.
- [ ] `external_identities` +`sync_instance_id`.
- [ ] new `scim_tokens`, `scim_identity_conflicts` (+ unique partial index on pending per `(workspace,connection,canonical_identity_key)`), `scim_sync_instances`.

## Rules
- Copy the DDL from ADR-025 §12 verbatim (it is the contract). Defaults preserve today's behavior.
- **Migration number is NOT reserved now** — assign from the real tree at integration (Q5).

## Build gate
`cd controller && go build ./...`; migration applies on a fresh DB; PENDING-04 integration tests still green.
