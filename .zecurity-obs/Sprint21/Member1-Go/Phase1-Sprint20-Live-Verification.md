---
type: phase
member: M1
person: Sathiya
sprint: 21
phase: 1
execution: V
title: Sprint 20 Live Verification
status: planned
depends_on: []
schema_change: false
tags:
  - acceptance
  - live
  - provider-dashboard
---

# Phase 1 (V) — Sprint 20 Live Verification

> **Runbook:** `docs/sprint20-live-acceptance-runbook.md` · **Run sheet:** `.zecurity-obs/Sprint20/Live-Acceptance-Run-Sheet.md`.
> This phase closes Sprint 20. It doesn't change code, unless it finds a bug; see "If a check fails".

## Goal

Run the Sprint 20 live acceptance checks (phases A–G) on a real dev stack, record the evidence, and close the remaining `.zecurity-obs/Sprint20/path.md` boxes.

## Run it on the Sprint 20 code, not on Sprint 21 work in progress

Sprint 21 changes provider login (Phase H: new required `PROVIDER_JWT_SECRET`, `sub`/`hd` binding) and relay renewal scheduling (Phase K, KI-1). The runbook was written for the Sprint 20 final state, so run it in a **separate worktree** at that commit:

```bash
git fetch origin
git worktree add ../zecurity-s20-live 46ab87d      # fixed-pendings after PR #99 (Sprint 20 final)
cd ../zecurity-s20-live
```

- Run everything from that worktree. It shares Docker volumes with your main checkout, so don't run both controllers at once.
- **After the run, reset the local DB** before going back to Sprint 21 work. Phase H adds the schema file `037_provider_identity_binding.sql` (development rule in `Sprint21/path.md`).

## Steps

- **V1 — Prerequisites (the day before):** runbook §3.1.
  - `PROVIDER_GOOGLE_REDIRECT_URI=http://localhost:8080/provider/auth/callback` in `controller/.env`, registered on the Google OAuth client.
  - `PROVIDER_BOOTSTRAP_EMAILS=<your email>`.
  - An admin login to an active workspace.
- **V2 — Run stages 1–8** (runbook §5–§13). It takes about 95 minutes in one sitting.
  - Env: `CONNECTOR_CERT_TTL=15m`, `CONNECTOR_RENEWAL_WINDOW=10m`, `RELAY_CERT_TTL=1h` (KI-1), `SHIELD_CERT_TTL=24h` (KI-2).
  - Fill in every run-sheet row with time, evidence and PASS/FAIL.
- **V3 — Close out:**
  - Tick **only** the passing boxes in `.zecurity-obs/Sprint20/path.md`: :143, :156, :169, :206, :207, :227 and the sprint criteria :246–:255. The run sheet maps each row to its line.
  - Update the run-sheet frontmatter `status:`.
  - Add a Session Log entry.
  - Clean up (runbook §14): nft table, processes, `/tmp/s20-live`, `.localdev/s20-*`, the worktree.

## If a check fails

- Record it in the run sheet's **Findings** table and leave its `path.md` box unticked.
- Tell Barath if it's in a phase he owns (A, C, F, G), or take it yourself if it's yours (B, D, E).
- A fix goes on a branch from `fixed-pendings` (not the worktree). It's documented in the Sprint 20 phase file's **Post-Phase Fixes** and `Sprint20/path.md` **Post-Sprint Fixes**, and the failed row is re-run afterwards.
- A failure isn't a reason to abort the run. Continue the remaining stages if you safely can.

## Acceptance Criteria

- [ ] AT-V.1 … AT-V.3 in [[Sprint21/Acceptance-Test-Plan]].

## Implementation Checklist

- [ ] V1 prerequisites
- [ ] V2 run complete; run sheet filled in
- [ ] V3 `Sprint20/path.md` boxes ticked for passing rows; findings filed; Session Log entry; clean-up; local DB reset

## Post-Phase Fixes

_None yet._
