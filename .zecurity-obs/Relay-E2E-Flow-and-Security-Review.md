---
type: review
scope: relay
status: draft
date: 2026-07-03
branch: integration/relay-merge
tags:
  - relay
  - security
  - architecture
  - review
---

# Relay — End-to-End Flow & Security Review

> Scope: the **entire relay subsystem** — relay server (`relay/`), controller relay
> package (`controller/internal/relay/`, `controller/internal/pki/relay.go`), connector
> relay client/selector (`connector/src/relay_*.rs`), and client relay path
> (`client/src/relay_pool.rs`, `transport.rs`, `net_stack.rs`). Covers all four hops:
> connector→relay, controller↔relay, client→relay, and client→connector-via-relay.
>
> This document has two halves: **(A) how the relay actually works today** (flows, with
> file:line references) and **(B) what is wrong and how to fix it** (severity-rated
> findings + a remediation roadmap).

---

## 1. Architecture at a Glance

The relay is a **platform-level, workspace-agnostic QUIC bridge**. It exists so a client
can reach a connector that has no public inbound address: the connector dials *out* to the
relay and registers; the client dials the relay and asks to be bridged to a connector; the
relay splices the two QUIC streams together. The relay **never terminates the tunnel TLS** —
client↔connector run an inner mTLS session end-to-end, so the relay only moves ciphertext.

```
                    ┌─────────────────────────── Controller ───────────────────────────┐
                    │  gRPC :9090 (mTLS + SPIFFE interceptor)   HTTP :8080 (admin JWT)   │
                    │   • RelayService/Provision  (UNAUTH — see F1)                      │
                    │   • RelayService/Heartbeat  (relay mTLS)                           │
                    │   • ConnectorService/Control (connector mTLS) → LabelledRelayList  │
                    │   • Platform Intermediate CA signs relay + connector + client certs│
                    └───▲───────────────▲──────────────────────────────▲────────────────┘
       provision/hb     │               │ heartbeat                    │ control stream
       (relay cert)     │               │ (capacity)                   │ (relay list, ACL)
                        │               │                              │
                  ┌─────┴─────┐   ┌─────┴──────┐                 ┌─────┴──────┐
                  │   Relay   │◄──┤  Connector │                 │   Client   │
                  │ QUIC :9093│   │  (dials out │                │  (daemon)  │
                  │  mTLS     │   │   registers)│                │            │
                  └─────▲─────┘   └────────────┘                 └─────┬──────┘
                        │                                              │
                        │   client opens Lookup(connector_id)         │
                        └──────────────────────────────────────────────┘
                              relay splices: client_stream ⇄ connector_stream
                              (inner client↔connector mTLS is opaque to relay)
```

**Trust anchors.** Everyone chains to one **Platform Intermediate CA**. The relay leaf lives
at the *global* trust domain `spiffe://zecurity.in/relay/<uuid>`; connectors and clients live
at *per-workspace* trust domains `spiffe://ws-<slug>.zecurity.in/<role>/<uuid>`. The relay
trusts the Intermediate CA as its only root and requires peers to present `leaf + Workspace CA`
so the chain builds `leaf → Workspace CA → Intermediate CA` (`relay/src/tls.rs:19-54`).

**Key property (good):** because the inner client↔connector session is TLS 1.3 mTLS
(`client/src/relay_pool.rs:116-138`, `connector/src/relay_handler.rs:159-194`), a malicious or
compromised relay **cannot read or forge tunnel payload** — it can only drop, delay, or
observe metadata.

---

## 2. PKI & Identity Model

| Entity | SPIFFE ID | Trust domain | Signed by | Validity |
|--------|-----------|--------------|-----------|----------|
| Relay | `spiffe://zecurity.in/relay/<uuid>` | global | Platform Intermediate CA | **30 days**, no renewal (`pki/relay.go:60-62`) |
| Connector | `spiffe://ws-<slug>.zecurity.in/connector/<uuid>` | per-workspace | Workspace CA | 7 days, auto-renew |
| Client device | `spiffe://ws-<slug>.zecurity.in/client/<uuid>` | per-workspace | Workspace CA | short-lived |

- Relay cert has **both** `ServerAuth` and `ClientAuth` EKU (`pki/relay.go:73-76`) — server for the
  QUIC listener, client for the heartbeat mTLS to the controller.
