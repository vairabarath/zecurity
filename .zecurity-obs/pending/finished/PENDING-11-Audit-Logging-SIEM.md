---
type: adr
status: implemented
id: PENDING-11
domain: operations
priority: P2
created: 2026-07-03
updated: 2026-11-12
related:
  - PENDING-10-Observability
  - PENDING-15-Durable-Outbox-Infrastructure
  - PENDING-19-Workspace-RBAC
tags: [pending, finished, adr, operations, audit, siem, compliance]
---

# PENDING-11: Audit logging and SIEM export

> **Implemented (2026-11-12), PR #187** `[schema: reset DB]`: S-1 … S-11 and the §9 Twingate-style delivery (W-1 … W-8). What shipped, where it lives and how it was verified: **§10 Implementation summary**. The status lines below are the build history.

**Status:** approved 2026-11-05 (Sathiya accepted O-1 … O-7 as recommended: S-5 … S-11). Build: S1a → S1b → S2 → S3, one PR. **S1a built 2026-11-05** (fc3561c1 on local `feat/pending11-siem`: migration 051, `ztna_app`, `audit.VerifyStream`; gate 1719/0/2). Lock order: workspace FOR KEY SHARE, then head (found while testing: the FK check after a BEFORE trigger would otherwise deadlock against a workspace FOR UPDATE + audit transaction).
**S1b built 2026-11-06** (local `feat/pending11-siem`): migration `052_audit_export.sql`, P-9 audit gaps, `internal/auditexport` (exporter, webhook + Splunk HEC, SSRF-safe client, sealed secrets), GraphQL sink API + `auditLogs` behind `AUDIT_ADMIN` / `CONSOLE_READ`, access-event retention worker, `zecurity-controller audit verify`. Decisions taken while building are in §7. **S2 built 2026-11-06** (b4565fde): `/audit-log` page and Settings → Audit export. **S3 live run PASS 2026-11-06** (§8).
**Base:** `main` (89655c83 at design time; `main` merged in before the PR merged). Migrations: `053_audit_chain.sql`, `054_audit_export.sql`, `055_audit_export_s3.sql` (built as 051 … 053, renumbered 2026-11-12 after `main` took 051/052 for F17 and F5).
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

### Approved 2026-11-05 (the open questions in §5, all as recommended)

| # | Decision |
|---|---|
| S-5 (O-1) | D-12 via a **separate role `ztna_app`** (created NOLOGIN in `051_`, given LOGIN by `zz_app_role_login.sh`). |
| S-6 (O-2) | Access events are purged after **90 days whatever their export state**. The exporter sends `export.gap`; the console warns once a sink is 7 days behind. |
| S-7 (O-3) | New capability **`AUDIT_ADMIN`** for the Admin and Security roles. |
| S-8 (O-4) | v1 adapters: **webhook + Splunk HEC**. |
| S-9 (O-5) | A read-only tenant **Audit Log page** (`CONSOLE_READ`) ships in v1. |
| S-10 (O-6) | Access-event loss while the control stream is down is **accepted and documented** for v1; follow-up opened. |
| S-11 (O-7) | **Client sign-in events** are in v1 (`auth.*` in the `admin` stream, with a `client` flag). |

## 4. Design

### P-1. Two streams per workspace

| Stream | Source table | Content |
|---|---|---|
| `admin` | `audit_logs` | who changed what |
| `access` | `connector_logs` | who reached which resource: allow, deny or error |

Each stream is ordered and hash-chained on its own. Provider rows live in another table, so D-14 holds by construction.

### P-2. Hash chain and the export cursor come from one mechanism

Migration `053_audit_chain.sql` (built as 051, renumbered at merge) does the following:

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
- `ztna_app` may **not delete workspaces**: the `ON DELETE CASCADE` from `workspaces` would remove the workspace's audit streams, bypassing D-12 and D-13. Tenant deletion (D-34, not v1) will run through an owner `SECURITY DEFINER` routine that exports first. (Found by the S1a review; the draft had treated the cascade as fine.)
- **Startup guard:** the controller checks its own privileges (`audit.CheckAppendOnly`). If it can UPDATE, DELETE or TRUNCATE an audit table, or delete workspaces, it refuses to start outside `ENV=development` and logs a warning in development.
- **What changes:**
  - the controller `DATABASE_URL` in `.env`, `.env.example` and the lab docs moves to `ztna_app`;
  - `docker-compose.yml` gains `APP_DB_PASSWORD`;
  - the CI Postgres service needs nothing new: its tests run as `ztna` and build their own databases from the `.sql` files;
  - the ~240 `*_TEST_DATABASE_URL` uses keep the owner URL;
  - everyone resets their local DB (`[schema: reset DB]`).

### P-4. Retention

