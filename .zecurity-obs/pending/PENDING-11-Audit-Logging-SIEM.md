---
type: adr
status: design-draft
id: PENDING-11
domain: operations
priority: P2
created: 2026-07-03
updated: 2026-11-05
related:
  - PENDING-10-Observability
  - PENDING-15-Durable-Outbox-Infrastructure
  - PENDING-19-Workspace-RBAC
tags: [pending, adr, operations, audit, siem, compliance]
---

# PENDING-11: Audit logging and SIEM export

**Status:** design draft, 2026-11-05. **Not approved. No code until approval.**
**Base:** `main` (89655c83). Next migration: `051_`.
**Scope:** its own epic. It also builds D-12 (DB-enforced append-only audit), which the Decision Record left to this phase.

---

## 1. Problem

Customers' security teams want Zecurity's events in their own SIEM (Splunk, Elastic, Sentinel, QRadar, and so on). They want two kinds of events:

- **who changed what** in the workspace (admin audit);
- **who reached which resource**, allowed or denied (access events).

Auditors (SOC 2, ISO 27001) also want proof that the log was not edited afterwards.

Today Zecurity records most of this, but nothing leaves the database. Nothing proves the rows are untouched either.

## 2. What exists (facts, verified 2026-11-05)

**Admin audit: `audit_logs` (migration 016)**

- Columns: `tenant_id, actor_user_id, actor_email, action, target_type, target_id, details JSONB, created_at`. `id` is a random UUID.
- It is written in about 60 places:
  - `auditAdmin` in `graph/resolvers/audit_helpers.go` has 38 call sites: networks, connectors, shields, resources, groups, access rules, device profiles, resource policies, invitations and scans;
  - IdP and SCIM writers;
  - device trust (`device.revoked`, `device.cert.renewed`, `device.re_enroll_required`, `device.cert.renew_denied`);
  - connector and shield token issue;
  - `outbox.recover`.
- Almost every GraphQL mutation is audited. `SetMemberRole`, `ApproveDevice`/`UnapproveDevice` and the SCIM conflict actions are audited through helpers.
- **Not audited (gaps):**
  - admin console sign-in, failed sign-in and sign-out (nothing in `internal/auth`);
  - invitation acceptance (`internal/invitation`);
  - client device enrollment (only renewals are audited);
  - workspace signup (`internal/bootstrap`);
  - `TestIdpConnection`.
- `audit.Record` runs after the change has committed and is best-effort: a failed write is logged, not fatal. `audit.RecordTx` writes inside the caller's transaction.
- **Tenants cannot read their own audit log.** There is no GraphQL query and no admin console page. The provider console reads only `provider_audit_logs` (D-14), never `audit_logs`.

**Access events: `connector_logs` (migrations 014 and 021)**

- The connector emits one `ConnectorLog` per tunnel decision (`connector/src/device_tunnel.rs`, `emit_access_log`, 9 call sites). The decision is `allow`, `deny` or `error`.
- Each event carries the resource, the client SPIFFE ID, the destination, the port and protocol, the route type (connector or shield) and `occurred_at`.
- The controller resolves the device and user ids and inserts the row (`control_stream.go` `handleConnectorLog`).
- The admin console shows these rows on the Access Log page (`connectorLogs(limit)`, `CONSOLE_READ`).
- There is **no retention**: the table grows without limit.
- Events are lost if the connector's control stream is down when they are emitted. They are fire-and-forget on an mpsc channel.
- There are no session-close events and no byte counts.

**Integrity**

- **D-12 (decided 2026-09-23) is not built.** The app connects as `ztna`, which is the Postgres superuser and owner of every table (docker-compose `POSTGRES_USER`, `DATABASE_URL`). "Append-only" is a code convention only.
- There is no hash chain and no export sink anywhere.

**Building blocks**

