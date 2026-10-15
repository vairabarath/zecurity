---
title: Connector Identity-Level Revocation — Architecture Investigation
date: 2026-10-13
type: read-only-investigation
status: evidence-for-decision (no implementation authorized)
branch: feat/sprint20-m1-phase2
revision: 9844a00
---

# Connector Identity-Level Revocation — Architecture Investigation (2026-10-13)

> The line references were re-verified against `9844a00` after drafting; corrected values are applied.

**Labels:**
- **[SRC]**: confirmed by source;
- **[TEST]**: confirmed by an automated test;
- **[LIVE]**: historical live evidence (10-12 / 10-13 reports);
- **[INF]**: inferred;
- **[NOT EST]**: not established;
- **[DECISION]**: open design decision.

Line numbers refer to `9844a00`.

## 1. Scope, branch, commit, working tree

- **Repo:** `~/zecurity`, branch `feat/sprint20-m1-phase2`, HEAD `9844a00`.
- **Working tree:** 26 pre-existing status lines (the earlier uncommitted work plus the untracked `connector-revocation/` directory).
  - The only change made by this investigation is this file.
  - The two earlier reports in this directory are unchanged (sha prefixes `e0c181bb…` and `3878d060…`, checked before and after).
- **Method:** read-only source and test inspection. No services touched, no tests run, no commit.
- **Inputs:**
  - the 10-12 and 10-13 live reports;
  - the 10-13 serial-history investigation (in chat);
  - `Sprint20/Live-Acceptance-Run-Sheet.md` Finding 34;
  - `Sprint20/path.md:289`.

## 2. Executive summary

1. **The connector SPIFFE ID is a derived attribute, not a stored key** [SRC].
   - It is `spiffe://<workspace trust_domain>/connector/<connectors.id>` (`controller/internal/appmeta/identity.go:85-87`).
   - `connectors.id` is a random UUID primary key, and the trust domain is the workspace's unique `trust_domain`.
   - It is stable across renewal **by construction** in the controller [SRC]. No automated test covers renewal-time SPIFFE equality for connectors [NOT EST by test].
2. **The controller already revokes at identity level** for its own control plane [SRC + TEST].
   - Every connector RPC maps the presented certificate's `(trust_domain, entity id)` to `connectors.id` and checks DB `status`/`revoked_at`.
   - The serial is never consulted, so every certificate of a revoked connector (current, previous, renewed) is refused by the controller.
3. **Outside the controller, no component receives authoritative connector-revocation state** [SRC].
   - Clients and other connectors learn of it only **implicitly**: the revoked connector disappears from the active-connector lists in ACL and transport snapshots.
   - Relays learn of it only via a **serial** on the workspace CRL, and that CRL holds one serial per connector (§10).
   - The revoked connector itself never interprets its `PermissionDenied`.
   - The shield receives nothing.
4. **An identity-level revocation mechanism already exists, but for client devices, not connectors** [SRC + TEST].
   - Device revocation drops the device SPIFFE ID from `ACLEntry.allowed_spiffe_ids`.
   - Connectors compare the presented client SPIFFE ID by exact equality, and their ACL-diff (`PolicyCache::update_and_revoked`) cancels established sessions for removed (spiffe, resource) pairs.
   - There is no connector-side or client-side equivalent for *connector* identities.
5. **Identity checks are inconsistent across components** [SRC]:
   - the direct-path client accepts **any** connector of the workspace (prefix match);
   - the relay-path client and the shield do exact matches;
   - the relay accepts any connector identity (it checks role, chain and CRL serial).
6. **Established sessions are revalidated nowhere for connector revocation** [SRC]; consistent with [LIVE] 10-12/10-13.
7. **Stale state:**
   - the only freshness bound on revocation data is the CRL's 24 h `nextUpdate` (connector, relay);
   - ACL, transport and shield peer lists have no expiry [SRC].
   - Acceptable staleness is an open human decision.

## 3. SPIFFE ID construction and stability

