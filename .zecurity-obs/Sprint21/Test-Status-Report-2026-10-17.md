---
type: test-report
sprint: 21
date: 2026-10-17
operator: Sathiya (M1), runs driven by Claude Code
status: sprint20-failed · sprint21-local-passed-lab-pending
tags: [sprint20, sprint21, acceptance, test-report, provider-console, live]
---

# Test Status Report — 2026-10-17

One place for every test **completed** so far, with the verdict and where the evidence lives.

**Detailed records:**
- Sprint 20: [[Sprint20/Live-Acceptance-Run-Sheet]] (rows, findings, close-out) and [[Sprint20/path.md]].
- Sprint 21: [[Sprint21/Acceptance-Run-Checklist]] (per-case evidence), [[Sprint21/Live-Demo-Notes]] (browser tooling).

## Summary

| Area | Verdict | Completed | Not done |
|---|---|---|---|
| **Sprint 20 live acceptance** (Phase V) | **FAILED** | 45 rows: 40 PASS, 1 PASS only under a TEMP patch, 3 FAIL, 1 not exercised | — (run complete, closed out) |
| **Sprint 21 acceptance, local** | **PASS** for everything runnable locally | AT-CORE 1–5, AT-H 1–11, AT-U 1–8, AT-R 1/6/7/8, AT-P 1–7, AT-DEV 1–3 | AT-R 2–5 (no relay or tenant locally), AT-K live, AT-C.4 browser notice |
| **Regression gate on current code** | **Not green** | Go full suite; Rust renewal suites; console | 4 tests fail when not skipped (F35) |

---

## 1. Sprint 20 — live acceptance (Phase V)

- **When / where:** 2026-09-26 and 2026-09-28. Lab hosts:
  - controller on this PC (192.168.1.164);
  - relay on udaya;
  - connector on manojinkyank;
  - shield and resource on nika.
- **Commit:** `5a4f940`, code-identical to the Phase V pin `46ab87d` (only 2 docs commits differ; code-dir diff empty, checked 2026-10-17).
- **Env:** `CONNECTOR_CERT_TTL=15m`, `CONNECTOR_RENEWAL_WINDOW=10m`, `RELAY_CERT_TTL=1h` (KI-1), `SHIELD_CERT_TTL=24h` → 168h from 17:16 on 09-26 (KI-2).
- **Closed out:** 2026-10-17.

### 1.1 Results by stage

| Stage | Phase / cases | Rows | Result |
|---|---|---|---|
| Setup | provider redirect, controller, token, relays | S1–S4 | PASS |
| 1 | C: relay SAN allowlist (AT-C.2, C.5) | C1–C3 | PASS |
| 2 | Bring-up: relay, connector, shield, client | U1, U3, U4 | PASS |
| 2 | Connector registers with relay | U2 | PASS **only under TEMP private-address patch** (Findings 4, 5, 22) |
| 2 | Long-lived tunnel | U5 | Tunnel open PASS; **long hold FAIL** (Finding 15) |
| 3 | E: controller gRPC cert rotation (AT-E.1, E.3) | E1–E4 | PASS (6 rotations, 10-min cadence, 0 failures) |
| 3 | G: connector renewal (AT-G.4, G.5) | G1–G3, G5–G8 | PASS (renewal every 5 min; direct, relayed and shield paths after original `NotAfter`). G8 on pre-17:16 evidence |
| 3 | G: duplicate ReEnroll ignored (AT-G.1) | G4 | **Not exercised** (no duplicate occurred; unit-tested) |
| 3 | G: established tunnel survives swap (AT-G.6) | G9 | **FAIL** (Finding 15) |
| 3 | A: relay steady state (AT-CORE-2) | A1–A2 | PASS |
| 3 | F: relay in-band renewal (AT-F.1, F.8 at 1h) | F1–F4 | PASS (16 renewals, same key, 2 h 30 m past first `NotAfter`) |
| 4 | A: relay stop → inactive ≤ 150 s, restart → active (AT-A.2, A.3) | A3–A5 | PASS |
| 5 | F: revoke + CRL (AT-F.6, F.7) | F5–F7 | PASS |
| 6 | D: disconnect watcher (AT-D.1) | D1–D2 | PASS (with `kill -STOP`; runbook's `kill -9` doesn't reach the watcher, Finding 30) |
| 7 | B: revocation terminal (AT-B.1, B.2, B.8) | B1–B3 | PASS (DB state + refused reconnects) |
| 8 | `buf generate` | R1 | PASS |
| 8 | Controller `go build/vet/test` | R2 | **FAIL** (4 `graph/resolvers` tests, Finding 35) |
| 8 | Rust connector, relay, shield, client | R3 | PASS |

