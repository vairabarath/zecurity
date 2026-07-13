---
type: runbook
scope: relay
status: draft
date: 2026-07-06
branch: integration/relay-merge
tags:
  - relay
  - wan
  - deployment
  - test-plan
---

# Relay — WAN End-to-End Test Plan

> **Goal.** Prove that a client on an *untrusted* network (a phone hotspot / public
> internet) can reach a resource on a *private* LAN, by tunnelling
> **client → relay(VPS) → connector(LAN) → resource** while the direct
> client→connector LAN path is unreachable. This is the core ZTNA remote-access
> value proposition and the first true WAN exercise of the relay subsystem.
>
> Companion docs: [Relay-E2E-Flow-and-Security-Review](../Relay-E2E-Flow-and-Security-Review.md)
> (how it works + findings), [Relay-Operational-Validation](Relay-Operational-Validation.md)
> (the LAN-only fallback runbook this extends).

---

## 0. The one constraint that dictates the whole topology

The controller is **not** in the tunnel hot path — but it **is** in the relay's
control path. The relay must reach the controller for **Provision** (get its cert)
and **Heartbeat** (stay advertised), over controller **gRPC :9090** and **HTTP :8080**
(`/ca.crt`).

Crucially, the controller computes each relay's **advertised public address from the
observed TCP peer IP of the relay's heartbeat connection**
(`controller/internal/relay/heartbeat.go:242-281`), classifies it
public/private/loopback, and — only if public — advertises `peerIP:listen_port` to
clients and connectors inside the ACL snapshot
(`controller/internal/policy/compiler.go:162-177`). **There is no controller-side
`RELAY_ADDR` override.**

Two consequences that this plan is built around:

1. **No reverse tunnel for the relay→controller path.** If the relay reaches the
   controller through an SSH/loopback tunnel, the controller sees `127.0.0.1`,
   classifies it non-public, and **never advertises the relay** — clients get an empty
   relay address and the WAN path silently doesn't exist. The controller must be
   reachable such that the relay's *real public source IP* is preserved.
2. **Because the controller must be publicly reachable anyway, the "log in on the LAN
   first, then roam" step is optional.** The client can log in from the hotspot too.
   The load-bearing part of the test — resource traffic forced through the relay
   because the direct LAN address is unreachable from the hotspot — happens
   automatically either way. We keep the "log in on LAN, then roam" sequence because it
   is the realistic scenario and a good smoke test, but it is not a requirement.

**Chosen topology: controller + relay both on the VPS.** This sidesteps home
CGNAT/port-forwarding entirely. Only the connector, its shield, and the resource stay
on the LAN (both dial *out*).

---

## 1. Target topology

```
        ┌───────────────────────── HOSTINGER VPS (public IP: <VPS_IP>) ─────────────────────────┐
        │                                                                                        │
        │   Controller                          Relay                                            │
        │   ├─ gRPC   :9090  (mTLS + SPIFFE)     ├─ QUIC  :9093  (UDP, mTLS)                       │
        │   ├─ HTTP   :8080  (/ca.crt, OAuth,    └─ dials controller :9090 (Provision+Heartbeat)  │
        │   │                 GraphQL, admin)        and :8080 (/ca.crt)  ── observed peer must    │
        │   ├─ Postgres (docker)                     be <VPS_IP>, NOT 127.0.0.1  (see §0, §4.3)    │
        │   └─ Valkey/Redis (docker)                                                              │
        └───────▲───────────────────▲───────────────────────────────▲──────────────────────────┘
                │ login / ACL sync   │ control stream (9090)          │ QUIC bridge (9093)
                │ (9090 + 8080)      │ + relay list                   │
                │                    │                                │
   ┌────────────┴─────────┐   ┌──────┴────────────── LAN ────────────┴──────────┐
   │  CLIENT (laptop)     │   │  Connector            Shield         Resource     │
   │  ├─ on LAN: login    │   │  ├─ dials ctlr :9090  ├─ hb→conn      (e.g. an     │
   │  │   + sync (optional)│   │  ├─ dials relay :9093    :9091        HTTP svc    │
   │  └─ on HOTSPOT:       │   │  ├─ listens :9092     └─ nftables     on LAN)     │
   │      direct fails →   │   │  │   (direct, LAN-only)                           │
   │      relay fallback   │   │  └─ listens :9091 (shield)                        │
   └──────────────────────┘   └──────────────────────────────────────────────────┘
        client dials OUT to relay :9093 (works through hotspot NAT/CGNAT)
        connector dials OUT to relay :9093 (works through LAN NAT)
```