| Property | Finding | Label / evidence |
|---|---|---|
| Format | `"spiffe://" + trustDomain + "/connector/" + connectorID` | [SRC] `appmeta/identity.go:85-87` `ConnectorSPIFFEID`; [TEST] `appmeta/identity_test.go:25` `TestConnectorSPIFFEID` |
| Trust domain | `"ws-" + slug + ".zecurity.in"` (`WorkspaceTrustDomain`, `identity.go:74`). Stored in `workspaces.trust_domain TEXT UNIQUE` (nullable column, backfilled from the slug; `migrations/002_connector_schema.sql:17-22`); `workspaces.slug` is UNIQUE (`001_schema.sql:29`) | [SRC]; [TEST] `TestWorkspaceTrustDomain` |
| Connector ID | `connectors.id UUID PRIMARY KEY DEFAULT gen_random_uuid()` (`002_connector_schema.sql:85`) | [SRC] |
| Where the identity is persisted | Not stored as a string. `connectors.trust_domain` is set at enrollment (`enrollment.go:160` area UPDATE). The full SPIFFE ID is re-derived wherever needed: transport compiler `transport/compiler.go:67`, policy compiler `policy/compiler.go:121` | [SRC] |
| Enrollment binding | Token claims carry `connector_id` and `trust_domain`. The JTI is burned and must map to the same connector. The row must be `pending` and in the same tenant (`enrollment.go:68-110`). The CSR must carry exactly the expected SPIFFE URI (`enrollment.go:142-143`, `csrHasSPIFFEURI` `:218`). The controller then **re-issues** the URI itself (`pki/workspace.go:150,175` `SignConnectorCert`) | [SRC]; [TEST] `TestEnroll_CSRSPIFFEMismatch`, `TestCSRHasSPIFFEURI*`, `TestEnroll_ConnectorNotPending`, `TestEnroll_ReplayAttack` (`connector/enrollment_test.go`) |
| Admin change of the identity | No resolver updates `connectors.id` or `trust_domain`. A slug change would change future trust domains; no slug-update path was found in the resolvers searched | [SRC] for resolvers inspected; workspace rename/delete flows [NOT EST] |
| Renewal preserves identity | `RenewCert` (`enrollment.go:229`) takes `connectorID` and `trustDomain` **from the authenticated presented certificate** (`SPIFFEEntityIDFromContext`, `:232`). `RenewConnectorCert` re-derives the URI with `ConnectorSPIFFEID` (`pki/workspace.go:270`, set as the only URI at `:293`); the CSR's own URIs are ignored (only `csr.PublicKey` is used, `:229-236`) | [SRC] (enforced by code); renewal SPIFFE equality has **no** connector test [NOT EST by test] |
| Renewal key reuse | **Connector side:** CSR built from the existing key (`connector/src/renewal.rs:94-97`) [SRC]; [TEST] `renewal_reuses_the_existing_key` (`renewal.rs`). **Controller side:** does **not** require the same key; it signs whatever public key the CSR carries (`pki/workspace.go:229-236`) [SRC]. By contrast the controller enforces same-key renewal for relays (`controller/internal/relay/renew.go:176`; [TEST] `TestRenewCert_RejectsDifferentKey`, `relay/renew_test.go:173`) and for client devices (`public_key_fingerprint`, `client/renew_cert_test.go`) | [SRC]/[TEST] as stated |
| Other renewal paths | One controller RPC (`RenewCert`). On the connector, a single-flight renewal (`connector/src/tls/cert_holder.rs` `install_renewed:136`; [TEST] `renewal_is_single_flight`, `install_verifies_persists_then_notifies`) | [SRC]/[TEST] |

## 4. Identity uniqueness and reuse

