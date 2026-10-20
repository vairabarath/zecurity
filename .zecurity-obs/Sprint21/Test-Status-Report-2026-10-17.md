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
| **Sprint 21 acceptance, local + live** | **PASS** for everything run | AT-CORE 1–5, AT-H 1–11, AT-U 1–8, AT-R 1–8, AT-P 1–7, AT-C.4, AT-K.1, AT-K.2, AT-K.3 (live), AT-DEV 1–3 | Full regression gate, #117 targeted tests |
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
| AT-H.7 | Valkey down → 503 `login_unavailable` | `docker stop ztna_valkey` → 503 in 1.5 s | PASS. Fix committed `cb07806`; a rate-limit bypass it introduced was fixed in `94f1196` (regression test added) |
| AT-H.9 | Argon2id PHC hash | DB prefix only | PASS |
| AT-H.10 | Google provider routes 404; CORS exact origin | curl | PASS (tenant Google still wired) |
| AT-H.11 | Single token-minting path; `amr: ["pwd"]` | Go test + token claims | PASS |
| AT-U.1–U.8 | Role matrix, add + first login, duplicates, role change, disable/enable/reset, guards, concurrency, audit/privacy | 44 scripted API checks + Go tests | PASS |
| AT-R.1 | Read role matrix | API | PASS |
| AT-R.2 | Relay liveness from Valkey: DB heartbeat backdated 4 min, API still fresh and `active` | Real local relay + DB/Valkey | PASS (live 2026-10-17) |
| AT-R.3 | Relay detail: allowlists, cert history with `is_current`, `revoked_at` after revoke, no `enrollment_token_jti` | Real local relay | PASS (live) |
| AT-R.4 | Tenant list counts match seed; status filter; detail collections; no secrets or identities | SQL-seeded test tenants | PASS (live) |
| AT-R.5 | Exactly one `tenant.read` audit row per detail call; none for list | API + DB | PASS (live) |
| AT-K.1 | Relay at 15m renews 8 m 45 s after install, then 585 s, 504 s, no loop | Real relay, `RELAY_CERT_TTL=15m` | PASS (live) |
| AT-C.4 | Role change, disable or reset by another admin → next request → Login with notice | Headed browser + API | PASS 3/3 (live) |
| AT-K.2 | Shield renews once per window: `SHIELD_CERT_TTL=20m` → window 8 m (AgentPolicy 480 s); renewals at 05:45:56, 05:57:58, 06:09:59 UTC (every 12 m) | Lab: shield on nika, connector on inkyank, `fixed-pendings` `a25bb33` (2026-10-20) | PASS (live) |
| AT-K.3 | No churn: 3 requests ↔ 3 renewals over 36 min, 0 errors, 0 restarts, no heartbeat gap | Same lab run | PASS (live; suppression paths not triggered, unit-tested) |
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
| #117 extras (S1, client renewal restart, lost renewal reply, CRL skew) | Not targeted | Local test run |
| Full regression gate (§10) | Only partial suites run for Sprint 21 | ~15 min local |

---

## 3. Evidence locations

- **Screenshots:** `/home/inkyank-01/sprint21-ui/` (01–22, live-1…4; 20–22 are AT-C.4).
- **Test scripts:** `/home/inkyank-01/sprint21-ui/scripts/` (`live_api.py`, `ui_run.js`, `ui_ratelimit.js`, `ui_c4.js`).
- **Browser tooling write-up:** `~/Desktop/sprint21-browser-automation-tools.md` and [[Sprint21/Live-Demo-Notes]] §4.
- **Sprint 20 raw logs:** were in `/tmp/s20-live`, deleted at clean-up. The run sheet keeps the evidence text.

## 4. Open hygiene

- **Repo:** `cb07806` + `94f1196` (provider login limiter) are on `test/merge-verification` only, not yet in `fixed-pendings`.
- **Live-run test data:** SQL-seeded tenants `at-r4-test` / `at-r4-suspended`, revoked relay `k1-relay` (`.localdev/k1-relay`), disabled operator `c4-op-112712@example.com`.
- **Sprint 20 Finding 32 still reproduces** on current code: a revoked relay keeps reconnecting its heartbeat.
- **Local leftovers:** `.worktrees/s20-live`, `.localdev/s20-relay1`, 30 leaked `resolvers_updsc_*` test databases.
- **Needs sudo:** nft table `s20test`.
- **Lab hosts:** update timers, test binaries, resource `s20-nika-summa`.
- **Local DB:** throwaway operators `at-op*` and `ui-op*`, two of them active super-admins.

## 5. Security fix found during this session

- **What:** the first AT-H.7 fix (`cb07806`) shared one 2 s timeout across the limiter check, the password check and the failure record. A login queued behind the Argon2id cap (more than 4 at once) therefore got **503 without the wrong password being counted**, while a correct password still got 200. That gave unlimited guesses with a 503/200 answer, bypassing the 6-per-email lockout.
- **Found by:** an automated security review of the commit, then confirmed with a new test that failed on `cb07806`.
- **Fix:** `94f1196`. `checkLimiter` and `recordFailure` each bound their own Valkey step; `recordFailure` is detached from request cancellation; `ChangePassword` is covered too.
- **Test:** `TestLogin_SlowAuthenticateStillCountsFailure`. `internal/provider` + `internal/middleware`: 105 PASS, 0 SKIP.
- **Recorded in:** [[Sprint21/Member2-Go-Rust/Phase1-Provider-Identity-Foundation]] → Post-Phase Fixes, and [[Sprint21/path.md]] → Post-Sprint Fixes.

