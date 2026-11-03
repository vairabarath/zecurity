---
type: adr
status: placeholder
id: ADR-026
domain: identity
priority: P3
created: 2026-08-06
related:
  - ADR-023-Identity-Philosophy
  - ADR-024-Identity-Linking-and-Provider-Migration
  - ADR-025-SCIM-Directory-Synchronization
  - PENDING-05-Directory-Sync-SCIM
tags: [adr, identity, governance, linking, placeholder]
---

# ADR-026 — Identity Governance & Identity Linking (PLACEHOLDER)

> **Status: PLACEHOLDER — reserved, not yet written.** This ADR number is reserved so it cannot collide
> later. It contains **no decisions** — do not implement from it. It will be written only when we decide
> to build **Stage 4 (Identity Governance)** of the identity maturity model — chiefly **explicit identity
> linking / contractor→employee conversion**.
>
> Until then, the reasoning already lives in
> [[Identity-Lifecycle-and-Ownership-Design-Review]] (§6, and the conflict matrix) and the maturity model
> in [[Identity-Architecture-v1.0]]. Nothing here gates PENDING-05.

## Why this is deferred (not missing)

Contractor→employee conversion is **Identity Governance**, which naturally follows federation (Stage 2)
and lifecycle management (Stage 3). It is deliberately *not* part of PENDING-05. See the maturity model.

## Scope (to be decided when written)

- Explicit, admin-initiated, **audited** identity linking — multiple `external_identities` → one canonical
  user — with **no automatic merge** (never by email; ADR-024 stands).
- Contractor → employee conversion workflow.
- **Source-of-authority arbitration** across linked identities (which connection owns lifecycle) — the
  prerequisite introduced in ADR-024's addendum.
- Possibly: access reviews / attestation.

## Non-decisions

This document decides nothing yet. When Stage 4 is scheduled, replace this placeholder with a full ADR
(Context → Decision → Consequences), following ADR-024/025.