| Question | Finding |
|---|---|
| Same SPIFFE ID across workspaces? | No: the trust domain differs (UNIQUE `workspaces.trust_domain`) [SRC] |
| Same SPIFFE ID within a workspace? | Only if two rows share an `id`, which the primary key prevents [SRC] |
| Re-enrollment with a different identity | Enroll requires `status='pending'` (`enrollment.go:110`); an active, disconnected or revoked connector can't re-enroll. A **new** connector gets a new random UUID, hence a new identity [SRC]; [TEST] `TestEnroll_ConnectorNotPending` |
| Reuse after deletion | `DeleteConnector` **hard-deletes** `pending`/`revoked` rows (`graph/resolvers/connector.resolvers.go:158-178`, `DELETE FROM connectors`). Afterwards no record of the ID remains, and nothing prevents the same UUID being inserted again except that `gen_random_uuid()` makes collision negligible. **Reuse prevention is probabilistic while no record exists, not enforced** [SRC/INF] |
| Comment vs code | Migration `029_connector_revocation.sql` says "Rows are never deleted — the CRL needs the serial until the certificate expires", and that the CRL makes "the connector inner mTLS reject it". **Both statements conflict with the source**: `DeleteConnector` deletes revoked rows, and the connector's inner mTLS checks only the *client's* serial (`connector/src/device_tunnel.rs:165`, `crl_manager.check(&cert_serial)` for the device) [SRC] |
| Existing certificates after deletion | Still chain-valid until `notAfter`. Their serial leaves the CRL (`pki/workspace.go:583-614` reads `connectors`). Controller RPCs fail (`SELECT … WHERE id=$1 AND trust_domain=$2` → `NotFound`, e.g. `control_stream.go:370`, `enrollment.go:242`) [SRC] |
| Workspace deletion and slug reuse | `connectors.tenant_id … ON DELETE CASCADE` (`002:86`) [SRC]. Whether a slug can be reused, and how a reused trust domain interacts with old certs (new workspace CA → chain fails) [NOT EST / INF] |

## 5. Identity-to-connector-record mapping

| Component | Presented identity | Mapping mechanism | Authoritative record | Revocation check | Evidence | Gaps |
|---|---|---|---|---|---|---|
| Controller (all connector gRPC) | leaf URI SAN, exactly 1 (`parseSPIFFEID`, `connector/spiffe.go:110`) | `(trust_domain, entity_id)` → `connectors WHERE id=$1 AND trust_domain=$2`. Trust domain must be an active workspace (`NewTrustDomainValidator`; [TEST] `TestNewTrustDomainValidator_*`) | DB `connectors` row | **identity** (`status`/`revoked_at` by connector ID), not serial. The chain is verified against the workspace CA only (`verifyConnectorCertificate`, `spiffe.go:256-282`) | [SRC] interceptors `UnarySPIFFEInterceptor` `spiffe.go:150`, `StreamSPIFFEInterceptor` `control_stream.go:913`; [TEST] `TestControl_RejectsRevokedConnector`, `TestControl_RejectsRevokedAtLegacyStatus`, `TestRenewCert_RevokedStatusDenied`, `TestRenewCert_RevokedAtLegacyDenied`, `TestRenewCert_RaceRevokeReturnsNoCert`, `TestHandleConnectorHealth_RevokedClosesStream` (`connector/revocation_test.go`) | none for the control plane; a deleted row gives NotFound, not "revoked" |
| Client, direct path | server cert URI | **prefix** `spiffe://<td>/connector/` (`client/src/tunnel_pool.rs:341`, `PrefixSpiffeVerifier:491-533`) | none at the handshake; selection comes from the transport snapshot | none (no connector CRL, no status) | [SRC]; prefix verifier has no dedicated test found | any connector of the workspace passes the handshake, so binding to the selected connector ID is not enforced at TLS |
| Client, relay path (inner TLS) | server cert URI | **exact** `connector_spiffe` from `TransportConnector.connector_spiffe` (`proto/client/v1/client.proto:266-272`; `relay_pool.rs:134` `ExactSpiffeVerifier`) | transport snapshot | none for the connector (relay CRL only for the relay hop) | [SRC]; [TEST] `exact_spiffe_verifier_rejects_wrong_spiffe` | — |
| Relay | peer URI (`relay/src/tls.rs:124-150` `authenticated_peer_identity`, role allow-list) | register/probe must match the authenticated entity ID (`relay/src/session.rs:69` entity binding; `:338` "Register connector_id does not match authenticated certificate"); registry keyed by `connector_id`. Workspace from the workspace CA's tenant URI (`tls.rs:85-110`) | none (no DB) | **serial** vs the per-workspace CRL at accept (`relay/src/listener.rs:156-173`) | [SRC]; [TEST] `register_must_match_authenticated_certificate`, `accepts_connectors_from_different_workspaces`, `listener.rs` `rejects_revoked`/`fails_closed_on_unavailable`, `relay/crl.rs` tests | no status; serial history gap |
| Shield | connector cert URI | **exact** `spiffe://<td>/connector/<state.connectors[0].connector_id>` (`shield/src/tls.rs:51-75,118-170`); persisted peer list in `state.json` | shield `state.json` (peer list pushed by its connector) | none | [SRC]; no shield TLS tests found | peer list has no revoked/disconnected distinction |
| Connector (as server, client identity) | client URI | exact equality against `ACLEntry.allowed_spiffe_ids` (`connector/src/policy/mod.rs:87-100`, `device_tunnel.rs:190-192`) | ACL snapshot | **identity** (allowed list) **plus** device serial vs CRL | [SRC]; [TEST] `policy/mod.rs` `deny_*`, `allow_known_resource_with_matching_spiffe`, `access_removal_still_revokes_pair`; `device_tunnel` `rejects_revoked` | applies to *clients*, not connectors |

