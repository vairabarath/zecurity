---
type: run-sheet
sprint: 20
title: Sprint 20 Live Acceptance — Run Sheet
operator: Sathiya (M1)
status: not-run   # not-run | in-progress | passed | failed
runbook: docs/sprint20-live-acceptance-runbook.md
tags:
  - acceptance
  - live
  - provider-dashboard
---

# Sprint 20 — Live Acceptance Run Sheet

> **How to use:** follow `docs/sprint20-live-acceptance-runbook.md` (the **runbook**) step by step, and fill in this sheet as you go. Each row gives the runbook section, what passes, and the `path.md` line it closes.
> - For every row, record the **clock time**, the **evidence** (a log line, SQL output or serial), and **PASS / FAIL**.
> - A FAIL isn't a reason to stop. Record it, continue if you safely can, and write it up in the phase file's Post-Phase Fixes.
>
> **Read runbook §2 (known issues KI-1 … KI-6) before starting.** Two of them change the env values below.

## Run details

| Field | Value |
|---|---|
| Date / operator | |
| Commit under test (`git rev-parse --short HEAD` on `fixed-pendings`) | |
| Controller env | `CONNECTOR_CERT_TTL=15m` · `CONNECTOR_RENEWAL_WINDOW=10m` · `RELAY_CERT_TTL=1h` (KI-1) · `SHIELD_CERT_TTL=24h` (KI-2) |
| Controller started at | |
| relay1 id / relay-c id | |
| connector id / shield id | |
| Workspace slug | |

## Reference values (record as you go)