- Secrets are sealed with `pki.EncryptSecret` (`internal/pki/secret.go`).
- The outbox has retry and backoff (`internal/outbox/backoff.go`).
- The GeoIP code already rejects private, loopback and link-local addresses (`internal/geoip/geoip.go:191`).
- Roles and capabilities live in `internal/rbac/capability.go`: 8 capabilities and 6 roles.

## 3. Decisions taken (2026-11-05, Sathiya)

| # | Decision |
|---|---|
| S-1 | **v1 scope: complete admin audit plus user resource-access audit.** Both `audit_logs` and `connector_logs` are exported. The admin audit gaps in §2 are closed. |
| S-2 | **Sink: a universal HTTPS webhook behind an extensible SIEM adapter interface.** v1 ships the webhook adapter. Other SIEMs are added as adapters without changing the core. |
| S-3 | **Tenant only.** Workspace admins configure export of their own workspace's events. `provider_audit_logs` is not exported in v1. |
| S-4 | **D-12 plus a hash chain.** The audit tables become insert-and-read-only for the app's DB role, and each stream is hash-chained so a SIEM can detect edits, deletions and gaps. |

Constraints already decided elsewhere still apply:

- D-14: provider actions never appear in a tenant's stream.
- D-11 and D-13: tenant audit is exported, then purged, through a privileged path.
- D-02: single controller instance.
- No secrets are written to any audit or export payload.

## 4. Proposed design

### P-1. Two streams per workspace

| Stream | Source table | Content |
|---|---|---|
| `admin` | `audit_logs` | who changed what |
| `access` | `connector_logs` | who reached which resource: allow, deny or error |

Each stream is ordered and hash-chained on its own. Provider rows live in another table, so D-14 holds by construction.

### P-2. Hash chain and the export cursor come from one mechanism

Migration `051_audit_chain.sql` does the following:

- It adds a table `audit_stream_heads (workspace_id, stream, last_seq bigint, last_hash bytea)`. It has one row per workspace and stream, and the row is created lazily.
- It adds `seq bigint`, `prev_hash bytea` and `row_hash bytea` to `audit_logs` and `connector_logs`, with `UNIQUE (workspace, seq)`.
- It adds a `BEFORE INSERT` trigger (`SECURITY DEFINER`) on both tables. The trigger:
  - locks the head row (`FOR UPDATE`);
  - sets `seq = last_seq + 1` and `prev_hash = last_hash`;
  - computes `row_hash = sha256(prev_hash || canonical(row))`;
  - updates the head.
- The genesis `prev_hash` is `sha256('zecurity-audit-v1:' || workspace_id || ':' || stream)`.

Why this works:

- **Every writer is covered without code changes.** That includes `audit.Record`, `RecordTx`, SCIM, device trust and `handleConnectorLog`. A new writer cannot forget the chain.
- **The cursor is safe.** The head lock is held until commit, so row N+1 cannot commit before row N. Within a stream, `seq` is gap-free and in commit order. An exporter that reads `seq > cursor` can never skip a row that commits late.
  - A cursor on `created_at` (the transaction start time) or on the random UUID `id` would skip such rows.
  - Publishing an outbox event next to the best-effort `audit.Record` could diverge from the table.
  - The exporter therefore tails the tables, not the outbox.
- **Cost:** appends to one stream are serialised per workspace. Under D-02 (one controller) and current volumes this is fine. The live run measures access-event insert latency.

**Head rows exist before the first insert.** An `AFTER INSERT` trigger on `workspaces` creates both head rows. As a fallback, the chain trigger does `INSERT … ON CONFLICT DO NOTHING` and then `SELECT … FOR UPDATE`, so two first inserts can't race.

**Lock order (deadlock rule).** The head lock is always the **last** lock a transaction takes: the audit insert is the last write before commit.

- Today four paths call `RecordTx` inside a larger transaction: `member_role.go` (after `LockActiveAdmins`), `client_helpers.go` (device approval), `scim/conflict.go` and `outbox/recover.go`. All four already insert the audit row last.
- Autocommit `Record` takes only the head lock.
- So no cycle is possible. S1a adds a test that runs concurrent `Record` and `RecordTx` (member-role change) in one workspace.
- The only non-default isolation level in the controller is a read-only RepeatableRead in `provider/read_handlers.go`, which never writes audit rows.

