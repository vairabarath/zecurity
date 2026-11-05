---
type: live-run
date: 2026-11-05
tester: Barath (M2) with Claude
branch: main 3f3fe998 (all components); baseline client e07fe524; pre-FU-9 shield f5bd339f
status: complete; two findings (F1, F2) tracked as FU-17 and FU-18
---

# Live run: the FU-9 / FU-16 untested gaps (2026-11-05)

**Reporting rule:** each row records what was observed (run files, journal lines, timestamps).
Anything inferred is marked as inferred.

## Summary

| Test | What | Result |
|---|---|---|
| LT-4 | Revocation and connector-stop tail on a **shield-route** resource | **PASS** (8 runs) |
| LT-1 | **Dead-peer keepalive**, connector ↔ shield (silent drop of TCP 9091) | **Mixed. Not a pass.** Detection **PASS** (shield +39–46 s, connector +51 s); termination of the active download **FAIL** (F1 / FU-17) |
| LT-2a | Client ↔ connector connection lost **without a close frame** (silent drop) | **PASS** (flow ends at the 30 s idle timeout) |
| LT-2b | Same, by **connector SIGKILL**; baseline comparison | **PASS**; baseline client behaves the same |
| LT-3a | Revocation over the **relay path** | **PASS** |
| LT-3b | **Relay SIGKILL** mid-download | **PASS** (idle timeout, +34 s) |
| LT-3c | **Connector stop behind the relay** | **FAIL → F2 / FU-18** |
| LT-6 | **Mixed versions** (pre-FU-9 shield, current connector) | **PASS** (clear refusal, no hang) |
| LT-5 | **Certificate-expiry cut-off** of a tunnel kept across a renewal | **PASS** |
| — | UDP through a real client | Not testable (the client does not route UDP) |

## Lab

- **This PC (192.168.1.223):**
  - controller `main` `3f3fe998`; throwaway database `lt_live`; Valkey `ci_valkey_6390` database 4;
  - provider super-admin set up through the API (password change and TOTP enrolment; secrets in mode-600 files, never printed);
  - relay `lt-relay` run in the foreground (`RELAY_IP_SANS=192.168.1.223`, :9093);
  - client: fixed `06098bfc…` (`main`) and baseline `2c06c99a…` (`e07fe524`), both built against the lab platform root;
  - ufw `lt-live`: TCP 8080, 8081, 9090 and UDP 9093 from .75.
- **inkyank (192.168.1.75):**
  - connector `a683448a…`; shield `d54773a6…`; pre-FU-9 shield `091a4fc6…` (LT-6 only);
  - resources: `lt-web` :8817 (connector route) and `lt-shield-web` :8827 (shield route), both serving the 60 MB file `big60.bin`.
- **Method:**
  - `measure.py` from FU-16 samples, every ~1 ms, curl's output size and its socket `Recv-Q` (`/proc/net/tcp`). Accuracy limits as in FU-16.
  - Cuts sent through an Orca terminal take 0.65 s; the cut happened inside that window.

## Results

### LT-4: shield-route tail (port 8827, direct path)

| Run | `Recv-Q` at cut | Bytes after cut | Client end of flow |
|---|---|---|---|
| stop 25K ×2 | 83 / 83 KB | 78 / 84 KB | `tunnel transport closed` +0.71 s |
| stop 300K ×2 | 951 / 773 KB | 951 / 614 KB | +0.66 / +0.71 s |
| revoke 25K ×2 | 57 / 83 KB | 108 / 83 KB | `tunnel stream reset by peer` (RVKD) +23 / +12 ms |
| revoke 300K ×2 | 906 / 906 KB | 909 / 618 KB | +12 / +10 ms |

