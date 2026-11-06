---
type: adr
status: approved
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

**Status:** approved 2026-11-05 (Sathiya accepted O-1 … O-7 as recommended: S-5 … S-11). Build: S1a → S1b → S2 → S3, one PR. **S1a built 2026-11-05** (fc3561c1 on local `feat/pending11-siem`: migration 051, `ztna_app`, `audit.VerifyStream`; gate 1719/0/2). Lock order: workspace FOR KEY SHARE, then head (found while testing: the FK check after a BEFORE trigger would otherwise deadlock against a workspace FOR UPDATE + audit transaction).
**S1b built 2026-11-06** (local `feat/pending11-siem`): migration `052_audit_export.sql`, P-9 audit gaps, `internal/auditexport` (exporter, webhook + Splunk HEC, SSRF-safe client, sealed secrets), GraphQL sink API + `auditLogs` behind `AUDIT_ADMIN` / `CONSOLE_READ`, access-event retention worker, `zecurity-controller audit verify`. Decisions taken while building are in §7. **S2 built 2026-11-06** (b4565fde): `/audit-log` page and Settings → Audit export. **S3 live run PASS 2026-11-06** (§8).
**Base:** `main` (89655c83). Migrations: `051_`, `052_`.
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

**Not covered live** (covered by integration tests): the retention `export.gap` (needs 90-day-old rows); refused sign-ins on an enterprise connection; viewer access to sinks (schema test). The console pages were checked by unit and sweep tests, not in a browser.

---

## History

Original pending ADR (2026-07-03) weighed three options: a first-party audit log with export (A), the observability pipeline (B), and access events only (C). It recommended A. The 2026-09-01 correction found that the audit tables exist but have no export. Decision Record D-12 (2026-09-23) chose DB-enforced append-only and deferred hash chaining and SIEM export to this phase. This note is option A, with S-1 … S-4 above.