**Why the relay path is exercised automatically:** the ACL snapshot the client caches
lists the connector's *direct* address as its **LAN IP :9092** (private, unreachable
from the hotspot) plus the **relay** at `<VPS_IP>:9093`. On the hotspot the client tries
direct first (2 s timeout, `client/src/transport.rs`), fails, and falls back to relay.

---

## 2. Reachability / firewall matrix

| From ↓ / To → | Ctlr 9090/tcp | Ctlr 8080/tcp | Relay 9093/udp | Conn 9092/udp | Conn 9091/tcp |
|---|---|---|---|---|---|
| **Relay** (VPS)        | ✅ dial | ✅ dial | — | — | — |
| **Connector** (LAN)    | ✅ dial | — | ✅ dial | listen | listen |
| **Shield** (LAN)       | — | — | — | — | ✅ dial |
| **Client on LAN**      | ✅ dial | ✅ dial | ✅ dial | ✅ dial (direct works) | — |
| **Client on hotspot**  | ✅ dial | ✅ dial | ✅ dial | ❌ (direct fails → relay) | — |

**VPS firewall (ufw / Hostinger panel) — open inbound:**
- `9090/tcp` (controller gRPC), `8080/tcp` (controller HTTP), `9093/udp` (relay QUIC).
- (SSH 22/tcp for you.)

**Relay QUIC is UDP.** Confirm the VPS provider and — later — the mobile carrier do not
block/throttle UDP or QUIC (see §7 risks).

---

## 3. Prerequisites & build artifacts

Build on a machine matching each target's architecture (or cross-compile). Binaries:

| Host | Binary / service | Build command |
|---|---|---|
| VPS | controller | `cd controller && go build ./cmd/server` |
| VPS | relay | `cargo build --release --manifest-path relay/Cargo.toml` |
| LAN | connector | `cd connector && cargo build --release` |
| LAN | shield | `cargo build --release --manifest-path shield/Cargo.toml` |
| Laptop | client | `cd client && cargo build --release` |

Also decide a **stable public name for the controller**. Strongly recommended: point a
DNS record at the VPS (e.g. `ctlr.example.com`) rather than using the raw IP, so the
controller's TLS server cert SAN is clean and OAuth redirect URIs are stable. The plan
uses `<CTLR_HOST>` for that name (may be an IP if you accept IP SANs).

---

## 4. Phase-by-phase runbook

### Phase 1 — Controller on the VPS

1. **Backing stores.** `cd controller && docker compose up -d` (Postgres + Valkey).
2. **Serving-cert SANs.** The controller mints its own gRPC/HTTP server cert from
   `CONTROLLER_ADDR` / `CONTROLLER_HTTP_ADDR` (`cmd/server/main.go:702-712`). These
   **must** contain the exact host the relay/connector/client will dial, or their TLS
   handshakes fail. Set them to the public name:
   ```bash
   export CONTROLLER_ADDR=<CTLR_HOST>:9090
   export CONTROLLER_HTTP_ADDR=<CTLR_HOST>:8080
   export GRPC_PORT=9090          # default
   export PORT=8080               # default
   # ... plus your DB/Valkey DSNs and JWT/secrets as usual
   ```
