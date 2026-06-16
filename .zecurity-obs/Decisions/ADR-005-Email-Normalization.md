---
type: decision
status: accepted
date: 2026-06-10
implemented: 2026-06-10
related:
  - "[[CodeStudy/02-Invite-Flow]]"
tags:
  - adr
  - controller
  - auth
  - bootstrap
  - invitation
  - email
  - data-hygiene
---

# ADR-005 — Emails Are Normalized to Lowercase at Write Time

## Context

The signup, login, and invite flows all match users by email. Bootstrap's pending-invite
lookup ([`bootstrap.go:79`](controller/internal/bootstrap/bootstrap.go#L79)) and the
workspace-membership join in `lookupWorkspacesByEmail`
([`schema.resolvers.go:138`](controller/graph/resolvers/schema.resolvers.go#L138)) both
use case-sensitive `email = $1` SQL equality.

PostgreSQL string equality is case-sensitive. Google OAuth ID tokens **always return
email in lowercase** (normalization at the IdP). But admins typing emails into the
invite UI ([`TeamUsers.tsx`](admin/src/pages/TeamUsers.tsx)) are not constrained — the
frontend only does `email.trim()`, no case normalization. HTML `type="email"` allows
mixed case.

### Bug — Case-sensitive lookup misses invites with mixed-case email

Concrete scenario:

1. Admin invites `Bob@Corp.com` in the TeamUsers UI.
2. Backend INSERTs `workspace_members` row with `email = 'Bob@Corp.com'`.
3. Bob signs in via Google. Google returns `email = 'bob@corp.com'`.
4. Bootstrap's pending-invite lookup at
   [`bootstrap.go:76-84`](controller/internal/bootstrap/bootstrap.go#L76-L84):
   `WHERE email = 'bob@corp.com' AND status = 'invited'` → **no match**.
5. Bootstrap falls through to `runBootstrapTransaction`.
6. Bob gets a **new workspace as admin** instead of joining as the invited member.

No error is raised at any step. Admin thinks the invite worked; Bob thinks he signed up
fresh. Silent privilege divergence.

### Why this is project-side (not consumer error)

The admin cannot control case sensitivity — emails are typed by humans and Google
lowercases without telling anyone. Storing what was typed verbatim is a project decision
that creates the mismatch. Case normalization is standard practice for email-keyed
systems (every production SaaS handles this).

## Decision

**Normalize all emails to lowercase at write time, before storage.**

Wherever an email enters the system, lowercase it before INSERT/UPDATE:

1. Invitation creation (`createInvitation` mutation handler).
2. User row creation in bootstrap (both `runBootstrapTransaction` and
   `runInvitedUserTransaction`).
3. Any other code path that writes to `users.email` or `workspace_members.email`.
4. One-time backfill migration to lowercase existing rows.

After this, every email comparison can use plain `=` and any indexes on `email` work
without modification. No application-side case folding on reads.

## Alternatives Considered

### Alt A — `LOWER()` on both sides at query time (rejected)

```sql
WHERE LOWER(email) = LOWER($1)
```

**Rejected because:**
- Disables index on `email` unless a functional index `(LOWER(email))` is added.
- Requires every email-comparing query to remember to apply `LOWER()` — easy to forget,
  hard to enforce.
- Stored emails remain mixed-case, so admin views (member list, audit logs) show
  inconsistent casing.

### Alt B — `citext` column type (rejected)

```sql
CREATE EXTENSION citext;
ALTER TABLE workspace_members ALTER COLUMN email TYPE citext;
ALTER TABLE users ALTER COLUMN email TYPE citext;
```

**Rejected because:**
- Adds a Postgres extension dependency (small but real ops/deploy concern).
- Schema becomes the case-insensitivity contract, but stored values remain mixed-case —
  same display inconsistency as Alt A.
- More schema migration risk than write-time normalization.

### Alt C — Frontend-only normalization (rejected)

Lowercase the email in the React form before sending.

**Rejected because:**
- Frontend is not a trust boundary — direct GraphQL POSTs bypass it.
- Doesn't help existing data already stored mixed-case.

## Consequences

### Wins
- Lookups become deterministic regardless of admin input casing.
- Existing indexes on `email` keep working at full performance.
- Admin views display canonicalized emails (consistent UX).
- No future code has to remember to apply case folding.

### Costs
- Touches ~4 files: invite store, bootstrap, signup user-create path, plus one migration.
- One-time backfill migration to lowercase historical data.
- Display loses the casing the user typed (acceptable — email is case-insensitive
  per RFC 5321; preserving case has no semantic meaning).

### Risk: corner cases
- **Mixed-case local-parts** (rare, technically allowed by RFC 5321 but treated as
  case-insensitive by virtually all mail providers). Accepting lowercase is universal in
  practice.
- **Existing user with mixed-case email** logging in: lowercase the lookup parameter
  even before backfill runs so login still works. Backfill is for cleanup; lookup-side
  lowercase is the safety net.

## Plan

### Phase 1 — Server-side normalization (load-bearing fix)
1. `invitation/store.go::CreateInvitation` — `email = strings.ToLower(strings.TrimSpace(email))` before INSERT.
2. `bootstrap.go::Bootstrap` — apply same normalization to the `email` param at function entry.
3. `bootstrap.go:79` — the lookup is safe once input is normalized; no SQL change needed.
4. `bootstrap.go:127-137, 216-222` — INSERTs use the normalized email.
5. Audit any other code path that writes to `users.email` or `workspace_members.email`; apply the same pattern.

### Phase 2 — Backfill migration

```sql
-- 016_email_lowercase.sql — Lowercase historical mixed-case emails.
-- Safe: emails are case-insensitive per RFC 5321; consolidation is the right move.
UPDATE workspace_members SET email = LOWER(email) WHERE email != LOWER(email);
UPDATE users             SET email = LOWER(email) WHERE email != LOWER(email);
```

### Phase 3 — UX polish (optional, frontend)
- `TeamUsers.tsx::createInvitation` mutation: `email: email.trim().toLowerCase()`.
- Cosmetic only; backend remains the source of truth.

### Phase 4 — Test coverage
- Add table-tests for `CreateInvitation` and `Bootstrap` with mixed-case input;
  assert normalization.
- Add an integration test that invite with `Bob@Corp.com` + login with `bob@corp.com`
  results in joining the invited workspace, not creating a new one.

## Notes

- The constraint is enforced at the application layer (not via DB CHECK or trigger).
  Reason: error messages from app code are cleaner than CHECK violations; we control
  the normalization point so triggers are not needed.
- This ADR closes Finding **F3** of the bootstrap audit.
- This ADR does NOT address Findings F1 (invite-token bypass) or F2 (multi-workspace
  ambiguity) — those are separate decisions.
