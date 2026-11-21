---
type: phase
sprint: 24
owner: Sathiya (Person 2, M1)
features: [Sprint 23 follow-ups (F7, F12), F26 alert rules + webhook/email, F27 incident timeline (stretch)]
phases: [X1, A1, A2, A3, T1]
decisions: [D-50, D-52, D-53, D-51, D-40]
status: signed off 2026-11-20; ready for implementation
---

# Sprint 24 — Sathiya: X1 follow-ups, A1 rules + evaluator, A2 delivery, A3 console (F26); T1 timeline (F27, stretch)

Read `Sprint24/path.md` first (order, conflict zones, rules). Binding decisions: Decision Record amendment **2026-11-20** — **D-50** (alert scopes and recipients), **D-52** (webhook format and security), **D-53** (rule and channel management), **D-51** (F14 email unchanged). Also binding:
- **D-06:** tenant-identifying data and management are super-admin only.
- **D-40 (extended):** relay-ops see platform relay alerts only, never tenant names or tenant alerts.
- **D-02:** single controller; the evaluator and delivery worker are process-local.
- **D-12, D-14, D-17:** append-only audit in `provider_audit_logs`, reasons for changes.
- **D-36:** the F14 daily email stays as it is.

Order: **X1 → A1 → A2 → A3 → (T1)**. A1 runs in parallel with Barath's M1 (shared relay reader, see `path.md`).

---

## X1 — Sprint 23 follow-ups

Found in the Sprint 23 acceptance run (2026-11-19). Each item is a separate small change. Items 1, 3 and 4 decided by Barath 2026-11-20.

1. **SCIM enable at the users limit** — *decided by Barath 2026-11-20.* `ProbeMapping` (`controller/internal/scim/mapping_probe.go`) provisions a probe user, which the S3 quota refuses at the limit, so SCIM can't be turned on.
   - **The mapping probe doesn't take a users quota slot.** Guardrails: **at most one probe user per workspace at a time** (serialised per workspace, same lock as the quota check); the probe user is created and deleted in the same flow; it has **no rights to enroll devices or join networks** (no client login, no device enrollment, no invitation) and never appears as a member.
   - **Real SCIM user creates at the limit** get a clear SCIM error: HTTP 403, `scimType` / `detail` saying the users quota is reached (`quota_exceeded`, with limit and used), not a generic failure.
   - Tests: enable SCIM at the limit succeeds; a second concurrent probe is refused; the probe user can't log in or enroll; a real create at the limit gets the quota error; the count after the probe is unchanged.
2. **Client quota message.** The client prints the device-quota refusal as a raw gRPC dump. Map `ResourceExhausted` with the `quota_exceeded` message to a clear line, as `client/src/login.rs` (~line 217) already does for the suspended message. Test: the mapped message, and other errors unchanged.
3. **SCIM while a tenant is suspended** — *decided by Barath 2026-11-20: refuse.* Every SCIM request (read and write) for a suspended tenant gets **HTTP 403** with a SCIM error body ("workspace suspended by the provider"); state stays frozen, and the IdP reconciles on its own after resume. The check is the existing eligibility check (`internal/tenantstate`), in the SCIM middleware, fail closed. Tests: each SCIM verb refused while suspended; works again after resume; a lookup error denies.
4. **D-45/D-46 export amendment** — *confirmed by Barath 2026-11-20; written into the Decision Record (amendment 2026-11-20, D-46 refinement).* The S4 deletion export is tenant-private: the tenant's S3 sink or `TENANT_PURGE_EXPORT_DIR`; an emailed link or an export-only sign-in; valid until the purge starts; the provider never shows tenant content.