3. **OAuth.** Client login is Google OAuth (`client/src/daemon.rs` PostLoginState).
   Configure the controller's OAuth client and set the **authorized redirect URI to the
   public HTTP URL** (`https://<CTLR_HOST>:8080/...`). If OAuth still points at
   `localhost`, login from the laptop will fail.
4. **Start** the controller and verify the CA is served publicly:
   ```bash
   ./server &
   curl -fsS http://<CTLR_HOST>:8080/ca.crt | openssl x509 -noout -subject
   ```
   **Expect:** the intermediate CA subject prints. This URL must be reachable **from the
   VPS relay process and from the laptop**.

> ⚠️ **Security note.** You are exposing the controller to the internet. Provisioning is
> currently unauthenticated (finding **F1**) and there is no CRL check on relay-facing
> mTLS (**F2**). For a *test* this is acceptable; do **not** treat this exposure as a
> production posture. Restrict inbound 9090/8080 to known source IPs if you can.

### Phase 2 — Relay on the VPS

1. **(Optional) Admin pre-register** the relay row and mint the (currently unenforced)
   provisioning token — mirrors production and gives you a stable `RELAY_ID`:
   ```bash
   curl -sS -H "Authorization: Bearer $ADMIN_TOKEN" \
        -X POST https://<CTLR_HOST>:8080/api/relays \
        -d '{"name":"relay-vps","dns_allowlist":["<CTLR_HOST>"],"ip_allowlist":["<VPS_IP>"]}' | jq .
   # → note the returned relay id (RELAY_ID) and provisioning_token
   ```
   (If you skip this, the relay self-provisions — F1 — and picks its own UUID. Fine for
   a test.)
2. **Pin the CA fingerprint** (out-of-band trust anchor):
   ```bash
   RELAY_CA_FINGERPRINT=$(curl -fsS http://<CTLR_HOST>:8080/ca.crt \
       | openssl x509 -outform DER | sha256sum | awk '{print $1}')
   ```
3. **Run the relay.** The decisive detail: point `CONTROLLER_ADDR` at the **public host**
   (`<CTLR_HOST>` / `<VPS_IP>`), **not** `localhost` — even though it's the same box —
   so the controller observes a public peer IP (see §0 and the §4.3 checkpoint). Put the
   public IP in the SANs:
   ```bash
   sudo CONTROLLER_ADDR=<CTLR_HOST>:9090 \
        CONTROLLER_HTTP_ADDR=<CTLR_HOST>:8080 \
        RELAY_ID=<RELAY_UUID> \
        RELAY_CA_FINGERPRINT=$RELAY_CA_FINGERPRINT \
        RELAY_BIND=0.0.0.0:9093 \
        RELAY_IP_SANS=<VPS_IP> \
        RELAY_DNS_SANS=<CTLR_HOST> \
        RELAY_MAX_CONNECTIONS=64 \
        LOG_LEVEL=info \
        ./scripts/relay-local-install.sh relay/target/release/zecurity-relay
   ```
   (Or run in the foreground with the env inline for iterative debugging.)
   **Expect** in relay logs:
   ```
   relay provisioning successful cert_serial=... cert_not_after=...
   relay heartbeat ok
   ```

### 4.3 — CHECKPOINT: verify the relay is advertised with the *public* IP

This is the make-or-break check for the whole WAN test. On the controller / DB:

```sql
-- in the controller Postgres
SELECT id, status, public_addr, address_scope, observed_ip, last_heartbeat_at
FROM relays ORDER BY last_heartbeat_at DESC LIMIT 5;
```

**Expect:** `status='active'`, `address_scope='public'`, and
`public_addr = <VPS_IP>:9093`.

**If `public_addr` is empty or `address_scope` is `loopback`/`private`:** the controller
saw the wrong source IP (the co-located gotcha from §0). Fixes, in order of preference:
- Ensure the relay's `CONTROLLER_ADDR` uses `<VPS_IP>`/`<CTLR_HOST>`, not `127.0.0.1`/`localhost`.
- If it still resolves to loopback, run the relay on a **separate host** from the
  controller so the peer IP is unambiguously public.
