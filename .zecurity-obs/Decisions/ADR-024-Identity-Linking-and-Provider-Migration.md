---
type: adr
status: accepted
id: ADR-024
domain: identity
priority: P1
created: 2026-07-27
decided: 2026-07-27
related:
  - PENDING-04-Multiple-IdPs-Enterprise-SSO
  - ADR-023-Identity-Philosophy
  - ADR-005-Email-Normalization
  - PENDING-05-Directory-Sync-SCIM
tags: [adr, identity, linking, provisioning, migration, security]
---

# ADR-024 — Identity Linking & Provider Migration

> **Status: ACCEPTED (2026-07-27).** The **product-behavior** decisions behind PENDING-04's
> `external_identities` model. These are the rules an implementer must not invent — they define how
> external logins map to canonical users, and are governed by [[ADR-023-Identity-Philosophy]].

## Context

Today identity is embedded in `users` as `(provider, provider_sub)` with
`UNIQUE(tenant_id, provider_sub)` (`migrations/001_schema.sql:41-56`) — provider is not even in the
unique key. With one Google IdP that is fine. The moment a workspace federates two OIDC connections
(e.g. employees on Entra, contractors on Okta), OIDC `sub` is only unique *per issuer*, so two
issuers can emit the same `subject` and collide into one user row. PENDING-04 introduces
`external_identities` (many external identities → one canonical `users` row). This ADR fixes the
rules that model must obey, because wrong linking behavior is a **security incident**
(account-recycling / cross-tenant identity confusion), not a bug.

> **Greenfield note (Sprint / Phase 3):** the platform is not yet deployed, so there is **no user
> backfill** — the schema is created fresh (`migrations/031_identity_federation.sql`) and
> `users.provider`/`provider_sub` are dropped once Phase 5 rewires login onto `external_identities`.

## Decision

The end-to-end flow this ADR governs (read top to bottom):

```text
Bootstrap Login  (Platform IdP: Google / Microsoft)
        │
        ▼
Create Workspace ───────▶ Configure Enterprise IdP  (Settings)
        │                              │
        │                              ▼
        │                   Enterprise Login  (workspace-first discovery API)
        └───────────────┬──────────────┘
                        ▼
              AuthenticationContext
                        ▼
        Identity Pipeline  (resolve → link → lifecycle)   [ADR-023]
                        ▼
             Principal ──▶ Session ──▶ Policy Engine
```

Nothing bypasses `External Identity → Principal → Session → Authorization`.

### 0. Connection model — two tiers: Bootstrap vs Enterprise IdPs

IdP connections live in one table, `identity_connections`, distinguished by whether `tenant_id` is set.
The two tiers differ by **purpose** (framing borrowed from Twingate):

- **Bootstrap IdPs** (`tenant_id IS NULL`, `managed = true`): platform-managed built-ins whose single
  OAuth client lives in **platform env config** (Google today; Microsoft/GitHub later), resolved at
  login by `provider` name — **no secret stored**. Owned by the provider/super-admin plane (ADR-021).
  Their job is **creating workspaces, recovery, and break-glass**. They are **not intended for routine
  employee authentication** once a workspace has adopted Enterprise SSO.
- **Enterprise IdPs** (`tenant_id = <ws>`, `managed = false`): a workspace's own BYO OIDC connection,
  `client_secret` **encrypted at rest** (`pki.EncryptSecret`, context `"idp-client-secret:"+tenantID`).
  Owned by the workspace admin. Their job is **daily login**.

The Postgres column `managed` distinguishes them; "Bootstrap / Enterprise" is the product vocabulary
used in the UI and the discovery API. Adding a new Bootstrap IdP (Microsoft/GitHub) later = one seed row
+ a small adapter, no schema change.

**Login is workspace-first and server-driven** (matching Twingate / Cloudflare / Teleport). The client
identifies the workspace first, then asks the controller which providers to show — it never hardcodes
provider buttons:

```
GET /workspaces/{slug}/auth
  → { "workspace": "acme",
      "providers": [ { "id": "...", "display": "Acme SSO", "type": "oidc" } ],
      "platformFallback": true }
```

Resolution rule:
- Workspace has ≥1 active Enterprise IdP → advertise those (shown first/prominently);
  `platformFallback = <platform_login_enabled>` (default true, see §5).
- Workspace has none → advertise the Bootstrap IdPs; `platformFallback = true`.

**Adding an Enterprise IdP does NOT silently hide Bootstrap login** — the workspace's founding admin
bootstrapped with a Bootstrap IdP and would otherwise be locked out (§5). "IdP available" ≠ "access
granted": membership/authorization still gates (`workspace_members`, policy engine).

### 1. Linking key

An external identity is `(connection_id, issuer, subject)`, stored uniquely as
`UNIQUE(tenant_id, connection_id, subject)`. The `tenant_id` is denormalized onto
`external_identities` so uniqueness stays **per-tenant even when `connection_id` is a *shared platform*
connection** — i.e. the same Google account in two workspaces remains two distinct users. It maps to
exactly one canonical `users.id`. **Email is never a linking key** — it can change, be reassigned, be
aliased, and be asserted by two IdPs.