Plan cases that can't be triggered live (A.1, A.4–A.6, B.3–B.7, B.9, C.1, C.3, C.4, C.6, C.7, D.2–D.4, E.2, E.4, F.2–F.5, F.9, G.2, G.3, G.7) are covered by the Go/Rust test suites in the regression gate.

### 1.2 Re-checks on current code (2026-10-17, `1c164c7`, contains `origin/fixed-pendings`)

| Check | Result |
|---|---|
| `go build ./...`, `go vet ./...` | PASS |
| `go test ./...` with CI env | exit 0: **1161 PASS, 0 FAIL, 32 SKIP**. 30 of the skips need `RESOURCE_TEST_SHIELD_ID` |
| Same 30 tests with a seeded shield (throwaway DB `ztna_r2_gate`, dropped afterwards) | 110 PASS, **4 FAIL**, 1 SKIP: the same 4 as R2 (Finding 35). Fix02 still `planned` |
| `TestAuthIntegration_LoginBootstrapAndJWTIssue` (Finding 36) | **PASS**, fixed in `411e216` (Sprint 21 K6) |

### 1.3 Findings status (medium and higher)

| Finding | Severity | Status 2026-10-17 |
|---|---|---|
| F34: a revoked connector keeps serving tunnels | **High (security)** | **Open.** Connector revocation is at Phase A only, not fail-closed |
| F33: a revoked shield still carried traffic | **High (security)** | Fixed in code (bug 3, PR #123). **Live check pending** |
| F15: client drops long-lived tunnels on transport change | High | Fix01 `partial`; live long hold PASS on later code 2026-10-10 |
| F28: client never passes the TCP close (FIN) | High | Tracked as Fix 05 Phase 5-A |
| F7/18/23/31: update timers replace test binaries | High (test validity) | No fix recorded |
| F35: 4 ACL-coherence tests fail, hidden by CI skip | Medium | **Still failing**; Fix02 planned |
| F36: auth integration test always skipped | Medium | **Fixed** `411e216` |
| F4/5/22: private-address relays not pushed; selector stuck in backoff | Medium | No fix found |
| F19/20/24/25: admin cascade, orphaned resource, host not editable, rules deleted | Medium | No fix recorded |
| F30: runbook `kill -9` misses the watcher | Medium (runbook) | Runbook §11 still says `pkill -9` |
| F32: a revoked relay retries every 5 s | Low | No fix found |

**What it takes to pass Sprint 20:**
1. Fix02 (F35), then re-run R2 with `RESOURCE_TEST_SHIELD_ID` (`scripts/resource-test-fixture.sh`).
2. A team decision on whether the 2026-10-10 long-hold PASS on later code is acceptable for G9 / AT-G.6.

---

## 2. Sprint 21 — acceptance on the local stack (2026-10-17)

- **Branch:** `test/merge-verification` @ `1c164c7`. It contains 38e38c6 (#117) and everything on `origin/fixed-pendings`.
- **Stack:** controller :8080/:9090, provider console :5174, Postgres and Valkey in Docker.
- **Accounts:** mutating and negative tests used throwaway operators only. The real super-admin was used just to sign in and out.

### 2.1 Automated suites

| Suite | Result |
|---|---|
| Go `internal/provider`, `internal/providerquery`, `internal/middleware` (DB-backed, CI env) | **175 PASS, 0 SKIP, 0 FAIL** |
| Provider console `npm ci`, `lint`, `test`, `build` | All pass; **374 tests** (incl. `api-surface.test.ts`) |
| Relay `cargo test renew` | 22 PASS (incl. 15m → ~9 min, 1h → ~36 min, no loop) |
| Connector `cargo test renew` / window + ReEnroll | 28 PASS / 8 PASS |
| Shield `cargo test renew` | 3 PASS |

### 2.2 Live checks

| ID | What was checked | How | Result |
|---|---|---|---|
| AT-CORE-1 | Tenant JWT → 401 on every `/provider/*` route; provider token positive control | curl (24/24 negatives; 8 routes) + Go token tests | PASS |
| AT-CORE-2 | Logout, password change and reset end every session | API + browser sign-out | PASS |
| AT-CORE-3 | Console writes only the allowed endpoints | Browser network capture, per role | PASS |
| AT-CORE-4 | A super-admin always remains | Live self-guards + Go concurrency tests | PASS |
| AT-CORE-5 | No password leaks (logs, audit, GET bodies, storage) | grep of 13 secrets → 0 hits | PASS |
| AT-H.1 | Bad `PROVIDER_JWT_SECRET` → startup refused | 3 cases (unset, short, equal) | PASS |
| AT-H.2–H.6, H.8 | Bootstrap, forced change, no enumeration, rate limit, revocation | API (+ earlier session) | PASS |
| AT-H.7 | Valkey down → 503 `login_unavailable` | `docker stop ztna_valkey` → 503 in 1.5 s | PASS (relies on the **uncommitted** `loginLimiterTimeout` fix in `auth_handlers.go`) |
| AT-H.9 | Argon2id PHC hash | DB prefix only | PASS |
| AT-H.10 | Google provider routes 404; CORS exact origin | curl | PASS (tenant Google still wired) |
| AT-H.11 | Single token-minting path; `amr: ["pwd"]` | Go test + token claims | PASS |
| AT-U.1–U.8 | Role matrix, add + first login, duplicates, role change, disable/enable/reset, guards, concurrency, audit/privacy | 44 scripted API checks + Go tests | PASS |
| AT-R.1 | Read role matrix | API | PASS |
| AT-R.6 | Audit filters, keyset pagination (78 rows, 12 pages, no gaps), limit clamp | API | PASS |
| AT-R.7 | Controller cert matches `openssl s_client :9090` | API + openssl | PASS |
| AT-R.8 | Bad cursor, limit or window → 400; unknown id → 404 | API | PASS |
| AT-P.1 | Console builds and tests | npm | PASS |
| AT-P.2 | Login: Home shows email, role, sign-in time, expiry; wrong password → generic message; 6th failure → 429 "Try again in 15 minutes" | Headed browser | PASS |
| AT-P.3 | Forced change: other pages bounce to `/change-password` | Headed browser | PASS |
| AT-P.4 | Read pages have no mutating controls | Headed browser | PASS |
| AT-P.5 | relay-ops sees only Relays; other pages → Forbidden | Headed browser | PASS |
| AT-P.6 | Provider users: add (temp password shown once), duplicate 409 message, role change, disable, enable, reset, all with confirmation; own row has no actions | Headed browser | PASS |
| AT-P.7 | No `/graphql` or tenant `/auth/*` calls; no secrets in URLs; `localStorage` empty | Network capture | PASS |
| AT-DEV.1–3 | DB docs, `.env.example` clean, console builds on :5174 | Files + build | PASS |

### 2.3 Partial or not done (Sprint 21)

| ID | Why | Needs |
|---|---|---|
| AT-R.2–R.5 | Local DB has 0 relays and 0 tenants; Go tests pass | A relay and a tenant workspace |
| AT-K.1, K.2 (live) | Unit tests pass; live run not done | Relay + shield running (sudo, resource host) |
| AT-C.4 | Browser notice after another admin ends your session not driven (API side passes) | ~5 min local |
| #117 extras (S1, client renewal restart, lost renewal reply, CRL skew) | Not targeted | Local test run |
| Full regression gate (§10) | Only partial suites run for Sprint 21 | ~15 min local |

---

## 3. Evidence locations

- **Screenshots:** `/home/inkyank-01/sprint21-ui/` (01–19, live-1…4).
- **Test scripts:** `/home/inkyank-01/sprint21-ui/scripts/` (`live_api.py`, `ui_run.js`, `ui_ratelimit.js`).
- **Browser tooling write-up:** `~/Desktop/sprint21-browser-automation-tools.md` and [[Sprint21/Live-Demo-Notes]] §4.
- **Sprint 20 raw logs:** were in `/tmp/s20-live`, deleted at clean-up. The run sheet keeps the evidence text.

## 4. Open hygiene

- **Repo:** uncommitted `loginLimiterTimeout` fix in `controller/internal/provider/auth_handlers.go`.
- **Local leftovers:** `.worktrees/s20-live`, `.localdev/s20-relay1`, 30 leaked `resolvers_updsc_*` test databases.
- **Needs sudo:** nft table `s20test`.
- **Lab hosts:** update timers, test binaries, resource `s20-nika-summa`.
- **Local DB:** throwaway operators `at-op*` and `ui-op*`, two of them active super-admins.