**Canonical encoding (so a customer can recompute the chain).** `canonical(row)` is a fixed byte string:

- the fields in a fixed order, each `len:value`;
- NULL is its own marker (`-`), distinct from the empty string (`0:`);
- timestamps are UTC epoch microseconds, never `timestamptz::text`, which depends on the session's `TimeZone` and `DateStyle`;
- `details` is the exact `jsonb` text Postgres produced.

A customer's JSON parser can't reproduce the `jsonb` text form. So every exported event carries **`details_canonical`**, the exact hashed string, next to the parsed `details` object.

The `SECURITY DEFINER` functions (the chain trigger, purge and retention) pin `SET search_path = pg_catalog, public`, because they run as the owner.

### P-3. D-12: the app loses UPDATE and DELETE on audit tables

- A new role `ztna_app` holds the app's privileges.
  - Migration `051_` creates it **NOLOGIN** (an idempotent `DO` block, because roles are cluster-wide) and does all GRANTs.
  - A compose init script `zz_app_role_login.sh` in `migrations/` runs after every `.sql` file. It sets `LOGIN PASSWORD` from env (`APP_DB_PASSWORD`).
  - The DB-backed tests apply `migrations/*.sql` by glob into a fresh database (for example `resource/testdb_test.go`), so they get the role without the `.sh`. A D-12 test uses `SET ROLE ztna_app`.
- Migrations still run as the owner `ztna`.
- `ztna_app` gets normal DML on application tables. It gets only **INSERT and SELECT** on `audit_logs`, `connector_logs` and `provider_audit_logs`, and nothing direct on `audit_stream_heads`; only the definer trigger writes there.
- `ALTER DEFAULT PRIVILEGES` covers future migrations.
- The controller's `DATABASE_URL` switches to `ztna_app`. DB-backed tests that need DDL or TRUNCATE keep an owner URL.
- Retention (P-4) and the D-13 tenant purge run through `SECURITY DEFINER` functions owned by `ztna`. They are the only delete paths.
- `ON DELETE CASCADE` from `workspaces` still works, because referential actions run as the table owner.
- **What changes:**
  - the controller `DATABASE_URL` in `.env`, `.env.example` and the lab docs moves to `ztna_app`;
  - `docker-compose.yml` gains `APP_DB_PASSWORD`;
  - the CI Postgres service needs nothing new: its tests run as `ztna` and build their own databases from the `.sql` files;
  - the ~240 `*_TEST_DATABASE_URL` uses keep the owner URL;
  - everyone resets their local DB (`[schema: reset DB]`).

### P-4. Retention

- **`admin` stream:** kept forever in v1. It is small.
- **`access` stream:** a purge job deletes rows older than `ACCESS_LOG_RETENTION_DAYS` (default 90) in batches, through the definer function.
  - Before it deletes, it records the last purged `(seq, row_hash)` on the head row, so the chain stays verifiable from that checkpoint.
  - Whether the purge waits for export is open (O-2).
- If the purge removes rows a sink has not delivered, the exporter **never skips silently**. It sends one `export.gap {from_seq, to_seq, checkpoint_hash}` event, so the SIEM can tell a retention gap from tampering, and then continues from the checkpoint.

### P-5. Exporter (inside the controller)

- One worker inside the controller (D-02). It polls every 2 s for streams that have an enabled sink with `last_seq > cursor`.
- There is one cursor per sink per stream: `audit_export_cursors (sink_id, stream, last_seq, last_success_at, last_error, failing_since)`.
- A batch is up to 500 events or 1 MB, in `seq` order. The cursor advances only after the adapter acknowledges the batch.
- Delivery is **at-least-once**. Each event carries `(workspace_id, stream, seq)` so the SIEM can deduplicate.
- Retries use exponential backoff from `outbox/backoff.go`, capped at 15 min.
- A failing sink stalls only its own cursor. It never blocks other sinks or workspaces: there is a bounded worker pool, and each delivery has a 10 s timeout.
- The console shows each sink's state: last success, last error and lag (head seq minus cursor).
- A new sink starts from a chosen point: **all retained history** (default) or **now**.

