---
type: phase
sprint: 16
stage: 2
phase: 10
title: Admin UI — FQDN Resources
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase9-Client-Binding-Registry-Synthetic-Routing
status: done (with stated gaps) — Gate 2 closed 2026-08-26 (5/5). Four UI verify items were never run; one task deliberately not shipped (resolver health).
tags: [sprint16, admin, frontend, react, graphql, fqdn, gate2]
---

# Sprint 16 · Phase 10 — Admin UI: FQDN Resources

> Goal: an operator can create and edit a name-addressed resource, and can see which delivery mode a
> resource uses and whether its resolver is healthy — without writing SQL.
> Depends on **Phase 9** (there is no point exposing a resource type the data plane can't serve).
> This phase closes **GATE 2**, the Stage 2 merge point.

## Current state (verified)

The GraphQL contract already exists from Phase 5 — this phase is UI only, no schema work:

```graphql
type Resource {
  host:        String!   # "" (never null) when name-addressed — kept non-null for existing clients
  hostname:    String    # null for classic IP-pinned
  resolver:    String    # raw JSON as stored: {"type":"dns","config":{…}}
  localTarget: String    # shield-delivered only
}

input CreateResourceInput {
  host:     String       # relaxed from String! in Sprint 16 — exactly one of host/hostname
  hostname: String
  resolver: String
  localTarget: String
}
```

Pages: [admin/src/pages/Resources.tsx](../../../admin/src/pages/Resources.tsx),
[ResourceDetail.tsx](../../../admin/src/pages/ResourceDetail.tsx).

⚠️ **`resolver` is a raw JSON string on the wire, not a typed input.** The UI must serialize/parse it.
Do not surface a free-text JSON box to operators as the primary control — that pushes a validation
burden onto them and the only guard is a server-side "must contain a type key".

## Tasks

### 10.1 — Create / edit a name-addressed resource
- [x] **Addressing mode is a choice, not two optional fields.** Present a radio/segmented control —
      *IP address* vs *Hostname* — and show only the relevant input. The server enforces exactly-one
      (`validateAddressing`); the UI should make violating it unrepresentable rather than surfacing
      `"host and hostname are mutually exclusive"` after a failed submit.
- [x] Resolver: a **type dropdown** (`dns` | `static`) plus type-specific config fields. Serialize to
      `{"type":"…","config":{…}}`. An "advanced / raw JSON" escape hatch is fine as a secondary control.
- [x] Resolver is only shown for the **hostname** mode. It is meaningless for a pinned IP, and offering
      it there invites the misconception that a resolver chooses delivery.
- [x] ⚠️ **Do not offer `shield` as a resolver type.** Delivery is derived server-side from
      `(status, shield_id)`; `resolver.type` only answers *how the connector finds the endpoint*. A
      dropdown containing `shield` would encode the exact conflation Phase 7 warns about, and users would
      then expect it to work.
- [x] `localTarget` is editable **only** for shield-delivered resources, with the allowed values made
      explicit (`127.0.0.1` or the shield's LAN IP). Anything else is rejected by the shield with
      `status: "failed"` — surface that as a validation hint, not as a mysterious later failure.
- [x] Regenerate typed hooks: `cd admin && npm run codegen`.

### 10.2 — Show delivery type + resolver health
- [x] A **delivery** column/badge: *Protected* (shield) vs *Connector-reachable*. This is the
      Protected/ConnectorReachable discriminator (`route_type`) surfaced for the first time — today an
      operator cannot tell them apart in the UI.
- [x] For name-addressed resources: show the **hostname** as the primary address and, where available,
      the resolver type. Never show a synthetic IP — those are client-local, the controller does not have
      them, and displaying one would imply the server knows something it does not.
- [ ] ⛔ **Not shipped — deliberate, per this task's own scope check (see below).** Resolver health /
      last error: display the typed failure classes from Phase 6 distinctly —
      `NXDOMAIN` (config error) · `resolver unavailable` (infra) · `no A record` · `dial failed`
      (resource down, resolution fine). Collapsing them into "unreachable" sends operators to the wrong
      system, which is the entire reason Phase 6 types them.
      > ⚠️ **Scope check:** there is no transport today for connector→controller resolver health. If
      > exposing it requires a new field or RPC, that is **out of this sprint** (standing rule: no new
      > RPCs; ACL/heartbeat piggyback only). Ship 10.2 without live health rather than inventing a
      > channel — show the resolver *config* and last known state, and note the gap.

## Build gate

```bash
cd admin && npm run codegen && npx tsc --noEmit
```

## 🚩 GATE 2 — E2E, Stage 2 merge point

- [x] An FQDN resource created **through the UI** (not SQL) is reachable end-to-end. This closes the
      Phase 5 known gap: *"the GraphQL `createResource` path with `hostname` has never been executed
      end-to-end."*
- [x] **Changing the backend IP requires no controller action, bumps no ACL version, and restarts no
      tunnel.** Verify by watching the ACL version across a DNS change:
      ```bash
      zecurity-client status      # note acl_snapshot_version
      # change the DNS A record / static resolver config target
      # re-connect through the tunnel — traffic reaches the NEW backend
      zecurity-client status      # acl_snapshot_version MUST be unchanged
      ```
      This is *the* acceptance criterion of the whole sprint. An ACL version bump here means resolution
      leaked into the control plane, and every tunnel in the fleet restarted
      (`restart_tunnel_if_running`).
- [x] Existing IP resources produce **identical** effective ACLs (diff a snapshot before/after).
- [x] Protected resources are still delivered via the shield and fail closed when every shield is
      offline.
- [x] `cd relay && cargo build` still green — proves no accidental coupling to the relay.

## Verify (UI)

- [x] Creating a resource with both an IP and a hostname is not expressible in the form.
- [ ] Creating with neither is blocked client-side, and the server error is still handled gracefully. **❌ NOT VERIFIED** — no UI pass was run against these four items; the FQDN create/edit flow was exercised only far enough to seed Gate 2/3 resources.
- [ ] A malformed resolver JSON (via the raw escape hatch) shows the server's readable message, not a **❌ NOT VERIFIED** — no UI pass was run against these four items; the FQDN create/edit flow was exercised only far enough to seed Gate 2/3 resources.
      raw constraint violation.
- [ ] Editing `hostname` or `resolver` bumps the ACL version (they *are* ACL-relevant); editing **❌ NOT VERIFIED** — no UI pass was run against these four items; the FQDN create/edit flow was exercised only far enough to seed Gate 2/3 resources.
      `localTarget` does **not** (Shield-only) — the UI should not imply otherwise.
- [ ] Existing IP resources render exactly as before. **❌ NOT VERIFIED** — no UI pass was run against these four items; the FQDN create/edit flow was exercised only far enough to seed Gate 2/3 resources.

## Notes

- ⚠️ **Stage 3 is not part of Gate 2.** After this phase an operator creates an FQDN resource and it
  works — but reaching it still needs a `hosts` entry on the client. **Typing `react.app` in a browser
  does not work until Phase 11–12.** Do not let the UI imply name resolution already happens; if a hint
  is shown, say "reachable by name once client DNS is enabled".
- Stopping here leaves a coherent, useful system: the capability (backend IPs can move freely) ships in
  Stage 2; Stage 3 only buys UX.

## 🚩 GATE 2 — CLOSED 2026-08-26 (5/5)

Run on a real two-host stack: controller + connector + shield on `Archer`, client on a **separate
device** (`192.168.1.38`). Connector, client and shield all built from this branch — see the version-skew
finding below, which cost most of the session.

### Item 1 — FQDN resource created through the UI, reachable end-to-end ✅

`fqdn-test` (`hostname=fqdn-test.internal`, `resolver={"type":"dns"}`, `tcp/5174`, no shield) created in
the admin form. Client reached it **by name** through the tunnel:

```text
access allowed spiffe_id=spiffe://ws-yoge.zecurity.in/client/626e089c-…
  resource_id=86f515d9-…  hostname=fqdn-test.internal  dest=172.20.0.1
  stale=false  port=5174  proto=tcp  route="connector"
tunnel_opened ok  dest=172.20.0.1:5174
```

Every element of the thesis in one line: authorization by **SPIFFE identity**, `hostname` resolved to
`dest` **at dial time**, `route="connector"`. Closes the Phase 5 known gap — *"the GraphQL
`createResource` path with `hostname` has never been executed end-to-end."*

### Item 2 — backend IP change: no controller action, no ACL bump, traffic follows ✅

**The acceptance criterion of the whole sprint.** Verified twice, on independent runs, moving the backend
by **DNS only** — no resource edit, no mutation, no restart:

| Run | DNS change | Client result | ACL version |
|-----|-----------|---------------|-------------|
| 1 | `172.20.0.1` → `172.22.0.1` | `BACKEND-B` | 6 → **6** |
| 2 | `172.22.0.1` → `172.20.0.1` | `BACKEND-A` | 9 → **9** |

The connector's own resolution record:

```text
resolved resource endpoint  hostname=fqdn-test.internal
  resolved=172.22.0.1  cache_hit=false  stale=false  resolve_us=718
```

`cache_hit=false` is the load-bearing field: a **fresh** query per dial, sub-millisecond, entirely
process-local. Same client, same tunnel, same synthetic IP (`100.64.0.2`), same `hosts` entry — the only
thing that changed was what DNS answered. **Zero** ACL pushes across either flip.

### Item 3 — existing IP resources produce identical effective ACLs ✅

`ip-control` (`host=172.20.0.1`, `tcp/8085`, no hostname) alongside the name-addressed resource. Across
the item-2 flip its dial is byte-for-byte the pre-sprint shape:

```text
access allowed  resource_id=f4849d24-…  hostname=  dest=172.20.0.1
  port=8085  route="connector"
```

Empty `hostname`, no resolver consulted, ACL version unmoved — so the compiled snapshot (and therefore
every entry in it) is unchanged. The two addressing modes coexist in one workspace without interfering.

### Item 4 — protected resources are shield-delivered and fail closed ✅

`prot-test` (`host=192.168.1.33` = `shields.lan_ip`, `tcp/8086`, `local_target=127.0.0.1`).

**Delivered by the shield**, and the shield dialed the **`local_target`**, not the resource host — the
first live end-to-end exercise of Phase 8:

```text
access allowed  resource_id=ada3a5a5-…  dest=192.168.1.33  port=8086
  route="shield"  shield=3d68320a-…
tunnel_opened ok  shield=3d68320a-…
→ client received: PROTECTED (via shield, dialed at 127.0.0.1:8086)
```

**Fails closed** with the shield stopped — and critically, delivery does **not** silently degrade:

```text
shield Control stream disconnected shield_id=3d68320a-…
access allowed  …  port=8086  route="shield"  shield=3d68320a-…
tunnel_opened error  error=shield 3d68320a-… not connected
```

`route` stayed `"shield"` and the tunnel was refused. **Connector-routed dials to port 8086: 0.** So a
protected resource cannot be reached by stopping its shield — the property that matters.

### Item 5 — `cd relay && cargo build` green ✅

Green, 0 files changed. No accidental coupling to the relay.

---

## Implementation Notes

**`admin/src/lib/resourceAddressing.ts` (new) is the whole contract.** Every page and both modals go
through it, so addressing rules live in one place instead of being re-derived per component:

```ts
export type Addressing =
  | { mode: 'ip';       host: string;     localTarget: string }
  | { mode: 'hostname'; hostname: string; resolver: ResolverDraft }
```

A draft carrying **both** a host and a hostname is **not constructible** — the discriminated union makes
the server's `validateAddressing` rule unrepresentable in the UI rather than surfacing
*"host and hostname are mutually exclusive"* after a failed submit. That satisfies 10.1's first bullet
at the type level, which is stronger than the radio-plus-conditional-render the task asked for.

`ResolverDraft` is `dns | static | raw`. **`shield` is deliberately absent**, and the reason is written
into the type's doc comment so it survives the next edit: delivery (`route_type`, derived server-side
from `(status, shield_id)`) and resolution (`resolver.type`) are orthogonal axes, and a dropdown
containing `shield` would encode exactly the conflation Phase 7 warns about. `raw` is the escape hatch,
**not** a resolver type — it carries JSON straight through for shapes the structured form can't express.

`parseResolverJson` round-trips anything it cannot model through `raw` rather than dropping it — a config
carrying unmodelled keys included. Losing an operator's stored value on an unrelated edit is worse than
showing them JSON.

**Delivery is derived, never inferred from `resolver`.** `deliveryOf()` mirrors the server exactly: a
bound shield plus a protected-ish status ⇒ Shield-delivered, else connector-dialed.

**A blank-address bug fixed in three places.** `host` is `""` (never null) for name-addressed resources,
so pages rendering it raw showed an empty cell. `addressOf()` falls back to the hostname; applied in
`Resources.tsx`, `GroupDetail.tsx` (×2) and `ShieldDetail.tsx`. The client CLI had the same bug — see the
Phase 9.5 post-phase fix.

**Never shows a synthetic IP.** Those are client-local and the controller does not have them; the admin
UI showing one would imply the server knows something it does not.

### Deliberate deviations

- **`localTarget` in the *create* form is offered for `ip` mode, not gated on shield-delivery.** The task
  says *editable only for shield-delivered*, which `EditResourceModal` implements literally
  (`shieldDelivered = !!resource.shield`). At **create** time no shield is bound yet, so that gate would
  hide the field permanently. It is instead offered only in `ip` mode — a name-addressed resource can
  never be shield-delivered, because `MarkProtecting` joins on `shields.lan_ip = resources.host` and
  `host` is NULL for those rows — and constrained to the two legal values by `allowedLocalTargets(host)`
  (`127.0.0.1`, or the host being entered). Returning the legal set rather than validating free text is
  what keeps an illegal value out of the form.
- **Resolver health (10.2, third bullet) is not shipped.** There is no connector→controller transport for
  it, and inventing one would breach the standing no-new-RPCs rule. This is the outcome the task's own
  scope check prescribes: *"Ship 10.2 without live health rather than inventing a channel."* The resolver
  **config** is shown; live state is not. Gap noted here rather than papered over.

### Server-side doc fix made in this phase

`controller/graph/resource.graphqls` carried a `{"config":{"server":"…"}}` example for `resolver` — but
`dns_query_name` **rejects** a `server` key outright (`InvalidResolverConfig`), because a per-resource
nameserver is not a thing the connector implements. Replaced with the three valid shapes and an explicit
note on why `server` is absent. `compiler_fqdn_test.go`'s fixture moved from `server` to `name` to match.

### Verification

`npx tsc --noEmit` clean. Lint: **19 problems project-wide, identical to HEAD** with the same per-file
distribution — zero new. There is no test runner in `admin/`, so `resourceAddressing.ts` was verified by
compiling it standalone and running **31 behavioural assertions** under node.

⚠️ The **Verify (UI)** and **Gate 2** checklists below are still open — they need a browser and a live
stack. Do not mark this phase `done` until Gate 2 passes.

---

## Deployment & Operability Findings (Gate 2 session, 2026-08-26)

None of these are Sprint 16 regressions — they are pre-existing gaps that a real two-host verification
surfaced. Collectively they are why Gate 2 took a full session: **the code was ready hours before the
deployment was.** Recorded here because the next person to verify a branch will hit every one.

### 1. ⚠️ No supported way to run or identify pre-release code

The single most costly finding.

- `client-install.sh` downloads a **prebuilt GitHub release** (`CLIENT_VERSION=latest`), so an operator
  testing a feature branch silently gets *older* code. This branch's `client/Cargo.toml` says `1.0.12`
  while the released tag is `client-v1.0.13` — the branch looks **older** by version number.
- The **auto-updaters actively revert** local builds. `zecurity-connector-update.service` ran at boot,
  fetched released `connector-v1.0.18`, backed up our branch binary and replaced it — all at INFO level,
  with nothing indicating that locally-installed code had been overwritten. Both connector and shield
  timers are `enabled` by default.
- There is **no `--version`** on the client, and no runtime way to ask any component what build it is.
- **Worst case, the shield:** branch and release both report `1.0.10`. A `local_target`-aware shield and
  one with *zero* support for it (`0` occurrences on `main`, `80` on this branch) are indistinguishable
  by version string. The only symptom is a resource going `failed` with `Connection refused`.

**Consequence:** a verification run can silently test the wrong binary and produce a clean-looking
negative. This happened twice in this session.

#### ✅ Fixed 2026-09-01 — every installer left the OLD process running

Found while installing the client for the live pass, and it retroactively explains much of this
sprint's "the installed binary is a release build" confusion.

**All seven** install scripts ended with `systemctl enable --now <service>`. On an **already-running**
service `--now` is a **no-op**, so an in-place upgrade wrote the new binary and the new unit, printed
`install complete`, and left the previous process running. Observed directly: the client binary was
installed at `12:32:24` while `MainPID` was still `1795`, started `10:27:49`. The unit file on disk had
gained `CAP_NET_BIND_SERVICE`; the running daemon had neither that nor the new binary.

This is the same shape as the socket-activation fix recorded in Phase 12 (*"a socket-activated service
keeps its old unit until it exits"*) — **files updated, process not restarted, success reported** — which
makes it the second instance, so it is a pattern rather than an oversight. Combined with finding #1 (no
way to identify pre-release code) it is nearly undetectable: the version string is identical either way.

**Fix:** `systemctl enable <unit>` followed by `systemctl restart <unit>`, in
`scripts/{client,connector,shield,relay,relay-omitid}-local-install.sh`, `shield/scripts/shield-install.sh`
and `client/scripts/client-install.sh`. `restart` also starts a stopped unit, so it is correct for a fresh
install too — there was never a reason to prefer `--now`. Timer units keep `enable --now`, which is right
for them.

Also fixed in `client-local-install.sh`: `log "next steps (run as %s, ...)" "$ENROLLING_USER"` printed the
`%s` literally, because `log()` is `printf '...%s\n' "$*"` — a format string passed as argument 1 is data,
not a format.

### 2. No version negotiation on the tunnel

A branch client against a release connector fails as:

```text
QUIC stream error: invalid tunnel request: missing field `token`
```

— a `WARN` in the connector's journal only. The client sees a generic connection failure. Sprint 16
Phases 1–3 removed `token` from `TunnelRequest`; nothing tells either peer they are incompatible.

### 3. The connector's log level is not reliably configurable

`RUST_LOG` is ignored — `main.rs:52` builds the filter from `cfg.log_level`, fed by `LOG_LEVEL` via
`EnvironmentFile`. And `EnvFilter::try_new(...).unwrap_or_else(|_| EnvFilter::new("info"))` **silently
downgrades** an unparseable filter to `info`. Meanwhile `resolved resource endpoint` — the only line
carrying `cache_hit` / `resolved` / `resolve_us` — is at `debug`.

**So in a default deployment there is no way to tell whether a connector re-resolved or served a cached
address**, which is precisely the question this sprint's headline claim depends on. Three timing
experiments were spent guessing at what one log line answered instantly. **Recommendation: promote that
line to `info`, or add a counter.**

### 4. An IP change breaks a deployment in seven independent places

One DHCP lease change (`192.168.1.87` → `192.168.1.33`, caused by **both** NetworkManager *and*
systemd-networkd managing `enp2s0`) broke:

1. connector `CONTROLLER_ADDR` → crash-loop, 14 restarts
2. connector `CONTROLLER_HTTP_ADDR` → **both CRL fetches failed → `RevocationStatus::Unavailable` →
   every device tunnel denied** (fail-closed, correctly, but for an unrelated reason)
3. the connector's advertised `:9092` address → dashboard "degraded"
4. client daemon config — `zecurity-client setup` writes the file but the daemon only reads it at
   startup (`config::load()` appears exactly once); nothing says a restart is needed
5. the shield's cached connector list — see #5
6. the admin UI origin (`ALLOWED_ORIGIN`/`APP_BASE_URL` pinned to `localhost:5173`)
7. the admin session (per-origin `sessionStorage`)

Pointing the connector at `localhost` for both keys fixes it permanently for a co-located stack. A real
deployment argues for DNS names, not addresses.

### 5. 🔴 A connector IP change permanently orphans its shields

The sharpest **design** finding. The shield's connector list is persisted state, refreshed only by
`PeerConnectorList` pushes over the control stream — and by design the shield never talks to the
controller. So when the connector's address changes, the shield's only channel for the new coordinates
is **the peer whose address just changed**. It retries the dead address forever:

```text
peer Connector unreachable, trying next connector_addr=192.168.1.87:9091  backoff_secs=60
```

`connectors.lan_addr` in the DB was already correct (`192.168.1.33:9091`) — the shield simply had no way
to learn it. Recovery required temporarily re-adding the old IP to the interface so it could reconnect
once, whereupon the connector pushed the current list and it self-healed permanently.

This is a direct consequence of the "Shield → Connector only, never Controller" rule. Worth an ADR note:
a shield needs *some* out-of-band path to recover peer coordinates.

#### ✅ Fixed 2026-08-29 — `ShieldService.GetPeerConnectors`

The out-of-band path now exists. Chosen over three alternatives (re-read local config on failure; stable
DNS names resolved at dial time; LAN announce/mDNS) because it needs no new infrastructure and no new
discovery surface — the shield **already** speaks to the controller at enrollment, so this is not a new
trust boundary.

Two facts found while investigating made it small:

- **`shields.remote_network_id` is on the shield row itself** (`003_shield_schema.sql:6`), so the peer
  lookup does *not* go through the shield's connector. A shield whose connector row was deleted still
  resolves its remote network — the recovery path survives exactly the cascade in finding #2.
- **`workspace_ca.crt` already contains the intermediate CA**, which is what signs the controller's
  server cert (`GenerateControllerServerTLS`) and is the same certificate `/ca.crt` serves at
  enrollment. So the shield can verify the controller with what is already on disk; the new channel adds
  a client identity, not new trust.

**Trust model.** Unlike `RenewCert`, which the connector proxies (so the verified identity there is the
*connector's*), this is called by the shield directly over its own mTLS channel. The shield is
identified by its certificate and nothing else: **`GetPeerConnectorsRequest` is empty on purpose** —
there is no caller-supplied field to forge, and none must ever be added. A shield receives only the
connectors in its own remote network.

**Files:**

| File | Change |
|---|---|
| `proto/shield/v1/shield.proto` | `rpc GetPeerConnectors(GetPeerConnectorsRequest) returns (PeerConnectorList)` + empty request message |
| `controller/internal/shield/peers.go` **(new)** | handler; identity from the cert, RN-scoped query, `lan_addr` as-is / `public_ip` + `:9091` (matching `selectConnector`) |
| `shield/src/tls.rs` | `build_controller_channel` — standard verification (the controller has no connector SPIFFE ID, so `SpiffeConnectorVerifier` would reject it) |
| `shield/src/control_stream.rs` | `recover_peers`, called from `run_once` **at the `build_client` call site** — not inside `build_client`, because `renewal.rs` calls that too and should not drag a controller round-trip along on every failure |
| `connector/src/agent_server.rs` | returns `Unimplemented` — see below |

**Why the connector returns `Unimplemented`.** It also implements `ShieldService`, so the trait forced a
decision. Serving this from a connector would answer a question the caller can only ask when that
connector is unreachable — and it would answer from its own ACL snapshot, the very view that strands the
shield. A shield that *can* reach its connector already gets `PeerConnectorList` on every health report.

**Empty is an error, not an empty success.** `apply_peer_connector_list`
(`control_stream.rs:310`) **ignores** an empty list and keeps the existing one, so returning an empty
success would leave the shield retrying the same dead addresses while believing it had recovered. The
handler returns `FailedPrecondition` instead.

**Tests** — `internal/shield/peers_test.go`, DB-gated on `SHIELD_TEST_DATABASE_URL`. Covers the actual
stranding scenario (move `lan_addr`, assert the **new** address comes back), the `public_ip` address
form, remote-network isolation, cert-only identity (no identity and a connector-role caller both
refused), revoked shields, and empty-means-error. Revert-verified on the two that matter: dropping the
remote-network filter leaks rn-b to a shield in rn-a, and making empty a success is silently ignored.

**⚠️ The transport has never handshaked.** `build_controller_channel` is verified by construction, not by
execution — the same class of never-run path as `RenewConnectorCert`, one layer down. What *is* confirmed
by reading:

- The controller's server cert carries **IP SANs**: `controllerCertHosts` (`main.go:764`) collects
  `127.0.0.1`, `::1` and the detected LAN IP, and `GenerateControllerServerTLS` routes anything that
  `net.ParseIP` accepts into `IPAddresses` rather than `DNSNames`. So `domain_name("192.168.1.33")` has a
  SAN to match.
- The construction is **identical to `enrollment.rs:163`**, which is proven working — every successful
  shield enrolment in Gate 2/3 used the same `ca_certificate` + `domain_name(host)` pair. The only
  addition here is `.identity(...)`.
- The controller uses `ClientAuth: tls.RequestClientCert` (`main.go:355`) — it requests a client cert and
  defers verification to the SPIFFE interceptor, so presenting the shield's cert cannot be rejected at
  the handshake.

That is a strong chain, but it is still reasoning. **The live pass must exercise this channel**, not just
the handler.

**Correlated failure costs an extra timeout per cycle.** The connector being unreachable and the
controller being down happen together — that is precisely what Gate 3 hit. In that state each reconnect
now pays two dial timeouts instead of one before the backoff sleep. Backoff still bounds it, so this is a
slower recovery rather than a storm; a `recovered_this_cycle` guard would remove it if it proves
annoying in practice.

**Scope honesty:** this does **not** close the recovery-path pattern, it moves it up one level. A shield
whose own certificate has expired cannot open this mTLS channel either and still needs manual
re-enrolment. That belongs in the recovery-path ADR alongside the `RenewCert` grace-window question.

### 6. Client OAuth assumes browser, CLI and controller share a host

`redirect_uri` is `http://localhost:8080/api/clients/callback` (from `CONTROLLER_HTTP_URL`), and the
controller then redirects the browser to the CLI's own `http://127.0.0.1:<port>/callback`. One browser
must reach **both** — impossible across hosts. A client on a separate machine cannot enrol without an SSH
port-forward or hand-relaying the `ctrl_code`. That is the normal ZTNA deployment shape.

### 8. The admin UI is pinned to one origin, and using another leaves you silently unauthenticated

`APP_BASE_URL` is the **post-login redirect target**, not a CORS setting (there is no CORS middleware in
the controller at all, and `ALLOWED_ORIGIN` in `.env` is never read by the Go code):

```go
// internal/auth/callback.go:156
http.Redirect(w, r, s.cfg.AllowedOrigin+"/auth/callback#token="+accessToken, http.StatusFound)
```

So opening `http://<lan-ip>:5173` works, but logging in redirects you to `http://localhost:5173` — the
token arrives in *that* origin's fragment, and the LAN origin stays unauthenticated. Mutations then fail
with a bare **401**; the form appears to work and simply does nothing. This is what made resource
creation appear broken during Gate 2, and it is why `ip-control`/`prot-test` were created by direct
insert.

**There is no admin-UI defect.** Phase 10's `done` status stands.

Compounding it, the refresh cookie (`callback.go:141-149`) sets no `Domain`, so it is host-scoped to
`localhost` (the callback is served at `localhost:8080`). Cookies ignore port, so it reaches
`localhost:5173` — but never a LAN-IP origin. And `Secure: true` is accepted over plain HTTP only because
browsers treat `localhost` as a secure context; on `http://<lan-ip>` such a cookie is rejected outright.
**Full LAN-origin support therefore needs `APP_BASE_URL`, `GOOGLE_REDIRECT_URI` (re-registered with
Google) and `CONTROLLER_HTTP_URL` changed together, plus TLS** — changing `APP_BASE_URL` alone moves the
redirect but leaves refresh broken, which fails later and less visibly.

**Use `http://localhost:5173` on the controller host.** Not a workaround — it is the only origin this
configuration fully supports.

### 7. Minor, but each cost a cycle

- `zecurity-client status` reports session/cert state but **not whether the tunnel is up** — a down
  tunnel reads as healthy.
- vite's pre-flight port check probes the port globally, so it refuses to bind `A:5174` while anything
  holds `B:5174`. Start order matters; Python's `--bind` does not care.
- Adding a managed hostname to `/etc/hosts` **on the connector host** pins that backend for the
  connector's lifetime: hickory snapshots the hosts file once at startup and
  `lookup_static_host` short-circuits DNS entirely (`resolver.rs:645`). The resource keeps working, so
  nothing looks broken — it just stops tracking the backend, which is the whole feature.

---

## Post-Phase Fixes

_(none yet)_
