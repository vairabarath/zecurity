# D21-R: a hostile connector at the shield (design)

**Date:** 2026-10-30 · **Owner:** Sathiya · **Status:** implemented 2026-10-30, unit level (§8); live check pending. Design approved 2026-10-30 (§6: Workspace CA key; §3: per-shield snapshot delivery) · **Source:** PR #135 (Barath's D21 review note) and the reply on it

D21 is reopened narrowly. Three parts, one branch (`fix/d21r-shield-hostile-connector` off `fixed-pendings`), separate commits:

| Part | What | Components |
|---|---|---|
| (a1) | Snapshot generation scoped to the active connector | shield |
| (a2) | Controller-signed peer list is authoritative | proto, controller, connector, shield |
| A′ | Workspace CRL forwarded on the shield stream | proto, connector, shield |
| D | Shield tunnel allowlist | shield |

## 1. Threats (verified 2026-10-30, `82238690`)

A connector that was revoked but keeps running with a valid certificate (up to `CONNECTOR_CERT_TTL`, 7 days) can:

1. open tunnels from the shield to any `destination:port` (`shield/src/tunnel.rs`);
2. empty `resource_protect` with an empty snapshot;
3. send `generation = u64::MAX`: the shield then ignores every honest snapshot until it restarts (`resources.rs` guard; honest generations go up by 1);
4. rewrite the shield's persisted peer list (`apply_peer_connector_list` saves whatever arrives) or name honest peers revoked: once its certificate expires, the shield has no connector to fail over to.

The shield checks the connector's chain and exact SPIFFE ID, never revocation.

## 2. (a1) Generation scoped to the connector

- The shield records the connector id (the one `build_client` verified against its SPIFFE ID) with the last applied generation.
- A snapshot from a different connector than the recorded one is accepted whatever its generation and becomes the new baseline.
- Same connector (reconnect, renewal handover): `generation <= last` is still rejected.
- So `u64::MAX` blocks only while that connector stays attached; the next connector's snapshot replaces it.

## 3. (a2) Signed peer list

**Payload** (`SignedPeerList`, protobuf bytes): `trust_domain`, `shield_id`, `issued_at_unix_ms`, `peers[] {connector_id, connector_addr}`.

**Signature:** ECDSA P-384 / SHA-384 over `"zecurity/shield-peer-list/v1\0" || payload`. Key: see §6.

