---
type: fix
sprint: 20
fix: 3
title: Auth integration test always skips (Valkey URL) and then fails (stale migrations)
status: planned
source_rows: [R2]
source_findings: [36]
severity: medium
component: controller (internal/auth) + CI
tags:
  - go
  - auth
  - valkey
  - regression-gate
  - ci
---

# Fix 03 — Auth integration test always skips, then fails once unblocked

> Source: live acceptance row **R2** (FAIL), Finding 36.
> `Acceptance-Test-Plan.md:17`: *"A skipped DB test is a failed acceptance case."*
> Status: **verified, not fixed.** No code changed.

## Failure (as observed)

`TestAuthIntegration_LoginBootstrapAndJWTIssue` →
`--- SKIP … valkey not available at redis://localhost:6379/15: dial tcp: lookup tcp/6379/15: unknown port`.

## Verification (2026-09-28, this re-check)

**Layer 1: the skip.** A throwaway probe test (`internal/auth/zz_probe_test.go`, deleted afterwards;
`git status` clean) called `parseValkeyAddr` directly:

| Input | Output |
|---|---|
| `redis://localhost:6379/15` | `localhost:6379/15` ← invalid dial address |
| `redis://localhost:6379` | `localhost:6379` |
| `redis://:***@localhost:6379/2` | `localhost:6379/2` ← same bug, and the password is dropped |

- `internal/auth/valkey.go:42-51` strips `redis://` and userinfo but not the `/<db>` path.
- The test default (`integration_test.go:~36`) **and** CI (`.github/workflows/ci.yml:80`
  `AUTH_TEST_VALKEY_URL: redis://localhost:6379/15`) both use `/15`, so the test skips **everywhere,
  including CI**.
- `cmd/server/main.go:980-990` has an identical copy of `parseValkeyAddr`. The production
  `VALKEY_URL=redis://localhost:6379` (`.env.example:5`) has no DB suffix, so prod works today, but any
  `/N` URL would break it the same way. `valkey.ClientOption` is never given a `SelectDB`, so the DB
  number is silently ignored even where parsing works.

**Layer 2: hidden failure behind the skip.** Re-ran the test against a **disposable** Valkey
(`docker run --rm -p 127.0.0.1:6390:6379 valkey/valkey:7.2-alpine`, stopped afterwards; the live
`ztna_valkey` was not touched, since the test runs `FlushDB`) with `AUTH_TEST_VALKEY_URL=redis://localhost:6390`:

```
integration_test.go:123: InitiateAuth: resolve connection: ERROR: column "subject_claim" does not exist (SQLSTATE 42703)
--- FAIL: TestAuthIntegration_LoginBootstrapAndJWTIssue
```

- `applyAuthMigration` (`internal/auth/integration_test.go:319-336`) applies only `001_schema.sql` and
  `031_identity_federation.sql`.
- `internal/idp/store.go:115` selects `subject_claim`, which was added by
  `migrations/034_scim_directory_sync.sql:24`. The hand-picked migration list went stale once 034 landed.

**Conclusion:** confirmed, and worse than R2 showed. The skip hides a real test failure caused by a
stale migration list.

## Goal

`TestAuthIntegration_LoginBootstrapAndJWTIssue` runs (no skip) in CI and locally, and passes.

## Design (proposal; confirm before implementing)

1. Parse Valkey URLs properly with `net/url` in **one** shared helper used by both
   `internal/auth/valkey.go` and `cmd/server/main.go`:
   - `host:port` → `InitAddress`;
   - the path `/N` → `SelectDB: N`;
   - userinfo → `Username` / `Password` (today the password is dropped silently).
2. `applyAuthMigration`: apply **all** `migrations/*.sql` in order, like
   `internal/client/device_trust_handler_test.go:55-75` `devTrustApplyMigrations`, instead of a
   hand-picked list.
3. CI ran-check (`ci.yml:119-134`): also run `./internal/auth -run TestAuthIntegration_LoginBootstrapAndJWTIssue`
   and fail on `--- SKIP`.

Open questions (recommended default in **bold**):
- Q1: Honour `/N` as SelectDB, or reject it? → **honour it** (the test relies on db 15 for FlushDB isolation).
- Q2: Share one parser or fix both copies? → **one shared helper** (the copies drifted once already).

## Files (expected)

| File | Change |
|---|---|
| `controller/internal/auth/valkey.go` | proper URL parse + SelectDB/auth |
| `controller/cmd/server/main.go` | use the shared parser |
| `controller/internal/auth/integration_test.go` | apply all migrations |
| `controller/internal/auth/valkey_test.go` (new) | table test for the parser (`/15`, userinfo, no path, bad scheme) |
| `.github/workflows/ci.yml` | ran-check covers auth |

## Acceptance Criteria

- [ ] Red first: the parser table test fails on `redis://localhost:6379/15` with current code.
- [ ] Green: parser test passes; `TestAuthIntegration_LoginBootstrapAndJWTIssue` passes with
      `AUTH_TEST_VALKEY_URL=redis://localhost:6379/15` against a disposable Valkey (never the live one).
- [ ] Controller still starts with the existing `VALKEY_URL=redis://localhost:6379`.
- [ ] CI ran-check fails if the auth integration test skips.