- **`admin` stream:** kept forever in v1. It is small.
- **`access` stream:** a purge job deletes rows older than `ACCESS_LOG_RETENTION_DAYS` (default 90) in batches, through the definer function `audit_purge_access(workspace, cutoff, batch)`.
  - **One workspace per call**, in a short transaction, so a purge stalls only that workspace's access inserts, and only for one batch.
  - **90-day floor enforced in the database:** a cutoff newer than `now() - 90 days` is refused. `ACCESS_LOG_RETENTION_DAYS` can only lengthen retention, so an injected call can't wipe the stream.
  - The purge deletes only a **prefix** of the stream (it stops at the first row newer than the cutoff), so a back-dated insert can't expose earlier rows.
  - Every purge appends an `audit.access_purged` row (from/through seq, row count, checkpoint hash) to the workspace's `admin` stream.
  - Lock order: workspace, `admin` head, `access` head.
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

## 5. Open questions (answered 2026-11-05, see S-5 … S-11)

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

## 7. Build decisions (S1b, 2026-11-06)

These narrow or pin down the design above; none adds an architectural choice.

- **`auth.sign_in_failed` scope (narrows P-9).** `audit_logs.tenant_id` is NOT NULL, and the admin stream is kept forever, so letting unauthenticated callers append to it would be a storage-DoS path. A refused sign-in is recorded only after the IdP has proven the identity **and** the connection is the workspace's own (enterprise) connection, for "not invited" and "account not active" alike, with one reason (`not_permitted`) so the row never tells the two apart. Failures before that (bad state, expired state, IdP error, platform connection) are not audited. Same rule on the client path (`TokenExchange`), where a platform connection would otherwise let any IdP account name any workspace.
- **`auth.sign_out`** is recorded only for a session the call actually ended, so replaying a token can't flood the stream. Console and client share `EndSession`; the client path marks its context (`auth.WithClientChannel`) for `details.client`.
- **Backoff:** `outbox.RetryPolicy{Base: 2s, Max: 15m}.Backoff(n)`, without `Validate` (which caps at the outbox's 5 min). The outbox constant is unchanged.
- **Adapter signature:** `Deliver(ctx, *http.Client, Destination, Batch)`: the sink's decrypted URL, signing key, auth header and config travel in `Destination`, and the shared SSRF-safe client is passed in, so adapters never build their own.
- **Splunk HEC:** the token is stored as the sealed auth header `Authorization: Splunk <token>`; index, source and sourcetype are the non-secret config. HEC requests are also signed (harmless).
- **A stored credential doesn't follow a sink to a new URL.** Any URL change on a sink that has an auth header or HEC token requires entering it again; otherwise an `AUDIT_ADMIN` user could point the sink at their own server (or another path on a shared relay host) and capture a token they never saw (found by the background security review).
- **"Now" start:** the cursor is set to the head before the sink's own `audit_export.sink.create` row is written, so that row is the first admin event a "now" sink receives.
- **Canonical bytes are built in Go** (`audit/canonical.go`) from the raw columns, for the verifier and the exporter; a test pins them to the database's `audit_*_canonical`. Neither trusts the database's hashing function.
- **Error text** stored in `last_error` and returned by `testAuditExportSink` never carries the URL (a query string may hold a token), headers or the response body. Audit rows for sink changes carry only the URL's scheme and host: a path or query may hold a token, and audit rows are readable by every console role and exported.
- **gqlgen** fails with Go 1.27 (`package "time" without types`); regenerate with `GOTOOLCHAIN=go1.26.2 go tool gqlgen generate --config graph/gqlgen.yml`, then revert the `go.mod`/`go.sum` tidy it does.

## 8. S3 live run (2026-11-06): PASS

**Setup:** branch build (2261aed8 code).
- **Database:** the shared local DB was reset (Sathiya's OK), so 051 and 052 ran on a fresh volume, and the controller ran as `ztna_app`. No append-only warning; no permission errors in Postgres or the controller log.
- **Workspace:** `p11live`, created by sign-up through a platform OIDC connection to the local mock IdP.
- **Connector:** branch build, in Docker; resource 172.31.11.20:9001 inside its namespace.
- **Client:** branch build (`dev-build.sh`), running as a root sandbox daemon.
- **Receiver:** a local HTTPS receiver (lab CA through `AUDIT_EXPORT_DEV_CA_FILE`, plus `AUDIT_EXPORT_DEV_ALLOW_PRIVATE`). It checked, for each event:
  - the HMAC signature;
  - seq continuity per sink and stream;
  - prev_hash links from genesis;
  - `row_hash = sha256(prev_hash || canonical)`;
  - that `details_canonical` is the hashed text.

| Check | Result |
|---|---|
| Sign-up audit | `workspace.create`, `identity.user.provisioned`, `auth.sign_in {client:false}` |
| Webhook sink, all history, both streams | admin 1..5 delivered and verified |
| `testAuditExportSink` | `export.test` delivered and verified |
| Splunk HEC sink (admin) | `Authorization: Splunk <token>`; index set; replayed from seq 1 on its own cursor |
| Client login and enroll | `auth.sign_in {client:true}` and `device.enroll` exported |
| Real access through the connector | `resource.access` / `allow` with subject user, device and SPIFFE; destination and port |
| Receiver down (20 accesses, 3 admin changes) | per-stream `lastError` "could not connect to the destination" (no URL), `failingSince`, backoff 5 attempts → 26 s |
| Receiver back | caught up on the next retry: access 4..23 and admin 18..20 on both sinks; **0 gaps, 0 duplicates, 0 errors** (63 events total) |
| UPDATE `audit_logs` / DELETE `connector_logs` / DELETE workspace as `ztna_app` | permission denied |
| `audit_purge_access(ws, now(), …)` as `ztna_app` | refused (inside the 90-day floor) |
| `audit verify` before tampering | admin 1..20 OK, access 1..23 OK, exit 0 |
| Owner edits admin seq 17, deletes access seq 10 | `audit verify`: seq 17 "row edited", seq 10 missing, exit 3 |

**Console live run (2026-11-06, Orca browser):** branch controller and admin console (vite) on the reset DB, signed in as the `p11live` admin through the mock IdP. All checks passed.

| Check | Result |
|---|---|
| Audit Log page | rows newest first (the browser sign-in as seq 21); action filter `auth.` → seq 21, 16, 3; details expand as formatted JSON |
| Paging (62 rows) | 62..13, then Older → 12..1 (Older disabled), Back to newer → 62..13 |
| Audit export list | both sinks with per-stream status, seq and last delivery |
| Test button | toast "Test event delivered."; `export.test` reached the receiver; while failing: "Test delivery failed: destination answered HTTP 503" |
| Rotate secret | secret shown once; gone after "I have copied it"; not in the Apollo cache, localStorage or sessionStorage; the receiver verified signatures with it |
| Edit the Splunk URL without the token | form error "Changing the URL requires entering the Splunk HEC token again."; the server refuses the same change; saved once the token was re-entered; the audit row has `https://127.0.0.1:9443` only, and no token appears in `audit_logs` |
| Add sink (webhook, auth header, admin only, "Only new events") | secret dialog; first event delivered was its own `audit_export.sink.create`; Add sink disabled at 3 |
| Receiver answering 503 | every sink shows "Failing", 1 pending and "destination answered HTTP 503"; it recovered to "up to date" |
| Pause | "paused" pill; no deliveries while paused |
| Delete | the confirm dialog explains the consequence; the sink is removed; Add sink is enabled again |
| Role = Viewer (live role) | Audit Log visible; Audit export hidden; `auditExportSinks` refused "requires capability AUDIT_ADMIN" |
| Role = Security | Audit export visible with both sinks (S-7); role restored to Admin |

**Not covered live** (covered by integration tests): the retention `export.gap` (needs 90-day-old rows) and refused sign-ins on an enterprise connection.

### Wazuh live run (2026-11-06): PASS

**Setup:**
- **Controller:** branch build 9a7bc9fc (sha checked against the running process), on the same DB as the S3 run (`p11live`; admin head 75, access head 23).
- **Wazuh:** single-node `wazuh/wazuh-manager:4.14.10` in Docker. Manager only: the indexer and dashboard didn't fit on the lab disk, so results were read from `alerts.json` and `archives.json` (`logall_json` on). Vulnerability detection, SCA and the indexer connector were off.
- **Path:** controller webhook sink → a local HTTPS bridge → Wazuh server API `POST /events`.
  - The bridge verifies `X-Zecurity-Signature`, checks the chain per stream and forwards one JSON string per event, in chunks of ≤ 100, with a JWT.
  - It answers 2xx only after Wazuh accepted every chunk, otherwise 502, so a Wazuh outage reaches the exporter.
  - Wazuh has no HTTPS webhook or HEC receiver of its own, so a bridge is needed on this path.
- **Rules:** custom `local_rules.xml` (100200 …). The Wazuh JSON decoder decodes events with no custom decoder. The top-level `action` is Wazuh's static `action` field, so rules match it with `<action>`. Nested keys flatten (`actor.email`, `details.destination`, `subject.user_id`).
- The two S3 sinks (dead receiver) were paused. A third webhook sink `wazuh` was created: all history, both streams.

| Check | Result |
|---|---|
| Full-history replay on sink create | access 1..23 (22 rows) and admin 1..75 delivered on the first poll; one alert per event: 100210 access allow, 100221 sign-in, 100223 export change (level 10), 100226 access-control change (level 8), 100220 other admin |
| Chain verified from Wazuh's own copy (`archives.json` `full_log`, recomputed `sha256(prev_hash ‖ canonical)`) | every link holds except the S3 tamper rows: admin seq 17 "row_hash mismatch" (edited) and access seq 10 missing (deleted); 0 duplicates. The SIEM copy alone shows the tampering |
| Live admin change (`updateAuditExportSink`) | seq 78, 79 alerted ~1.2 s after `recorded_at` (2 s poll) |
| `testAuditExportSink` | `export.test` delivered synchronously, alerted in ~0.2 s (rule 100201: it has no seq or row_hash) |
| Wazuh manager stopped; 3 sign-ins and 3 sink changes made meanwhile | `lastError` "destination answered HTTP 502" (no URL), `failingSince` set, cursor held at 79 (lag 6); retries at +1, +4, +6, +10, +18 s; `testAuditExportSink` answered "destination answered HTTP 502" |
| Manager started | caught up on the next retry: admin 80..85 in one batch; lag 0, error cleared; chain 1..85 with no new gaps and 0 duplicates |
| Forged signature to the bridge | 401, nothing forwarded |
| Correlation rule 100213 (5 denials in 120 s for one `subject.user_id`, level 12) | fires on the 5th deny; checked with `wazuh-logtest` only (no connector or client in this run, so no live deny events) |

**Not covered in this run:** live access-deny events (the replayed access history has only `allow`), and the Splunk HEC kind into Wazuh (Wazuh has no HEC endpoint).

## 9. Addendum: Twingate-style SIEM delivery (approved 2026-11-06)

**Ask (Sathiya, 2026-11-06):** "I want the Twingate-style SIEM, nothing else", after the Wazuh live run showed that push needs a bridge in front of Wazuh.

**How Twingate does it.** Two channels; nothing is pushed into the customer's SIEM.

1. **Control-plane events: files in the customer's S3 bucket.**
   - Audit logs, network events and DNS logs are synced as NDJSON about every 5 minutes.
   - Each line carries an `event_type`.
   - The SIEM pulls from the bucket.
   - Auth is OIDC role federation or an IAM user.
   - There is no webhook, no SIEM push and no documented tamper evidence.
2. **Data-plane events: the connector's own output.**
   - With `TWINGATE_LOG_ANALYTICS=v3`, the connector prints `ANALYTICS {json}` lines to stdout/journald.
   - The customer's log agent on the connector host collects them. For Wazuh, that is the Wazuh agent.

Wazuh supports both channels natively:
- its agent reads journald and files (`log_format journald` in 4.14);
- its `aws-s3` module pulls "Custom Logs Buckets" (S3 + SQS notifications). That reader (`wodles/aws/subscribers/s3_log_handler.py`, v4.14.0) parses NDJSON, has a dedicated `.jsonl.gz` path, and nests each event's fields under `aws.*`.

**Decisions W-1 … W-8 (approved 2026-11-06, Sathiya, as proposed; live test: full path via LocalStack):**

- **W-1 Scope.** Build these two channels only.
  - No verify tool, Wazuh rules pack or bridge.
  - The webhook and Splunk HEC sinks from S1b stay as shipped.
- **W-2 Connector real-time access log.**
  - Turned on with `ZECURITY_LOG_ANALYTICS=v1` (`config.rs`); off by default.
  - `emit_access_log` in `device_tunnel.rs` is the single place a `ConnectorLog` is built, so every access decision goes through it, relay-routed ones included. There it also writes one line, `ANALYTICS {json}`, to stdout:
    - written whole, under the stdout lock;
    - not through `tracing`, which would prepend a timestamp and level.
  - Fields:
    - `schema` = `zecurity.connector.analytics.v1`;
    - `event_type` = `access_decision`;
    - `timestamp` (RFC 3339);
    - `decision` (`allow` / `deny` / `error`) and `error`;
    - `resource{id, destination, port, protocol}`, `route_type`;
    - `client{spiffe_id}` plus the id parsed from it (`client/{id}` or `client_device/{id}`);
    - `connector{id}`.
  - **Not in v1:**
    - user email, which the connector doesn't know (adding it is a proto change);
    - Twingate's established/closed pairs with rx/tx/duration, since the connector doesn't track connection end today.
  - The line is written even when the control stream is down, so the O-6 loss (S-10) no longer applies to the local copy.
  - These lines are unchained. The chained copy is still the controller's `access` stream.
  - **No proto, controller or shield change.**
- **W-3 `s3` sink kind.**
  - Non-secret config: `bucket`, `region`, `prefix`, an optional `endpoint` for S3-compatible stores (path-style), and `access_key_id`.
  - The secret access key goes in the existing sealed auth-header slot, like the HEC token.
  - **Static IAM user keys only.** OIDC role federation would need the controller to act as a public OIDC issuer, so it is deferred.
  - Any change to bucket, region, endpoint or access key id requires the secret again (the same rule as a HEC URL change, §7).
  - The docs recommend an IAM policy of `s3:PutObject` on the prefix only.
- **W-4 Object format and key.**
  - gzip NDJSON with one `zecurity.audit.v1` event per line, unchanged; the `stream` field plays Twingate's `event_type` role.
  - Key: `<prefix>/zecurity/v1/<workspace_id>/<stream>/YYYY/MM/DD/HH/<first_seq>-<last_seq>.jsonl.gz`, where the hour is the first event's `recorded_at` in UTC.
  - Delivery stays at-least-once. A retry that picks up more events writes a different key, so consumers deduplicate on `(workspace_id, stream, seq)`.
- **W-5 Five-minute batching, stateless.**
  - The exporter takes an `s3` batch only when the event at cursor+1 has a `recorded_at` at least 5 minutes old, or when 500 events are pending.
  - There is no in-memory timer, and nothing changes for the other kinds.
  - `testAuditExportSink` writes one `export.test` object at once.
- **W-6 SigV4, hand-written.**
  - One operation: `PutObject` with `x-amz-content-sha256`, about 150 lines, tested against AWS's published SigV4 test vectors.
  - It goes through the shared SSRF-safe client (P-6). The default endpoint is `https://<bucket>.s3.<region>.amazonaws.com`.
  - This avoids adding aws-sdk-go-v2 and keeps the dial-time SSRF guard in charge.
- **W-7 Schema and console.**
  - `055_audit_export_s3.sql` (built as 053) drops the kind CHECK from 054 and adds it back with `'s3'` (052 itself is never edited; `[schema: reset DB]`).
  - The GraphQL enum gains `S3`.
  - The console's Audit export form gains an "Amazon S3" kind.
- **W-8 Docs.** One setup section per channel, like Twingate's:
  - connector env var + Wazuh `localfile` (journald) + the decoder snippet. A prematch on `ANALYTICS ` with `plugin_decoder` `JSON_Decoder` at `offset="after_prematch"`, proved with `wazuh-logtest`.
  - bucket + IAM policy + the Wazuh `aws-s3` custom-bucket block.

**Live test.**
- **Channel 1:** a real connector with the Wazuh agent reading journald (or the Docker log) into a Wazuh manager.
- **Channel 2, the full path needs room:** LocalStack (S3 + SQS + STS on one endpoint) plus the manager's `aws-s3` wodle with `service_endpoint`. That needs about 1.5 GB more on `/` than the 3.8 GB free, on top of the Wazuh image saved in `~/dev/docker-images`.
- **Channel 2 fallback:** MinIO for the PutObject path, then the produced `.jsonl.gz` fed to the manager.

### Build (2026-11-06, local branch `feat/pending11-twingate-siem`, stacked on #187)

- **d71d2b10, connector (W-2):** `analytics.rs`; the line is emitted at the single `emit_access_log`. 338 connector tests pass.
- **863ecf4f, controller (W-3 … W-7) `[schema: reset DB]`:**
  - `s3.go` (adapter, key, gzip NDJSON, `URLDeriver`) and `sigv4.go` (hand-written PutObject signing);
  - exporter `Windowed` batching;
  - store rules: URL derived from config; secret required; secret again on a bucket, region, endpoint or access key ID change; no URL, ClearAuth or auth header on S3;
  - `055_audit_export_s3.sql`; GraphQL `S3` kind, `S3Input` and `s3*` fields.
  - Gate: 1753 pass / 0 fail / 2 skip (the known two).
  - SigV4 is checked against AWS's published get-vanilla and S3 PUT examples, and against botocore.
  - The wire path is the signed path. Prefixes are limited to `A-Z a-z 0-9 / _ . -`, since botocore sends `+` literally while an encoder would sign `%2B`.
- **6f371273, console (W-7):** Amazon S3 in Settings → Audit export. 294 admin tests pass; lint unchanged (17 pre-existing).
- **Docs (W-8):** `docs/siem-export.md`.

**Build decisions (within W-1 … W-8):**
- The object carries `x-amz-meta-zecurity-signature`, the sink's HMAC of the gzip body, so the signing secret still means something on S3.
- In the API, `authHeaderName` is null for S3 sinks: the secret's storage slot is not a header.
- When the window isn't due, only `next_attempt_at` moves, to the moment the oldest event turns 5 minutes old (only when `attempts = 0` and the cursor is unchanged).
- Wazuh needs two decoders: one for plain lines, and one bound to `program_name` `zecurity-connector`. Once a syslog header names a program, Wazuh no longer tries decoders without a `program_name`.

### Live run (2026-11-06): PASS

**Setup:** an isolated stack, so the shared DB was not reset.
- **Database:** a throwaway Postgres on 55432 built from every schema file (`053_` ran clean), with the controller as `ztna_app`. Branch controller build (sha checked), workspace `p11s3` (sign-up through the local test identity provider), Valkey DB 14.
- **S3:** moto 5.1.4 (S3 + SQS + STS) behind a lab-CA TLS front. IAM was enforced after the 10 setup calls:
  - writer key: `s3:PutObject` on `zecurity-audit/siem/*` only;
  - reader key for Wazuh: GetObject/ListBucket, SQS and GetCallerIdentity.
- **Wazuh:** `wazuh/wazuh-manager:4.14.10`, manager only (no disk for the indexer).
  - `aws-s3` `<subscriber type="buckets">`. SQS and STS were pointed at moto through `endpoint_url` in `/root/.aws/config` (boto3 1.34), because the module passes `service_endpoint` to S3 only.
  - `localfile` on the connector's log file.
- **Connector:** branch build, in a container on the moto network (`postgres:15` image for glibc). A connector on the same host as the client loops: the client captures the connector's own dial to the resource.
- **Client:** sandbox root daemon, rebuilt to embed the isolated stack's platform root.
- **Resource:** moto 172.20.0.2:5000.

| Check | Result |
|---|---|
| moto enforcement | wrong key `InvalidAccessKeyId`, wrong secret `SignatureDoesNotMatch`, write outside the prefix `AccessDenied` |
| S3 sink create (GraphQL) | URL derived (`https://127.0.0.1:9445/zecurity-audit`), `authHeaderName` null |
| 5-minute window | no PUT for young events; cursor `next_attempt_at` ≈ 4 min 42 s; then admin 1..8 in one object `1-8.jsonl.gz` |
| `testAuditExportSink` | `export.test-<id>.jsonl.gz` written at once, HTTP 200 under IAM enforcement (the hand-written SigV4 accepted) |
| Wazuh pull | bucket notification → SQS → `aws-s3` module; location `Wazuh-AWS`, Zecurity fields under `data.aws.*` |
| S3 down (TLS front stopped), 4 admin changes | after the window: `lastError` "could not connect to the destination" (no URL), cursor held at 8, backoff |
| S3 back | `9-12.jsonl.gz` on the next retry; lag 0, error cleared |
| Chain from Wazuh's copy (`archives.json`, `data.aws.*`) | admin 1..22 over 3 objects and access 1..124 in `1-124.jsonl.gz`: no gaps, every `row_hash` and link OK, 0 duplicates |
| Connector `ANALYTICS` lines, real tunnels | 3 allowed requests (HTTP 200) → 3 `allow` lines; access removed while a request loop ran → `deny no_acl_match` from the connector; Wazuh decoded all (`zecurity-connector-analytics`) with every field |
| Controller stopped for 12 s, 3 requests | 3 lines written and decoded by Wazuh during the outage. The controller copy also arrived after reconnect (`connector_logs` 121..124), because the connector queued them |
| S3 access stream vs connector lines | the same decisions in both: 123 `allow`, 1 `deny` |
| Decoders, `wazuh-logtest` | plain line and the journald (syslog-header) form both decode |

**Post-run fix (review, 2026-11-06): the full-batch half of W-5 now works in the polling loop.**
- **Bug:** a cursor rescheduled for its window was skipped by `dueCursors` (`next_attempt_at <= now()`) however many events piled up, and exactly 500 pending counted as "not full". The integration test had called `DeliverOnce` directly and missed it.
- **Fix:**
  - `dueCursors` also lists a cursor with `attempts = 0` and ≥ `MaxBatchEvents` pending;
  - a batch of `MaxBatchEvents` counts as full.
- **Test:** `TestS3Sink_FullBatchWakesARescheduledCursor` goes through `dueCursors`. It fails on the old code.
- **Other changes:** HTTP 400 is no longer worded as a region problem, and the docs add the `/root/.aws/config` region the Wazuh module requires.
- **Gate:** 1754 pass / 0 fail / 2 skip.

**Post-run fixes (security review, 2026-11-12):**
- **Connector, a stalled log reader could stall tunnels.** `analytics::emit` wrote to stdout under its lock from the async tunnel task; a log reader that stopped reading would block it. Now `emit` does a non-blocking `try_send` into a bounded queue (`QUEUE_CAPACITY` 4096) drained by an `analytics-writer` thread; a full queue drops the line and counts it, and the writer thread logs the count. Test `full_queue_drops_and_counts_instead_of_blocking`.
- **Store, a region change kept the secret on an S3-compatible endpoint.** The secret-again rule compared only the access key ID plus the derived URL, and with an endpoint the region isn't in the URL. Now `s3Target` compares bucket, region, endpoint and access key ID, matching the API, console and docs. Not a leak (the key still went to the same host), but the rule didn't match what it said. `TestS3Sink_UpdateCredentialRules` gains a `region` case that fails on the old code.

**Merge with main (2026-11-12): migrations renumbered.** Main took 051 (`051_provider_audit_append_only.sql`, F17) and 052 (`052_relay_bandwidth.sql`, F5) while this work was in review, so ours became `053_audit_chain.sql`, `054_audit_export.sql` and `055_audit_export_s3.sql` (built and live-run as 051 … 053; contents unchanged apart from cross-references). F17's append-only triggers and this note's `ztna_app` REVOKE on `provider_audit_logs` both apply; F5's new `relays` columns get `ztna_app` DML through the default privileges. Gate after the merge: 1782 pass / 0 fail / 2 skip (one timing flake in `TestDisconnectWatcher_RevokedUntouchedNoNotify` under full-suite load, 3/3 pass on rerun, untouched code); admin 294 pass.

**Not covered live:**
- the journald `localfile` block on an agent (only the decoder was checked, with logtest);
- the Docker journald log driver;
- the 500-event "full batch" path (integration test only);
- real AWS (moto stood in, with IAM enforcement on).


## 10. Implementation summary (as merged, PR #187)

### What shipped

| Area | What | Where |
|---|---|---|
| Hash-chained streams (S-1, S-4) | Two streams per workspace: `admin` (`audit_logs`) and `access` (`connector_logs`). A BEFORE INSERT trigger takes the workspace FOR KEY SHARE, then the `audit_stream_heads` row, and stamps a gap-free, commit-ordered `seq` plus `row_hash` (SHA-256 over canonical bytes and the previous hash). `seq` is also the export cursor. | `053_audit_chain.sql`; `internal/audit/chain.go`, `canonical.go` |
| D-12 append-only (S-5) | Runtime role `ztna_app` (NOLOGIN in SQL, login via `zz_app_role_login.sh` with `APP_DB_PASSWORD`): no UPDATE/DELETE/TRUNCATE on `audit_logs`, `connector_logs`, `provider_audit_logs`; SELECT only on heads; no DELETE on `workspaces` (cascade bypass). Startup check `audit.CheckAppendOnly`: fatal outside `ENV=development`, warning in development. | `053_audit_chain.sql`; `internal/audit/audit.go`; `cmd/server/main.go`; `.env.example` (`DATABASE_URL` as `ztna_app`) |
| Retention (S-6) | Access events purged after `ACCESS_LOG_RETENTION_DAYS` (default and floor 90, floor enforced in SQL by `audit_purge_access`, one workspace per call, prefix-only, checkpoint kept). The admin stream is kept forever. A sink behind the purge gets one `export.gap` event. | `internal/audit/retention.go`; `053_audit_chain.sql` |
| Admin-audit gaps (P-9, S-11) | Sign-in/out, failed sign-in (own enterprise connection only), invite accept, device enrol, sign-up, `idp.test`, client sign-in events. | `internal/auth`, `internal/invitation`, `internal/client`, resolvers |
| Exporter (P-5) | In-process, polls every 2 s, up to 500 events per batch, 10 s delivery timeout, backoff on failure, at-least-once (consumers deduplicate on workspace, stream, seq). Windowed adapters (S3) wait until the oldest event is 5 min old unless a full batch is pending. | `internal/auditexport/exporter.go` |
| Adapters (S-2, S-8, W-3) | HTTPS webhook (HMAC signature header), Splunk HEC, Amazon S3 (gzip NDJSON objects `<prefix>/zecurity/v1/<ws>/<stream>/YYYY/MM/DD/HH/<first>-<last>.jsonl.gz`, hand-written SigV4 PutObject). | `adapter.go`, `adapters.go`, `s3.go`, `sigv4.go` |
| Outbound safety | One SSRF-safe client for every sink: https only, public addresses only (checked at dial; IPv6 only 2000::/3), no redirects. Secrets (auth header, HEC token, S3 secret key) sealed, write-only, never logged or audited; changing the destination requires entering the secret again. | `httpclient.go`, `store.go` |
| Sink API (S-7) | GraphQL `auditExportSinks`, create/update/delete/test/rotate, up to 3 sinks per workspace, behind `AUDIT_ADMIN` (Admin, Security); `auditLogs` behind `CONSOLE_READ`. | `graph/audit.graphqls`; `resolvers/audit_export_helpers.go`; `internal/rbac` |
| Console (S-9) | `/audit-log` page (filter, paging) and Settings → Audit export (webhook, Splunk HEC, Amazon S3; test, rotate, pause, status). | `admin/src/pages/AuditLog.tsx`, `components/audit/` |
| Verify CLI | `zecurity-controller audit verify` recomputes canonical bytes in Go from raw columns (does not trust the DB's function) and reports gaps, edits and deletions. | `cmd/server/audit_verify.go` |
| Connector access lines (W-2) | `ZECURITY_LOG_ANALYTICS=v1`: one `ANALYTICS {json}` line per access decision on stdout, written by an `analytics-writer` thread through a bounded queue (4096); a stalled reader drops and counts lines, never stalls a tunnel. Off by default; no proto change. | `connector/src/analytics.rs` |
| Docs | Setup for webhook, HEC, S3 and Wazuh (decoders, `aws-s3`, journald). | `docs/siem-export.md` |

### Configuration

- `DATABASE_URL` must use `ztna_app`; `APP_DB_PASSWORD` gives it a login (compose).
- `ACCESS_LOG_RETENTION_DAYS` (default 90, minimum 90).
- `AUDIT_EXPORT_DEV_ALLOW_PRIVATE`, `AUDIT_EXPORT_DEV_CA_FILE`: lab only, honoured only with `ENV=development`.
- Connector: `ZECURITY_LOG_ANALYTICS=v1`.

### Verification

- **Gate (after merging `main`, 2026-11-12):** controller 1782 pass / 0 fail / 2 skip (the known two), all CI `*_TEST_DATABASE_URL` set; connector 335 pass; admin 294 pass, build OK.
- **Live runs, all PASS:** backend S3 run (§8), console run in the Orca browser (§8), Wazuh webhook run (§8), Twingate-style S3 + connector-lines run with moto and Wazuh `aws-s3` (§9), and the run on merged `main` with moto and MinIO (below).
- **Security review (manual, 2026-11-12):** no vulnerabilities; two hardening fixes (non-blocking access lines; secret again on an S3 region change). Earlier push review: IPv6 blocklist fix.

### Live run on merged `main` (2026-11-12): PASS

**Setup:** the shared local DB recreated from `main` (migrations 001 … 055 in order, then `zz_app_role_login.sh`); controller built from `main` (5987f6e9), running as `ztna_app` with no append-only warning; workspace `p11main`. Two S3 sinks, both prefix `siem`, each with a writer key that may only `PutObject` under `zecurity-audit/siem/*`:
- **moto 5.1.4** (S3 + SQS + STS, IAM enforced) behind a lab-CA TLS front, notifying an SQS queue that Wazuh 4.14.10's `aws-s3` subscriber reads;
- **MinIO** (Chainguard build of MinIO's source, RELEASE.2026-11-10; MinIO no longer publishes images on Docker Hub or quay.io) with TLS and its own policy engine: a second, independent SigV4 implementation.

The connector (branch build of `main`, `ZECURITY_LOG_ANALYTICS=v1`) ran in its own container; a sandbox client daemon drove real tunnels.

| Check | Result |
|---|---|
| Both servers refuse an unknown key, a wrong signature, and a write outside `siem/` | moto and MinIO: `InvalidAccessKeyId`, `SignatureDoesNotMatch`, `AccessDenied` |
| `testAuditExportSink` on each sink | both `ok`: MinIO accepts the hand-written SigV4 as well as moto |
| Region change without the secret (security-review fix) | refused; a prefix-only change accepted |
| 5-minute window | admin 1–31 in one object once the oldest event was 5 min old |
| Full batch at once (W-5 fix 74995a86) | access 1–500 written immediately as `1-500.jsonl.gz`, without waiting for the window (first live check; §9 covered it by test only) |
| Both S3 servers down, 3 sign-ins | both sinks `lastError` "could not connect to the destination" (no URL), cursors held (admin 31, access 500); on restart `32-35` and `501-746` written, errors cleared |
| Connector lines with the controller stopped | 3 `allow` lines written and decoded by Wazuh during the outage; the controller copy (`connector_logs` 749–751) arrived after restart |
| Chain from Wazuh's copy (pulled from moto) | admin 1–35, access 1–752: no gaps, every `row_hash` and link OK, 0 duplicates |
| Chain from MinIO, read directly | the same ranges, all OK, and every `row_hash` identical to the moto/Wazuh copy |
| `zecurity-controller audit verify` | admin 1–35 OK, access 1–753 OK |
| Decisions | 753 `allow` lines from the connector, all 753 decoded by Wazuh; the S3 access stream holds the same `allow` events (the 753rd was in the next batch) |
| Wazuh decoders exactly as given in `docs/siem-export.md` | plain and journald forms decode (`wazuh-logtest`) |

**Not reproduced in this run:** a connector `deny` line. A `deny` is logged only when a client asks for something the connector's ACL no longer allows. Here the client stopped routing first each time (access removed during a request loop, three times; and a resource bound to an `approved`-only device profile). Because the test host reaches the resource's Docker network directly, those requests went around the tunnel. The deny path is unchanged since its live PASS on 2026-11-06 (§9) and shares `emit_access_log` and the writer thread with `allow`.

### Known limits and follow-ups

- Access events are lost while a connector is disconnected from the controller (S-10 / O-6); the connector's local `ANALYTICS` lines are the complete record for that window.
- Not run against real AWS (moto with IAM enforcement stood in). Not checked live: Wazuh reading journald on an agent; the Docker journald log driver.
- Not in v1: provider audit log export, signed external checkpoints, WORM / Object Lock, syslog, OCSF, session-close and byte-count events.
- `gqlgen` needs `GOTOOLCHAIN=go1.26.2` (Go 1.27 breaks it); revert its `go.mod` tidy.
- Every local DB must be recreated after pulling (`docker compose down -v && docker compose up -d`) and `controller/.env` pointed at `ztna_app`.

---

## History

Original pending ADR (2026-07-03) weighed three options: a first-party audit log with export (A), the observability pipeline (B), and access events only (C). It recommended A. The 2026-09-01 correction found that the audit tables exist but have no export. Decision Record D-12 (2026-09-23) chose DB-enforced append-only and deferred hash chaining and SIEM export to this phase. This note is option A, with S-1 … S-4 above.
