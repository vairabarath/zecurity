---
type: brief
status: for-discussion
sprint: 17
tags: [pending-05, scim, identity, discussion-brief]
---

# PENDING-05 (SCIM Directory Sync) — Implementation Brief for External Review

> Self-contained brief for discussing the SCIM implementation with an external reviewer (no repo access).
> It carries the grounding: what exists today, the locked contract, the real reuse surface, the phase plan,
> and the open design questions. **Authoritative contract = ADR-025 (ACCEPTED). This brief implements it,
> it does not redesign it.**

---

## 0. One-paragraph summary

Zecurity is a ZTNA platform (Go controller, Rust connector/shield/client, React admin). Users today are
created **just-in-time at first OIDC login**. PENDING-05 adds **SCIM 2.0 push provisioning** so an
enterprise IdP (Okta / Microsoft Entra / JumpCloud / …) becomes the source of truth for the workspace
roster and group membership: it creates, updates, and **deprovisions** users and groups over the SCIM REST
API. Deprovision must **cut Zecurity access immediately** (kill sessions + invalidate policy) and
**durably trigger device-trust revocation** via an already-merged transactional outbox. The whole thing is
strictly scoped per `(workspace, connection)` and must never key identity on email.

---

## 1. What already exists (the ground we build on)

**Identity federation (PENDING-04, shipped).** Generic OIDC login through a clean pipeline:

```
protocol adapter (internal/auth/providers)  →  AuthenticationContext (neutral claims)
   →  Resolver  →  Lifecycle gate  →  Linker (JIT-create, never merge by email)
   →  Principal  →  Session (JWT + refresh, stamps identity_generation)  →  Event (audit)
```

Key existing schema (migration `031_identity_federation.sql`):

- `identity_connections` — per-workspace IdP config. Two tiers: **platform** (`tenant_id IS NULL`,
  `managed=true`, creds from env — e.g. Google) and **workspace BYO** (`tenant_id` set, client secret
  encrypted at rest). Columns include `provider, issuer, client_id, encrypted_client_secret, scopes,
  claim_mappings JSONB, status('active'|'disabled')`.
- `external_identities(tenant_id, user_id, connection_id, issuer, subject)`, with
  **`UNIQUE(tenant_id, connection_id, subject)`**. This is THE identity key. `subject` = the immutable
  per-issuer OIDC `sub`. Identity is **never** keyed on email.
- `users` — canonical user. `status IN ('active','suspended','deleted','locked')`.
  `identity_generation INT` backs mass session revocation: bump it and every JWT/refresh stamped with an
  older generation is invalid at next `/auth/refresh`.

**The reuse surface (real Go signatures — SCIM plugs into these, does not reinvent them):**

```go
// internal/identity
type PrincipalCore struct { UserID, TenantID, Role, Email, Status string; Generation int }

type Resolver  // (connection, subject, tenant) -> existing user
func (r *Resolver) Resolve(ctx, connectionID, subject, tenantID string) (*PrincipalCore, bool, error)

type ProvisionInput struct { Email, Provider, Subject, Name, ConnectionID, Issuer, WorkspaceName string }
type Provisioner interface { Provision(ctx, ProvisionInput) (*PrincipalCore, error) } // JIT-creates user + external_identities in ONE tx
type Linker struct{ /* wraps a Provisioner; never email-merges */ }

func CheckLifecycle(status string) error   // gate: only active proceeds

type Revoker  // mass session revocation
func (r *Revoker) BumpGeneration(ctx, tenantID, userID, actorEmail string) (int, error) // ⚠ uses its own pool, NOT a caller tx

type EventPublisher interface { Publish(ctx, Event) error } // audit sink today
```

**Durable outbox (PENDING-15, shipped as Sprint 18, merged).** A generic transactional outbox — SCIM
does **not** build this, it calls it:

```go
// internal/outbox
type Event struct { EventType string; WorkspaceID uuid.UUID; UserID *uuid.UUID; CorrelationID uuid.UUID; Payload json.RawMessage }
func (o *Outbox) Enqueue(ctx, tx pgx.Tx, evt Event) error  // ✅ inserts INSIDE the caller's tx (commits atomically with the identity mutation)
// + ClaimEvents (FOR UPDATE SKIP LOCKED), retry/backoff, crash/lease recovery, background processor, handler registry
```