### X1 build record (branch `feat/x1-sprint23-followups`, 2026-11-20)
- **Item 1:**
  - `scope.probe` is set only by `probeMappingWithClaims`; `resolveScope` never sets it.
  - The probe provisioner calls the new `quota.ReserveMappingProbe`, which takes the workspace lock but no slot, and inserts the probe user with `status = 'suspended'`.
  - `DirectoryService.probing` (a `sync.Map`, process-local under D-02) allows one probe per workspace; a second probe returns "another mapping probe is already running".
  - Real SCIM creates at the limit already returned 403 with the `quota exceeded: users limit is N (in use: M)` detail (S3); a test now pins it.
  - Known effect: while a probe runs (milliseconds), its suspended user appears in the users count, since SCIM-suspended users count under D-47.
  - Tests: `TestSCIMMappingProbe_Quota` (`internal/scim/quota_integration_test.go`).
- **Item 2:**
  - `client/src/login.rs` `user_facing_error` maps a `ResourceExhausted` "quota exceeded…" status to "Sign-in refused: <message>" on both TokenExchange (user limit) and EnrollDevice (device limit); the suspended message is unchanged; other errors pass through.
  - Tests: `login::user_facing_error_tests`.
- **Item 3:**
  - The SCIM `AuthMiddleware`, after token lookup, calls `tenantstate.Check`: suspended → 403 SCIM error "this workspace is suspended by the provider" for every method; lookup error → 503 (denied).
  - Tests: the `TestScimAuthMiddleware` subtest for suspend and resume.
- **Mutation checks:** making the probe take a slot, removing the suspension check, and removing the single-probe guard each make a test fail.

Live check: items 1–3 on the lab, recorded in `Pending-Live-Tests.md`.

---

## A1 — Alert rules and evaluator — D-50, D-53

### What exists
- Relay state: heartbeat values, B2 headroom (`relays.link_capacity_bps`), offline detection (`last_heartbeat`).
- Pools: `relaypool.EffectivePool`, `no_eligible_relays` (F11-1). Never re-derive the effective pool.
- Control plane: `internal/health` registry (`Checker`, F25), `GET /provider/health`.
- Quotas: `over` per key (S3, `internal/providerquery`).
- Connectors and shields: status and last heartbeat per tenant.
- Certificates: `providerquery` certificate entries (F15/F14).

### Build
- **Schema `069_alerts.sql`** (A1 and A2 tables together):
  - `alert_rules (id, kind, scope, enabled, params JSONB, severity, for_seconds, renotify_seconds, created_*, updated_*)`, `scope IN ('platform','tenant')`; each `kind` has a fixed scope (checked in code and by a CHECK listing kinds per scope).
  - `alert_states (rule_id, target_type, target_id, scope, tenant_id NULL, status, pending_since, firing_since, last_notified_at, resolved_at)`, PK `(rule_id, target_type, target_id)`.
  - `alert_events (id, rule_id, scope, tenant_id NULL, target_type, target_id, target_name, event_type, status, severity, summary, occurred_at)` — append-only (no UPDATE/DELETE grant for the app role).
  - **CHECK on states and events:** `(scope = 'tenant') = (tenant_id IS NOT NULL)`.
  - A2's `alert_channels` and `alert_deliveries` go in the same file (see A2).
- **Rule kinds** (built-in, parameters only; no query language):
  | Kind | Scope | Target | Params |
  |---|---|---|---|
  | `relay.offline` | platform | relay | minutes |
  | `relay.over_capacity` | platform | relay | threshold % of link capacity (B2), minutes; no capacity → never fires |
  | `pool.no_eligible_relay` | platform | pool | minutes |
  | `controlplane.degraded` | platform | controller | which F25 checks, minutes |
  | `certificate.expiring` | platform | certificate | days (provider-owned certificates, as D-36) |
  | `connector.offline` | tenant | connector | minutes |
  | `shield.offline` | tenant | shield | minutes |
  | `tenant.no_relay_path` | tenant | tenant | minutes |
  | `tenant.over_quota` | tenant | tenant | keys (informational, severity `info`) |