### P-6. Adapter interface and the webhook adapter

```go
type Adapter interface {
    Kind() string                                 // "webhook", later "splunk_hec", ...
    ValidateConfig(cfg json.RawMessage) error
    Deliver(ctx context.Context, b Batch) error   // nil = acknowledged
}
```

- Adapters are registered by `Kind` at startup.
- An adapter receives the canonical events and formats them its own way.
- Shared code, not the adapter, handles HTTP: an SSRF-safe client, timeouts and no redirects.

**Webhook adapter (v1):**

- `POST` with an `application/json` body: `{"schema":"zecurity.audit.v1","workspace_id":…,"stream":…,"events":[…]}`.
- Signature header: `X-Zecurity-Signature: t=<unix>,v1=<hex HMAC-SHA256(key, t + "." + body)>`, plus `X-Zecurity-Delivery: <uuid>`.
- Optional extra static auth header, for example `Authorization: Splunk <token>` for collectors that require one.
- Any 2xx response is an acknowledgement. Anything else is retried.
- **HTTPS only. SSRF guard:**
  - the target's IPs are checked at dial time (a custom `DialContext`, which defeats DNS rebinding);
  - loopback, private, link-local, CGNAT, multicast and unspecified addresses are rejected;
  - redirects are not followed.
  - A dev-only override (`AUDIT_EXPORT_DEV_ALLOW_PRIVATE`, honoured only when `ENV=development`) exists for the lab, like `GEOIP_DEV_OVERRIDES`.
- **Secrets:**
  - the HMAC key is generated by the server and shown **once**, on create and on rotate (like SCIM tokens);
  - the HMAC key and the extra auth header value are sealed with `pki.EncryptSecret`;
  - they are write-only, and never logged, audited or returned.

### P-7. Event format: `zecurity.audit.v1`

Common envelope:

```
workspace_id, stream, seq, prev_hash, row_hash, occurred_at, recorded_at,
action, outcome, actor{type,user_id,email}, target{type,id},
subject{user_id,device_id,spiffe_id} (access only), details{…}, details_canonical, canonical
```

- `details_canonical` is the exact hashed `jsonb` text.
- `canonical` is the whole hashed byte string (P-2), base64-encoded. A verifier checks `row_hash = sha256(prev_hash || canonical)` and needs no Postgres knowledge.

- **`admin` events:**
  - the `action`, target and `details` map straight from the row;
  - `actor.type` is `user` or `system`.
- **`access` events:**
  - `action` is `resource.access`;
  - `outcome` is `allow`, `deny` or `error`;
  - `details` holds `{resource_id, destination, port, protocol, route_type, connector_id, error}`.
- The schema is versioned. Fields are only ever added, never changed.

### P-8. Configuration API and console

- New GraphQL operations:
  - `auditExportSinks`;
  - `createAuditExportSink` and `updateAuditExportSink` (name, kind, URL, streams, enabled);
  - `rotateAuditExportSinkSecret`;
  - `deleteAuditExportSink`;
  - `testAuditExportSink`, which sends one signed `export.test` event.
- At most 3 sinks per workspace.
- Every sink change is itself audited in the `admin` stream (`audit_export.sink.create`, etc.), with no secrets in `details`.
- **Admin console:** an "Audit export" section under Settings: the sink list, a create/edit dialog, a one-time secret display, a test button, and status and lag.
- The capability is open (O-3).

### P-9. Closing the admin-audit gaps (S-1 "complete")

New audited actions:

- `auth.sign_in`, `auth.sign_in_failed` (no account-existence leak in `details`) and `auth.sign_out`;
- `invitation.accept`;
- `device.enroll`;
- `workspace.create`;
- `idp.test`.

No passwords, tokens or codes go into `details`.