---

## 2. The one hard boundary (three separate tracks)

```
PENDING-05 (this sprint) → DECIDE + ENQUEUE   — identity lifecycle; SideEffectSink.Enqueue in-tx
PENDING-15 (done)        → DURABLE DELIVERY    — outbox persists/claims/retries/recovers
PENDING-13 (separate)    → DEVICE EXECUTION    — consumes device.trust.* and revokes the device/cert
```

SCIM's only responsibility at the boundary is to **enqueue a device-trust event inside the identity
transaction**. It never revokes a device synchronously; PENDING-13 registers the outbox handler that does.

---

## 3. Locked design decisions (ADR-025, accepted — do not relitigate, but understand them)

1. **Canonical Identity Key is configurable, not hardcoded.** Per connection: `subject_claim` (which OIDC
   claim is the subject, default `sub`) and `scim_identifier` (which SCIM attribute maps to that subject,
   default `externalId`). Both resolve to `external_identities.subject`. Never assume `sub == externalId`.
   Never resolve by email.
2. **Ownership = two columns.** `provisioned_by` (immutable: who created the user — `jit`/`manual`/`scim`)
   + `provisioning_owner` (mutable: who currently governs it — `jit`/`manual`/`scim`/`unmanaged`).
   When `provisioning_owner=scim`, the **directory owns directory attributes**: local edits to those
   fields are **rejected at the mutation layer** (not merely hidden in the UI). Zecurity always owns role,
   policy bindings, and manually-created groups.
3. **Conflict, not silent takeover.** If SCIM provisions a Canonical Identity Key that already belongs to a
   JIT/manual user → return **`409 identity_conflict`** and record a *pending* conflict. An admin resolves
   it (Accept-Link / Reject / Reopen). No auto-merge; never by email. (Full contractor→employee conversion
   is deferred to Stage 4 / ADR-026.)
4. **Break-glass is a dedicated permission, not ADMIN.** `identity.mapping.break_glass` is the smallest
   fine-grained permission primitive; ADMIN does not implicitly hold it. Required to override a failed
   mapping validation; every use carries a mandatory reason + audit (+ MFA once PENDING-06 lands).
5. **Mapping validation is an active round-trip, fail-closed.** `testIdpConnection` provisions a probe
   user (`POST → GET → verify identity key resolves → DELETE`); read-only fallback where the directory
   forbids test writes. SCIM stays **disabled** until the mapping is proven, unless explicitly overridden
   via break-glass.
6. **One engine, provider profiles.** A single SCIM engine + built-in provider **profiles**
   (Okta/Entra/JumpCloud/Keycloak/Generic) with per-connection overrides. No per-provider handler classes.
7. **Per-connection SCIM bearer tokens.** HMAC-SHA256, stored hashed (`SCIM_TOKEN_HASH_KEY`), bound to
   `(workspace_id, connection_id)`. Dual-token rotation: ≤2 active, 24h grace. Plaintext shown once.
8. **Atomic transaction.** identity mutation + audit + policy-notify + generation-bump +
   `SideEffectSink.Enqueue` must commit in **one** transaction; a downstream (device) failure must never
   roll back the identity mutation. **(See §7 — the primitives are not all tx-aware today. This is the
   main open question.)**
9. **Connection lifecycle.** `active → disabled → deleted`. DISABLE is a reversible off-switch (suspends
   users, kills sessions, `provisioning_owner scim→unmanaged`). DELETE is guarded (0 linked users or an
   explicit destructive confirmation) — it never silently mass-suspends.
10. **Everything scoped from the token.** Every SCIM path derives `(workspace_id, connection_id)` from the
    authenticated token, **never** from the request payload.

---

## 4. Data model — new migration `034_scim.sql`