- Seed one enabled default rule per platform kind and disabled defaults for tenant kinds (super-admin turns them on).
- **Evaluator job** (`main.go`, own block; every 30 s by default):
  - per rule and target: `ok → pending` when the condition holds, `pending → firing` after `for_seconds`, `firing → resolved` when it stops holding; one `alert_events` row per transition (`fired`, `resolved`);
  - no repeat while firing, except after `renotify_seconds`;
  - **a read error leaves the state unchanged** (never a false "resolved"), logged;
  - a target that disappears (relay deleted, tenant purged) resolves with a reason;
  - suspended tenants: tenant rules skip them (agents are deny-all by design; no noise).
  - Hands fired/resolved events to A2 (an interface; A1 merges with a no-op or log sink).
- **Read API (as built):** `GET /provider/alerts?status=&scope=&tenant_id=` (active, with a `truncated` flag), `GET /provider/alert-events?scope=&tenant_id=&cursor=&limit=` (history), `GET /provider/alert-rules`. Action `alert.read`: super-admin all; relay-ops **platform relay and pool alerts only, without tenant names** (D-40). Tenant-scoped reads always filter by `tenant_id`.
- **Rule writes (as built):** `PATCH /provider/alert-rules/{id}` (name, params, severity, enabled, for/renotify) with `{reason}`; actions `alert.rule.update` (super-admin, D-53); audit before/after. Create/delete of additional rules of a kind: `POST /provider/alert-rules`, `DELETE /provider/alert-rules/{id}` with reason.

### Tests
- Each kind: fires after `for`, no repeat, renotify, resolves; target removed → resolved.
- DB CHECK: a tenant row without `tenant_id` and a platform row with one are refused.
- Read error → state unchanged.
- Authz: relay-ops can't see tenant alerts or names, can't write rules → 403.
- **Mutation checks:** drop the scope filter for relay-ops → its test fails; resolve on read error → its test fails.

### A1 build record (branch `feat/a1-alert-rules`, 2026-11-20)
- **Schema `069_alerts.sql`:**
  - Creates `alert_rules`, `alert_states`, `alert_events`, plus A2's `alert_channels` and `alert_deliveries`, so A2 needs no schema change.
  - Scope CHECKs on states and events: `(scope = 'tenant') = (tenant_id IS NOT NULL)`. `alert_rules_kind_scope` ties each kind to its scope.
  - Tenant rows reference `workspaces` `ON DELETE CASCADE`, so the S4 purge removes them (D-46).
  - The runtime role can only insert into events and deliveries (no update or delete).
  - Nine rules are seeded with fixed ids `00000000-0000-0000-0002-00000000000N`: platform rules enabled, tenant rules disabled.
- **Shared relay reader:** `providerquery.RelayLoads` gives every non-revoked relay with its bandwidth and headroom, built from the same columns and logic as `GET /provider/relays`. M1's sampler must reuse it (path.md conflict zone).
- **`internal/alerting`:**
  - `kinds.go`: kinds, scopes and `ParseParams` (unknown fields refused, defaults applied).
  - `signals.go`, one reader per kind:
    - `relay.offline` = status `inactive`;
    - `relay.over_capacity` = `Headroom.Utilization` ≥ threshold, only with a capacity set;
    - `pool.no_eligible_relay` = 0 eligible relays and at least one tenant;
    - `controlplane.degraded` = a F25 component not up;
    - `certificate.expiring` = CAs within `days`, relay and controller certificates within 24 h (D-36 scope);
    - `connector.offline` and `shield.offline` = `disconnected`;
    - `tenant.no_relay_path` = an effective pool (`relaypool.EffectivePool`) with no eligible relay while the tenant has connectors;
    - `tenant.over_quota` = `quota.AllUsage` `Over`.
    Tenant kinds skip suspended tenants.
  - `store.go`: `Apply` runs the state machine in one transaction: pending, then firing after `for_seconds`, renotify, then resolved. `DeleteRule` resolves the rule's firing alerts in the same transaction.
  - `evaluator.go`: when a rule can't be read, its states are left unchanged. A disabled rule resolves its alerts. `Notifier` is the hook A2 implements (`LogNotifier` for now).
  - `reads.go` and `handler.go`: the API in `handler.go`'s header comment.