- Confirm the controller is not behind an L4/L7 proxy that rewrites the source IP
  (finding **F14**).

Do not proceed to Phase 3 until `public_addr = <VPS_IP>:9093`.

### Phase 3 — Connector + Shield + resource on the LAN

1. **Connector.** It needs only `CONTROLLER_ADDR=<CTLR_HOST>:9090` (plus its enrollment
   token / workspace) — **no static relay address**; it learns the relay from the
   controller's `LabelledRelayList` push over the control stream
   (`connector/src/control_stream.rs`, `relay_selector.rs`). Install/run via
   `scripts/connector-local-install.sh`.
   **Expect** in controller logs: `connector registered id=<uuid> workspace=<ws>`.
   **Expect** in connector logs: it receives a relay list and selects/registers on
   `<VPS_IP>:9093`.
2. **Verify registration on the relay.** Relay logs should show a `Register` from the
   connector's SPIFFE id, and its next heartbeat reports `registered_connectors >= 1`.
3. **Shield + resource.** Bring up the shield next to the resource
   (`scripts/shield-local-install.sh`); it heartbeats to the connector `:9091` and
   applies nftables. Define a resource (e.g. an HTTP service on the LAN) and a policy
   that grants your test user access. Without an attached shield the connector answers
   `SHIELD_NOT_ATTACHED` and the tunnel won't complete.

### Phase 4 — Client baseline on the LAN (direct path)

Do this **while the laptop is on the LAN** to establish a known-good baseline before
roaming.

```bash
zecurity-client setup --workspace <WS_SLUG> --controller-address <CTLR_HOST>   # dev override of the compiled default
zecurity-client login          # OAuth; enrolls device cert (7-day TTL)
zecurity-client up
zecurity-client sync           # pull ACL snapshot (contains relay <VPS_IP>:9093 + connector LAN:9092)
journalctl -u zecurity-client -f --output=cat &
curl -v http://<RESOURCE>:80/  # first stream open
```

**Expect:** resource responds; **no** `direct path failed; used relay fallback` warning
(on the LAN the direct connector address is reachable, so direct wins).

Sanity-check the cached snapshot actually carries the relay coords:
```bash
zecurity-client resources      # or inspect daemon logs; relay_addr should be <VPS_IP>:9093
```

### Phase 5 — Roam to the hotspot (the actual WAN test)

1. Disconnect the laptop from LAN Wi-Fi/Ethernet and connect to the **phone hotspot**.
   Leave the client daemon running (do **not** log out). The cached ACL snapshot +
   device cert remain valid; the data plane authenticates to the connector with the
   **device cert (7-day TTL)**, not the 15-minute access token, so roaming offline from
   the controller is fine for the test window.
2. Trigger a **fresh** stream (a new resource or after pooled connections idle out):
   ```bash
   curl -v http://<RESOURCE>:80/
   ```
   **Expect** in client logs within ~2 s of the dial:
   ```
   WARN zecurity_client::transport: direct path failed; used relay fallback
     direct_err="..." relay_addr="<VPS_IP>:9093"
   ```
   and the resource responds. Traffic path is now
   **laptop(hotspot) → relay(<VPS_IP>:9093) → connector(LAN) → resource**.
3. **Confirm the bridge on the relay:** relay `connection_count` increments (visible in
   its next heartbeat / the `relays` row) while the stream is open.
4. **Confirm confidentiality:** the relay only moves ciphertext — the inner
   client↔connector mTLS is end-to-end (relay never terminates tunnel TLS). No plaintext
   resource data should be observable on the relay.

---

## 5. Success criteria

- [ ] `relays` row: `status=active`, `address_scope=public`, `public_addr=<VPS_IP>:9093`.
- [ ] Connector registers on the relay; relay heartbeat reports `registered_connectors>=1`.
- [ ] Client ACL snapshot (synced on LAN) contains `relay_addr=<VPS_IP>:9093`.
- [ ] LAN baseline: resource reachable via **direct** path, no fallback warning.
- [ ] Hotspot: resource reachable via **relay fallback** — the
      `direct path failed; used relay fallback` warning appears and the response returns.