Answers to Investigation B:

1. **Primary key:** the DB UUID is primary. The SPIFFE ID is derived from it [SRC].
2. **Mapping every certificate to the record:** yes for the controller, because the certificate encodes the ID. Relay and client map only to snapshot or registry entries [SRC].
3. **What revocation is checked against:**
   - controller: connector ID;
   - relay: serial;
   - client and shield: neither;
   - connector, for clients: SPIFFE ID plus serial [SRC].
4. **A non-CRL revocation check:** only the controller has one [SRC].
5. **Old and renewed certificates map to the same identity:** yes, identical URI [SRC].
6. **Deleted record:** the controller returns NotFound; relays would accept the cert once its serial leaves the CRL [SRC/INF].
7. **Two records with one SPIFFE ID:** prevented by the PK and trust-domain uniqueness [SRC].
8. **Deletion and reuse:** only through a UUID collision; not prevented by a tombstone [INF].
9. **Consistency across components:** no; see the prefix vs exact differences above [SRC].
10. **Prefix matching:** the client direct path uses a prefix [SRC].

## 6. Current CRL and certificate-level architecture

- **Workspace CRL:**
  - generated per request by `GenerateClientCRL` (`pki/workspace.go:530-660`), served at `/ca.crl`;
  - covers revoked client devices (query at `:556`) plus connectors with `revoked_at IS NOT NULL AND cert_serial IS NOT NULL` (query at `:590`);
  - `nextUpdate` = +24 h (`:626`);
  - entries are serials and dates only; there is **no identity information** [SRC];
  - [TEST] none for the connector part (only `client/device_trust_handler_test.go` exercises it for devices).
- **Relay CRL:** separate (`/relay.crl`), backed by `relay_certificates` history (migration 027) [SRC]; [TEST] `pki/relay_crl_test.go`.
- **Consumers and freshness:**

| Consumer | CRL | Refresh | On failure | Stale bound | Evidence |
|---|---|---|---|---|---|
| Connector | workspace CRL | 60 s + 0–15 s jitter (`connector/src/main.rs:178-191`) | keeps last good | 24 h `nextUpdate`, then `Unavailable` → deny | [SRC]; [TEST] `connector/src/crl.rs` (12 tests incl. `expired_cached_crl_is_not_valid_or_usable`, `keeps_last_good_crl_after_failed_validation`) |
| Connector | relay CRL | same | same | same | [SRC] |
| Relay | workspace CRL, per workspace | `spawn_refresh(60, 15)` (`relay/src/main.rs:68-69`) plus fetch-on-miss | `Unavailable` → reject | 24 h | [SRC]; [TEST] `relay/crl.rs` |
| Client | relay CRL only | periodic (`client/src/crl.rs` `spawn_refresh`) | relay routing fails closed | 24 h | [SRC]; [TEST] `relay_verifier_*` |
| Shield | none | — | — | — | [SRC] |