**revoke25-1 (108 KB vs 57 KB):**
- 1 ms before the cut, curl had just read 25 KB from its socket (`Recv-Q` 83 → 57 KB) and had not written it to the file yet.
- Until the client's reset at +24 ms, the client refilled curl's socket window to 81 KB.
- Tail = 25 KB (in curl's memory) + 81 KB (in the socket). This is within the FU-16 boundary ("plus what the app's reading admits until the reset"). It shows the cut can land right after an app read.

### LT-1: dead-peer keepalive, connector ↔ shield

- **Setup:** a 25 KB/s shield download plus an idle shield connection.
- **Cut:** at +10 s, the nftables table `lt_block` on inkyank silently dropped TCP 9091 in both directions.

| Run | Shield noticed | Connector noticed | Idle connection | Download |
|---|---|---|---|---|
| 1 (drop 15:03:10.19) | +45.6 s `Control stream ended: its tunnels were closed closed=2` | +51.5 s `shield Control stream disconnected`; both tunnel streams failed, sessions unregistered | reset at +52.2 s | **kept receiving until +146 s, then a clean close** (curl 18, partial file, 4.00 MB) |
| 2 (drop 15:08:13.73) | +39.3 s | +51.4 s | reset at +52.1 s | same: +145 s, curl 18, 3.99 MB |

- **Keepalive:** within the configured budget (30 s ping + 20 s timeout) on both sides.
- **Recovery:** after the block was removed, the shield reconnected and the route answered 200 after 24.6 s.
- **F1 (FU-17):** see Findings.

### LT-2: client ↔ connector lost without a close frame

The flow end is timed from the cut command; the cut itself happens within the next ~0.65 s.

| Series | Client | Rate | Bytes after cut | Flow failed |
|---|---|---|---|---|
| 2a silent drop ×2 | fixed | 25K | 901 / 868 KB | +32.1 / +31.1 s, `tunnel transport closed` |
| 2b SIGKILL ×2 | fixed | 25K | 868 / 878 KB | +31.0 / +31.1 s |
| 2a silent drop ×2 | baseline | 25K | 897 / 868 KB | +32.1 / +31.1 s, `connection lost` |
| 2a silent drop | baseline | 10K | 430 KB | +35.1 s |
| 2b SIGKILL | baseline | 10K | 410 KB | +32.1 s |
| 2a silent drop | fixed | 10K | 410 KB | +33.1 s |

- Without a close frame, both clients end the flow at quinn's 30 s idle timeout.
- Before that the client has no signal, so it delivers at the app's pace. After it, nothing more is delivered: at 10 KB/s the ~800 KB the client held would last ~80 s, and both stopped at ~33 s.
- **Corrects an FU-16 inference:** quinn keeps handing out buffered stream data after the connection ends only when the peer **sends** a close (FU-16 R1, connector stop). It does not do so on an idle timeout.
- **Scope of the FU-16 fix:**
  - It addresses **graceful connection closure**: the peer sends a close, and quinn would otherwise keep handing out buffered data.
  - It does **not** change **silent transport loss** (idle timeout), where quinn already stops at the timeout.
  - Here, fixed and baseline clients end within run-to-run variation (31–35 s).

### LT-3: relay path

The client's direct path was blocked by the nftables table `lt_block2` on this PC; `direct path failed; used relay fallback` was logged for every measured flow.

| Run | `Recv-Q` at cut | Bytes after cut | End |
|---|---|---|---|
| 3a revoke 25K ×2 | 83 / 83 KB | 74 / 78 KB | FU-8 hot-remove +38 ms |
| 3a revoke 300K ×2 | 1,099 / 1,099 KB | 926 / 1,101 KB | hot-remove |
| 3b relay SIGKILL 25K ×2 | 83 / 83 KB | 971 / 971 KB | `tunnel transport closed` +34.0 s (idle timeout) |
| **3c connector stop 25K ×2** | 83 / 83 KB | **1,774 / 1,716 KB** | client: `stream reset by peer: error 0x524c4142` ("RLAB") at **+66 / +64 s** |
| **3c connector stop 300K ×2** | 1,050 / 905 KB | **4,030 / 4,026 KB** | +61 / +61 s; run 1: last byte +14 s, then **47 s of silence** |

**LT-3c, relay log (run 1, stop at 16:07:05):**
- `16:08:10.087 Connector removed from Relay registry`, then `Relay stream bridge ended with error bytes_down=3582911 … forwarded_code="0x524c4142" error="pipe Connector to Client: read: connection lost: timed out"`.
- The relay learned of the stop only at its idle timeout, +65 s → **F2 (FU-18)**.

### LT-6: mixed versions

Pre-FU-9 shield `f5bd339f` (version 1.0.10) with the current connector:
- the shield's Control stream connects and its firewall rules apply;
- every tunnel is refused at once with `shield rejected tunnel: shield does not support dedicated tunnel streams; upgrade the shield` (connector log);
- the client gets an error in 10 ms (curl 56), with no hang; the connector route is unaffected;
- the shield logs nothing about the refusal.

The current shield was restored, and the route answered 200 again.

### LT-5: certificate-expiry cut-off

- **Setup:** controller restarted with `SHIELD_CERT_TTL=10m`, so the renewal window is clamped to 2/5 of the TTL, i.e. 4 min. A second shield record, `lt-shield-inkyank-2`, was enrolled, because only a pending shield gets a token.
- **Timeline:**
  - 16:22:36: certificate A issued (recorded expiry 16:32:36.7);
  - 16:23:01: a 25 KB/s download started on connection A;
  - **16:28:37:** the connector requested renewal; the shield renewed (certificate B, expiry 16:38:37); `Control stream handed over, tunnels kept tunnels=1` on both sides;
  - **16:32:41.68:** the connector unregistered the old tunnel's session, and the client logged at once `tunnel ended while the app was not reading … reset by peer: 1381387076` (RVKD); curl's last byte came 3 s later (its `Recv-Q`);
  - afterwards a new shield tunnel answered 200.
- **Observation, not a defect:** the cut came ~5 s after the expiry the controller recorded for certificate A.
  - The expected semantics are not established: which `notAfter` the connector uses (the peer certificate's, as presented on the connection) versus the database value, and any clock skew allowance.
  - Not investigated; recorded so a later check can decide whether it matters.

## Findings

### F1 → FU-17: a failed shield tunnel ends the device stream gracefully (truncation looks like a clean end)

- **Observed (LT-1):** after the connector declared the shield link dead and unregistered the session (+51 s), the client kept delivering the download for ~95 s more (≈3.7 MB after the cut). It then ended with a **clean FIN** (curl 18 "partial file"). The client logged no error.
- **Cause (code, `connector/src/agent_tunnel.rs`, `relay_stream`, `End::Failed`):**
  - the device stream is only dropped (`d2s.abort()`; comment: "the device stream is dropped (abrupt, unlike a clean shutdown)");
  - for a QUIC device stream, quinn treats a dropped `SendStream` as a graceful **finish**: everything already queued (quinn's default send window is 10 MB per connection) is still delivered, followed by FIN;
  - FU-15 fixed this for `End::Aborted` (explicit `abort()`), but not for `End::Failed`, which covers: a shield stream error, a stream that ended without `TunnelEnd`, and the dead-peer case.
- **Impact:**
  - an app receives stale data for up to minutes after the tunnel failed;
  - a truncated response looks like a normal end; HTTP with Content-Length catches it, while a protocol without framing would not.
- **Fix direction:** abort (reset) the device stream on `End::Failed`, as on `End::Aborted`, with a test that a failed shield stream resets the device stream.

### F2 → FU-18: on `systemctl stop` the connector does not close its relay connection

- **Observed (LT-3c):** relayed clients kept receiving relay-buffered data (1.7–4.0 MB) after the connector stopped, then hung silently until the relay's idle timeout (+60–65 s) forwarded a reset ("RLAB"). On the direct path the same stop ends flows in < 1 s (LT-4, FU-16).
- **Cause (code, `connector/src/main.rs` `graceful_shutdown`):**
  - the shutdown closes the client QUIC endpoint (`quic_listener`), shield streams and says goodbye to the controller;
  - the relay QUIC connection (`relay_client.rs`) is not closed;
  - the relay log confirms it learned of the stop only from `connection lost: timed out`.
- **Fix direction:** close the relay connection(s) with a reason in `graceful_shutdown` (bounded wait, like the client endpoint), so the relay resets the bridged streams at once.
- **Related, still open:** the client's relay-path watch does not see a reset inside TLS while the app is not reading (FU-16 limitation). F2 is the dominant effect here.

## Observations (no action proposed)

- **Sudo caching on inkyank was unreliable during the run.** Once, a connector-stop series sent its commands into a pending password prompt. That series was aborted, its runs discarded, and the stray prompt cancelled; the connector was never stopped by it.
- **Credential hygiene:** the inkyank tab's scrollback re-printed the connector enrollment token (single-use, already consumed at enrolment) into the session output. Lab tooling should send enrollment tokens from a file on the host instead of in the command text.