### 2. First login with a never-seen `(connection, subject)`

JIT-create a **new** canonical user + one `external_identities` row. Do **not** auto-merge onto an
existing user by matching email. (Existing invite matching by email in `workspace_members` stays —
it decides *role/workspace membership*, not identity linking.)

### 3. Same email, different IdP/subject — do NOT auto-merge

If `barath@acme.com` exists via connection A (subject X) and a login arrives via connection B
(subject Y, same email), they are **different identities → different accounts**.

- **Shipped now — Option A:** create a separate canonical account.
- **Documented follow-ups (not built):** (B) admin-approved explicit link; (C) verified self-service
  link. Ship A; revisit B/C when the provider console lands.

Rationale: silent email-merge is the classic account-takeover vector (email reassigned to a new
employee inherits the old account). Fail toward *separate*, never toward *merged*.

### 4. Provider migration (workspace swaps Google → Entra)

A deliberate, admin-initiated, audited operation — never an implicit side effect of a login:

1. Admin adds the new connection (e.g. Entra) alongside the old (Google).
2. On each user's first login through the new connection, a **new `external_identities` row** is
   created and linked to the **same canonical `user_id`**. Email is used only as an
   **admin-visible matching hint** to pre-associate; the actual link is confirmed by the validated
   token's `(connection, subject)`, never auto-joined by email alone.
3. The old connection is disabled/removed only after migration, and doing so bumps
   `identity_generation` (kills stale sessions). Every step emits an `identity.migrate` audit event.

### 5. Ownership & no-lockout

- Only workspace **admins** (`@hasRole(ADMIN)`) may add/modify/remove connections.
- A workspace **can never lock itself out**: removing the last active connection requires a
  configured per-workspace break-glass path (mirrors the provider tier's `PROVIDER_BOOTSTRAP_EMAILS`
  precedent). When an IdP is unreachable, authentication fails **closed** for new logins; existing
  sessions follow the normal generation/expiry rules.
- **Disabling Bootstrap (platform) login** is an explicit, guarded admin action — never an automatic
  consequence of adding an Enterprise IdP. It flips a per-workspace `platform_login_enabled` flag to
  false (so discovery returns `platformFallback = false` and clients show only Enterprise IdPs). It is
  **refused unless all three pass**:
  1. a per-workspace **break-glass** path is configured,
  2. **every workspace admin already has a working Enterprise-IdP identity** (no admin left
     Bootstrap-only), and
  3. at least **one *healthy* Enterprise IdP** exists — where **healthy** means not merely
     `status = 'active'`, but last verified reachable: discovery succeeded, JWKS fetched, and (for
     `manual_oidc`) the client credentials validated. A merely-configured-but-unreachable connection
     does **not** satisfy this check.

  This delivers the enterprise "only our SSO" experience while making lockout structurally impossible.
  The flag + checks ship with the admin API (Phase 6/7); until then `platform_login_enabled` is
  implicitly true.

### 6. Lifecycle independence

Canonical-user lifecycle (`active` / `invited` / `suspended` / `locked` / `deleted`) lives on the
**Principal**, independent of any external identity. Disabling an IdP connection does not delete
users; suspending a user blocks login regardless of which connection they use.

### 7. How an admin configures an Enterprise IdP (Twingate-modelled)

**Decision.** Configuring an Enterprise IdP produces an `identity_connections` row via one of two
**provisioning methods**, mirroring how Twingate actually behaves (verified-app *consent* for the
providers it integrates; *manual app registration* for the rest). Both methods yield the same row, so
the second can be added later with no schema change.

- **`manual_oidc` — universal, ships now (the default path).** The admin provides the **issuer URL**
  (e.g. `https://acme.okta.com`) plus a `client_id` / `client_secret` created by a one-time app
  registration in their IdP. Zecurity **auto-discovers** everything else via
  `<issuer>/.well-known/openid-configuration` (endpoints, JWKS) — the admin never hand-enters endpoints.
  Works for **any** OIDC IdP (Okta, Entra, Ping, Auth0, Keycloak, custom) immediately. This is the
  Phase-4 adapter's native mode. Equivalent to Twingate's Okta/Entra "create an app, then connect" flow.

- **`oauth_consent` — guided one-click, layered later per provider (the "simple login" UX).** For a
  small set of providers where Zecurity ships and *publishes* a verified app (Google Workspace,
  Microsoft Entra), the admin clicks **"Connect Google Workspace"**, signs into their tenant, and
  authorizes — Zecurity captures the grant and auto-fills the connection. **No client_id/secret paste.**
  This is Twingate's Google-Workspace experience and the target UX. Its prerequisite is *operational,
  not code*: Zecurity must register/verify a Marketplace / multi-tenant app with each such provider, so
  it ships **per provider as those verifications land**, not on day one.