- [ ] Relay `connection_count` rises while the hotspot stream is open.
- [ ] Denied resource on the hotspot returns a tunnel-denied error (from the connector),
      **not** a relay fallback loop.

---

## 6. Test variations (optional, once the happy path passes)

- **Login on the hotspot** (not on LAN first) to confirm the controller is fully public
  and OAuth redirects resolve — proves the LAN-first step really is optional.
- **Kill the relay** mid-session: after ~90 s the controller's eviction sweep should
  drop it from the advertised list (note finding **F4** — eviction may be broken on this
  branch; verify against the DB, don't assume).
- **Second relay** on another host to exercise probe-based selection/migration
  (ADR-016) and per-connector placement.

---

## 7. Risks & gotchas

| Risk | Why | Mitigation |
|---|---|---|
| Relay advertised as loopback/private | Controller co-located; relay dialed `localhost` (§0) | Dial `<VPS_IP>`; verify §4.3; or separate hosts |
| Reverse tunnel for relay→controller | Controller sees 127.0.0.1 → relay never advertised | Don't. Use direct public reachability |
| Controller TLS handshake fails | Serving-cert SAN ≠ dialed host | Set `CONTROLLER_ADDR`/`_HTTP_ADDR` to `<CTLR_HOST>`; regenerate cert |
| OAuth login fails from laptop | Redirect URI still `localhost` | Set redirect to `https://<CTLR_HOST>:8080/...` |
| UDP/QUIC blocked on cellular or VPS | Relay :9093 and connector :9092 are QUIC/UDP | Confirm carrier/VPS allow UDP; test on a second carrier; watch for MTU/fragmentation over cellular |
| Client "expires" after 15 min offline | Access token TTL is 15 min | Irrelevant to data plane — tunnel uses the 7-day device cert. Only controller RPCs (sync/refresh) fail offline |
| Device cert expires | 7-day TTL; renewal needs controller | Complete the test inside 7 days of last login, or keep controller reachable |
| Unauthenticated provisioning / no CRL | Findings F1, F2 on this branch | Acceptable for a closed test; restrict inbound 9090/8080; not a prod posture |
| Direct path adds ~2 s on first hotspot dial | Client tries direct (LAN IP) first, times out | Expected. To make fallback instant during testing, block the connector's direct addr client-side |

---

## 8. Teardown

- Laptop: `zecurity-client down && zecurity-client logout` (revokes the device SPIFFE).
- LAN: stop shield + connector (`scripts/*-uninstall.sh`).
- VPS: stop relay (`scripts/relay-uninstall.sh`), stop controller, `docker compose down`.
- **Close the VPS firewall** (9090/8080/9093) — do not leave the controller exposed.
- Delete the relay row / revoke its cert if you pre-registered.

---

## Appendix — key code references

| Fact | Location |
|---|---|
| Relay advertised addr = observed public peer IP | `controller/internal/relay/heartbeat.go:242-281` |
| Per-connector relay coords injected into ACL | `controller/internal/policy/compiler.go:118,162-177` |
| No controller-side `RELAY_ADDR` override | `controller/cmd/server/main.go` (DB-driven only) |
| Controller serving-cert SANs from env | `controller/cmd/server/main.go:702-712` |
| Connector learns relay from control stream | `connector/src/control_stream.rs`, `relay_selector.rs:98-119` |
| Client direct-first, relay-fallback (2 s) | `client/src/transport.rs`, `relay_pool.rs` |
| Data plane needs no controller (cached ACL) | `client/src/daemon.rs:267-278` |
| Token TTL 15 m / refresh 7 d-30 d / cert 7 d | `controller/internal/auth/config.go:114-126`, `client/internal/service.go:39` |
| Relay config env | `relay/src/config.rs` |