- **Authz:**
  - `alert.read`: super-admin, and relay-ops filtered to the relay and pool kinds. A relay-ops request for the tenant scope or a tenant gets an empty result.
  - `alert.rule.read`, `.create`, `.update`, `.delete`: super-admin only (D-53).
- **`main.go` env:** `ALERT_EVAL_INTERVAL` (default 30 s, at least 5 s); `ALERTS_ENABLED=false` turns the evaluator off.
- **Tests:**
  - `TestAlertsDB_SchemaScopes`, `TestAlertsDB_StateMachine`, `TestAlertsDB_Signals`, `TestAlertsDB_API`;
  - `TestParseParams`, `TestKindScopes`, `TestRuleValidate`, `TestDecide_AlertMatrix`.
- **Review fixes (PR #234):**
  - a rule changed mid-tick is skipped (`updated_at` check);
  - workspace CA alerts carry no tenant name;
  - a target that left scope resolves with the reason ("tenant suspended", "relay removed");
  - the pool alert counts only active, non-suspended tenants;
  - a stored rule with bad params is skipped, not fatal;
  - `GET /provider/alerts` returns `truncated`.

  Test: `TestAlertsDB_ReviewFixes`.
- **Mutation checks**, each making a test fail:
  - a read error resolves alerts;
  - the relay-ops filter is removed;
  - suspended tenants are included;
  - relay-ops may update rules.

---

## A2 — Delivery: webhook and email — D-52

### What exists
- `internal/auditexport/adapter.go`: `Signature(key, t, body)` = `t=<unix>,v1=<hex HMAC-SHA256(key, "<unix>." + body)>`, header `X-Zecurity-Signature`.
- `internal/outbox`: retry with backoff.
- `internal/certalert`: `Sender` interface (plain-text email over `SMTP_*`; stdout in dev), super-admin recipient lookup.
- Sealing: the PKI service's `EncryptSecret` (as used by `internal/idp`, `internal/mfa`).
- SSRF patterns: relay address handling with `RELAY_DEV_ALLOW_PRIVATE_ADDR` (`internal/relay/provision.go`).

### Build
- **Schema (in `069_alerts.sql`):**
  - `alert_channels (id, name, type IN ('webhook','email'), scope_filter IN ('all','platform','tenant'), min_severity, enabled, url TEXT NULL, encrypted_secret, secret_nonce, recipients TEXT[] NULL, created_*, updated_*)`;
  - `alert_deliveries (id, event_id, channel_id, attempt, status, http_status NULL, error TEXT NULL, at)` — append-only.
- **Payload v1** (exactly the D-52 fields): `version`, `event_id`, `event_type`, `status`, `severity`, `scope`, `tenant_id`, `target {type,id,name}`, `summary`, `started_at`, `timestamp`, `console_url`. Tenant names only for channels allowed to see tenant alerts. No secrets, tokens or tenant content. A golden-file test pins the schema.
- **Webhook:** POST JSON, `X-Zecurity-Signature` over timestamp + body (reuse `auditexport.Signature`), `X-Zecurity-Event-Id` for receiver dedupe; document the replay window (receivers reject `t` older than 5 minutes).
  - HTTPS only unless the dev flag is set; **SSRF guard**: resolve and refuse private, loopback, link-local, multicast and metadata ranges, **checked in the dialer at connect time** (DNS rebinding); no redirects; 10 s timeout; response body read capped.
  - Secret generated server-side, shown **once** on create/rotate (`Cache-Control: no-store`), sealed at rest, never logged, audited or returned again.
- **Email:** via the certalert `Sender`; recipients = active super-admins or the channel's explicit list; plain text with the same fields.
- **Routing:** each fired/resolved event goes to every enabled channel whose `scope_filter` and `min_severity` match.
- **Retries:** through the outbox with backoff; each attempt a row in `alert_deliveries`; final failure recorded and visible in A3.
- **Channel API:** `POST /provider/alert-channels`, `PUT /provider/alert-channels/{id}`, `DELETE …`, `POST …/{id}/test`, `POST …/{id}/rotate-secret`, all with `{reason}` (test included, so it's audited); actions `alert.channel.*` super-admin only (D-53).

### Tests
- Signature verifies with the shared secret; payload golden file; tenant names stripped for a platform-only channel.
- SSRF: private IP literal, DNS name resolving to a private address, redirect → all refused (outside dev).
- Secret never appears in responses after create, logs or audit (test scans).
- Retry and final failure recorded; scope filter respected.
- **Mutation checks:** remove the connect-time check → the rebinding test fails; send tenant alerts to a platform-only channel → its test fails.

### A2 build record (branch `feat/a2-alert-delivery`, 2026-11-20)
- **Retries: own queue, not the outbox.** `outbox_events.workspace_id` is NOT NULL, so platform alerts can't be queued there. Chosen by Sathiya on 2026-11-20 and recorded as a D-52 refinement in the Decision Record; **it awaits Barath's confirmation** (he is the product owner).
  - Migration `070_alert_delivery_queue.sql` adds a table with one row per (event, channel), leased by the worker.
  - `alerting.insertEvent` queues every matching enabled channel in the event's own transaction: `scope_filter` all or the same scope, and severity at or above `min_severity`.
- **`delivery.go` (`Deliverer`):**
  - It implements `Notifier`, which only wakes it; it also polls every 5 s.
  - It leases due rows with `FOR UPDATE SKIP LOCKED` and a 1-minute lease, then sends.
  - It appends each attempt to `alert_deliveries` (`delivered`, `retrying` or `failed`) and deletes or reschedules the queue row.
  - Backoff is `outbox.RetryPolicy` with a 5 s base, 5 min maximum and 10 attempts.
  - Webhooks: `POST` with `X-Zecurity-Signature` (`auditexport.Signature` over timestamp + body) and `X-Zecurity-Event-Id`. Receivers reject `t` older than 5 minutes (`ReplayWindow`).
  - Email goes through the F14 sender (`invitation.Emailer.SendPlain`), to the listed recipients or every active super-admin.
- **`payload.go`:** the D-52 payload v1, pinned by `TestPayload_Golden`. `console_url` comes from `PROVIDER_CONSOLE_URL` (falling back to `PROVIDER_CONSOLE_ORIGIN`); it is null when unset.
- **`channels.go`:**
  - The secret is generated server-side (`whsec_…`, 256 bits) and sealed with `pki.EncryptSecret` (context `alert-webhook-secret:<id>`). It is returned only by create and rotate, with `no-store`.
  - The URL is returned and audited only as scheme and host (`auditexport.RedactURL`).
  - URL checks: `auditexport.ValidateURL`, plus a refusal of non-canonical numeric hosts; then the dial-time check in `auditexport.NewClient`.
  - Dev: `ALERT_WEBHOOK_DEV_ALLOW_PRIVATE=true` with `ENV=development` allows private targets and plain http.
- **API as built** (super-admin only, `alert.channel.*`): `GET/POST /provider/alert-channels`, `PATCH/DELETE /provider/alert-channels/{id}`, `POST …/{id}/test`, `POST …/{id}/rotate-secret`, `GET /provider/alert-events/{id}/deliveries`. Every write needs a reason and is audited (test sends included, with their outcome).
- **Tests:** `TestPayload_Golden`, `TestURLPolicy`, `TestWebhook_DialTimeGuardAndRedirects`, `TestDeliveryDB_WebhookEndToEnd`, `TestDeliveryDB_Routing`, `TestDeliveryDB_ChannelValidation`, `TestDecide_AlertChannelMatrix`. They cover the signature, the fields, delivered / retrying / failed, rotation, the secret and URL path never in the list or audit, routing, email, validation and relay-ops 403.
- **Mutation checks:** removing the dial-time check makes the rebinding test fail; routing tenant alerts to a platform-only channel makes the routing test fail.

---

## A3 — Alerts console

### Build
- **Alerts page** (new nav entry): active alerts (severity, scope, target, since), history with filters, rules (edit params, enable/disable, reason dialog), channels (create, edit, test, rotate secret — the secret shown once, never stored in the console).
- **Overview badge** with the count of firing alerts (`Home.tsx`, own section).
- Relay-ops view: platform relay/pool alerts only, no management controls (server enforces; the UI only hides).
- Every new write in `provider-console/src/api/api-surface.test.ts` (D-30 amended).

### Tests
- `npm test`, `npm run lint`, `npm run build`; reason required in dialogs; secret displayed once and cleared.

### A3 build record (branch `feat/a3-alerts-console`, 2026-11-20)
- **`src/api/alerts.ts`:** types, five reads (`/provider/alerts`, `/alert-events`, `/alert-events/{id}/deliveries`, `/alert-rules`, `/alert-channels`) and six writes (rule `PATCH`; channel `POST`, `PATCH`, `DELETE`, `/test`, `/rotate-secret`). All are in `api-surface.test.ts` on purpose (D-30 amended by D-53).
- **`src/pages/Alerts.tsx`:**
  - Tabs **Active** and **History** for both roles. The controller filters relay-ops to relay and pool alerts; the console also hides the scope filter and tenant links from them.
  - Tabs **Rules** and **Channels** for super-admin only; a `?tab=rules` URL falls back to Active for relay-ops.
  - Active shows the truncation note; History pages by cursor and opens a Deliveries dialog per event (super-admin).
- **`src/components/AlertDialogs.tsx`:**
  - Edit rule: enable, severity, threshold or days, fire-after and re-notify delays.
  - Add / edit / test / rotate / delete channel.
  - Every write needs a reason (`ReasonField`).
  - `SecretDialog` shows the webhook secret once and drops it on close; it is never stored. `static-guards.test.ts` allows clipboard access here.
- **Navigation:** an Alerts section for both roles (`roles.ts`, Bell icon).
- **Overview:** a "Firing alerts" tile (`GET /provider/alerts?status=firing`, both roles) linking to `/alerts`. `Home.test.tsx` was updated to pin the new request.
- **Review fixes (PR #236):**
  - History resets its paging when the scope changes (`key={scope}`).
  - `console_url` (`/alerts?event=<id>`) opens History on that event; for super-admins it also opens the event's deliveries.
  - An unknown `tab` or `scope` in the URL falls back to the default.
  - Webhook URLs must be `https://` in the console too.
  - `invalid_request`, `invalid_filter` and `server_error` get clear messages.

  4 new tests.
- **Not in A3:** creating or deleting rules from the console (the API exists; the nine seeded rules are edited in place).
- **Tests:** `src/pages/Alerts.test.tsx` (9). Full suite: 675 tests, lint and build clean. Mutation check: giving relay-ops all four tabs makes the relay-ops test fail.

---

## T1 — Incident timeline (F27, stretch)

Only if A1–A3 are merged with time left.
- One read-only, time-ordered view merging `alert_events`, `provider_audit_logs` rows and relay/pool state changes; filters by relay, pool and tenant; same authz as A1 reads (relay-ops: no tenant rows or names).
- No new data; no schema change.

### T1 build record (branch `feat/t1-incident-timeline`, 2026-11-21)
- **Open point checked first: is relay and pool state stored as history?**
  - Pool failover and failback: yes. The system writes provider audit rows `relay_pool.failover` and `relay_pool.failback` (actor `system:relay-pool-failover`) in the switch's transaction.
  - Relay active/inactive: no. `relays.status` is current state only (`EvictRelays`, `RecordHeartbeat`). The timeline shows the `relay.offline` alert events instead (after the rule's `for` delay) and never invents a transition.
  - Pools without a failover pool: only the `pool.no_eligible_relay` alert events.
- **API:** `GET /provider/timeline?relay_id=|pool_id=|tenant_id=&source=alert|audit&since=&until=&cursor=&limit=` returns `Page[TimelineItem]`, newest first.
  - The query is `providerquery.QueryTimeline`: one statement, a `UNION ALL` of `alert_events` and `provider_audit_logs`, keyset on (time, id).
  - The handler is `alerting.TimelineHandler`.
  - No schema change and no write.
- **Filters:**
  - At most one of relay, pool or tenant; otherwise 400 `invalid_filter`.
  - `pool_id` also matches the relays now in the pool (membership is current state).
  - `tenant_id` matches the tenant's alert events and the audit rows that target that tenant or workspace.
- **Audit rows shown:**
  - Only targets `relay`, `relay_pool`, `tenant`, `workspace`, `alert_rule` and `alert_channel`.
  - `*.read` actions are left out: every tenant-detail view writes `tenant.read` (OQ-1).
  - The name is the recorded one, else the target's current name.
- **Authz:**
  - `alert.read` is required.
  - Alert rows follow A1: relay-ops get platform relay and pool kinds only, and a `tenant_id` request gets no rows.
  - Super-admins (`audit.view`) get every incident-relevant audit row.
  - **Amended by D-55 (2026-11-21):** relay-ops (`timeline.relay_audit`, a separate `decide()` action given to relay-ops) also get relay and pool operation audit rows from the allowlist `provider.RelayOpsTimelineAuditActions`, with action, actor and reason but no raw details. Tenant events that touch a relay or pool stay hidden (`relay.placement_rejected`, `tenant.relay_pool.assign`), as do tenant, operator-account and read rows. relay-ops still get 403 on `GET /provider/audit`. The console shows them the source chips; tenant focus and links stay super-admin only.
- **Console:**
  - `src/pages/Timeline.tsx` and a nav section **Timeline** for both roles (`roles.ts`, History icon).
  - Source chips (All / Alerts / Operator actions) for super-admin only.
  - Rows link to the relay, pool, tenant or Alerts tab; **focus** sets `relay_id`, `pool_id` or `tenant_id` in the URL, and a chip clears it. A malformed id in the URL is ignored. relay-ops get no tenant focus or links.
  - Action labels are in `lib/alert-format.ts`. `listTimeline` is in `api/alerts.ts`; `GET /provider/timeline` is in `api-surface.test.ts` `ALLOWED_READS` on purpose (no new write).
  - Relay, pool and tenant detail pages are untouched (M2 works on them).
- **Tests:**
  - `TestTimelineDB_SuperAdmin`, `_Paging` (across a same-instant alert/audit tie), `_RelayOps`, `_BadRequests`.
  - `src/pages/Timeline.test.tsx` (7). Console: 686 tests, lint and build clean.
- **Mutation checks**, each making a test fail:
  - no relay-ops action allowlist (D-55);
  - audit details returned to relay-ops (D-55);
  - `timeline.relay_audit` not granted to relay-ops (D-55);
  - no `audit.view` gate;
  - no relay-ops kind filter;
  - `*.read` rows kept.

---

## Live check (each phase)

Record in `.zecurity-obs/Planning/Pending-Live-Tests.md`:
- **X1:** SCIM at the limit, the client quota message, SCIM while suspended (per the answers).
- **A1:** stop a relay → `relay.offline` fires after its `for` and resolves on restart; empty a pool; stop a tenant's connector → tenant alert with its `tenant_id`; relay-ops view.
- **A2:** a local webhook receiver verifies the signature and fields; a platform-only channel gets no tenant alert; receiver down → retries, then delivered; email arrives; private URL refused outside dev.
- **A3:** manage rules and channels from the console; badge count; audit rows with reasons and no secret.

## Post-Phase Fixes

_None yet._