## 7. Revocation distribution matrix

| Component | Source of info | Data received | Identity fields | Mechanism | Cache | After revocation | Missing / unknown |
|---|---|---|---|---|---|---|---|
| **Controller** | DB | `status='revoked'`, `revoked_at`, reason (`connector.resolvers.go:113-155`) | connector ID | `RevokeConnector`; `NotifyPolicyChange` + `NotifyTopologyChange` (version bumps, no reason) | none (live DB per RPC) | Control/RenewCert/heartbeat/activation refused; live stream closed at the next health report (`control_stream.go`) | no outward revocation event, API or snapshot field for other components [SRC] |
| **Client** | ACL + transport snapshots (`proto/client/v1/client.proto` `ACLSnapshot`, `TransportSnapshot`) | the revoked connector is **absent** from `ACLRemoteNetwork`/`TransportRemoteNetwork` | `connector_id`, `connector_spiffe` of **active** connectors only | 60 s poll plus early resync | memory, no expiry | stops selecting the connector [LIVE 10-12, 10-13] | no revoked flag; absence ≡ disconnected (Q3 report); no connector CRL |
| **Relay** | workspace CRL | serial | none | poll 60 s + fetch-on-miss | memory, ≤ 24 h | rejects a **new** outer handshake presenting the listed serial [SRC]; [LIVE] BLOCKED | earlier serials not listed; no status |
| **Connector (revoked itself)** | control-plane error | `PermissionDenied "connector revoked"` | — | stream recv | — | logs and retries forever (`connector/src/control_stream.rs` reconnect loop) [SRC]; [LIVE] both runs | error code not interpreted; never checks its own serial |
| **Other connectors** | ACL snapshot `remote_networks` | revoked peer absent | peer connector IDs | push | memory | `peers_of_connector` shrinks ([TEST] `peers_of_connector_*`) | — |
| **Shield** | its connector's `PeerConnectorList` (`shield/src/control_stream.rs:281-338`) | peer list (ID and addr); an empty list is ignored | connector IDs | pushed by its own connector (from that connector's ACL, `connector/src/agent_server.rs:195-217`) | `state.json`, persistent | **nothing** when its own connector is the revoked one ([LIVE] 10-13 R2: list messages stopped, shield silent, no failover) | no revoked-vs-disconnected distinction; fails over only on connection failure (`run_once`, `control_stream.rs:60-76`) |

Removal from an active-connector snapshot is **not** an authoritative revocation event. The same absence results from disconnect, shutdown and revocation (see the Q3 four-case report).

## 8. Established-connection lifecycle

| Traffic path | Identity known to component | Existing-session ownership | Revocation observation | Termination mechanism | Evidence | Gaps |
|---|---|---|---|---|---|---|
| Client → connector (direct QUIC) | client: connector URI at the handshake (prefix-checked); connector: client SPIFFE ID and serial | client: `net_stack` flow pinned to its stream, pooled QUIC per map build; no flow → connector index ([SRC], prior investigation). Connector: `SessionRegistry` keyed by (client SPIFFE ID, resource ID) (`connector/src/session_registry.rs:52`) | client: snapshot absence affects **new** selection only. Connector: none for itself | client: per-resource key reset on ACL removal (`client/src/net_stack.rs:555` `remove_listeners_and_close`); connector: ACL-diff `CancellationToken` per session (`control_stream.rs:638-645`) | [SRC]; [TEST] `established_flow_survives_map_swap_that_removes_its_connector` (`client/src/net_stack.rs:1485`), `eviction_does_not_close_connection_and_existing_flow_survives`, `policy/mod.rs` revoke tests; [LIVE] 10-12 HOLD-D survived 19 min 28 s incl. cert expiry | no connector-revocation-triggered close on either side |
| Client → relay → connector | relay: peer identity at accept; client: exact connector SPIFFE ID on inner TLS | relay registry by authenticated `connector_id` (`relay/src/session.rs:69-104`) | relay: CRL at **accept only** (`listener.rs:156-173`); client and connector monitor **relay** revocation (`relay_pool.rs:253`, `connector/src/relay_client.rs:466`) | removal from the relay registry when the connector session ends | [SRC]; [LIVE] **BLOCKED** 10-12 and 10-13 | no recheck of connector serial or status on established relay sessions [SRC]; runtime [NOT EST] |
| Connector → shield → app | shield: exact connector ID at its mTLS; connector: shield ID | shield `TunnelHub` / connector agent server | none | connection failure only | [SRC]; [LIVE] 10-13 R2: protected session survived revocation and the revoked cert's expiry (702 × 200) | — |
| Control plane | controller: identity per RPC | controller stream registry | live DB status | stream closed at the next health report; reconnects refused | [SRC]; [TEST] `TestHandleConnectorHealth_RevokedClosesStream`; [LIVE] both runs (≈12.6 s / 3 s after revoke) | closing it does not close the data plane [SRC + LIVE] |