| Value | Recorded | Time |
|---|---|---|
| `ctl_serial_0` / `ctl_notafter_0` (controller's original gRPC cert) | | |
| `ctl_serial_1` (first rotation) | | |
| `ctl_serial_2` (second rotation) | | |
| `conn_serial_0` / `conn_notafter_0` (connector's **original** cert) | | |
| `conn_serial_1` (first renewal) | | |
| `relay_serial_0` / `relay_notafter_0` (relay1's first cert) | | |
| relay1 `relay.key` sha256 (after provisioning) | | |
| relay1 renewals: serial @ time (list) | | |
| `tunnel_opened_at` (long-lived tunnel) | | |

---

## Setup — runbook §3, §5

| # | Check | Pass criteria | Time | Evidence | Result |
|---|---|---|---|---|---|
| S1 | Provider redirect fixed (KI-3) | `PROVIDER_GOOGLE_REDIRECT_URI=…/provider/auth/callback`, registered with Google | | | |
| S2 | Controller up with the short TTLs | `seeded provider super-admin`, `gRPC server listening on :9090` | | | |
| S3 | Provider token | `GET /provider/me` returns 200 | | | |
| S4 | relay1 + relay-c created | two rows, `pending`; relay1 `ip_allowlist={127.0.0.1}`, relay-c `{}` | | | |

## Stage 1 — Phase C: relay SAN allowlist (runbook §6)

Closes `path.md` **:169** and sprint criterion **:250**.

| # | Case | Pass criteria | Time | Evidence | Result |
|---|---|---|---|---|---|
| C1 | AT-C.2 | relay-c with `RELAY_IP_SANS=127.0.0.1` is rejected: controller logs `requested IP SAN 127.0.0.1 not in allowlist`, relay sees `PermissionDenied` | | | |
| C2 | AT-C.2 | the token survives: `EXISTS relay:provisioning:jti:<jti>` = 1, row still `pending` with its JTI | | | |
| C3 | AT-C.5 | retry with no IP SAN and the **same** token → `Relay provisioned`; JTI key = 0; row no longer `pending` | | | |

## Stage 2 — Bring-up (runbook §7)

| # | Check | Pass criteria | Time | Evidence | Result |
|---|---|---|---|---|---|
| U1 | relay1 up | `Relay heartbeat acknowledged`; first `delay_secs` about 360–1080 (not 0) | | | |
| U2 | Connector enrolled fresh | `enrollment complete`, `:9091` + `:9092` up, `Connector registered with Relay`, **no** `initial Relay CRL fetch failed` | | | |
| U3 | Connector's original cert served | `:9091` and `:9092` TLS serial = `conn_serial_0` (runbook §15.1–15.2) | | | |
| U4 | Shield enrolled fresh | shield renews every ~15 s (KI-2) with `cert renewed`; connector logs `proxying shield cert renewal to controller` | | | |
| U5 | Client up; long-lived tunnel open | tunnel open to a connector-served TCP resource (not shield-routed RDE) | | | |

## Stage 3 — Watch window (runbook §8)

### Phase E — controller gRPC rotation (§8.1)

Closes sprint criterion **:252**.

| # | Case | Pass criteria | Time | Evidence | Result |
|---|---|---|---|---|---|
| E1 | AT-E.1 | about 10–11 min after controller start, `served localhost:9090` shows a **new** serial, same SANs | | | |
| E2 | AT-E.1 | no `controller cert rotation failed` in the controller log | | | |
| E3 | AT-E.3 | after `ctl_notafter_0`, new connections still work: connector control stream, client `sync`/`status`, relay heartbeats | | | |
| E4 | AT-E.1 | second rotation, about 10 min after the first | | | |

### Phase G — connector renewal (§8.2)

Closes `path.md` **:227** and sprint criterion **:254**.

| # | Case | Pass criteria | Time | Evidence | Result |
|---|---|---|---|---|---|
| G1 | AT-G.4 | about 5 min after enrollment: controller logs `ReEnroll sent`; connector logs `certificate renewed, persisted and published` | | | |
| G2 | AT-G.4 | `connectors.cert_serial` changed; `cert_not_after` moved forward | | | |
| G3 | AT-G.4 | consumers switched: `device tunnel (QUIC) switched…`, `Shield-proxy controller channel rebuilt…`; `:9091` and `:9092` TLS serve the new serial | | | |
| G4 | AT-G.1 | a duplicate ReEnroll within 60 s is ignored (`renewed moments ago`), not renewed twice | | | |
| G5 | AT-G.5 | **after `conn_notafter_0`**, no restart: a new **direct** (QUIC) tunnel works, with no relay fallback | | | |
| G6 | AT-G.5 | after `conn_notafter_0`: `:9092` TLS serves the current serial | | | |
| G7 | AT-G.5 | after `conn_notafter_0`: a **relayed** tunnel works (UDP 9092 blocked, `direct path failed; used relay fallback`) | | | |
| G8 | AT-G.5 | after `conn_notafter_0`: the shield keeps renewing via this connector (`cert renewed`, no `proxy failed`) | | | |
| G9 | AT-G.6 | the long-lived tunnel from U5 is still open after several connector renewals | | | |

### Phase A — relay steady state (§8.3)

Closes part of `path.md` **:143** and sprint criterion **:246**.

| # | Case | Pass criteria | Time | Evidence | Result |
|---|---|---|---|---|---|
| A1 | AT-CORE-2 | relay1 is `active` at every ~2 min sample for ≥ 10 min (it may stay stale up to about 5 min between throttled writes) | | | |
| A2 | AT-CORE-2 | no `relay expiry: evicted relay <relay1>` during the window | | | |

A1 samples (time → status / hb_age):

### Phase F — relay in-band renewal (§8.4)

Closes `path.md` **:206** and part of sprint criterion **:253**.

| # | Case | Pass criteria | Time | Evidence | Result |
|---|---|---|---|---|---|
| F1 | AT-F.1 | 6–18 min after provisioning: relay logs `Relay certificate renewed and installed`; controller logs `relay renew: … superseded=0` | | | |
| F2 | AT-F.1 | same key (`relay.key` sha256 unchanged); new `relay_certificates` row; `relays.cert_serial` = newest; old serial not revoked | | | |
| F3 | AT-F.1 | the next `delay_secs` is sensible (not 0) after each renewal | | | |
| F4 | AT-F.8 | **after `relay_notafter_0`** (+60 min), no restart: relay1 `active`, heartbeats acknowledged, connector attached, relayed tunnel works | | | |

## Stage 4 — Phase A: relay stop / restart (runbook §9)

Closes the rest of `path.md` **:143** and sprint criterion **:247**.

| # | Case | Pass criteria | Time | Evidence | Result |
|---|---|---|---|---|---|
| A3 | AT-A.2 | relay1 stopped → `inactive` within **≤ 150 s**; controller logs `relay expiry: evicted relay` | | stopped at: · inactive at: | |
| A4 | AT-A.2 | the connector drops relay1 (fails over or backs off) | | | |
| A5 | AT-A.3 | restarted within 5 min → `active` on the **first** heartbeat; the connector attaches again | | restarted at: · active at: | |

## Stage 5 — Phase F: relay revoke + CRL (runbook §10)

Closes `path.md` **:207** and the rest of sprint criterion **:253**.

| # | Case | Pass criteria | Time | Evidence | Result |
|---|---|---|---|---|---|
| F5 | AT-F.6 | `POST /provider/relays/<id>/revoke` → 204; `relays.status=revoked`; zero unrevoked `relay_certificates` rows | | | |
| F6 | AT-F.7 | `/relay.crl` lists every **unexpired** relay1 serial, current and previous | | serials: | |
| F7 | AT-F.7 | the connector drops relay1 within about 76 s (`error=relay certificate revoked`) and doesn't dial it again | | revoked at: · dropped at: | |

## Stage 6 — Phase D: disconnect watcher (runbook §11)

Closes sprint criterion **:251**.

| # | Case | Pass criteria | Time | Evidence | Result |
|---|---|---|---|---|---|
| D1 | AT-D.1 | connector `kill -9` → `disconnected` within about 90–120 s; controller logs `disconnect watcher: marked connector(s) disconnected` | | killed at: · disconnected at: | |
| D2 | AT-D.1 | client gets a **new** transport snapshot version without the connector, with no other action | | version before → after: | |

## Stage 7 — Phase B: revocation is terminal (runbook §12)

Closes sprint criteria **:248** and **:249**.

| # | Case | Pass criteria | Time | Evidence | Result |
|---|---|---|---|---|---|
| B1 | AT-B.1 | revoke connector → its stream closes on the next health report; row `revoked` + `revoked_at` after the close defer | | | |
| B2 | AT-B.2 / CORE-1 | reconnect attempts and a process restart are refused (`PermissionDenied`); still `revoked` after 2 min | | | |
| B3 | AT-B.8 / CORE-1 | revoke shield → stays `revoked` while the connector keeps sending status batches; `lan_ip` unchanged | | | |

AT-B.5 (`Goodbye` on revoked) can't be triggered live, because the connector never sends `Goodbye`. It is covered by `revocation_test.go` (runbook §12).

## Stage 8 — Regression gate (runbook §13)

Also closes the unticked Phase B build gate, `path.md` **:156**.

| # | Check | Pass criteria | Time | Evidence | Result |
|---|---|---|---|---|---|
| R1 | `buf generate` | no diff | | | |
| R2 | Controller | `go build`, `go vet`, `go test ./...` pass; DB tests run (only skip: `TestEnroll_CSRSignatureInvalid`) | | | |
| R3 | Rust | connector + relay `cargo build && cargo test`; shield + client `cargo build` | | | |

---

## Close-out checklist (runbook §14)

- [ ] Every row above has a time, evidence and PASS / FAIL.
- [ ] `path.md` boxes ticked **only** for rows that passed: :143, :156, :169, :206, :207, :227 and sprint criteria :246–:255. Failed rows keep their box unticked, with a note linking the row here.
- [ ] Bugs recorded in the phase file's Post-Phase Fixes and in `path.md` Post-Sprint Fixes.
- [ ] Session Log entry added.
- [ ] Frontmatter `status:` updated. Clean-up done (nft table, processes, `/tmp/s20-live`, `.localdev/s20-*`).

## Findings during the run

| # | Row | What happened | Severity | Follow-up |
|---|---|---|---|---|
| | | | | |