**Delivery:** a new field on `ResourceSnapshot` (the per-shield message the controller already sends to the shield's connectors through `ResourceSnapshotBatch`, which the connector caches and replays when the shield connects). It's connector-only and per-shield. The ACL snapshot also reaches clients, so it wasn't chosen.
- Built in `buildSnapshotMsg` with every snapshot.
- Also pushed for every shield in a remote network when one of its connectors is enrolled, revoked, deleted or changes address, so Phase E pruning doesn't depend on a resource change.

**Shield rules:**
- Verify the signature, the `trust_domain`, and that `shield_id` is its own.
- Accept only `issued_at` greater than the highest one it has seen. That value is persisted in `state.json`, so a restart can't roll it back.
- The accepted list replaces `connectors` (current head kept when still listed).
- An empty list is accepted but not saved (state.json needs a connector to start from); the shield dials nothing.
- The signed list is processed before, and independently of, the generation guard: identical resources keep the same generation.
- The unsigned `PeerConnectorList` (`peers`, `revoked_connector_ids`) is ignored completely; the connector stops sending it. Rewritten tests: Phase E prune, Fix 13 F41.
- Revoked connectors simply aren't in the signed list; A′ refuses the connector the shield is attached to.

## 4. A′: workspace CRL on the shield stream

- **Connector:** forwards the raw DER of the workspace CRL (the one that lists every revoked connector serial, bug 9) on the shield stream, right after the shield connects and after each refresh.
- **Shield:**
  - Verifies the CRL against the Workspace CA (`crl_issuers`, a copy of the connector's, with the same test vectors).
  - Keeps the newest one: the later `thisUpdate` (the controller's CRL number is random, not ordered). It's persisted in the state dir.
  - Records the serial of the connector certificate it verified.
  - Leaves that connector through the terminal-failover path when:
    - the serial is listed;
    - the newest CRL is past `nextUpdate`;
    - no valid CRL arrives within 30 s of opening a stream.
- **Bound:** a hostile connector can withhold or replay a CRL only until that CRL's `nextUpdate` (24 h). An honest connector's revocation takes effect at its next CRL refresh.

## 5. D: tunnel allowlist

A `TunnelOpen` is served only when `destination` parses as an IP and `(ip, protocol, port)` falls within an entry of the active set; otherwise `TunnelOpened{ok:false}`. TCP and UDP.

**Residual risk:** snapshots stay unsigned. A hostile connector can still empty `resource_protect` while attached; A′ bounds that to the CRL's `nextUpdate`. D limits the pivot to the shield's own protected ports.

## 6. Decision: the signing key for (a2): Option 1 (Sathiya, 2026-10-30)

- **Option 1 (recommended): the Workspace CA key**, domain-separated by the context prefix.
  - No schema change; shields already hold the Workspace CA certificate.
  - The CA key signs a non-X.509 object for the first time. The prefix can't parse as a `TBSCertificate`/`TBSCertList` (both start with `0x30`).
  - The key is loaded and decrypted per signing, as `GenerateClientCRL` does.
- **Option 2: a dedicated per-workspace signing key.**
  - New table (`044_`, `[schema: reset DB]`).
  - The public key goes to the shield at enrolment and renewal; existing shields re-enrol.
  - Cleaner key separation, more moving parts.

## 7. Gates

- `buf generate`; controller `go build` and tests, DB-backed tests run, none skipped.
- connector, shield, client tests. Each new test fails without its fix.
- Live check on the lab only with Sathiya's go-ahead.

## 8. Implementation (2026-10-30)

Branch `fix/d21r-shield-hostile-connector` (off `fixed-pendings` `710ed362`), not pushed.

| Part | Commit | What changed |
|---|---|---|
| (a1) | `25415b26` | `SharedResourceState::admit_snapshot(connector_id, generation)`; `handle_connector_msg` passes the stream's connector id |
| (a2) | `b837a10f` | proto `SignedPeerList` / `PeerListPayload`; `pki.SignShieldPeerList`; `connector/peer_list.go` (issuer serialised, `PushPeerListsForNetworkOf`); triggers: connector revoke, connector health change, shield moved; shield `peer_list.rs`, `crl.rs` (copy of `crl_issuers`), `apply_signed_peer_list`, `peer_list_issued_at_ms` in state.json; unsigned `PeerConnectorList` ignored |
| (a2) | `d21ac815` | an empty signed list still saves its `issued_at` (found by the automated security review) |
| (a2) | `025ebae4` | peer-list pushes run in the background, off the Connector's `AuthorizationAck` path (D18); the issuer mutex covers only `issued_at` and the read, and signing happens after it |
| (a2) | next commit | background pushes coalesced (`pushCoalescer`): one worker per key, latest trigger wins, panic-safe, at most 4 at once (automated security review: a reconnect storm started one goroutine per trigger) |
| A′ | `19fd600b` | proto `WorkspaceCrl` (oneof 15); connector `CrlManager::current_der`, forwarded on stream open and with health reports when newer; shield `CrlStore` (verify, newest by `thisUpdate`, `workspace.crl`), serial captured in `tls.rs`, `crl_verdict`, `LeftConnector` → terminal failover |
| D | `c40f02d5` | `SharedResourceState::allows_tunnel`; refused `TunnelOpen` → `TunnelOpened{ok:false}` |
| A′ | `ef95a903` | review fixes: the CRL wait is counted across streams; quarantine past it (instructions and snapshots ignored, tunnels refused); a clean close past it is terminal; listed Connector refused before anything is sent; serial taken from the completed TLS session; the Connector sends the CRL before the snapshot replay |

Changes to the design while building:
- **§4 "no valid CRL within 30 s of opening a stream" is now "within 30 s since the Shield first had no valid CRL",** counted across streams. While the Shield holds no valid CRL past that wait, it acts on nothing from the Connector (quarantine). Per stream, a withholding Connector could reconnect to restart the wait (automated security review).
- **CRL ordering:** the newest CRL is the one with the later `thisUpdate`. The controller's CRL number is random (`newSerialNumber`), not ordered.
- **Peer-list contents:** the Connectors of the shield's Remote Network with status `active` or `disconnected`, `revoked_at IS NULL`, and a `lan_addr`. A temporarily disconnected Connector stays a peer.
- **Connector unchanged for (a2):** it forwards the snapshot as it is. It still sends `PeerConnectorList`, which the shield ignores; removing that is cleanup, not security.

Residual risks (accepted):
- **Snapshots are unsigned.** While attached, a hostile Connector can still change the active resource set. With D its pivot is limited to the shield's protected ports. A′ ends it within the `nextUpdate` of the last CRL the shield saw (≤ 24 h).
- **(a1) cross-Connector reset.** A snapshot from a different Connector is admitted whatever its generation (Sathiya's choice), so an honest peer's slightly stale cached snapshot can apply briefly. The controller now pushes a snapshot as soon as a shield moves to a Connector, which corrects it.
- **Fail closed on a long controller outage.** When no Connector has a CRL valid now (outage longer than the 24 h CRL validity), shields leave their Connectors, the same as Connectors deny clients.
- **Serial lookup.** The Connector's certificate serial comes from the Shield's last completed handshake with that Connector id (`tls::connector_serial`). Every unexpired certificate of a revoked Connector is in the CRL (bug 9).
- **Not unit-tested:** the clean-close path inside `run_once`. Left to the live check.
- **Phase E behaviour change.** An honest Connector no longer prunes revoked peers from the shield at once. Pruning now comes from the controller: its push on a revoke, and the snapshot it pushes when a shield moves. A′ also refuses a revoked Connector the shield is attached to.
- **Long controller outage.** Protection changes made while a shield is quarantined reach it only by the drift resync. The snapshot was ignored, and the Connector replays it only when the shield connects.

Live-check plan (Sathiya's go-ahead first; confirm the running controller's sha):
- **Deploy order:** controller, then Connectors, then Shields. A new Shield on an old Connector gets no CRL and leaves every Connector after 30 s.
- **Revoke:** revoke a Connector under traffic; the shield fails over.
- **Hostile Connector:** the revocation-ignoring Connector from the Phase E live run. The shield leaves it within 5 s of a CRL that lists it.
- **Clean close:** without a valid CRL past the wait, a Connector that closes the stream is left.
- **D under real traffic:** a port-range resource and an `any` resource; honest traffic isn't refused.

Tests:
- **Controller:** `TestBuildSignedPeerList`, `TestPushPeerListsForNetworkOf`, `TestPeerListSigningIsOutsideTheLock`, `TestShieldFacingAddr`, all DB-backed.
- **Shield:** generation scoping; signed lists (forged, altered, other shield or workspace, rollback across a restart, unsigned ignored, empty list, publish); a Go-signed vector verifies in Rust; the CA chosen by role; the CRL store; the CRL verdict; a CRL that lists the Connector ends the stream; leaving is terminal; the tunnel allowlist, also end to end.
- **Connector:** the CRL is forwarded once per CRL per stream.
- Every new test fails with its fix removed (mutant-checked).

Gates: controller 1535 / 0 / 2 (DB-backed), connector 270, shield 73; client and relay build.