**Sequence today:**
1. Admin revoke → DB row `revoked` → policy and topology versions bump.
2. Control plane:
   - the connector's control stream is closed at its next heartbeat;
   - reconnects get PermissionDenied, indefinitely;
   - the connector keeps listeners, ACL cache and sessions;
   - the shield's status stops reaching the controller → shield marked `disconnected` by the watcher.
3. Data plane:
   - the client stops selecting the connector at its next sync;
   - before then, new sessions are accepted ([LIVE] 10-13 R3-V: 115 accepted, 114 after control rejection; whether these were fresh handshakes is **not established**);
   - existing sessions continue until the transport or the app ends them ([LIVE]).
   - The relay would refuse a new outer handshake once its CRL lists the current serial [SRC, not live-verified].

## 9. Disconnected components and stale state

| Component | Cache source | Persisted? | Refresh | On refresh failure | Freshness bound | Existing sessions | Evidence gaps |
|---|---|---|---|---|---|---|---|
| Client | ACL / transport snapshots; relay CRL | snapshots in memory (plus encrypted local state for login); CRL in memory | 60 s poll / early resync; CRL periodic | keeps cached snapshots (`client/src/daemon.rs:4949` "keeping cached snapshot"; `:4633` "fail-open on staleness"); relay CRL keeps last good | snapshots: none; relay CRL: 24 h | untouched | an isolated client keeps selecting a revoked connector indefinitely [INF from SRC] |
| Relay | workspace CRL per workspace | memory | 60 s + fetch-on-miss | keeps last good; `Unavailable` after `nextUpdate` → reject new | 24 h | not rechecked | — |
| Connector | ACL (`PolicyCache`), workspace CRL, relay CRL | memory | ACL push over the control stream; CRL 60 s | ACL: retained, no expiry; CRL: last good until 24 h | ACL: none; CRL: 24 h (affects device checks only) | untouched | it serves without a controller indefinitely as long as the client CRL is fresh [SRC]; [LIVE] it served 19+ min after revocation |
| Shield | peer list `state.json` | **persisted** | pushed by its connector | ignores an empty list | none | untouched | — |

**Situations:**
- **(A) Authoritative revocation received.** Only the controller ever "receives" one. No other component has a representation for it [SRC].
- **(B) Not yet received, but able to communicate.**
  - The client converges at the next sync, but only by absence.
  - The relay converges at its next CRL refresh, current serial only.
  - The connector and the shield never converge [SRC].
- **(C) Isolated.** All components keep using cached state.
  - The only bound is the 24 h CRL `nextUpdate`, and only for serial checks.
  - Certificate `notAfter` bounds only **new** handshakes, not established sessions ([LIVE] sessions outlived it).
  - The acceptable bound is a **[DECISION]**.

## 10. Certificate renewal and history

- Renewal overwrites `connectors.cert_serial` (`enrollment.go:265-276`); no per-certificate history exists for connectors [SRC].
- Renewed certificates are backdated by 1 h (`pki/workspace.go:277`, "clock skew tolerance").
- Relays have per-certificate history: `relay_certificates` (migration 027), with supersede-on-renewal and revoke-all (`relay/store.go:248-262,284,362`) [SRC].
- **Gap:** [LIVE] 10-13 R4: two prior, still-valid serials were absent from the CRL after revoke.

