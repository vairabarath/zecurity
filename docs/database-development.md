# Database Development Guide

> **Status:** Pre-production rule, in force since Sprint 21 (2026-09-28).
> **Applies to:** every developer and AI agent working on the controller's Postgres schema and DB-backed tests.
> **Source:** Sprint 21 planning decision (`.zecurity-obs/Sprint21/path.md` → "Development rule"), DEV-1.

Zecurity is **pre-production**. For now the team has chosen the simplest schema workflow that is safe: **when the schema changes, recreate the local database.** There is **no migration framework**. This guide explains how the local database is built, what to do when the schema changes, and how tests get their schema.

---

## 1. How the local database is built

- The controller's Postgres runs in Docker (`controller/docker-compose.yml`, container `ztna_postgres`, database `ztna_platform`).
- `controller/migrations/` is mounted into the container as `/docker-entrypoint-initdb.d`.
- **Postgres runs every `*.sql` file in that directory, once, in lexical order, only when its data volume (`ztna_pgdata`) is first created.**
- After that first start, **nothing runs those files again**.
  - Nothing records which files have run (there is no `schema_migrations` table).
  - **Nothing upgrades an existing database.** A new file added later is simply ignored by a database that already exists.

> The directory is called `migrations/` for historical reasons. The files are plain SQL schema files, applied from scratch. There is no migration tool behind them.

## 2. The rule: schema change → recreate the local database

**After pulling or writing any change under `controller/migrations/`:**

```bash
cd controller
docker compose down -v      # stops the containers AND deletes the data volume
docker compose up -d        # recreates Postgres; every schema file runs again from scratch
```

- **Local data is disposable** during this phase. `down -v` deletes everything in the local database: tenants, users, connectors, provider accounts.
- The `-v` matters. Without it, the old volume survives and the new schema file is **not** applied.
- **Symptom of forgetting:** errors like `column "…" does not exist` or `relation "…" does not exist` for something a recent PR added.
- Valkey (`ztna_valkey`) holds only caches and rate-limit counters. Recreating it is harmless but usually unnecessary.

## 3. After a reset: getting back to a working setup

1. **Provider operator.** The first provider super-admin is created by the **create-only bootstrap** (Decision Record D-26). Set these in `controller/.env`, then start the controller:
   ```
   PROVIDER_JWT_SECRET=<32+ random bytes, different from JWT_SECRET>
   PROVIDER_BOOTSTRAP_EMAIL=you@inkyank.com
   PROVIDER_BOOTSTRAP_PASSWORD=<temporary, 12–128 characters>
   ```
   - The first login returns a password-change-only token, so change the password (`POST /provider/auth/password`, or the provider console).
   - Then **remove `PROVIDER_BOOTSTRAP_PASSWORD`** from the environment. Once any super-admin exists, the bootstrap variables are ignored.
2. **Tenant.** Sign up a fresh workspace through the admin UI. Re-enroll connectors, shields and clients against it as needed.

## 4. Adding a schema change

1. **Add a new file.** Use the next free number (check with `ls controller/migrations | tail -3`). The latest is `037_provider_local_auth.sql`, so the next is **`038_`**. Name it for what it does: `038_<short_description>.sql`.
2. **Never edit an existing schema file.** Other developers' databases and every test database are built from those files. Changing one means everyone's history silently diverges. Always add a new file, even to fix a mistake in an old one.
3. **Keep it plain SQL** that runs correctly on a fresh database, after all the earlier files, in one pass. Comment the Sprint/phase and the reason at the top.
4. **Label the PR `[schema: reset DB]`** (title or description), and put the reset command in the PR description, so reviewers and the other developer know to recreate their database after merging.
5. **Test it on a fresh database** before opening the PR. Either run `docker compose down -v && docker compose up -d`, or apply every file in order to a throwaway database; the DB-backed Go tests do this automatically (section 5).