## 6. Commits (branch `test/merge-verification`, **not pushed**)

| Commit | What |
|---|---|
| `cb07806` | fix(provider): bound login rate-limiter calls so a dead Valkey answers 503 (AT-H.7) |
| `1eedd7c` | docs(acceptance): Sprint 20 close-out and Sprint 21 local acceptance results |
| `94f1196` | fix(provider): give each login rate-limiter step its own time budget (security fix, §5) |
| `19474e8` | docs(sprint21): live AT-K.1, AT-R.2–R.5 and AT-C.4 results |

The two `fix(provider)` commits must reach `fixed-pendings`; tell Barath.

## 7. State of the local dev stack (end of session)

- **Controller:** `94f1196`, built to the session scratchpad, running from `controller/` with the normal `.env` settings (the `RELAY_CERT_TTL=15m` run is over); :8080 / :9090.
- **Provider console:** dev server on :5174 (restarted after `npm ci`).
- **Postgres and Valkey:** Docker `ztna_postgres`, `ztna_valkey`. Valkey was stopped and restarted once for AT-H.7, which cleared the login-limiter counters.
- **Test relay `k1-relay`:** stopped and revoked.

## 8. Still to do (everything open, in one list)

**Sprint 21:**
1. ~~AT-K.2 / K.3 live (shield)~~: **done 2026-10-20**, PASS (see §2.2 and §9).
2. **Full regression gate (plan §10)**, about 20 min locally: all Rust suites (`connector`, `relay`, `shield`, `client`), the `admin` build and `buf generate`. Only the provider Go suites, the full Go suite and the Rust `renew` filters were run.
3. **#117 fixes, targeted tests:** S1 relay revocation, client tunnel restarting onto a renewed cert, connector re-asked to renew after a lost reply, CRL clock skew.

**Sprint 20 (to turn FAILED into passed):**

4. **R2:** Barath fixes Fix02 (Finding 35: 3 extra `NotifyPolicyChange` calls, 1 outdated test), then re-run with `RESOURCE_TEST_SHIELD_ID` (`scripts/resource-test-fixture.sh`). CI should also set it, or the tests stay hidden.
5. **G9 / AT-G.6:** a team decision on whether the 2026-10-10 long-hold PASS on later code is accepted, or a re-run.

**Security (lab):**

6. **F33 shield revocation (bug 3):** live end-to-end check: revoke a real shield mid-session; sessions end and it can't reconnect.
7. **F34 connector revocation:** still at Phase A, not fail-closed. Re-test after Barath's Phase B1.
8. **Finding 32:** a revoked relay keeps reconnecting its heartbeat (re-confirmed today); low, needs a fix ticket.

**Other:**

9. **Fix 05:** re-test F-1/F-2 and decide whether Barath takes them.
10. **Send Barath:** F35 still failing on `fixed-pendings` (hidden by CI skips); F34 status; the two `fix(provider)` commits.

**Clean-up:**

11. **Local DB:**
    - test tenants `at-r4-test` / `at-r4-suspended` (SQL-seeded) and their rows;
    - revoked relay `k1-relay`;
    - throwaway operators `at-op*`, `ui-op*`, `c4-op-*` (`at-op2-152625` and `at-op2-152709` are **active super-admins**);
    - 30 leaked `resolvers_updsc_*` databases.
12. **Files:** `.localdev/k1-relay`, `.localdev/s20-relay1`, `.worktrees/s20-live` (`git worktree remove`), `orca.deb` (untracked).
13. **Needs sudo:** `sudo nft list tables`, confirm `s20test` is gone; check why `zecurity-shield` shows `activating`.
14. **Lab hosts:** update timers, test binaries, resource `s20-nika-summa` / group `s20-summa-users`.
15. **Optional:** re-enable Okta IdP `1779ca3e` if it exists in any non-local DB.

## 9. Live lab run — AT-K.2 / AT-K.3 (2026-10-20)

- **Code:** `origin/fixed-pendings` `a25bb33` (PR #128, enrollment-token binding), built in worktree `.worktrees/fp-live`. The local `fixed-pendings` branch was fast-forwarded to it at the user's request.
- **Controller:** user unit `k2-controller`, `SHIELD_CERT_TTL=20m` (user asked for 15–20 min instead of 24h). The admin UI ran as unit `k2-admin`.
- **Workspace:** `xiyo` (409e57e0…), created by the user's Google sign-in. Network `k2-net` (95f2a823…). Connector `k2-conn` (9c1078f5…) on **inkyank 192.168.1.75**; shield `k2-shield` (6e90ae91…) on **nika 192.168.1.39**.
- **Binaries:** connector `57cd82ef2fe6`, shield `16804d600202`; update timers disabled on both hosts.
- **Tokens:** created through the admin session and stored in 0600 files; shipped with tty echo off and verified by sha prefix. They were never printed, and the agents removed them from their config after enrolling.
- **Results:** see the AT-K.2 / AT-K.3 rows in §2.2 and the checklist.
- **Lab notes:**
  - **IPs changed again (DHCP):** nika .39, udaya .37, manojinkyank .49, inkyank .75.
  - **The manojinkyank SSH session dropped mid-run.** One command then reached this PC's local shell. zsh rejected it at the first word, so nothing ran (no `~/k2` was created locally). The helper now quotes its markers and refuses to send when a terminal is back at the local prompt.
  - **udaya's old connector** is in an `activating` restart loop against the reset DB.
- **Still running after the test:** controller (20m shield TTL), admin UI, connector on inkyank, shield on nika (renewing every 12 min). The LAN file server `k2-serve` stops after 30 min.
