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
| S | `c345425b` | signed resource sets (Sathiya, 2026-10-30: "snapshot-only", "shield-wide"): proto `SignedResourceSet` / `ResourceSetPayload` (desired set + pending removes); `pki.SignShieldResourceSet` (context `zecurity/shield-resource-set/v1\0`); the shield applies only verified sets, acks removes "unprotected", ignores `ResourceInstruction` (handlers removed) and the unsigned snapshot fields; shield-wide order with `resource_generation_floor` in state.json. Supersedes a1 |
| A′ | `ef95a903` | review fixes: the CRL wait is counted across streams; quarantine past it (instructions and snapshots ignored, tunnels refused); a clean close past it is terminal; listed Connector refused before anything is sent; serial taken from the completed TLS session; the Connector sends the CRL before the snapshot replay |

Changes to the design while building:
- **§4 "no valid CRL within 30 s of opening a stream" is now "within 30 s since the Shield first had no valid CRL",** counted across streams. While the Shield holds no valid CRL past that wait, it acts on nothing from the Connector (quarantine). Per stream, a withholding Connector could reconnect to restart the wait (automated security review).
- **CRL ordering:** the newest CRL is the one with the later `thisUpdate`. The controller's CRL number is random (`newSerialNumber`), not ordered.
- **Peer-list contents:** the Connectors of the shield's Remote Network with status `active` or `disconnected`, `revoked_at IS NULL`, and a `lan_addr`. A temporarily disconnected Connector stays a peer.
- **Connector unchanged for (a2):** it forwards the snapshot as it is. It still sends `PeerConnectorList`, which the shield ignores; removing that is cleanup, not security.

Residual risks (accepted):
- ~~**Snapshots are unsigned.**~~ **Closed by S (`c345425b`):** the shield applies resources only from a controller-signed set and ignores instructions. While attached, a hostile Connector can now only withhold or replay sets (an older one is refused).
- ~~**(a1) cross-Connector reset.**~~ **Closed by S:** ordering is shield-wide again on the signed generation, saved across restarts.
- **Fail closed on a long controller outage.** When no Connector has a CRL valid now (outage longer than the 24 h CRL validity), shields leave their Connectors, the same as Connectors deny clients.
- **Serial lookup.** The Connector's certificate serial comes from the Shield's last completed handshake with that Connector id (`tls::connector_serial`). Every unexpired certificate of a revoked Connector is in the CRL (bug 9).
- **Clean close:** the decision is `stream_closed_cleanly`, unit-tested (`a_clean_close_without_a_valid_crl_leaves_the_connector`, 2026-10-30). Its one-line call in `run_once` is checked by reading; not live-tested (needs a connector that keeps serving after revocation).
- **Phase E behaviour change.** An honest Connector no longer prunes revoked peers from the shield at once. Pruning now comes from the controller: its push on a revoke, and the snapshot it pushes when a shield moves. A′ also refuses a revoked Connector the shield is attached to.
- **Long controller outage.** Protection changes made while a shield is quarantined reach it only by the drift resync. The snapshot was ignored, and the Connector replays it only when the shield connects.

Live-check plan (Sathiya's go-ahead first; confirm the running controller's sha):
- **Deploy order:** controller, then Connectors, then Shields. A new Shield on an old Connector gets no CRL and leaves every Connector after 30 s.
- **Revoke:** revoke a Connector under traffic; the shield fails over.
- **Hostile Connector:** the revocation-ignoring Connector from the Phase E live run. The shield leaves it within 5 s of a CRL that lists it.
- **Clean close:** without a valid CRL past the wait, a Connector that closes the stream is left.
- **D under real traffic:** a port-range resource and an `any` resource; honest traffic isn't refused.
- **S:** protect, unprotect and delete a resource; each completes (protected, unprotected, reaped) through signed sets alone, with no instructions applied at the shield.

Tests:
- **Controller:** `TestBuildSignedPeerList`, `TestPushPeerListsForNetworkOf`, `TestPeerListSigningIsOutsideTheLock`, `TestShieldFacingAddr`, all DB-backed.
- **Shield:** generation scoping; signed lists (forged, altered, other shield or workspace, rollback across a restart, unsigned ignored, empty list, publish); a Go-signed vector verifies in Rust; the CA chosen by role; the CRL store; the CRL verdict; a CRL that lists the Connector ends the stream; leaving is terminal; the tunnel allowlist, also end to end.
- **Connector:** the CRL is forwarded once per CRL per stream.
- Every new test fails with its fix removed (mutant-checked).