**Existing duplicate prefixes:** `016_`, `031_` and `034_` each have two files. They apply in lexical order, which is the order they were written for. **Leave them as they are.** Don't renumber existing files.

## 5. How DB-backed Go tests get their schema

**The rule: every DB-backed test creates its own throwaway database and applies every schema file itself.** It must never rely on the shared database already having the schema, and never write to the developer's `ztna_platform`.

- **The pattern.** The `*_TEST_DATABASE_URL` environment variable is used **only as an admin connection**. The test then:
  1. runs `CREATE DATABASE <unique name>`;
  2. applies every file in `controller/migrations/*.sql` in lexical order;
  3. drops the database in `t.Cleanup`.

  Reference helpers: `internal/provider/store_test.go` `newTestStore`, `internal/resource/testdb_test.go` `newResourceTestDB`.
- **Environment variables.** Suites read different variables, and all can point at the same server:
  ```bash
  DB='postgres://ztna:ztna_dev_secret@localhost:5432/ztna_platform?sslmode=disable'
  PKI_TEST_DATABASE_URL=$DB ENROLLMENT_TEST_DATABASE_URL=$DB SHIELD_TEST_DATABASE_URL=$DB \
  RESOURCE_TEST_DATABASE_URL=$DB AUTH_TEST_DATABASE_URL=$DB \
  go test -count=1 ./...
  ```
  - Valkey-backed tests (e.g. the provider login limiter) read `AUTH_TEST_VALKEY_URL`.
- **A skipped DB test is a failed acceptance case.**
  - An unset variable makes a suite skip silently and still report `ok`. Check with `go test -v … | grep -- '--- SKIP'`.
  - CI sets all of the variables, and a guard step fails the job if the canary integration test skips.
- **Reproduce CI locally with an empty admin database.** CI's `ztna_platform` starts **empty**; nothing provisions its schema. A test that only passes because your local database already has tables will fail in CI. To reproduce CI faithfully:
  1. create an empty database, e.g. `ci_empty_admin`;
  2. point every `*_TEST_DATABASE_URL` at it;
  3. run with `-count=1`.
- **The Go test cache can hide broken tests.** `go test` replays cached passes (`(cached)` in the output) without running anything. A broken test can therefore stay green until some change, such as a `go.mod` edit, invalidates the cache. PR #105 fixed two such latent defects. Always use `-count=1` for gate runs.
- **Never `FlushDB` Valkey in a test.** The shared Valkey URL helpers keep only `host:port` and drop the `/db` suffix, so a test's `FlushDB` clears **database 0**: your local dev Valkey, and anything else on that server. Use unique key prefixes and delete only your own keys (see `internal/provider/ratelimit_test.go`).
  - The known exception, `internal/auth` `TestAuthIntegration_LoginBootstrapAndJWTIssue`, is tracked as Sprint 21 Phase K item K6.

## 6. Scope and what's not decided

- This is a **pre-production rule**, chosen because local data is disposable.
- **Production schema upgrades are out of scope and undecided.** When Zecurity approaches production, the team will decide how existing databases are upgraded: a migration framework, versioned upgrade scripts, or something else. Until then, **don't add migration tooling** (no `golang-migrate`, Goose, Atlas, or a `schema_migrations` table). That's an explicit Sprint 21 decision.

## Quick reference

| Situation | Do this |
|---|---|
| Pulled a PR labelled `[schema: reset DB]` | `cd controller && docker compose down -v && docker compose up -d`, then re-bootstrap (section 3) |
| `column/relation "…" does not exist` locally | You skipped the reset: see above |
| Writing a schema change | New `NNN_description.sql` (next: `038_`); never edit old files; PR label `[schema: reset DB]` |
| Writing a DB-backed test | Throwaway DB + apply all schema files (section 5); no `FlushDB` |
| Running the gate | All `*_TEST_DATABASE_URL` set, `-count=1`, check for `--- SKIP` |