- SPIFFE parsing is strict everywhere: exactly one URI SAN, canonical lowercase UUID, no
  port/query/fragment/userinfo (`relay/src/spiffe.rs:26-88`).
- Relay bootstrap pins the Intermediate CA by **SHA-256 fingerprint** supplied out-of-band
  (`RELAY_CA_FINGERPRINT`), so the plaintext `GET /ca.crt` fetch is safe against substitution
  (`relay/src/provision.rs:97-135`, `relay/src/config.rs:61-66`).

---

## 3. Flow 1 — Relay Provisioning (Relay ↔ Controller)

**Goal:** a fresh relay obtains a Platform-CA-signed leaf certificate.

1. **(Optional) Admin pre-registration.** `POST /api/relays` (admin JWT, role=admin,
   `controller/cmd/server/main.go:256-261`) inserts a `relays` row with status `pending`,
   a DNS/IP allowlist, and issues a single-use 24h provisioning JWT whose `jti` is stored in
   Valkey and on the row (`admin_handler.go:36-86`, `token.go:29-49`).
2. **Relay bootstrap.** On first start (`relay/src/main.rs:42`), if no cert material exists the
   relay: fetches the CA over HTTP, verifies the pinned fingerprint, generates an ECDSA P-384
   keypair + CSR with SPIFFE + DNS/IP SANs (`relay/src/csr.rs`), and calls
   `RelayService/Provision` over server-authenticated TLS (`relay/src/provision.rs:26-57,137-171`).
3. **Controller signs.** `Provision` validates the UUID, SANs, and CSR self-signature, then
   `pki.SignRelayCert` issues a 30-day leaf and the row is marked `active` (self-inserting one
   if absent) (`provision.go:84-148`, `pki/relay.go:34-158`).
4. Relay stores `relay.key` (0600), `relay.crt`, `intermediate-ca.crt` and starts the listener +
   heartbeat (`relay/src/provision.rs:75-95`, `main.rs:42-70`).

> ⚠️ **The provisioning token is generated by the admin API but never checked by the server.**
> See finding **F1** — this is the most serious issue in the subsystem.

---

## 4. Flow 2 — Heartbeat, Capacity Labeling & Relay-List Distribution

**Relay → Controller heartbeat** (`relay/src/heartbeat.rs`, gRPC mTLS every 30s):
- Payload: version, hostname, uptime, `registered_connectors`, `listen_port`,
  `connection_count` (live bridged streams, `ACTIVE_STREAMS`), `max_connections`
  (`relay/src/heartbeat.rs:71-99`, `relay/src/session.rs:19,261-263`).
- Controller verifies the relay leaf against the Intermediate-CA-from-DB, re-checks
  role/trust-domain/UUID/URI-SAN (`internal/relay/heartbeat.go:56-70`, `spiffe.go:226-246`).
- Observed address derived from the **TCP peer address** (`peer.FromContext`, not a header — not
  header-spoofable) and classified public/private/loopback; a public relay's advertised address
  is `peerIP:listen_port` (`heartbeat.go:242-281`).
- Redis caches liveness + metadata; DB write throttled to every 5m unless metadata changed
  (`heartbeat.go:172-214`).

**Capacity state machine** (`internal/relay/capacity_label.go`):
- `fill_ratio = connection_count / max_connections`. Tiers with dead-band hysteresis:
  High (enter <0.45 / exit ≥0.50), Medium (enter <0.75 / exit ≥0.80), Low (≥0.80, dropped).
- Hold-down (default 60s) before a label change is promoted; the read-decide-write runs in a
  `SELECT … FOR UPDATE` tx so concurrent heartbeats can't race (`store.go:366-421`).

**LabelledRelayList → Connectors** (`internal/connector/control_stream.go`):
- `BuildLabelledRelayList` selects `status='active' AND capacity_label IN ('high','medium') AND
  (public_addr present OR observed public)` (`store.go:294-349`).
- Pushed to every connected connector on **pool change** (heartbeat promotion / address change /
  eviction) and on **connector connect** (`main.go:147-156`, `control_stream.go:266-281,350-360`).

**Eviction** (`internal/relay/expiry.go`, every 60s): relays with `last_heartbeat_at` older than
90s are marked inactive and workspaces are notified. **This is currently broken — see F6.**