```sql
-- extend identity_connections (SCIM config on an existing connection)
ALTER TABLE identity_connections
  ADD COLUMN scim_enabled     BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN subject_claim    TEXT    NOT NULL DEFAULT 'sub',        -- OIDC side of the Canonical Identity Key
  ADD COLUMN scim_identifier  TEXT    NOT NULL DEFAULT 'externalId', -- SCIM side of the Canonical Identity Key
  ADD COLUMN last_sync_at     TIMESTAMPTZ,
  ADD COLUMN provider_profile TEXT;                                  -- 'okta'|'entra'|'jumpcloud'|'keycloak'|'generic'
-- status gains 'deleted'
ALTER TABLE identity_connections DROP CONSTRAINT identity_connections_status_check;
ALTER TABLE identity_connections ADD  CONSTRAINT identity_connections_status_check
  CHECK (status IN ('active','disabled','deleted'));

-- ownership on the canonical user
ALTER TABLE users
  ADD COLUMN provisioned_by    TEXT NOT NULL DEFAULT 'jit'   CHECK (provisioned_by    IN ('jit','manual','scim')),
  ADD COLUMN provisioning_owner TEXT NOT NULL DEFAULT 'jit'  CHECK (provisioning_owner IN ('jit','manual','scim','unmanaged')),
  ADD COLUMN sync_instance_id  UUID;

-- group origin (manual vs scim vs system) + directory external id
ALTER TABLE groups
  ADD COLUMN origin      TEXT NOT NULL DEFAULT 'manual' CHECK (origin IN ('manual','scim','system')),
  ADD COLUMN external_id TEXT;                                   -- connection-scoped directory group id

ALTER TABLE external_identities ADD COLUMN sync_instance_id UUID;

-- per-connection SCIM bearer tokens (hashed)
CREATE TABLE scim_tokens (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     UUID NOT NULL REFERENCES workspaces(id)           ON DELETE CASCADE,
  connection_id UUID NOT NULL REFERENCES identity_connections(id) ON DELETE CASCADE,
  token_hash    TEXT NOT NULL,                                    -- HMAC-SHA256(SCIM_TOKEN_HASH_KEY, plaintext)
  status        TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','revoked','expired')),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  expires_at    TIMESTAMPTZ                                       -- set on rotation (grace window)
);
CREATE INDEX idx_scim_tokens_lookup ON scim_tokens (token_hash) WHERE status = 'active';

-- pending identity conflicts (SCIM vs JIT/manual for the same Canonical Identity Key)
CREATE TABLE scim_identity_conflicts (
  id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id            UUID NOT NULL REFERENCES workspaces(id)           ON DELETE CASCADE,
  connection_id        UUID NOT NULL REFERENCES identity_connections(id) ON DELETE CASCADE,
  canonical_identity_key TEXT NOT NULL,                          -- the resolved subject
  existing_user_id     UUID NOT NULL REFERENCES users(id)        ON DELETE CASCADE,
  scim_payload         JSONB NOT NULL,
  state                TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','accepted','rejected')),
  created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  resolved_at          TIMESTAMPTZ
);
CREATE UNIQUE INDEX idx_scim_conflict_pending
  ON scim_identity_conflicts (tenant_id, connection_id, canonical_identity_key) WHERE state = 'pending';

-- one sync instance per directory connect (detect stale-vs-current on reconnect)
CREATE TABLE scim_sync_instances (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     UUID NOT NULL REFERENCES workspaces(id)           ON DELETE CASCADE,
  connection_id UUID NOT NULL REFERENCES identity_connections(id) ON DELETE CASCADE,
  opened_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  closed_at     TIMESTAMPTZ
);
```

> Migration number `034` is the next free slot after the merged `030..033`. There is already a
> double-`031` on the tree (`031_identity_federation` + `031_device_profile_manual_trust`); confirm no
> ordering surprise at branch-cut.

---

## 5. SCIM protocol surface (RFC 7643/7644)

Base path `/scim/v2/` (bearer-authed, per connection):

| Method + path | Behavior |
| --- | --- |
| `GET /ServiceProviderConfig`, `/ResourceTypes`, `/Schemas` | conformance metadata |
| `POST /Users` | provision; `409 identity_conflict` on Canonical-Key collision with a JIT/manual user |
| `GET /Users/{id}`, `GET /Users?filter=...` | read; support `eq` filter on the identifier; tombstones hidden |
| `PUT /Users/{id}`, `PATCH /Users/{id}` | update directory-owned attrs only; `active=false` → suspend |
| `DELETE /Users/{id}` | soft-delete + deprovision side effects; idempotent |
| `POST/PUT/PATCH/DELETE /Groups` | group + membership sync (origin=`scim`, connection-scoped `external_id`) |

RFC details to honor: SCIM envelopes + `schemas`, `meta.version` (ETag seam), list `totalResults`,
`PATCH` op semantics, error `scimType`, out-of-order group member → `404` (no dangling membership).

