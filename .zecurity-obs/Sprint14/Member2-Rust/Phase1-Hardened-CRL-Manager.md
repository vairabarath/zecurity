---
type: phase
member: M2
sprint: 14
phase: 1
title: Hardened CRL Manager
status: done
depends_on: []
tags: [rust, crl, pki, security, revocation, pending-02]
---

# Phase 1 — Hardened CRL Manager

> Depends on nothing — Day 1. **Blocking** for all consumers (M2-F). Also fixes a **pre-existing
> security bug**: the current `connector/src/crl.rs` verifies nothing.

## Goal

Build a CRL client that **verifies** what it downloads before trusting it, then refactor the existing
connector CRL onto it. Today `connector/src/crl.rs:36-56` parses the CRL and reads serials but does
**not** verify the signature, issuer, or validity dates — and it fetches over plaintext HTTP
(`connector/src/main.rs:156`). A network attacker could serve a forged/empty CRL and un-revoke
everyone.

## Files

| File | Change |
|------|--------|
| new hardened CRL module (e.g. `connector/src/crl.rs` rewritten, or a shared helper) | verifying refresh + cache |
| `connector/src/crl.rs` | refactor onto the hardened logic |
| `connector/src/main.rs` | 60s + jitter refresh interval (was 300s) |

## Required verification (all four, on every refresh)
1. **Signature** — verify the CRL is signed by the expected issuer CA public key.
2. **Issuer / AKI** — matches the expected CA.
3. **`thisUpdate` ≤ now.**
4. **now < `nextUpdate`** — **deny (treat as no valid CRL) once past `nextUpdate`.**

Behavior:
- On any verification failure → **reject the fetched CRL, keep the last-good cache** (never replace a
  good CRL with a bad one; never un-revoke on a blip).
- **Cold-boot fail-closed:** if no valid CRL has ever been loaded, the consumer must **deny** (this is
  enforced at the call sites in M2-F; the manager exposes state clearly, e.g. `has_valid_cache()`).
- Refresh every **60s + jitter**.

## API sketch
- `is_revoked(serial: &[u8]) -> bool` — as today, but only meaningful when `has_valid_cache()`.
- `has_valid_cache() -> bool` — true only if a signature/issuer/date-verified, unexpired CRL is held.
- `refresh(url, issuer_ca) -> Result<()>` — fetch + verify(1-4) + atomic swap; error keeps last-good.
- `spawn_refresh(url, issuer_ca, interval_secs, jitter)`.

## Notes
- Reuse the crate already in the tree for CRL parsing (`x509-parser`); add explicit
  signature-verification (it is **not** done by `parse_x509_crl` alone).
- Keep the manager generic over the issuer CA so both the **workspace CRL** (existing connector use,
  Workspace CA) and the **relay CRL** (M2-F, Intermediate CA) can use it.

## Tests
- Rejects: wrong-signature CRL, wrong-issuer CRL, `nextUpdate` in the past, `thisUpdate` in the future.
- Keeps last-good on a failed refresh.
- `has_valid_cache()` false before first successful verified fetch.

## Build Check
```bash
cd connector && cargo build
```

## Implementation Checklist
- [x] **M2-E1** hardened manager: verify signature + issuer + thisUpdate + nextUpdate; deny past nextUpdate; keep-last-good; expose `has_valid_cache()`.
- [x] **M2-E2** refactor `connector/src/crl.rs` onto it; 60s + jitter in `main.rs`.
- [x] **Build gate:** `cd connector && cargo build`

## Pre-Implementation Corrections (validated review — codex)
- **Client has no CRL module + version skew (must-fix).** `connector/src/crl.rs` exists only in the
  connector crate; **the client has no CRL module**, and the `x509-parser` versions differ
  (`connector = 0.18`, `client = 0.16`). A single shared crate needs the versions reconciled — or ship
  a **client-specific manager**. Add the client's CRL dependencies and lifecycle wiring (spawn refresh
  from `client/src/daemon.rs:1336`). Decide shared-crate-vs-per-crate during implementation; don't
  assume a drop-in shared module.

## Post-Phase Fixes

### Fix: Verified DER installation seam for integration tests
**Issue:** Relay probe integration tests needed a valid, signed CRL cache after the hardened manager
made cold-boot state fail closed, but the only public loading path required an HTTP server.

**Fix Applied:** `connector/src/crl.rs` now exposes `install_verified_der()`, which runs the same
signature, issuer, AKI/SKI, and validity checks as HTTP refresh before atomically replacing the
cache. Invalid DER still preserves the last-good cache.