---

## 5. Flow 3 — Connector ↔ Relay

The connector is the **server side** of the bridge: it registers and then accepts relay-opened
streams. Relay *selection* (which relay to register with) is the ADR-016 probe/rank machinery.

**Selection & migration** (`connector/src/relay_selector.rs`):
1. Controller pushes `LabelledRelayList`; the selector picks a relay — warm-start from persisted
   ranking, else a random Tier-1 (thundering-herd defense), else Tier-2.
2. Background probe sweep (`relay_probe.rs`): dial each relay over QUIC mTLS, send `Probe{request_id}`,
   measure RTT, score = RTT. Migrate only if a relay beats the current by >15% **and** >10ms.
3. Migration is make-before-break: register on the new relay, drain the old (idle-based, hard cap),
   then abort the old session.

**Registration** (`relay/src/session.rs:120-143`):
- `Register{connector_id, spiffe_id}` is validated against the **authenticated cert**:
  `identity.entity_id == connector_id` and `identity.uri == spiffe_id` (`session.rs:331-342`) —
  a connector can only register **its own** id. Registry keyed by connector_id with a
  `registration_id` guard so a stale close can't evict a newer registration (`state.rs:24-61`).

**Probe** (`relay/src/session.rs:64-119`): concurrency-capped + per-connector rate-limited, echoes
`request_id`, never registers. **The rate-limit key is attacker-controlled — see F5.**

---

## 6. Flow 4 — Client → Relay → Connector

This is the data path that carries user traffic.

**Client picks a transport** (`client/src/transport.rs:71-103`): **direct first** (2s timeout),
relay only on fallback. An auth failure on the direct path returns immediately (no relay retry);
a connect failure or timeout falls through to relay.

**Client → Relay** (`client/src/relay_pool.rs`):
- QUIC mTLS to the relay; relay cert verified by **exact SPIFFE match** against the
  controller-supplied `relay_spiffe_id` (`relay_pool.rs:59-72`, verifier `tunnel_pool.rs:69-147`).
- Sends `Lookup{connector_id}` (4-byte length-prefixed JSON, 16 KiB cap), reads `RelayAck`
  (`relay_pool.rs:92-114`).

**Relay bridges** (`relay/src/session.rs:144-265`):
- Only a `client`-role peer may Lookup. The relay finds the registered connector and enforces
  **workspace isolation**: `connector.trust_domain == client.trust_domain` **and**
  `same_workspace(connector_spiffe, client_spiffe)` — a client in workspace A cannot bridge to a
  connector in workspace B (`session.rs:236-246`, `spiffe.rs:120-132`).
- On success it opens a stream to the connector and `pipe_streams` splices the two with
  `tokio::io::copy` in both directions (`session.rs:248-329`).

**Client → Connector (inner tunnel, opaque to relay)** (`relay_pool.rs:116-149`):
- TLS 1.3 mTLS layered over the bridged stream; connector cert verified by exact SPIFFE match;
  client presents its device cert. The connector re-authenticates the client and checks the
  local ACL snapshot + CRL before dialing the resource (`connector/src/relay_handler.rs:159-194`,
  `device_tunnel.rs`).

---

## 7. What's Already Solid

These are working correctly and should be preserved through any refactor:

- **End-to-end confidentiality.** Inner client↔connector mTLS (TLS 1.3) means the relay only sees
  ciphertext; a rogue relay cannot read or forge tunnel data.
- **Exact SPIFFE matching** on every hop (relay verifier, client→relay, client→connector,
  connector→relay) — no prefix/substring trust.
- **Workspace isolation at bridge time** — cross-workspace Lookup is denied (`session.rs:236-246`).
- **Registration bound to authenticated identity** — no connector-id impersonation (`session.rs:331-342`).
- **Race-safe capacity state machine** (`FOR UPDATE`) with hysteresis + hold-down to prevent flapping.
- **Bounded relay concurrency** — connection / lookup-bridge / probe semaphores (`listener.rs`, `session.rs`).
- **CA fingerprint pinning** on relay bootstrap; **0600** key file perms.
- **Observed IP from the TCP peer**, not a spoofable header.
- **Parameterized SQL** throughout; no injection observed.

---

## 8. Findings