### P-10. Explicitly not in v1

- `provider_audit_logs` export (S-3).
- External anchoring of chain heads (for example, periodic signed checkpoints).
- WORM or S3 archive.
- Syslog.
- OCSF formatting.
- Session-close and byte-count access events. These need a connector and proto change.
- Lossless connector-side buffering of access events.
- Cross-replica coordination (D-02).

## 5. Open questions (need answers before build)

| # | Question | Recommendation |
|---|---|---|
| O-1 | D-12 via a real **separate login role** (`ztna_app`, every env and test URL changes) or via `SET ROLE` on connect (cheap, but the owner can `RESET ROLE`, so it stops bugs, not an attacker with SQL)? | **Separate login role.** That is what D-12 says. The hash chain covers the rest. |
| O-2 | Should access-event retention **wait for export**? Purging unexported rows loses events while a sink is down. Never purging lets a dead sink grow the table without limit. | Purge rows older than 90 days, whatever their export state, but warn in the console once a sink's lag passes 7 days. |
| O-3 | Who may manage sinks? There are three options: reuse `IDENTITY_ADMIN` (Admin only), add a new `AUDIT_ADMIN` capability (Admin + Security), or reuse `MEMBERS_ADMIN`. | **New `AUDIT_ADMIN` for Admin and Security.** Security teams own SIEM wiring. It touches the PENDING-19 role table. |
| O-4 | Which adapters ship in v1: the webhook only, or the webhook plus **Splunk HEC** so the adapter interface is proven by a second real adapter? | **Webhook + Splunk HEC.** HEC is the most common target, and a second adapter keeps the interface honest. |
| O-5 | Should a tenant-facing **Audit Log page** (read `audit_logs` in the admin console) ship with v1? Today tenants can't see their own audit trail at all. | **Yes, read-only, `CONSOLE_READ`.** It is small and lets admins check what is being exported. |
| O-6 | Access-event loss when the control stream is down. Accept it in v1 and document it, or add a connector-side bounded buffer (a connector change, no proto change)? | **Accept and document for v1**, and open a follow-up. |
| O-7 | Are **end-user client sign-in events** (client login, token refresh denied, sign-out, `internal/auth` serves both console and client) in v1, or only admin console sign-in? SIEMs usually correlate them with access events. | **Yes, include them** in the `admin` stream as `auth.*` with `actor.type=user` and a `client` flag. The volume is low. |

## 6. Build plan (after approval)

One PR, `[schema: reset DB]`, migration `051_`:

1. **S1a, DB:** chain trigger and heads, D-12 role split, definer purge and retention functions. Tests cover chain continuity under concurrent inserts, rejected UPDATE and DELETE as `ztna_app`, and a purge checkpoint.
2. **S1b, backend:** the P-9 audit gaps, the exporter worker and cursors, the adapter registry, the webhook adapter (plus HEC if O-4), the SSRF client, sealed secrets, the GraphQL sink API, and a chain verifier (`zecurity-controller audit verify --workspace …`).
3. **S2, console:** the Audit export settings and, if O-5, the Audit Log page.
4. **S3, live run:** a real connector and client produce access events. The receiver's certificate is trusted through a dev-only CA-bundle setting (`AUDIT_EXPORT_DEV_CA_FILE`, `ENV=development`), next to the private-IP override. A local HTTPS receiver checks signatures, seq continuity and hash recomputation. Then: kill the receiver and confirm retry and catch-up with no gaps or duplicates beyond the at-least-once overlap; attempt UPDATE as `ztna_app` (refused); edit a row as the owner (the verifier reports the break).

---

## History

Original pending ADR (2026-07-03) weighed three options: a first-party audit log with export (A), the observability pipeline (B), and access events only (C). It recommended A. The 2026-09-01 correction found that the audit tables exist but have no export. Decision Record D-12 (2026-09-23) chose DB-enforced append-only and deferred hash chaining and SIEM export to this phase. This note is option A, with S-1 … S-4 above.