---

## 6. Phase plan (solo M1; the outbox is already merged)

Branch `feat/pending-05-scim` off `fixed-pendings`. Each phase ends at a green `go build ./...` + tests.

| # | Phase | Depends | Independent of outbox? | New/changed files (indicative) |
| --- | --- | --- | --- | --- |
| 1 | **Schema** — migration `034_scim.sql` + store column reads | — | yes | `migrations/034_scim.sql`, `internal/idp/store.go` |
| 2 | **SCIM token auth** — hashed tokens, dual rotation, bearer middleware | 1 | yes | `internal/scim/token.go`, `.../middleware.go`, GraphQL mint/rotate/revoke |
| 3 | **Break-glass permission primitive** — `workspace_permissions`, `HasPermission`, register `identity.mapping.break_glass` | — | yes | `internal/permission/*` |
| 4 | **Provider profiles + mapping validation** — built-in profiles, active probe-user round-trip, fail-closed | 2,3 | yes | `internal/scim/profiles.go`, `.../validate.go` |
| 5 | **Users: provision + update** — `internal/scim` engine + `DirectoryService`; reuse `Resolver`/`Linker`; mutation-layer ownership | 4 | yes | `internal/scim/service.go`, `.../users.go`, a SCIM `Provisioner` impl |
| 6 | **Users: deprovision + reactivate + SideEffectSink → outbox** | 5 | **enqueue is durable; identity effects independent** | `internal/identity/side_effect_sink.go`, `internal/scim/side_effect_sink_outbox.go`, `.../users.go`, `cmd/server/main.go` |
| 7 | **Groups** — origin/external_id, membership sync, `NotifyPolicyChange`, out-of-order → 404 | 5 | yes | `internal/scim/groups.go` |
| 8 | **Identity conflict workflow** — `409`, pending record, Accept-Link/Reject/Reopen | 5 | yes | `internal/scim/conflict.go`, GraphQL conflict queue |
| 9 | **Connection lifecycle + health + sync instances** — active/disabled/deleted, Identity Health, reconcile | 5 | yes | `internal/idp/store.go`, `internal/scim/sync_instance.go`, GraphQL |

**Phase 5 detail — the SCIM Provisioner.** `internal/bootstrap.Service` implements today's `Provisioner`
but is *workspace-creating* (web signup). SCIM provisions into an **existing** workspace with
`provisioned_by=scim, provisioning_owner=scim` and a `sync_instance_id`. So Phase 5 adds a **second
`Provisioner` implementation** (a SCIM directory provisioner) reusing the same `external_identities`
invariant — it does **not** reuse bootstrap's workspace-creation path.

**Phase 6 detail — the SideEffectSink.**

```go
// internal/identity
type DeviceTrustEvent struct { WorkspaceID, UserID uuid.UUID; Type, Reason string; CorrelationID uuid.UUID }
type SideEffectSink interface { Enqueue(ctx, tx pgx.Tx, evt DeviceTrustEvent) error }

// internal/scim  (the only implementation)
type DurableOutboxSink struct { ob *outbox.Outbox }
func (s DurableOutboxSink) Enqueue(ctx, tx pgx.Tx, evt DeviceTrustEvent) error {
    payload, _ := json.Marshal(evt)
    return s.ob.Enqueue(ctx, tx, outbox.Event{
        EventType: evt.Type, WorkspaceID: evt.WorkspaceID, UserID: &evt.UserID,
        CorrelationID: evt.CorrelationID, Payload: payload,
    })
}
```

Event types match ADR-025 §5.1 exactly: `device.trust.revoke.requested` (on deprovision),
`device.trust.re_enrollment_required` (on reactivate). PENDING-13 registers the handler that executes them.

---

## 7. THE open design question (please pressure-test this)

ADR-025 §8 requires deprovision to be **atomic** across: (a) the `users`/`external_identities` mutation,
(b) `identity_generation` bump, (c) audit event, (d) `policy.Notifier` invalidation, (e)
`SideEffectSink.Enqueue`. But the existing primitives are **not uniformly tx-aware**:

- `outbox.Enqueue(ctx, tx, evt)` — ✅ takes a tx.
- `Revoker.BumpGeneration(ctx, tenantID, userID, actorEmail)` — ❌ runs on its **own pool**; its
  session-invalidation + audit are already documented as *best-effort, post-commit*.