## 11. Test coverage

| Property | Tests | Proves | Doesn't prove |
|---|---|---|---|
| SPIFFE format | `TestConnectorSPIFFEID`, `TestWorkspaceTrustDomain` | string construction | renewal preservation |
| SPIFFE stability across renewal | **none** for connectors | — | gap |
| Same-key renewal | connector `renewal_reuses_the_existing_key` | the connector sends the existing key | that the controller rejects a different key (it doesn't; relay/device tests cover their own paths) |
| Enrollment identity binding | `TestEnroll_CSRSPIFFEMismatch`, `TestCSRHasSPIFFEURI*`, `TestEnroll_ReplayAttack`, `TestEnroll_ConnectorNotPending` | CSR identity = token identity; no re-enroll unless pending | uniqueness after deletion |
| Identity → record, revoked rejection (controller) | `TestControl_RejectsRevoked*`, `TestRenewCert_*Revoked*`, `TestRenewCert_RaceRevokeReturnsNoCert`, `TestHandleConnectorHealth_RevokedClosesStream`, `TestHandleConnectorHealth_RevokedNeverGetsReEnroll` | identity-level control-plane rejection | rejection of *older* certificates explicitly (implied by ID-based checks) |
| Parsing / identity | `TestParseSPIFFEID_*`, `TestNewTrustDomainValidator_*` | URI parsing, trust-domain gating | connector chain verification (`verifyConnectorCertificate`) has no direct test found |
| Relay identity binding | `register_must_match_authenticated_certificate`, `client_cannot_register_as_connector`, `accepts_connectors_from_different_workspaces` | entity binding at registration | revoked connector on established sessions |
| CRL behaviour | connector `crl.rs` (12), relay `crl.rs`, `listener.rs` | parsing, validity, fail-closed on new peers | connector-serial generation in the workspace CRL; outage behaviour end-to-end |
| PermissionDenied "connector revoked" handling by the connector | **none** | — | gap |
| Prevention of the connector serving after revocation | **none** | — | gap |
| Direct / relay / shield session termination on connector revocation | **none** | — | gap |
| Established-flow behaviour on map change | `established_flow_survives_map_swap_that_removes_its_connector`, `eviction_does_not_close_connection_and_existing_flow_survives` | flows survive by design | — |
| Normal renewal without interruption | connector `renewal_opens_new_stream_first_then_switches_and_drops_old` and related (`control_stream.rs`); controller `TestControlLifecycle_*` | make-before-break | — |
| Client identity revocation via ACL | `policy/mod.rs` `access_removal_still_revokes_pair`, `tuple_change_revokes_all_previously_allowed_spiffes` | identity-level session cancellation for **clients** | — |

## 12. Comparison of revocation models (no selection)

| Aspect | Model 1: certificate-level | Model 2: identity-level | Model 3: combined |
|---|---|---|---|
| Already available | workspace CRL; relay and connector consume it; relay-style history schema exists as a pattern (027) | the controller checks identity; transport snapshots carry exact `connector_spiffe`; the shield and relay-path client check exact IDs; identity-based session cancellation exists for **clients** (allowed SPIFFE IDs + ACL diff) | both of the above; the controller already separates ID-based control-plane checks from serial-based CRL generation |
| Required new capabilities | per-certificate history for connectors (all unexpired serials); CRL consumption by the client (connector certs) and the shield; established-session recheck | an authoritative revoked-connector signal that components can consume (none exists outside the controller); enforcement in the client (direct path is prefix-only), relay (no status input), shield, and the revoked connector itself | the union, plus defined precedence and failure semantics |
| Components that can't enforce today | client direct path (no connector CRL), shield (no CRL), connector itself (doesn't check its own serial) | relay (no status input), shield (peer list lacks state), connector itself (ignores the error code), client (absence ≠ revocation) | same |
| Established connections | needs serial recheck plus termination hooks (relay and client have relay-revocation monitors as a precedent: `relay_pool.rs:253`, `relay_client.rs:466`) | needs identity → session ownership (client flows have no connector ID; the connector registry is keyed by client identity) | both |
| Disconnected components | bounded by CRL `nextUpdate` (24 h) for new handshakes | no existing freshness bound | [DECISION] |
| Reuse / deletion risk | history must outlive the certificates; `DeleteConnector` currently drops CRL coverage | depends on IDs never being reused; currently only probabilistic (UUID), no tombstone after hard delete | both |
| Compatibility risk | larger CRLs; renewal churn creates more serials | the prefix verifier must move to exact or status-aware checks; protocol additions | complexity; consistent semantics across components |
| Tests | CRL parsing exists; no history or enforcement tests | controller identity tests exist; none elsewhere | — |