Severity: **C**ritical / **H**igh / **M**edium / **L**ow.

| ID | Sev | Title | Location |
|----|-----|-------|----------|
| F1 | **C** | Relay provisioning is unauthenticated (token machinery is dead code) | `internal/relay/provision.go:80-148`, `spiffe.go:160-164`, `token.go:53,81` |
| F2 | **H** | No CRL/revocation check on relay outer mTLS or controller heartbeat mTLS | `relay/src/tls.rs:79-105`, `internal/relay/spiffe.go:239` |
| F3 | **M** | Client rebuilds transports on action-triggered ACL changes, but has no standalone background refresh loop | `client/src/daemon.rs:198-241,669-698,1007-1073` |
| F4 | **H** | Relay eviction is broken — `inactive` violates the status CHECK constraint | `internal/relay/store.go:247-250`, `migrations/019_relays.sql:14` |
| F5 | **H** | Relay probe rate-limiter keyed on unauthenticated `connector_id` → bypass + unbounded map | `relay/src/session.rs:34,64-109` |
| F6 | **M** | Client relay fallback still lacks operation timeouts | `client/src/relay_pool.rs:92-149`, `transport.rs:86-100`, `net_stack.rs:416-423` |
| F7 | **M** | Unbounded channels/buffers in client net_stack (memory DoS) | `client/src/net_stack.rs:104-121,216-217` |
| F8 | **M** | Self-asserted SAN allowlist on self-provision path | `internal/relay/provision.go:93-100`, `pki/relay.go:120-155` |
| F9 | **M** | `max_connections == 0` ⇒ relay permanently labeled `high` | `internal/relay/capacity_label.go:68-72` |
| F10 | **M** | No relay cert renewal; 30-day cert expires silently | `relay/src/main.rs:42`, `internal/relay/` (no renew RPC) |
| F11 | **M** | LabelledRelayList version inconsistency (wall-clock vs monotonic) | `main.go:153`, `control_stream.go:353` |
| F12 | **L** | Direct connector trust store is broader than necessary | `client/src/tunnel_pool.rs:256-262` |
| F13 | **L** | No rate limiting on Provision → unbounded row creation + CA signing | `internal/relay/provision.go:84` |
| F14 | **L** | Deployment: `listen_port` client-controlled; observed-IP wrong behind LB | `internal/relay/heartbeat.go:242-281` |
| F15 | **L** | Leftover debug `println("=== STDOUT TEST ===")` and `connetor_addr` typo | `control_stream.go:287`, `client/src/daemon.rs:745` |

---

## 9. Detailed Findings & Fixes

### F1 — Relay provisioning is unauthenticated  **(Critical)**

