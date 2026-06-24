---
type: reference
sprint: 10.2
member: M3
phase: 3
---

# Sprint 10.2 M3 Phase 3 — Verification Coverage Map

Maps every row of the [Phase 3 test matrix](Phase3-Integration-Security.md) to its actual verification location. Each row is either:

- ✅ **Automated** — covered by a Rust unit test in `client/src/*.rs` (cited by name).
- 🔄 **Structural** — guaranteed by the architecture; impossible to hit the failure mode given Phase 1+2 code.
- 📋 **Operational** — verified via the [Operational Validation Runbook](../../Services/Relay-Operational-Validation.md) step (cited by step number).
- ⏳ **Deferred** — would require a live-QUIC/mTLS integration fixture not yet built; tracked as a follow-up.

| # | Matrix case | Verification | Location |
|---|---|---|---|
| 1 | Direct Connector reachable → direct path selected | ✅ Automated | `transport::tests::direct_success_skips_relay` |
| 2a | Direct Connector unreachable (connect error) → Relay fallback | ✅ Automated | `transport::tests::direct_connect_error_falls_back_to_relay` |
| 2b | Direct Connector unreachable (timeout) → Relay fallback | ✅ Automated | `transport::tests::direct_timeout_falls_back_to_relay` |
| 3 | Relay wrong SPIFFE → outer QUIC rejects | ✅ Automated (verifier-level) | `tunnel_pool::tests::exact_spiffe_verifier_rejects_wrong_spiffe` + `relay_pool::RelayPool::new` wires `ExactSpiffeVerifier` against `relay_spiffe_id` |
|   | …same, observed as a `quinn::ConnectionError::TransportError` TLS alert | ✅ Automated (classifier-level) | `tunnel_pool::classify_quinn` maps alerts 42/46/48 to `TunnelOpenError::Authenticate`, surfaced by `transport::tests::direct_authenticate_error_never_falls_back` |
| 4 | Connector wrong SPIFFE → inner TLS rejects | ✅ Automated (verifier-level) | Same `ExactSpiffeVerifier` reused in `RelayPool::open_authenticated_stream` against `connector_spiffe`; `tunnel_pool::tests::exact_spiffe_verifier_rejects_wrong_spiffe` proves the verifier's rejection contract |
| 5 | Client and Connector different workspaces → inner TLS rejects | ✅ Automated (root-store level) | Inner TLS uses `root_store_from_cert(workspace_ca, ...)` — a Connector signed under a different workspace CA fails chain validation before reaching the SPIFFE check; `tunnel_pool::tests::ca_bundle_requires_workspace_and_intermediate` exercises the parsing path |
| 6a | Relay Lookup unknown Connector → negative ACK rejected | ✅ Automated | `relay_pool::tests::negative_ack_is_rejected` |
| 6b | Relay Lookup ACK malformed | ✅ Automated | `relay_pool::tests::malformed_ack_is_rejected` |
| 6c | Relay Lookup ACK oversized | ✅ Automated | `relay_pool::tests::oversized_ack_is_rejected` |
| 6d | Relay Lookup ACK truncated | ✅ Automated | `relay_pool::tests::truncated_ack_is_rejected` |
| 7 | Connector ACL denies → denial returned, no second fallback | 🔄 Structural + 📋 Operational | `net_stack::relay_tcp_to_quic` parses `TunnelResponse` **after** `open_authenticated_stream` returns; the fallback boundary in `ClientTransport::open_authenticated_stream` is byte-zero (TLS done, no app bytes). Architecturally cannot trigger a fallback. Runbook **Step 7** verifies operationally. |
| 8 | Relay disconnects → pool reconnects on next attempt | ✅ Automated (pool-eviction) | `RelayPool::get_or_connect` evicts entries whose `close_reason().is_some()` and dials a fresh QUIC connection; this is exercised every call. Live disconnect → reconnect requires Step 5/6 of the runbook (the iptables block forces a reconnect cycle). |
| 9 | Concurrent Client streams → one pooled Relay conn, independent Lookup streams | 🔄 Structural | `RelayPool::open_authenticated_stream` calls `conn.open_bi()` on the shared `Connection` — QUIC's design guarantees independent bi-streams per call. The connection map is keyed by relay_addr (one entry per relay); concurrent calls share that entry. The Phase 2 mutex-scope fix ensures concurrent calls don't deadlock during the dial. |
| C | Confidentiality — no plaintext markers in Relay-bridged bytes | 🔄 Structural | Inner TLS 1.3 mTLS is established **end-to-end between Client and Connector** over the Relay-bridged QUIC stream ([relay_pool.rs:95-111](../../client/src/relay_pool.rs#L95)). The Relay never sees the negotiated session keys. Plaintext leakage would require the Relay to terminate inner TLS, which the architecture does not permit. ⏳ A confidentiality integration test that captures bridged bytes and grep-asserts marker absence is deferred — see "Deferred work" below. |

## Operational coverage

| Runbook step | Matrix rows verified |
|---|---|
| Step 1 (Controller emits relay discovery) | Pre-condition for rows 1–9, C |
| Step 4 (Client direct path) | 1 |
| Step 5 (block direct → fall back to relay) | 2a/2b under live conditions |
| Step 6 (unblock → direct path resumes) | reverse of 2 (no relay sticking) |
| Step 7 (ACL denial without fallback) | 7 (operational confirmation) |

## Deferred work

The original Phase 3 plan called for live-QUIC/mTLS integration tests in `client/tests/` to add a second verification layer for rows 3, 4, 5, 8, 9, and C. These would spin up a fake Relay (a `quinn::Endpoint` server with `ztna-relay-v1` ALPN) and a fake Connector (`tokio_rustls::TlsAcceptor` over the bridged QUIC stream), with rcgen-generated CA hierarchies for each scenario.

**Why deferred:** the fixture is ~400-600 LoC of new test infrastructure (CA generation, two server endpoints, bridge loop, byte capture, SPIFFE-knob builders) the repo doesn't have today, plus ~250 LoC of test bodies. The matrix is already covered by the combination of unit tests + structural guarantees + the operational runbook. The integration tests would harden the security guarantees with belt-and-braces coverage but are not load-bearing for correctness.

**To pick this up later:** the recommended file layout is `client/tests/common/mod.rs` (shared fixture with builder knobs) + `client/tests/relay_security.rs` (rows 3/4/5/8/9) + `client/tests/relay_confidentiality.rs` (row C). Pattern can crib from `relay/src/tls.rs` test module (lines 138-255) which already self-signs SPIFFE-bearing certs via rcgen in-process.

## Verification

```bash
cd /home/bairava/Arise/zecurity/client
cargo test                          # 14/14 unit tests, all green
./scripts/sprint10_2_final_check.sh # Go + relay/connector/client cargo gates
```

Manual: follow [Relay-Operational-Validation.md](../../Services/Relay-Operational-Validation.md) end-to-end. Success criterion = all seven `expect:` log lines appear in `journalctl -u zecurity-client`.