## 13. Findings confirmed by source
- The SPIFFE ID is derived from `(workspace trust_domain, connectors.id UUID)`. Renewal re-derives it from the presented certificate's identity.
- The controller does not enforce same-key connector renewal; the connector reuses its key by choice.
- The controller's connector authorization is identity / DB-status based; serial is not used.
- The workspace CRL carries one serial per revoked connector; there is no history.
- `DeleteConnector` hard-deletes revoked rows, contrary to the migration 029 comment.
- The client direct path verifies connectors by **prefix**; the relay-path client and the shield by **exact** ID; the relay by role + chain + CRL serial.
- No non-controller component receives authoritative connector-revocation state.
- No established session is rechecked against connector revocation anywhere.
- Identity-level revocation with session teardown exists for client devices only.

## 14. Findings confirmed by automated tests
Listed in §11: controller identity-level control-plane rejection; enrollment identity binding; SPIFFE string format; connector key reuse; CRL validation and fail-closed on new peers; relay registration identity binding; client-identity ACL session revocation; established flows surviving map swaps.

## 15. Historical live observations (not re-run)
- **10-12** (controller binary `controller-f5c`, built before `4781bc8`):
  - existing direct and shield flows survived revocation, client sync and certificate expiry;
  - new flows were accepted until the client sync;
  - the control stream was closed and reconnects were refused;
  - the relay path was BLOCKED.
- **10-13** (HEAD build `9844a00`):
  - **R2:** no shield failover; the protected session survived the revoked certificate's expiry; B refused new connections with `SHIELD_NOT_ATTACHED`.
  - **R3-V:** 115 new sessions were accepted before the client sync (114 after control rejection). Whether they were fresh handshakes was not established.
  - **R4:** serial-history gap confirmed.
  - **R1:** relay BLOCKED.
- **Finding 34** (09-28, pre-2-A): a revoked connector kept serving existing traffic.

## 16. Inferences needing verification
- A relay would accept an earlier (renewed-away) connector certificate after revocation [INF; R1 blocked].
- A client would complete a direct handshake with *any* connector of its workspace if pointed at it (prefix verifier) [INF from SRC].
- An isolated client keeps selecting a revoked connector indefinitely [INF].
- R3-V sessions likely rode the pre-revocation QUIC connection [INF].
- Behaviour of reused trust domains or slugs after workspace deletion [NOT EST].
- Whether any path updates `connectors.trust_domain` or `workspaces.slug` [NOT EST beyond the resolvers inspected].

## 17. Open questions requiring human decisions
1. The unit of administrative connector revocation: certificate serial, connector identity, or both. **[DECISION]**
2. Which components must enforce it (client direct path, relay, shield, the connector itself), and whether the connector should self-terminate on control-plane `PermissionDenied`. **[DECISION]**
3. Whether established sessions must be terminated, and within what bound. **[DECISION]**
4. Acceptable staleness for isolated components (today: unbounded for snapshots, 24 h for CRL serial checks). **[DECISION]**
5. Whether connector IDs must be permanently non-reusable (tombstones vs hard delete), and whether `DeleteConnector` may remove revocation coverage. **[DECISION]**
6. Whether connector renewal should be bound to the existing key at the controller, as relays and devices are. **[DECISION]**
7. Whether the client's direct-path prefix verification should bind to the selected connector identity. **[DECISION]**

## 18. Authorization statement
This was a read-only investigation. **No implementation, architecture choice, protocol change, schema change or test change was authorized or made.** The only file created is this report.