**Why not consent-only.** The frictionless "just log in" flow only exists for providers where the
vendor *is* a verified app; a generic/self-hosted OIDC IdP has no such shortcut, so a universal product
**must** offer `manual_oidc`. We therefore ship `manual_oidc` (with discovery, so it's already low-
friction) as the floor, and treat `oauth_consent` as a per-provider enhancement on top — exactly
Twingate's split. Directory sync (SCIM) is a **separate** capability from login for both methods
(PENDING-05), matching Twingate's separate SCIM step.

**Schema note.** `manual_oidc` uses the existing columns (`issuer`, `client_id`,
`encrypted_client_secret`, `discovery_url`). `oauth_consent` will store its grant/refresh token in the
same encrypted-secret columns (or a small additive column) — no redesign of `031_identity_federation`.

### 8. Runtime robustness (cache, failure, discovery evolution)

- **Identity-cache invalidation.** The external-identity→Principal cache (ADR-023 seam) is a hint, not a
  source of truth. Its entries **must be invalidated** on: identity linked, identity removed, provider
  migration, connection disabled/deleted, and `identity_generation` bump. A stale hit must never
  resurrect a de-linked or suspended identity — on any miss/uncertainty, fall back to the database.
- **Failure behavior — fail closed for new auth, never for live sessions.** Expected security behavior
  (not implementation) when a dependency misbehaves:
  - *Discovery endpoint / JWKS unavailable, or provider timeout / rate-limited:* new logins via that
    connection **fail closed** (deny, surface a retryable error); the per-issuer JWKS cache serves the
    last good keys within its TTL so transient blips don't break login. Never accept an unverifiable
    token.
  - *Clock skew:* allow a small bounded `exp`/`iat`/`nbf` leeway (seconds, not minutes); reject beyond.
  - *Existing sessions* are unaffected by IdP-side outages — they ride the normal JWT expiry +
    `identity_generation` rules. An IdP outage must not mass-invalidate live sessions.
- **Discovery response is versioned/extensible.** `GET /workspaces/{slug}/auth` may grow optional,
  additive capability hints (e.g. MFA/`acr` requirements tied to PENDING-06) so clients adapt without
  hardcoding provider knowledge. Clients must ignore unknown fields; absence means "unknown", not
  "false".

## Consequences

- Fixes the two-IdP `sub` collision structurally (issuer-scoped uniqueness).
- One human can hold several external identities under one Principal, GitHub/Atlassian-style —
  enabling clean provider migration without data surgery.
- Email is demoted to a human-readable hint everywhere in identity resolution.
- Every linking/migration/lifecycle action is an audited identity event (ADR-023 plane rules).

## Alternatives considered

- **Link by normalized email (ADR-005).** Rejected as an identity key — reintroduces the
  account-recycling vulnerability. Email normalization still applies to invite matching only.
- **Keep identity embedded in `users`, add issuer column.** Rejected — cannot represent one human
  with multiple IdPs and makes provider migration a destructive rewrite.

## Status / follow-ups

Accepted as the baseline for PENDING-04 implementation. Options B/C (assisted/self-service linking)
and the SCIM-driven lifecycle transitions ([[PENDING-05-Directory-Sync-SCIM]]) are explicit
follow-ups.

---

## Addendum — Lifecycle & ownership decisions (2026-08-06)

Architectural rules ratified from the [[Identity-Lifecycle-and-Ownership-Design-Review]], which holds
the full reasoning, alternatives, enterprise comparison, and the operational/UX detail. **Only the
decisions live here.**

- **Source of Authority.** Every canonical user has exactly one authoritative source that owns its
  lifecycle and directory-owned attributes (`users.source_of_authority`). V1 always has one external
  identity per user, so this equals that identity's connection (or `manual` for invited users).
  Multi-identity linking is **not permitted without it** — that is the future Identity-Governance ADR.
- **Email is never in identity *or membership* logic.** Extending the no-email-merge rule: SCIM does
  **not** fulfill invitations by email. A pending invite expires by its own TTL; provisioning is
  independent. Email is a notification address, never a join key.
- **Ownership is enforced at the mutation layer.** Directory-owned attributes of a directory-managed
  user are **rejected by GraphQL mutations**, not merely greyed out in the UI. The "no conflict"
  guarantee is API-enforced, not cosmetic.
- **Workspace Mode.** A workspace is `Hybrid` (platform + enterprise IdPs) or `Enterprise-Managed`
  (enterprise only). Selecting Enterprise-Managed disables platform login (the §5 `platform_login_enabled`
  toggle) — the intent-level expression of that switch ("the company now owns all identities").
- **Duplicate-identity detection.** When two canonical users share an email under distinct
  `(connection, subject)` keys, the system surfaces a **warning** to admins — never an automatic merge.
  Detection only; resolution is manual (or the future linking workflow).