Gates: controller 1536 / 0 / 2 (DB-backed), connector 270, shield 79; client and relay build.

## 9. Live check (2026-10-30, Sathiya's lab, manoj's host not used)

Builds from `d6818a13`: controller `3ada62421e1395e2`, connector `823c5d3513d7e445`, shield `ac8209eb4ea20d98`. Each sha256 was checked on its host after install. The controller ran with the default connector cert TTL (not the 15 min of journey 7), so a revoked connector's certificate stayed in the CRL during the checks.

**Setup.** The journey-7 connector certificates (15 min TTL) had expired while the lab was idle, so:
- **Connectors:** two new ones in `e2e-net`: `e2e-conn-inkyank-d21r` on inkyank `.75` (the old install replaced, the relay's config kept) and `e2e-conn-seechan-d21r` on seechan5 `.42`. On seechan5, `/etc/zecurity/ca.crt` belonged to the revoked, stopped relay `e2e-relay-seechan2`; it was backed up to `ca.crt.bak-relay`.
- **Shield:** the old shield's saved peers were manoj's connector (offline) and the replaced inkyank connector, so it could reach none and could never receive a signed list. That's the case where a re-enrolment is needed. The new shield `e2e-shield-nika-d21r` was enrolled on nika `.33`.
- **Client:** logged in again (local test login).

| Case | Observed |
|---|---|
| Baseline | Shield stream to seechan5; `workspace CRL installed revoked=0`; `peer connector list refreshed (signed) peers=4` (the two new connectors plus the disconnected old inkyank and manoj records); signed set applied at generation 1 |
| S: protect | `e2e-d21r-web` (tcp 8815–8816 on `.33`) protected in < 1 s through the signed set (generation 2); direct from inkyank `200` → `000`; client tunnel `200` (13 ms) |
| S: unprotect | completed in < 1 s (`unprotected`, from the removes ack); direct from inkyank `200` again |
| S: edit | range changed to 8816–8817: the shield applied it (generation 5); direct `8815` → `200`, `8816` → `000`. Status `failed: port not listening`: the shield's health check, since nothing listens on 8816 (expected) |
| S: delete | tombstone reaped in ≤ 1 s, the same second the shield applied the set with `resource_count=0` (reaped by the shield's ack: inferred from the code path and timing, not logged) |
| Shield restart | before: `state.json` `resource_generation_floor: 7`, `peer_list_issued_at_ms` set, `workspace.crl` saved; after: stream at 11:36:31.200, set applied 39 ms later (`resource_count=1`); direct `000`, tunnel `200` |
| Revoke under traffic | revoke 11:37:09.018 → shield refused by seechan5 at 09.036 → stream on inkyank 11:37:11.37; signed list `peers=3` at 11:37:12.997; probe 88/90 `200`, 2 failures (≈ 2.5 s); CRL with `revoked=1` forwarded at 11:37:41 (32 s after the revoke, the connector's CRL refresh) |
| Controller restart | connector stream 11:39:11.757 → first `AuthorizationAck accepted` 11:39:11.772 (15 ms; the peer-list push for that change runs in the background); the shield stayed on inkyank, got a new CRL; tunnel `200`, resource `protected` |

**Not run:**
- **Refused at open (hostile connector):** the shield refusing a connector its stored CRL lists, at stream open.
- **The clean-close path.**

Both need a connector that keeps serving after revocation (the Phase A build used in the Phase E live run). Every honest revoked connector closes its shield streams itself. The unit tests cover both.

**Lab left running:** the branch controller (user unit `e2e-controller`), file server `e2e-serve` (`:8081`), connector `e2e-conn-inkyank-d21r` (active), connector `e2e-conn-seechan-d21r` (revoked; process running, not serving), shield `e2e-shield-nika-d21r`, the client (tunnel up), a test web server on nika `:8815` (user process), and resource `e2e-d21r-web2` (protected). manoj's host was not touched.
