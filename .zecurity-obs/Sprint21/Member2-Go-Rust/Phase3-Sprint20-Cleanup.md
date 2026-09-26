---
type: phase
member: M2
person: Barath
sprint: 21
phase: 3
execution: K
title: Sprint 20 Cleanup (KI-1, KI-2, KI-3, .env.example JWTs)
status: planned
depends_on: [1]   # after H (both edit controller/.env.example); scheduled after U
schema_change: false
tags:
  - rust
  - go
  - relay
  - connector
  - shield
  - hygiene
  - provider-dashboard
---

# Phase 3 (K) — Sprint 20 Cleanup

> **Source:** `.zecurity-obs/Sprint20/path.md` → **Known Issues** (KI-1 … KI-3), and runbook §2 (`docs/sprint20-live-acceptance-runbook.md`).
> **Ordering:** Barath's third phase, after H and U. It depends only on H, because both edit `controller/.env.example`. K1 and K2 don't depend on H or U and can be picked up whenever there's a gap. **If the sprint runs short, K2 (KI-2) is the item that may move to Sprint 22.** It's low-risk and unrelated to the console.

## Problem (verified)

### KI-1 — Relay short-TTL renewal loop

- **Cause:** the controller issues relay certificates with `NotBefore = now − 1h` (`controller/internal/pki/relay.go:61`).
  - The relay scheduler computes `lifetime = not_after − not_before` and renews when 2/5 of that remains (`relay/src/renewal.rs` `plan()`, `cert_manager.rs:43,174`).
  - So `renew_at = issue + 0.6·TTL − 24 min`.
- **Effect:** at `RELAY_CERT_TTL` ≤ 40 min the relay renews immediately after every install, in a continuous `RenewCert` loop.
  - The 30-day default is unaffected.
  - The unit test for the "~9 min at 15m" case assumes no backdate.
- This is the same class of bug as Sprint 20 PF-1 (controller rotator).

### KI-2 — Shield renewal window not wired; shields renew every ~15 s at TTL ≤ 48h

- **The connector's window is hard-coded:** it decides a shield needs renewal with `DEFAULT_RENEWAL_WINDOW_SECS = 48h` (`connector/src/agent_server.rs:33`, `cert_needs_renewal`).
- **The controller's value is unused:** the controller parses `SHIELD_RENEWAL_WINDOW` (`cmd/server/main.go:202` → `internal/shield/config.go:25`), but nothing uses it.
- **No throttle on the connector:** it sends `ReEnroll` on **every** shield health report (15 s, `shield/src/control_stream.rs:28`) while the shield is inside the window.
- **No debounce on the shield.**
- **Effect:** with `SHIELD_CERT_TTL` ≤ 48h, the shield renews and reconnects its control stream about every 15 s.

### KI-3 — Dev provider redirect URI

- `controller/.env.example` sets `PROVIDER_GOOGLE_REDIRECT_URI=http://localhost:8080/auth/callback`. That is the **tenant** callback, so provider login never yields a token.
- **Superseded by D-24 (amendment 2026-09-26):** provider login is now local (email + password), and Phase H **removes** the provider Google OAuth path and the `PROVIDER_GOOGLE_REDIRECT_URI` variable. There's no redirect URI left to fix. K3 only confirms the variable is gone from `.env.example` and closes KI-3.

### Committed JWTs in `.env.example`

- Leftover `sudo … ENROLLMENT_TOKEN=eyJ…` install commands, with real-looking enrollment JWTs and LAN IPs, are committed in `controller/.env.example`, around lines 70–85.
- These are dev enrollment tokens (24 h TTL), long expired, so no rotation is needed. They're still a hygiene and secret-scanning problem.

### Minor

- `connector/src/crl.rs:30` says CRLs refresh "every 5 minutes". The real cadence is 60 s + 0–15 s jitter (`main.rs:193-206`).

## Goal

- Relay renewal schedules correctly for any TTL.
- Shield renewal follows the configured window and never churns.
- The dev env template works for provider login and carries no tokens.

## Files

| File | Change |
|------|--------|
| `relay/src/renewal.rs`, `relay/src/cert_manager.rs` (K1, option per decision) | Correct lifetime basis; tests |
| `controller/internal/pki/relay.go` (K1 **option B only**) | Backdate change |
| `connector/src/agent_server.rs` (K2) | Configurable window; per-shield `ReEnroll` throttle |
| `shield/src/control_stream.rs` / `renewal.rs` (K2) | Renewal debounce |
| `proto/connector/v1/connector.proto`, `controller/internal/connector/*` (K2 **option A only**) | Additive config message |
| `controller/.env.example` (K3, K4) | Confirm `PROVIDER_GOOGLE_REDIRECT_URI` is gone (removed by H); token scrub |
| `.github/workflows/ci.yml` (K4) | JWT grep guard |
| `connector/src/crl.rs` (K5) | Comment |

## Steps

### K1 — KI-1 relay renewal scheduling

Choose at phase start and record the choice here.

- **Option A (recommended):** the relay measures the lifetime from **when it installed the certificate**, not from `NotBefore`.
  - At install (provisioning or renewal), persist `issued_at_unix` next to the certificate, e.g. `relay.crt.meta` JSON written atomically with `write_atomic`.
  - `plan()` uses `start = max(not_before, issued_at)` and `lifetime = not_after − start`. This mirrors PF-1's `issuedAt`.
  - For a certificate without metadata (existing relays), fall back to `not_before`, so current behaviour is unchanged.
