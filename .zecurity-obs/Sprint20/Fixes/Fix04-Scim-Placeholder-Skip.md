---
type: fix
sprint: 20
fix: 4
title: Unconditional t.Skip placeholder in the SCIM enable suite
status: planned
source_rows: [R2]
source_findings: [36]
severity: low
component: controller (graph/resolvers tests)
tags:
  - go
  - scim
  - regression-gate
  - test-hygiene
---

# Fix 04 — Unconditional `t.Skip` placeholder in the SCIM enable suite

> Source: live acceptance row **R2** (FAIL). R2 accepts exactly one skip
> (`TestEnroll_CSRSignatureInvalid`); this is a second, unconditional one.
> Status: **verified, not fixed.** No code changed.

## Failure (as observed)

`TestUpdateScimConfig_CanonicalKeyMismatchFailsClosed` →
`--- SKIP … covered by internal/scim/mapping_probe_test.go (autoAdd=false) + gate-level config rejection`,
in every run (with and without the TEMP change).

## Verification (2026-09-28, this re-check)

- `controller/graph/resolvers/idp_update_scim_config_enable_test.go:120-131`: the test body is only a
  comment plus `t.Skip(...)`. There's no env check, so it skips on every machine, CI included.
- Added in `0a962f41` (2026-08-31).
- The comment says the behaviour is covered by `internal/scim/mapping_probe_test.go` (T2/T3, autoAdd=false)
  and by `TestUpdateScimConfig_UnprovenMappingRefused` (config-invalid gate). R2 ran both packages; both
  are green, so the coverage claim holds and the placeholder adds nothing.

**Conclusion:** confirmed. It isn't a product bug; it's a placeholder that violates the plan's "no
skipped acceptance tests" rule and makes the R2 skip count noisy.

## Goal

The only skip left in `go test ./...` is the accepted `TestEnroll_CSRSignatureInvalid`.

## Design (proposal; confirm before implementing)

- **Delete** `TestUpdateScimConfig_CanonicalKeyMismatchFailsClosed`, and move its explanation into a
  comment above `TestUpdateScimConfig_UnprovenMappingRefused`, pointing to `mapping_probe_test.go` T2/T3.
- Alternative: turn it into a real test that drives `UpdateScimConfig` with a stubbed prober returning
  a canonical-key mismatch, asserting fail-closed. Only worth it if the prober can be injected without
  touching production wiring.

Open question (recommended default in **bold**):
- Q1: Delete or implement? → **delete + comment** (the behaviour is already covered at the right layer;
  keep the ADR-025 terms verbatim in the comment).

## Files (expected)

| File | Change |
|---|---|
| `controller/graph/resolvers/idp_update_scim_config_enable_test.go` | remove the placeholder, keep the rationale as a comment |

## Acceptance Criteria

- [ ] `go test -count=1 -v ./graph/resolvers/ -run TestUpdateScimConfig` → 0 SKIP, all PASS.
- [ ] Full R2 gate: `grep -c -- '--- SKIP'` = 1 (`TestEnroll_CSRSignatureInvalid` only), together with
      Fix 02 and Fix 03.