**What.** `RelayService/Provision` is in the SPIFFE interceptor's skip list (`spiffe.go:160-164`)
and the handler **ignores** `provisioning_token` (`provision.go:82-84` comment: *"reserved for a
future authenticated operator flow and is ignored"*). `VerifyProvisioningToken` and
`BurnProvisioningJTI` exist in `token.go` but have **zero callers**. The relay client even sends
`provisioning_token: String::new()` (`relay/src/provision.rs:157`).

**Impact.** Anyone who can reach the controller gRPC port can submit a self-signed CSR with any
canonical UUID and receive a valid Platform-Intermediate-CA-signed relay leaf
(`spiffe://zecurity.in/relay/<uuid>`, ServerAuth+ClientAuth). The handler even **self-inserts an
`active` relays row**. The rogue relay can then heartbeat, appear in the `LabelledRelayList` that
*all workspaces'* connectors and clients trust, and attract traffic. Because it can pick a UUID
that collides with a real relay, it can also **hijack a legitimate relay's identity** and (via the
next heartbeat's observed-IP) repoint that relay's advertised address at itself. Tunnel payload
stays encrypted (good), but this enables **DoS, traffic/metadata analysis, and relay-identity
takeover**, plus anonymous CA signing.

**Fix (low effort — the code already exists).**
1. Wire the existing token flow into `Provision`: require `provisioning_token`, call
   `VerifyProvisioningToken(jwtSecret, token)`, assert `claims.RelayID == req.RelayId`, then
   `BurnProvisioningJTI` atomically (reject if already burned). Reject with `Unauthenticated` on
   any failure.
2. Remove the self-provisioning `InsertProvisionedRelay` fallback, or gate it behind a verified
   token — provisioning should require a pre-created `pending` row from the admin API.
3. Keep `Provision` in the mTLS-skip list (bootstrap has no cert yet) but consider additionally
   restricting the endpoint at the network layer (the admin token is the real control).
4. Make the relay client send the real token (env/file), mirroring connector enrollment.

### F2 — No revocation checking on relay-facing mTLS  **(High)**

**What.** The relay's QUIC verifier (`relay/src/tls.rs`) and the controller's heartbeat verifier
(`internal/relay/spiffe.go:239`) validate the chain + SPIFFE but never consult a CRL. A revoked
(but unexpired) connector, client, or relay cert still authenticates. The connector's *inner* mTLS
does check the CRL (`device_tunnel.rs`), so a revoked client can't reach the resource — but a
revoked **connector** can still register on the relay and receive bridged streams, and a revoked
**relay** keeps heartbeating for up to 30 days.

**Fix.** Add CRL enforcement to the relay's client-cert verifier and the controller's relay-cert
verifier. The connector already has a `CrlManager` (`connector/src/crl.rs`) that periodically pulls
the CRL — reuse that pattern on the relay (fetch from the controller, cache, refresh) and add a
revocation check to `verifyRelayCertificate`.

### F3 — Client rebuilds transports on action-triggered ACL changes, but has no standalone background refresh loop  **(Medium)**

**What.** This finding is partially fixed. The daemon now compares the fetched ACL snapshot version
with the in-memory version and restarts the running tunnel when the version changes during explicit
client actions: `sync`, `resources` after TTL refresh, and login/PostLoginState. Restarting performs
`down` then `up`, rebuilding routes, listeners, and the per-resource connector transport lists from
the new ACL. The client also receives all connector options for a remote network, orders the
preferred connector first, and tries the remaining connectors if a connector cannot open the stream
or returns `SHIELD_NOT_ATTACHED`.

**Remaining gap.** There is no standalone background ACL refresh loop that restarts the tunnel
without any IPC/user action. If the client stays up and no `sync`, `resources`, login, or `up`
action occurs, it can keep the previous transport map until the next action-triggered refresh.
For the current design this is acceptable, because failover is handled by trying the connector and
relay options already present in the snapshot; the practical availability risk is now bounded by
the missing relay/handshake timeouts covered in F6.

**Fix.** If fully autonomous convergence is required later, add a background ACL refresh task that
calls the existing `sync_acl_now` + `restart_tunnel_if_running` path when the snapshot version
changes. A lower-disruption long-term alternative is to push updates into `net_stack` through a
watch channel or `ArcSwap` and swap transport maps without a full TUN restart.

### F4 — Relay eviction is broken (status CHECK violation)  **(High)**

**What.** `EvictExpiredRelays` runs `UPDATE relays SET status='inactive' …` (`store.go:247-250`),
but the column constraint is `CHECK (status IN ('pending','active','deleted'))`
(`migrations/019_relays.sql:14`) and no later migration adds `'inactive'`. The UPDATE therefore
fails with a constraint violation on every sweep; the error is logged and swallowed
(`expiry.go`), so **dead relays are never evicted** and keep appearing in `BuildLabelledRelayList`.
Connectors/clients continue to be told to use a crashed relay.

**Fix.** Either (a) add a migration extending the CHECK to include `'inactive'`, or (b) change the
eviction to a status already allowed. Whichever is chosen, ensure `RecordHeartbeat`
(`WHERE status <> 'deleted'`) does **not** silently resurrect an evicted relay without it going
back through eligibility — an evicted relay that heartbeats again should return to `active` only
via the normal path. Add an integration test that runs eviction against a real Postgres (the unit
test likely uses a mock and misses the constraint).

### F5 — Probe rate-limit key is attacker-controlled  **(High)**

**What.** The per-connector probe rate limiter is keyed on the `connector_id` **from the Probe
message body**, not the authenticated cert identity, and `Probe` (unlike `Register`) never checks
`connector_id == identity.entity_id` (`session.rs:64-109`). The `probe_rate_tracker` HashMap is
never pruned (`session.rs:34`). A single authenticated-but-misbehaving connector can send probes
with unlimited unique `connector_id` strings to (a) evade the rate limit entirely and (b) grow the
map without bound → memory-exhaustion DoS on the relay.

**Fix.** Key the rate limiter on `identity.entity_id` (the authenticated connector UUID) and
reject any Probe whose body `connector_id != identity.entity_id`. Periodically prune stale windows
(or use a bounded LRU / a fixed-size sliding window keyed by the finite set of real connectors).

### F6 — Client relay fallback still lacks operation timeouts  **(Medium)**

**What.** The old partial-read bug on `TunnelResponse` has been fixed: the client now uses a
4-byte length-prefixed framed JSON read. The remaining issue is timeout coverage. The relay branch
of `open_authenticated_stream` still has no timeout on relay connect/cache lookup, `open_bi`, the
Lookup write, the Ack read, or the inner TLS handshake (`relay_pool.rs:92-149`); only the *direct*
leg is bounded (2s, `transport.rs:72`). `net_stack::relay_tcp_to_quic` also writes `TunnelRequest`
and reads framed `TunnelResponse` without a timeout (`net_stack.rs:416-423`). A stalled or
malicious relay/connector can therefore hang that connection attempt and delay failover to the next
connector transport.

**Fix.** Wrap the whole relay fallback attempt in an overall timeout, and add per-step timeouts for
relay connect/open, Lookup write/Ack read, inner TLS, and TunnelRequest/TunnelResponse. On timeout,
log and continue to the next connector transport.

### F7 — Unbounded channels/buffers in client net_stack  **(Medium)**

**What.** `net_stack` uses `mpsc::unbounded_channel` for both directions of every relay plus the
TUN tx, and an unbounded `write_buf: VecDeque<u8>` overflow buffer (`net_stack.rs:104-121,216-217`).
A slow/stalled peer lets these grow without bound → memory DoS.

**Fix.** Use bounded channels with backpressure (drop-oldest or await-capacity as appropriate),
and cap the overflow buffer, closing the flow if the cap is exceeded.

### F8 — Self-asserted SAN allowlist on self-provision  **(Medium)**

**What.** `Provision` checks the CSR's DNS/IP SANs against `req.DnsSans`/`req.IpSans` **from the
same request** (`provision.go:93-100` → `pki/relay.go:120-155`). The operator-registered
`dns_allowlist`/`ip_allowlist` on the pre-created row is not consulted, so the SAN restriction is
self-asserted on the self-provisioning path.

**Fix.** Once F1 requires a token tied to a pre-created row, load that row's stored allowlist and
validate the CSR SANs against it — not against caller-supplied fields.

### F9 — `max_connections == 0` ⇒ permanent `high`  **(Medium)**

**What.** `computeCandidateLabel` returns `high` whenever `max == 0` (`capacity_label.go:68-72`),
so a relay that never reports a ceiling stays maximally eligible forever, defeating capacity-based
shedding.

**Fix.** Treat `max_connections == 0` as **ineligible** (drop from the list) or clamp to a
conservative configured default, and log a warning. Require relays to report a ceiling.

### F10 — No relay certificate renewal  **(Medium)**

**What.** The relay provisions once and never renews (`main.rs:42` short-circuits when files
exist; heartbeat never renews). The 30-day cert will expire and the relay silently stops
authenticating.

**Fix.** Add a renewal path (a `RenewRelayCert` RPC over the existing heartbeat mTLS, proof-of-
possession like the connector's `RenewCert`), and have the relay renew at ~⅔ of cert lifetime.
Shorten the TTL once renewal exists.

### F11 — LabelledRelayList version inconsistency  **(Medium)**

**What.** The broadcast path overwrites the store-derived monotonic version with wall-clock
`time.Now().Unix()` (`main.go:153`), while the connect-time push uses the store version
(`control_stream.go:353`). Connectors use the version to decide whether to re-probe; inconsistent
or non-monotonic versions defeat that optimization and can cause redundant probing or missed
updates.

**Fix.** Use one monotonic version source (the store-derived label-change epoch) in both paths.

### F12 — Direct connector trust store is broader than necessary  **(Low)**

**What.** Connector certificates are signed by the tenant Workspace CA, while relay certificates are
signed by the Platform Intermediate CA. The relay paths already reflect this split: relay outer TLS
trusts the Platform Intermediate CA, and relay inner connector TLS trusts only the Workspace CA
(`relay_pool.rs:59-61,116-119`). The direct connector path currently trusts both Workspace CA and
Intermediate CA (`tunnel_pool.rs:256-262`), which is broader than needed.

This is not an `UnknownIssuer` availability bug in the relay path; that earlier interpretation
assumed connector certificates chain directly through the Intermediate. The remaining concern is
defense in depth: direct connector TLS should reject other workspace-issued connector chains before
the SPIFFE verifier has to enforce workspace isolation.

**Fix.** Keep the trust anchors path-specific:
- Direct connector TLS: Workspace CA only.
- Relay outer TLS: Platform Intermediate CA only.
- Relay inner connector TLS: Workspace CA only.

Concretely, remove `ca_bundle.intermediate_ca` from the direct connector `RootCertStore` in
`TunnelPool::new()`. Keep the existing SPIFFE checks.

### F13 / F14 / F15 — Lower-priority hardening & cleanup

- **F13:** add basic rate limiting / quota to `Provision` (mostly subsumed by F1).
- **F14:** document that the controller must not sit behind an L4/L7 proxy for relay heartbeats
  (observed IP would become the proxy's); if it must, add a trusted-proxy config. Consider
  validating/permitting `listen_port` ranges.
- **F15:** remove the debug `println("=== STDOUT TEST ===")` (`control_stream.go:287`) and fix the
  `connetor_addr` log-field typo (`client/src/daemon.rs:745`).

---

## 10. Remediation Roadmap (suggested order)

**Phase 1 — Close the trust hole (do first):**
1. **F1** — authenticate `Provision` (wire the existing token verify + JTI burn; require a
   pre-created row). This is the single highest-value fix.
2. **F8** — validate CSR SANs against the row's stored allowlist (rides on F1).
3. **F2** — add CRL checking on relay + controller relay-cert verifiers.

**Phase 2 — Correctness / availability:**
4. **F4** — fix eviction (migration or allowed status) + real-DB test.
5. **F3** — rebuild client transports on ACL version change.
6. **F5** — bind probe rate-limit to the authenticated identity + prune the map.
7. **F9** — treat `max_connections == 0` as ineligible.

**Phase 3 — Hardening:**
8. **F6 / F7** — timeouts on the relay leg; bounded channels/buffers on the client.
9. **F10** — relay cert renewal + shorter TTL.
10. **F11** — single monotonic relay-list version.
11. **F12 / F13 / F14 / F15** — trust-anchor alignment, provision rate limiting, deployment docs,
    cleanup.

---

## 11. File Map (relay subsystem)

**Relay server** — `relay/src/`: `main.rs` (bootstrap+wiring), `provision.rs` (enroll),
`csr.rs`, `tls.rs` (server config + peer identity), `spiffe.rs` (parse/validate),
`listener.rs` (accept loop + conn cap), `session.rs` (register / probe / lookup / bridge),
`state.rs` (connector registry), `heartbeat.rs`, `config.rs`, `protocol.rs` (wire format).

**Controller** — `controller/internal/relay/`: `provision.go`, `token.go` (dead), `admin_handler.go`,
`store.go`, `heartbeat.go`, `capacity_label.go`, `expiry.go`; `internal/pki/relay.go` (signing);
`internal/connector/control_stream.go` (relay-list build/broadcast + placement); wiring in
`cmd/server/main.go`; auth interceptor `internal/spiffe/…`.

**Connector** — `connector/src/`: `relay_client.rs` (session), `relay_selector.rs` (selection/
migration), `relay_probe.rs`, `relay_ranking.rs`, `relay_attachment.rs`, `relay_handler.rs`
(inner mTLS + drain), `control_stream.rs` (receives LabelledRelayList).

**Client** — `client/src/`: `relay_pool.rs` (dial + Lookup + inner tunnel), `transport.rs`
(direct-vs-relay), `tunnel_pool.rs` (direct + verifiers), `net_stack.rs` (data plane),
`daemon.rs` (transport build + ACL sync).

**Proto** — `proto/relay/v1/relay.proto` (Provision + Heartbeat), `proto/connector/v1/connector.proto`
(LabelledRelayList, ConnectorRelayState), `proto/client/v1/client.proto` (ACLConnector relay coords).