- **Option B:** the controller reduces the relay-certificate `NotBefore` backdate, e.g. to 5 min.
  - One line, but it changes PKI behaviour for every relay and narrows clock-skew tolerance, so it needs care.
- **Tests:**
  - `plan()` for a backdated 15m cert renews at about 9 min (± jitter), not immediately;
  - a 1h cert renews at about 36 min;
  - the 30 d default is unchanged;
  - the metadata round-trips;
  - a missing-metadata fallback.

### K2 — KI-2 shield renewal window wiring

Choose the wiring at phase start.

- **Option A (recommended; "wire the window"):** the controller tells the connector.
  - Add a new controller→connector message, e.g. `AgentPolicy { uint64 shield_renewal_window_secs = 1; }`, as the **next free** field number in the `ConnectorControlMessage.body` oneof.
  - **Additive only; never renumber.** Field 16 is reserved.
  - The controller sends it when the control stream is established, from `shield.Config.RenewalWindow`.
  - The connector stores it (default 48h until received) and uses it in `cert_needs_renewal`.
  - Run `buf generate`.
- **Option B:** a connector env var `SHIELD_RENEWAL_WINDOW` (duration), and delete the controller's unused `SHIELD_RENEWAL_WINDOW`. Simpler, but configured in two places.
- **Option C:** the connector derives the window as 2/5 of the shield certificate's lifetime (same ratio as relay and client), and the controller's unused var is removed. No config.
- **Whichever option is chosen, also add:**
  - **Connector throttle:** at most one `ReEnroll` per shield per stream every 10 min, the same as G-1's `reEnrollResendInterval`. Record the time only on a successful send.
  - **Shield debounce:** ignore a `ReEnroll` within 60 s of a successful renewal, the same as the connector's `REENROLL_DEBOUNCE`.
- **Tests:**
  - the window is respected (inside → one `ReEnroll`; outside → none);
  - the throttle suppresses duplicates, then re-asks after the interval;
  - the shield debounce ignores a duplicate;
  - (option A) the proto message round-trips, and a default applies until received.

### K3 — KI-3 closed by Phase H

- Phase H deletes the provider Google OAuth routes and stops reading `PROVIDER_GOOGLE_REDIRECT_URI` (D-24).
- K3 confirms `controller/.env.example` has no `PROVIDER_GOOGLE_REDIRECT_URI` line (H removes it) and records KI-3 as **resolved by removal** in `.zecurity-obs/Sprint20/path.md` Known Issues.
- The tenant `GOOGLE_REDIRECT_URI` stays as it is.

### K4 — Remove committed JWTs

- Delete every install command containing `ENROLLMENT_TOKEN=eyJ…` (and any other `eyJ` JWT) from `controller/.env.example`. Replace them with placeholders: `ENROLLMENT_TOKEN=<paste token from admin UI>`.
- Keep the placeholders for the Phase H variables (`PROVIDER_JWT_SECRET=<32+ random bytes, different from JWT_SECRET>`, etc.).
- **CI guard:** add a step that fails if `grep -nE 'eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.' controller/.env.example` matches, so the scrub can't regress.
- **Don't rewrite git history.** The tokens are expired dev enrollment tokens; note this in the PR.
- `controller/.env` is local and uncommitted: remind the team to check their own copy. Its plaintext SMTP password is out of scope for the repo.

### K5 — Minor

Fix the `crl.rs:30` comment to "every 60 s (+0–15 s jitter)".

## Invariants

1. Relay renewal remains same-key (D-19), and expired certificates are never renewed (D-20).
2. Existing relay certificates without K1 metadata keep today's schedule. There's no forced renewal on upgrade.
3. A shield inside the window still renews. The throttle and debounce only remove duplicates.
4. Proto changes are additive only; `buf generate` diff is limited to the new message and field.
5. `.env.example` keeps every variable the controller requires (`mustEnv`); only token values change.

## Tests / Build Check

```bash
cd relay && cargo build && cargo test
cd connector && cargo build && cargo test
cargo build --manifest-path shield/Cargo.toml && cargo test --manifest-path shield/Cargo.toml
buf generate && git diff --stat proto/ controller/gen/      # option A only: additive
cd controller && go build ./... && go test ./internal/connector/... ./internal/shield/...
grep -nE 'eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.' controller/.env.example && exit 1 || true
```

## Acceptance Criteria

- [ ] AT-K.1 … AT-K.6 in [[Sprint21/Acceptance-Test-Plan]] pass.
- [ ] Live (optional; it strengthens AT-K.1): `RELAY_CERT_TTL=15m` renews at about 9 min with no loop, and `SHIELD_CERT_TTL=24h` no longer churns.

## Implementation Checklist

- [ ] K1 option chosen and recorded; implemented; tests
- [ ] K2 option chosen and recorded; window + throttle + debounce; tests
- [ ] K3 KI-3 confirmed resolved by H (no `PROVIDER_GOOGLE_REDIRECT_URI` in `.env.example`)
- [ ] K4 JWTs removed; CI guard
- [ ] K5 comment
- [ ] Update `.zecurity-obs/Sprint20/path.md` Known Issues: mark KI-1…KI-3 fixed, with a link to this phase
- [ ] Update runbook §2 notes if the recommended env values change (KI-1 → 15m becomes valid)

## Post-Phase Fixes

_None yet._