- `Provisioner.Provision(ctx, in)` — ❌ opens its **own** internal tx.
- `EventPublisher.Publish(ctx, e)` — ❌ pool-based.
- `policy.Notifier` — in-memory signal, not tx-bound.

**Two candidate resolutions (this is what I want to debate):**

- **Option A — make the primitives tx-aware.** Add `BumpGenerationTx(ctx, tx, ...)`, a tx-accepting SCIM
  `Provision`, and a tx-bound `Publish`. Everything the ADR lists commits in one real transaction. Cost:
  refactoring shared PENDING-04 code + retesting the login path that also uses them.
- **Option B — tx-owns-the-truth, effects-are-post-commit.** SCIM opens one tx and writes the
  *authoritative* rows in it: the `users`/`external_identities` mutation, the `identity_generation`
  increment (inline SQL, not via Revoker), **and** `outbox.Enqueue`. Audit, session-invalidation, and
  policy-notify happen **after** commit as best-effort — which is already exactly how `BumpGeneration`
  treats invalidation + audit. Cost: "atomic" then means *"the security-critical state + the durable
  device-trust intent commit together; observability/notification are best-effort after"* — arguably the
  honest interpretation, but it's a narrower guarantee than a literal reading of §8.

My leaning is **Option B** (it matches the codebase's existing best-effort posture and avoids destabilizing
the shared login path), scoping the atomic set to exactly {identity row mutation, generation increment,
outbox enqueue}. **Question for review: is that the right atomic set, or does the generation bump's audit
event also need to be inside the tx for compliance/forensic reasons?**

Other questions worth a second opinion:

1. **Group member ordering.** SCIM clients often `POST /Groups` referencing members before all members
   exist. We return `404` for an unknown member (no dangling membership). Is strict-404 correct, or should
   we buffer/relax for specific providers (Entra is notoriously chatty)?
2. **Soft-delete retention.** DELETE soft-deletes and we retain the tombstone (identity key stays reserved
   to prevent resurrection under a new user). Indefinitely, or a TTL? What do we do if the directory
   re-`POST`s the same `externalId` after delete — resurrect the same canonical user or conflict?
3. **`provisioning_owner` transition on connection DISABLE.** We flip `scim → unmanaged` so a disabled
   directory doesn't leave users frozen-but-directory-owned. On RE-enable, do we auto-reclaim
   (`unmanaged → scim`) on next sync, or require admin confirmation?
4. **Update semantics for a partially-owned user.** A user is `provisioning_owner=scim` for profile attrs
   but Zecurity owns role/policy. A SCIM `PUT` (full replace) that omits role must **not** wipe role. Is
   attribute-scoped PUT (ignore non-directory attrs) the right RFC-compatible behavior, or should we only
   accept `PATCH` for owned users and `501` on `PUT`?
5. **Token model.** HMAC-SHA256 hashed at rest, bound per connection, dual-rotation with 24h grace. Is a
   per-connection bearer sufficient, or should we support short-lived tokens / IP allowlist for the SCIM
   endpoint given it's an internet-facing write API?

---

## 8. Test strategy

- **Unit (fakes):** token rotation/grace/scope; mapping validation fail-closed; deprovision identity
  effects with a **fake sink** (suspend/delete + generation + session-kill), no DB outbox needed;
  conflict FSM ("rejected never auto-approves"); ownership rejection at mutation layer.
- **DB integration:** provision/update scope-isolation (`(workspace,connection)` from token only); the
  atomic deprovision writes an `outbox_events` row **in the same tx** (forced enqueue error aborts the
  whole tx — identity not mutated); group out-of-order → 404; connection disable→suspend→re-enable→restore.
- **SCIM conformance suite** now; **live Okta/Entra interop marked PENDING tenant access** — do not mark
  interop "done" without real tenants.

---

## 9. Explicitly out of scope this sprint

Contractor→employee identity linking/conversion (Stage 4, ADR-026); MFA on break-glass (activates with
PENDING-06); pull-based sync (Google Directory / MS Graph); broker-owned SCIM; optimistic concurrency
(`If-Match`) beyond emitting `meta.version`; the PENDING-13 device-trust handler that consumes the outbox
events.
