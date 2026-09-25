# Sprint 20 — Live Acceptance Runbook

> **Status:** Ready to run. Nothing has been run live yet.
> **Date:** 2026-09-25 · **Code under test:** `fixed-pendings` at `0a7b78e` or later. All Sprint 20 phases (A–G) are merged.
> **Operator:** Sathiya (M1). **Record results in:** `.zecurity-obs/Sprint20/Live-Acceptance-Run-Sheet.md`.
> **Scope:** the live dev-stack checks that tests cannot close. Every phase already has unit and integration tests. This runbook proves the same behaviour with real processes over real minutes.

All commands run from the repository root unless a step says otherwise. Every command, env var, log line and SQL column below was checked against the source. File references are given so you can confirm them if anything looks different.

---

## Contents

1. [What this run proves](#1-what-this-run-proves)
2. [Known issues that change how you run it](#2-known-issues-that-change-how-you-run-it) — **read this first**
3. [Prerequisites](#3-prerequisites)
4. [Timeline](#4-timeline)
5. [Setup (T-10 → T0)](#5-setup-t-10--t0)
6. [Stage 1 — Relay SAN allowlist (Phase C)](#6-stage-1--relay-san-allowlist-phase-c)
7. [Stage 2 — Bring up the long-running stack](#7-stage-2--bring-up-the-long-running-stack)
8. [Stage 3 — Watch window: E, G, A, F](#8-stage-3--watch-window-e-g-a-f)
9. [Stage 4 — Relay stop / restart (Phase A)](#9-stage-4--relay-stop--restart-phase-a)
10. [Stage 5 — Relay revoke + CRL (Phase F)](#10-stage-5--relay-revoke--crl-phase-f)
11. [Stage 6 — Disconnect watcher (Phase D)](#11-stage-6--disconnect-watcher-phase-d)
12. [Stage 7 — Revocation is terminal (Phase B)](#12-stage-7--revocation-is-terminal-phase-b)
13. [Stage 8 — Regression gate](#13-stage-8--regression-gate)
14. [Closing out](#14-closing-out)
15. [Reference: commands, logs, SQL](#15-reference-commands-logs-sql)
16. [Troubleshooting](#16-troubleshooting)

---

## 1. What this run proves

| Stage | Acceptance cases | Sprint criterion (`path.md`) | What it proves |
|---|---|---|---|
| 1 | AT-C.2, AT-C.5 | :169, :250 | A relay cannot get a SAN outside its allowlist, and a rejection does not use up the provisioning token. |
| 3 | AT-E.1, AT-E.3 | :252 | The controller swaps its gRPC certificate before expiry, and everything keeps connecting past the first certificate's `NotAfter`. |
| 3 | AT-G.4, AT-G.5, AT-G.6 | :227, :254 | The connector renews automatically. After its **original** certificate expires, with no restart, the following all still work: direct tunnels, relayed tunnels, `:9091`, `:9092` and shield renewals. Open tunnels are not dropped. |
| 3 | AT-CORE-2, AT-A (steady state) | :143, :246 | A healthy relay stays `active` for ≥ 10 min, across at least two throttled DB-write intervals. |
| 3 | AT-F.8, AT-F.1 | :206, :253 | A relay renews itself in-band with the same key and keeps serving past its first certificate's `NotAfter`. |
| 4 | AT-A.2, AT-A.3 | :143, :247 | A stopped relay goes `inactive` within ≤ 150 s. A restarted relay is `active` on its first heartbeat. |
| 5 | AT-F.7, AT-F.6 | :207, :253 | Revoking a relay revokes **every** serial. Both serials appear on `/relay.crl`, and connectors drop the relay. |
| 6 | AT-D.1 | :251 | A connector the watcher marks `disconnected` drops out of the next transport snapshot with no other event. |
| 7 | AT-CORE-1, AT-B.1/2/8 (B.5 `Goodbye` is test-only, see §12) | :248, :249 | A revoked connector or shield stays `revoked` through stream close, reconnects, `Goodbye`, `RenewCert` and status batches. |
| 8 | §9 regression gate | :255 | Every build and test suite is green. |

---

## 2. Known issues that change how you run it

These came up while preparing this runbook. None of them blocks the run, but each one changes a step, and **ignoring them produces false failures**. Where a step depends on one, it says so.

### KI-1 — A 15-minute relay certificate renews in a tight loop. Use `RELAY_CERT_TTL=1h`.

- **Where:** the controller issues relay certificates with `NotBefore = now − 1h` (`controller/internal/pki/relay.go:61`). The relay treats the lifetime as `not_after − not_before` and renews when 2/5 of that remains (`relay/src/renewal.rs:67-73`, `cert_manager.rs:43,174`).
- **Effect:** renewal lands at `0.6 × TTL − 24 min` after issue.
  - For TTLs of 40 min or less, that time is already past, so the relay renews immediately, installs, and immediately renews again. It is a continuous `RenewCert` loop.
  - Production (30-day default) is unaffected: renewal happens about 18 days in.
- **Workaround for this run:** use `RELAY_CERT_TTL=1h`. The relay then renews 6–18 min after each issue (12 min ± 6 min jitter). Its first certificate expires at +60 min.
- **Consequence:** the relay part of the run takes about 65 minutes. The ~9 min expectation in AT-F.8 was calculated without the 1-hour backdate, so ignore it.
- This is the same class of bug as PF-1 (Phase E), which was fixed only in the controller rotator. **Not fixed; it is a follow-up** (see `path.md` → Known issues).

### KI-2 — With `SHIELD_CERT_TTL` of 48h or less, shields renew every ~15 s

- **Where:** the connector decides that a shield needs renewal using a **hard-coded** 48 h window (`connector/src/agent_server.rs`, `DEFAULT_RENEWAL_WINDOW_SECS`). The controller's `SHIELD_RENEWAL_WINDOW` is parsed but never used. The shield has no debounce.
- **Effect:** with a short `SHIELD_CERT_TTL`, every 15 s shield health report triggers `ReEnroll`. The shield renews through the connector's `:9091` and reconnects its control stream.
- **Why we still use it:** a shield renewal is the only way to exercise the `:9091` server and the shield-proxy controller channel after the connector's certificate has been renewed. With the 7-day default, no shield renewal would happen during the run.
- **So:** run with `SHIELD_CERT_TTL=24h`. Expect a `cert renewed` line from the shield roughly every 15 s; treat that churn as expected. **Pre-existing, not fixed.** Don't use a shield-routed (RDE) tunnel for the "open tunnels are not dropped" check, because the churn reconnects the shield's stream.

### KI-3 — Provider login is misconfigured in the dev `.env`

- **Where:** `controller/.env` and `.env.example` set `PROVIDER_GOOGLE_REDIRECT_URI=http://localhost:8080/auth/callback`. That is the **tenant** callback. The provider callback that returns a provider token is `/provider/auth/callback` (`controller/cmd/server/main.go:348`).
- **Effect:** the tenant handler has no provider branch, so you never get a provider token.
- **Fix before the run:** see §3.1.

### KI-4 — Some certificate swaps log nothing on success

- No log line: controller gRPC rotation success (only failures log: `controller cert rotation failed: %v`), the `:9092` TLS swap, and the `:9091` swap.
- For these, check the **served certificate** with `openssl s_client` (§15.1).

### KI-5 — The client cannot be told to use TLS or the relay

- The client's direct path is QUIC only, to the connector's `:9092` UDP. It never uses the `:9092` TCP/TLS listener.
- The relay is used **only as a fallback**: a direct connect error or a 2 s timeout falls back to the relay (`client/src/transport.rs:112-160`).
- **So:**
  - Check the TLS listener with `openssl s_client`.
  - Force the relay path by blocking UDP to `:9092` with nftables (§15.4).
  - After a direct failure the client waits 30 s before trying direct again, doubling each time up to 2 h. Restart the client daemon after unblocking to reset that.

### KI-6 — Minor points

- **Provider token lifetime:** it expires after **15 minutes** (`main.go:342`). Log in again before Stage 5.
- **Relay helper script:**
  - Always export `RELAY_ID` from the `POST /provider/relays` response. Otherwise `scripts/run-relay-local.sh` generates its own UUID, and provisioning then fails.
  - The script replaces an empty `RELAY_IP_SANS` with `127.0.0.1`, so Stage 1's retry runs the relay binary directly.
- **Hard-coded timings:** the relay sweep (60 s) and threshold (90 s), and the relay heartbeat interval (30 s, sent by the controller), can't be configured.
- **Stale comment:** a doc comment in `connector/src/crl.rs:30` says "every 5 minutes". The real CRL refresh is every 60–75 s.

---

## 3. Prerequisites

### 3.1 One-time fixes (do these the day before)

1. **Provider redirect (KI-3).** In `controller/.env`, set:
   ```
   PROVIDER_GOOGLE_REDIRECT_URI=http://localhost:8080/provider/auth/callback
   ```
   Then add that exact URI to **Authorized redirect URIs** on the Google OAuth client the dev controller uses.
2. **Provider admin.** Set `PROVIDER_BOOTSTRAP_EMAILS=<your Google email>` in `controller/.env`. On startup the controller upserts that address as `super-admin` and logs `seeded provider super-admin: <email>`.
3. **Admin access.** You need an admin login to an `active` workspace. You'll use it for connector and shield enrollment tokens (admin UI → Remote Network → add connector / shield) and for revoking.
4. **Tools:** `psql`, `valkey-cli` (or `docker exec -it ztna_valkey valkey-cli`), `openssl` (3.x), `curl`, `jq`, `nft`, `grpcurl` (optional).

### 3.2 Build everything

```bash
buf generate
(cd controller && go build ./...)
cargo build --manifest-path connector/Cargo.toml
cargo build --manifest-path relay/Cargo.toml
cargo build --manifest-path shield/Cargo.toml
(cd client && cargo build)
```

### 3.3 Free the ports

A connector, shield or relay left over from earlier work will fight for the ports.

```bash
sudo systemctl stop zecurity-connector zecurity-shield zecurity-relay 2>/dev/null || true
ss -ltnup | grep -E ':(8080|9090|9091|9092|9093)\b'   # only the controller (8080/9090) should appear once it is started
```

Ports in use during the run:

| Port | Protocol | Used by |
|---|---|---|
| 8080 | TCP | controller HTTP |
| 9090 | TCP | controller gRPC |
| 9091 | TCP | connector, shield-facing |
| 9092 | TCP + UDP | connector device tunnel (TLS + QUIC) |
| 9093 | UDP | relay |

### 3.4 Run directory

Use fresh state everywhere. Short certificate lifetimes only apply to certificates issued **after** the controller starts with the new env, so reusing an enrolled connector or shield would give you a 7-day certificate.

```bash
export RUN=/tmp/s20-live
rm -rf "$RUN" && mkdir -p "$RUN"/{logs,connector,shield,relay-c}
export DB='postgres://ztna:ztna_dev_secret@localhost:5432/ztna_platform?sslmode=disable'
```

---

## 4. Timeline

It's one sitting of about **95 minutes**. Most of it is waiting. Stage 3 is a watch window in which four phases are checked together.

```
T-10  Setup: infra, controller with short TTLs, provider token, create relays      §5
T0    Stage 1  Phase C  (relay-c: reject, token survives, retry succeeds)            §6  ~10 min
T+10  Stage 2  start relay1, enroll connector + shield, client up, open long tunnel  §7  ~10 min
T+20  Stage 3  watch window ─────────────────────────────────────────────────────    §8  ~65 min
        · controller gRPC cert rotates ~10 min after start, first cert expires at 15 min   (E)
        · connector ReEnroll ~5 min after enrolment, original cert expires at 15 min      (G)
        · relay1 stays active the whole time                                               (A)
        · relay1 renews every 6–18 min; first relay cert expires 60 min after provisioning (F)
T+85  Stage 4  stop / restart relay1                                                 §9  ~5 min
T+90  Stage 5  revoke relay1, check /relay.crl, connector drops it                   §10 ~3 min
T+93  Stage 6  kill connector, watch disconnect + snapshot                           §11 ~3 min
T+96  Stage 7  revoke connector + shield, confirm terminal                           §12 ~5 min
      Stage 8  regression gate (can run any time on a separate checkout)             §13
```

Write the **clock time** of each event in the run sheet. Several checks compare times.

---

## 5. Setup (T-10 → T0)

### 5.1 Infrastructure

```bash
(cd controller && docker compose up -d)          # ztna_postgres :5432, ztna_valkey :6379
```

### 5.2 Controller with short lifetimes

Exported variables override `controller/.env`, because `godotenv.Load` does not overwrite values that are already set.

```bash
cd controller
export CONNECTOR_CERT_TTL=15m          # connector certs AND the controller gRPC cert (Phase E reuses it)
export CONNECTOR_RENEWAL_WINDOW=10m    # connector gets ReEnroll ~5 min after each issue
export RELAY_CERT_TTL=1h               # KI-1: not 15m
export SHIELD_CERT_TTL=24h             # KI-2: shields renew via :9091 every ~15 s
go run ./cmd/server 2>&1 | tee "$RUN/logs/controller.log"
```

**Expect:**
- `loaded environment from .env`
- `✓ Valkey ... connected`
- `seeded provider super-admin: <email>`
- `gRPC server listening on :9090`
- `listening on :8080`

In another shell, record the controller's first gRPC certificate. This is **the controller's original certificate**:

```bash
export RUN=/tmp/s20-live
ctl_cert() { openssl s_client -connect localhost:9090 -alpn h2 </dev/null 2>/dev/null | openssl x509 -noout -serial -startdate -enddate; }
ctl_cert | tee "$RUN/ctl-cert-0.txt"
```

Record `ctl_serial_0` and `ctl_notafter_0` in the run sheet.

### 5.3 Provider token

```bash
curl -s http://localhost:8080/provider/auth/initiate | jq -r .auth_url
# open the URL, sign in with the bootstrap Google account; the callback page shows {"token": "...", "expires_in": 900}
export PTOKEN='<token>'
curl -s -H "Authorization: Bearer $PTOKEN" http://localhost:8080/provider/me | jq .
```

A `403 not_a_provider_user` response means the signed-in email isn't in `PROVIDER_BOOTSTRAP_EMAILS`.

### 5.4 Create both relays (the token lasts 15 min, so do this now)

```bash
# relay1: the long-running relay (A, F). Its allowlist matches the helper script's default SAN.
curl -s -X POST http://localhost:8080/provider/relays -H "Authorization: Bearer $PTOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"s20-relay1","dns_allowlist":[],"ip_allowlist":["127.0.0.1"]}' | tee "$RUN/relay1.json"

# relay-c: the Phase C relay, with an EMPTY allowlist
curl -s -X POST http://localhost:8080/provider/relays -H "Authorization: Bearer $PTOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"s20-relay-c","dns_allowlist":[],"ip_allowlist":[]}' | tee "$RUN/relay-c.json"
```

Each response is `{"relay_id","provisioning_token","expires_at"}`. Record both `relay_id`s. Both rows are now `pending`:

```bash
psql "$DB" -c "SELECT id, name, status, ip_allowlist, enrollment_token_jti FROM relays WHERE name LIKE 's20-%';"
```

---

## 6. Stage 1 — Relay SAN allowlist (Phase C)

**Cases:** AT-C.2 (rejected, token kept) and AT-C.5 (retry with the same token succeeds).

### 6.1 Find the token's JTI

```bash
export RC_ID=$(jq -r .relay_id "$RUN/relay-c.json")
export RC_TOKEN=$(jq -r .provisioning_token "$RUN/relay-c.json")
export RC_JTI=$(psql "$DB" -Atc "SELECT enrollment_token_jti FROM relays WHERE id='$RC_ID'")
valkey-cli EXISTS "relay:provisioning:jti:$RC_JTI"      # → 1
```

### 6.2 Provision with an IP SAN that isn't allowed → rejected

The helper defaults `RELAY_IP_SANS=127.0.0.1`, which is not in relay-c's empty allowlist.

```bash
RELAY_ID="$RC_ID" RELAY_PROVISIONING_TOKEN="$RC_TOKEN" \
  scripts/run-relay-local.sh s20-relay-c 9094 2>&1 | tee "$RUN/logs/relay-c-attempt1.log"
# the relay exits with a provisioning error; Ctrl-C if it retries
```

**Pass:**
- Controller log: `relay provision: relay=<RC_ID> requested IP SAN 127.0.0.1 not in allowlist`.
- The relay reports gRPC `PermissionDenied: requested SAN not in relay allowlist`.
- `valkey-cli EXISTS "relay:provisioning:jti:$RC_JTI"` still returns **1**. The token was not used up.
- `SELECT status, enrollment_token_jti FROM relays WHERE id='$RC_ID'` returns `pending` with the JTI still set.

### 6.3 Retry with no IP SAN and the same token → provisioned

The helper can't send an empty `RELAY_IP_SANS` (KI-6), so run the binary directly:

```bash
FP=$(curl -fsS http://localhost:8080/ca.crt | openssl x509 -outform DER | sha256sum | awk '{print $1}')
env -u RELAY_IP_SANS -u RELAY_DNS_SANS \
  RELAY_ID="$RC_ID" CONTROLLER_ADDR=localhost:9090 CONTROLLER_HTTP_ADDR=localhost:8080 \
  RELAY_CA_FINGERPRINT="$FP" RELAY_PROVISIONING_TOKEN="$RC_TOKEN" \
  RELAY_BIND=0.0.0.0:9094 RELAY_STATE_DIR="$RUN/relay-c/pki" RELAY_MAX_CONNECTIONS=8 LOG_LEVEL=info \
  relay/target/debug/zecurity-relay 2>&1 | tee "$RUN/logs/relay-c-attempt2.log"
```

**Pass:**
- Relay log: `stored Relay certificate material`, then `Relay provisioned; starting multi-workspace mTLS QUIC listener`.
- `valkey-cli EXISTS "relay:provisioning:jti:$RC_JTI"` now returns **0** (the token is used).
- The DB row has left `pending` and `enrollment_token_jti` is NULL.

Stop relay-c with Ctrl-C. It isn't needed again; it will go `inactive`, which is expected.

---

## 7. Stage 2 — Bring up the long-running stack

### 7.1 relay1

```bash
export R1_ID=$(jq -r .relay_id "$RUN/relay1.json")
RELAY_ID="$R1_ID" RELAY_PROVISIONING_TOKEN="$(jq -r .provisioning_token "$RUN/relay1.json")" \
  LOG_LEVEL=info scripts/run-relay-local.sh s20-relay1 9093 2>&1 | tee "$RUN/logs/relay1.log"
```

**Expect:**
- `Relay provisioned; starting multi-workspace mTLS QUIC listener`
- `Relay heartbeat connected`
- `Relay heartbeat acknowledged`
- `next Relay certificate renewal scheduled` with `delay_secs` between about **360 and 1080** (KI-1).

Record `relay_serial_0` and `relay_notafter_0` (§15.3 SQL) and the provisioning time. Also record `relay1_active_since`: the clock time the row became `active`.

> If `delay_secs` is `0` and renewals repeat back to back, the controller is not running with `RELAY_CERT_TTL=1h` (KI-1).

### 7.2 Connector (fresh enrollment)

In the admin UI, open Remote Network → add connector, and copy the enrollment token. Run the connector in the foreground with its own state directory:

```bash
CONTROLLER_ADDR=localhost:9090 CONTROLLER_HTTP_ADDR=localhost:8080 ENROLLMENT_TOKEN='<jwt>' \
  STATE_DIR="$RUN/connector" AUTO_UPDATE_ENABLED=false LOG_LEVEL=info \
  connector/target/debug/zecurity-connector 2>&1 | tee "$RUN/logs/connector.log"
```

**Expect:**
- `no state found — starting enrollment`
- `enrollment complete`
- `Shield gRPC server starting on :9091`
- `device tunnel listeners spawned on :9092 (TLS+QUIC)`
- `received LabelledRelayList push`
- `Connector registered with Relay`

There must be **no** `initial Relay CRL fetch failed` line.

Record `conn_id`, the enrollment time, and **`conn_serial_0` / `conn_notafter_0`**, the connector's **original** certificate (§15.3). Also check that `:9091` and `:9092` serve that same serial (§15.1).

### 7.3 Shield (fresh enrollment)

In the admin UI, open Remote Network → add shield, and copy the token. The shield needs `CAP_NET_ADMIN`:

```bash
sudo env CONTROLLER_ADDR=localhost:9090 CONTROLLER_HTTP_ADDR=localhost:8080 ENROLLMENT_TOKEN='<jwt>' \
  STATE_DIR="$RUN/shield" AUTO_UPDATE_ENABLED=false LOG_LEVEL=info \
  shield/target/debug/zecurity-shield 2>&1 | tee "$RUN/logs/shield.log"
```

**Expect:**
- `no state.json found — starting enrollment`
- then, repeating about every 15 s (KI-2): `connector requested cert renewal` → `starting certificate renewal` → `cert renewed` → `cert renewed — reconnecting Control stream`
- Connector log: `proxying shield cert renewal to controller`, with **no** `shield cert renewal proxy failed`.

### 7.4 Client and a long-lived tunnel

Use the existing client install, or `scripts/client-local-install.sh`:

```bash
zecurity-client setup --workspace <slug> --controller localhost:9090 --http-base http://localhost:8080   # first time only
zecurity-client login
zecurity-client up
zecurity-client resources          # pick a TCP resource served by THIS connector (not shield-routed RDE, KI-2)
journalctl -u zecurity-client -f | tee "$RUN/logs/client.log" &
```

**Open the long-lived tunnel now**, before the connector's first renewal. It is the AT-G.6 witness. Use an interactive SSH session to the resource, or a slow stream:

```bash
ssh <user>@<resource-ip>            # keep it open; type a command every few minutes
# or: while true; do date; sleep 20; done | nc <resource-ip> <port>
```

Record `tunnel_opened_at`.

---

## 8. Stage 3 — Watch window: E, G, A, F

Leave everything running. Check the items below as they fall due. The times are relative to the event named in each subsection.

### 8.1 Phase E — controller gRPC rotation (AT-E.1, AT-E.3)

The rotator swaps the certificate at about 2/3 of its lifetime, measured from when it stored the certificate (the PF-1 fix). With `CONNECTOR_CERT_TTL=15m` that's about **10 min after controller start**. The rotator checks once a minute, so allow 1 min of slack.

- [ ] **About +11 min:** `ctl_cert` shows a **new serial** with the same SANs. Record `ctl_serial_1` and the time.
- [ ] The controller log has **no** `controller cert rotation failed` lines.
- [ ] **After `ctl_notafter_0`** (+15 min): new connections to the controller still succeed.
  - `ctl_cert` works.
  - The connector's control stream reconnects after its next renewal. Look for `cert renewed successfully` followed by normal traffic.
  - The client works: `zecurity-client sync` / `status`.
  - relay1 heartbeats continue: `Relay heartbeat acknowledged`.
- [ ] **About +21 min:** a second rotation (`ctl_serial_2`) confirms that rotation keeps going.

### 8.2 Phase G — connector renewal (AT-G.4, AT-G.5, AT-G.6)

With a 15 min certificate and a 10 min window, the controller sends `ReEnroll` about **5 min after each issue**, then throttles resends to one per 10 min per stream.

- [ ] **About 5 min after enrollment:**
  - Controller log: `control stream: connector <conn_id> cert expires ... (inside renewal window 10m0s) — ReEnroll sent`.
  - Connector log, in order:
    - `controller requested cert renewal — starting renewal`
    - `starting certificate renewal`
    - `certificate renewed, persisted and published` (fields `serial=` and `new_expiry=`)
    - `device tunnel (QUIC) switched to renewed certificate`
    - `Shield-proxy controller channel rebuilt with renewed certificate`
- [ ] `connectors.cert_serial` changed to `conn_serial_1`, and `cert_not_after` moved forward (§15.3).
- [ ] The served certificates match (§15.1):
  - `:9092` TLS serial = `conn_serial_1`;
  - `:9091` serial = `conn_serial_1`.
  - `state.json` `cert_not_after` was updated in `$RUN/connector`.
- [ ] Duplicate suppression: any extra ReEnroll within 60 s logs `certificate renewed moments ago — ignoring duplicate ReEnroll`, not a second renewal.
- [ ] Renewals repeat about every 10 min (`conn_serial_2`, …) with no connector restart.

**After `conn_notafter_0`** (the connector's **original** certificate has expired), with no restart:

- [ ] **Direct tunnel (QUIC `:9092`):** open a **new** connection to the resource. It works. The client log does **not** show `direct path failed; used relay fallback`.
- [ ] **`:9092` TLS listener:** `openssl s_client` (§15.1) presents the current serial, not the original.
- [ ] **Relayed tunnel:** block direct UDP (§15.4) and open a new connection. It works, and the client log shows `direct path failed; used relay fallback`. Unblock, then `sudo systemctl restart zecurity-client` to clear the direct-path cooldown.
- [ ] **Shield renewal via the connector:** shield `cert renewed` lines keep appearing after `conn_notafter_0`, with `renewed_via=` this connector. The connector log has no `shield cert renewal proxy failed`.
  - This proves both that `:9091` presents the renewed certificate (the shield checks it on every connect) and that the shield-proxy channel uses the renewed identity toward the controller.
- [ ] **AT-G.6:** the long-lived tunnel from §7.4 is **still open** and responsive after several connector renewals.

### 8.3 Phase A — relay steady state (AT-CORE-2)

- [ ] relay1 stays `active` for **≥ 10 min** of continuous observation. That's at least two 5-minute throttled DB-write intervals. Run the relay query (§15.3) every ~2 min and record status and `hb_age`.
  - `hb_age` can grow to about 5 min between throttled writes. That's expected.
  - `status` must never read `inactive`.
- [ ] The controller log has **no** `relay expiry: evicted relay <R1_ID>` line during this window.
- [ ] Valkey shows `relay:heartbeat:last:<R1_ID>` updating (`valkey-cli GET`) roughly every 30 s.

### 8.4 Phase F — relay in-band renewal (AT-F.1, AT-F.8)

- [ ] **6–18 min after provisioning:**
  - Relay log: `Relay certificate renewed and installed` (`old_serial`, `new_serial`), then `Relay listener switched to renewed certificate`, then `Relay certificate renewed; reconnecting heartbeat with the new identity`.
  - Controller log: `relay renew: relay=<R1_ID> old_serial=... new_serial=... not_after=... superseded=0`.
- [ ] **Same key:** `.localdev/s20-relay1/pki/relay.key` is unchanged. Record `sha256sum` right after provisioning and again after each renewal; `relay.crt` changes, `relay.key` must not.
- [ ] **DB:** a new `relay_certificates` row exists, `relays.cert_serial` equals the newest serial, and the previous serial has `revoked_at IS NULL` (§15.3).
- [ ] Each renewal is followed by the next `next Relay certificate renewal scheduled` with a sensible `delay_secs` (not 0).
- [ ] **After `relay_notafter_0`** (+60 min from provisioning), with no relay restart:
  - relay1 is still `active`;
  - heartbeats are still acknowledged;
  - the connector is still `Connector registered with Relay` (or reconnects to it);
  - a relayed tunnel (§15.4) still works.

---

## 9. Stage 4 — Relay stop / restart (Phase A)

**Cases:** AT-A.2 and AT-A.3.

1. Stop relay1 with Ctrl-C and record `relay1_stopped_at`.
2. Poll every 15 s: `psql "$DB" -c "SELECT status, now()-last_heartbeat_at AS hb_age FROM relays WHERE id='$R1_ID'"`.
   - [ ] `status = inactive` within **≤ 150 s** of `relay1_stopped_at` (90 s threshold + 60 s sweep).
   - [ ] Controller log: `relay expiry: evicted relay <R1_ID>`.
   - [ ] Connector log: the relay is dropped, via `Active relay session failed; failing over` or `LabelledRelayList is empty; entering backoff`.
3. Restart it within 5 minutes, with the same command as §7.1 minus the token, since it is already provisioned. Record `relay1_restarted_at`.
   - [ ] `status = active` on its **first** heartbeat, within a few seconds. There's no 5-minute wait, because eviction cleared the throttle marker.
   - [ ] The connector attaches again: `Connector registered with Relay`.

---

## 10. Stage 5 — Relay revoke + CRL (Phase F)

**Cases:** AT-F.7 and AT-F.6. The provider token has expired by now, so log in again (§5.3).

1. Record every serial relay1 has had (§15.3, relay certificate history). There should be at least 3 or 4 after 60+ minutes.
2. Revoke:
   ```bash
   curl -s -o /dev/null -w '%{http_code}\n' -X POST \
     -H "Authorization: Bearer $PTOKEN" "http://localhost:8080/provider/relays/$R1_ID/revoke?reason=s20-live"
   # → 204
   ```
   Record `relay1_revoked_at`.
3. Check:
   - [ ] `relays.status = revoked`.
   - [ ] `SELECT count(*) FROM relay_certificates WHERE relay_id='$R1_ID' AND revoked_at IS NULL` returns **0**. **Every** serial is revoked.
   - [ ] `/relay.crl` lists the unexpired serials, including the current one and the previous one:
     ```bash
     curl -s http://localhost:8080/relay.crl | openssl crl -inform DER -noout -text | grep 'Serial Number'
     ```
     Serials of certificates that have **already expired** are correctly left off (`not_after > NOW()` filter). Compare against the SQL in §15.3.
   - [ ] **Within about 76 s** (60 s CRL refresh + ≤ 15 s jitter + 1 s check), the connector drops relay1. Look for `Relay session failed` with `error=relay certificate revoked`. The connector doesn't dial relay1 again.
   - [ ] A relayed tunnel attempt no longer uses relay1.

---

## 11. Stage 6 — Disconnect watcher (Phase D)

**Case:** AT-D.1.

1. Note the client's current transport snapshot version: the last `transport snapshot stored` line in `$RUN/logs/client.log` (field `version`).
2. Kill the connector **without** a clean shutdown: `pkill -9 -f 'connector/target/debug/zecurity-connector'`. Record `conn_killed_at`.
3. Check:
   - [ ] `connectors.status = disconnected` within **~90–120 s** (`CONNECTOR_DISCONNECT_THRESHOLD` 90 s + the 30 s watcher tick).
   - [ ] Controller log: `disconnect watcher: marked connector(s) disconnected workspaces=1`.
   - [ ] The client picks up a **new** snapshot version, with no other action: `transport snapshot stored` with a higher `version`, and/or `background sync: version changed, restarting tunnel`. The snapshot no longer lists this connector.

> The transport version is in memory in the controller and resets on controller restart, so don't restart the controller during Stages 3–6.

4. Restart the connector with the §7.2 command minus `ENROLLMENT_TOKEN`; its state is in `$RUN/connector`. It goes back to `active`, and a new snapshot version appears.

---

## 12. Stage 7 — Revocation is terminal (Phase B)

**Cases:** AT-CORE-1 and AT-B.1, B.2, B.8. Do this last, because it ends the connector and shield.

1. **Revoke the connector** in the admin UI (Connector → Revoke; GraphQL `revokeConnector`). Record the time.
   - [ ] The connector's control stream is closed on its next health report, within about 15 s.
   - [ ] `connectors.status = revoked` and `revoked_at` is set, and **stays** that way after the stream-close defer runs:
     ```sql
     SELECT status, revoked_at FROM connectors WHERE id = '<conn_id>';
     ```
   - [ ] The connector keeps trying to reconnect. Each `Control` attempt is refused with `PermissionDenied`, and the row stays `revoked`. Re-check after 2 minutes.
   - [ ] Stopping the connector process (Ctrl-C) closes its stream. The close defer does **not** change `revoked` to `disconnected`.
   - [ ] Restarting the connector process: still refused; still `revoked`.
   - Note: the connector binary has no shutdown hook and never sends `Goodbye`. The `Goodbye`-on-revoked path (AT-B.5) is covered by `revocation_test.go` against a real database and can't be triggered from a live connector.
2. **Revoke the shield.** First restart a non-revoked connector so the shield has a live path again: enroll a second connector if needed. Then revoke the shield in the admin UI (Shield → Revoke; GraphQL `revokeShield`).
   - [ ] `shields.status = revoked` and **stays** that way while the connector keeps sending shield status batches. Re-check over 2 minutes.
   - [ ] The shield's `lan_ip` doesn't change afterwards.
   - Note: shields have **no** `revoked_at` column and no CRL. Revocation is by `status` only; that's expected, per `path.md` I6.

---

## 13. Stage 8 — Regression gate

This is from `Acceptance-Test-Plan.md` §9. It can run before or after the live run, but not against the controller being tested. Point the DB-backed suites at a **throwaway** database. They create and drop their own databases, so never run them against a database you care about.

```bash
buf generate && git status --short          # expect no diff
cd controller && go build ./... && go vet ./... && \
  ENROLLMENT_TEST_DATABASE_URL="$DB" PKI_TEST_DATABASE_URL="$DB" SHIELD_TEST_DATABASE_URL="$DB" \
  go test -count=1 ./...
cd ../connector && cargo build && cargo test
cd ../relay && cargo build && cargo test
cargo build --manifest-path ../shield/Cargo.toml
cd ../client && cargo build
```

- [ ] Everything passes. A **skipped** DB test counts as a failure (`Acceptance-Test-Plan.md:17`). The only accepted skip is the pre-existing, unconditional `TestEnroll_CSRSignatureInvalid`.

---

## 14. Closing out

1. Fill in every row of `.zecurity-obs/Sprint20/Live-Acceptance-Run-Sheet.md` with a time, the evidence, and PASS / FAIL.
2. Tick the matching boxes in `.zecurity-obs/Sprint20/path.md`. The run sheet lists which line each check closes.
   - Tick a box only when its check passed.
   - For a failure, leave the box unticked and add a note that links to the run-sheet row.
3. Record any bug found in the phase file's **Post-Phase Fixes**, and summarise it in `path.md` **Post-Sprint Fixes** (the `CLAUDE.md` format).
4. Add a Session Log entry to `.zecurity-obs/Planning/Session Log.md`.
5. Clean up:
   - `sudo nft delete table inet s20test`
   - stop all processes
   - `rm -rf /tmp/s20-live .localdev/s20-relay1 .localdev/s20-relay-c`
   - put `PROVIDER_GOOGLE_REDIRECT_URI` back only if other work needs the old value.

---

## 15. Reference: commands, logs, SQL

### 15.1 Served-certificate checks

TLS 1.3 sends the server certificate before it checks the client, so these print the server's certificate even though the handshake later fails for lack of a client certificate.

```bash
served() {  # served <host:port> <alpn>
  openssl s_client -connect "$1" -alpn "$2" </dev/null 2>/dev/null | openssl x509 -noout -serial -enddate
}
served localhost:9090 h2               # controller gRPC (Phase E)
served 127.0.0.1:9091 h2               # connector, shield-facing (G-2b)
served 127.0.0.1:9092 ztna-tunnel-v1   # connector device tunnel, TLS (G-2a)
```

The relay (`:9093`) is QUIC only, so check its serial through the DB (`relays.cert_serial`) and its logs.

### 15.2 Serial format

- The DB stores serials as lowercase hex with no leading zeros (`SerialNumber.Text(16)`).
- `openssl` prints `serial=` in uppercase hex, sometimes with leading zeros.

Normalise before comparing:

```bash
norm() { tr 'A-F' 'a-f' | sed -E 's/^serial=//; s/^0+//'; }
served 127.0.0.1:9092 ztna-tunnel-v1 | grep serial= | norm
```

### 15.3 SQL

```sql
-- relays
SELECT id, name, status, last_heartbeat_at, now()-last_heartbeat_at AS hb_age,
       cert_serial, cert_not_after, ip_allowlist, enrollment_token_jti
FROM relays WHERE name LIKE 's20-%' ORDER BY created_at;

-- relay certificate history (F)
SELECT rc.serial, rc.issued_at, rc.not_after, rc.revoked_at, rc.revocation_reason,
       (rc.serial = r.cert_serial) AS is_current
FROM relay_certificates rc JOIN relays r ON r.id = rc.relay_id
WHERE rc.relay_id = '<R1_ID>' ORDER BY rc.issued_at;

-- after revoke: must be 0
SELECT count(*) FROM relay_certificates WHERE relay_id = '<R1_ID>' AND revoked_at IS NULL;

-- what /relay.crl carries (same predicate as internal/relay/store.go ListRevokedRelaySerials)
SELECT serial, revoked_at FROM relay_certificates
WHERE revoked_at IS NOT NULL AND not_after > NOW() ORDER BY revoked_at;

-- connectors (the workspace column is tenant_id)
SELECT id, name, status, revoked_at, cert_serial, cert_not_after,
       last_heartbeat_at, now()-last_heartbeat_at AS hb_age
FROM connectors WHERE id = '<conn_id>';

-- shields (no revoked_at column)
SELECT id, name, status, lan_ip, cert_serial, cert_not_after, last_heartbeat_at
FROM shields WHERE id = '<shield_id>';
```

### 15.4 Force the relay path (block direct QUIC)

```bash
sudo nft add table inet s20test
sudo nft add chain inet s20test out '{ type filter hook output priority 0; }'
sudo nft add rule inet s20test out udp dport 9092 drop
# ... test the relayed tunnel ...
sudo nft delete table inet s20test
sudo systemctl restart zecurity-client     # clear the direct-path cooldown (KI-5)
```

### 15.5 Valkey keys

| Key | Meaning |
|---|---|
| `relay:heartbeat:last:<relay_id>` | Last heartbeat time (unix seconds). The sweep refreshes the DB timestamp from this instead of evicting while it is fresh. |
| `relay:heartbeat:db-write:<relay_id>` | 5-minute Postgres write throttle marker. Cleared on eviction. |
| `relay:heartbeat:metadata:<relay_id>` | Last reported metadata. Cleared on eviction. |
| `relay:provisioning:jti:<jti>` | Unused provisioning token (24 h TTL). Deleted with `GETDEL` only after a successful provision. |
| `relay:renew:result:<relay_id>:<serial>` / `relay:renew:lock:<relay_id>:<serial>` | `RenewCert` idempotency cache (1 h) and lock (30 s). |

### 15.6 Log lines by component

**Controller**

| Event | Log line |
|---|---|
| Relay eviction | `relay expiry: evicted relay %s` |
| Relay renewed | `relay renew: relay=%s old_serial=%s new_serial=%s not_after=%s superseded=%d` |
| Relay renewal, cached reply | `relay renew: relay=%s presented=%s returned cached renewal (idempotent retry)` |
| SAN rejection | `relay provision: relay=%s requested IP SAN %s not in allowlist` (DNS: `requested DNS SAN %q not in allowlist`) |
| Connector ReEnroll | `control stream: connector %s cert expires %s (inside renewal window %s) — ReEnroll sent` |
| Rotation failure (success is silent) | `controller cert rotation failed: %v` |
| Connector disconnect watcher | `disconnect watcher: marked connector(s) disconnected workspaces=%d` |
| Relay heartbeat (every beat) | `relay heartbeat: received from relay=%s ... cert_serial=%s cert_not_after=%s` |

**Connector**
- Renewal: `controller requested cert renewal — starting renewal` → `starting certificate renewal` → `certificate renewed, persisted and published` → `cert renewed successfully, new expiry: …`
- Duplicate ReEnroll: `certificate renewal already in progress — ignoring duplicate ReEnroll` / `certificate renewed moments ago — ignoring duplicate ReEnroll`
- Consumers switching to the new certificate: `device tunnel (QUIC) switched to renewed certificate`, `Shield-proxy controller channel rebuilt with renewed certificate`
- Relay: `Connector registered with Relay`, `Relay session failed` (`error=relay certificate revoked`), `initial Relay CRL fetch failed; relay dialing is fail-closed: …`
- Shield proxy: `proxying shield cert renewal to controller` / `shield cert renewal proxy failed`

**Relay**
- `next Relay certificate renewal scheduled` (`delay_secs`)
- `Relay certificate renewed and installed`
- `Relay listener switched to renewed certificate`
- `Relay certificate renewed; reconnecting heartbeat with the new identity`
- `Relay heartbeat acknowledged`
- An expired certificate: `Relay certificate has expired; RenewCert is not attempted. This relay must be replaced (D-20)`

**Shield**
- `connector requested cert renewal` → `starting certificate renewal` → `cert renewed` (`new_expiry`, `renewed_via`) → `cert renewed — reconnecting Control stream`

**Client**
- `direct path failed; used relay fallback`
- `transport snapshot stored` (`version`)
- `background sync: version changed, restarting tunnel`

---

## 16. Troubleshooting

| Symptom | Likely cause | Action |
|---|---|---|
| No provider token; the callback goes to a tenant login | KI-3 redirect URI | §3.1 step 1 |
| `403 not_a_provider_user` | Email not bootstrapped | Set `PROVIDER_BOOTSTRAP_EMAILS`, restart the controller |
| Relay provisioning fails with `FailedPrecondition` | `RELAY_ID` doesn't match the created relay, or the row isn't `pending` | Export `RELAY_ID` from the POST response (KI-6); create a new relay if the row was already provisioned |
| Relay renews back to back, `delay_secs=0` | Short `RELAY_CERT_TTL` (KI-1) | Restart the controller with `RELAY_CERT_TTL=1h`, then create and provision a **new** relay |
| Connector never gets ReEnroll | Connector enrolled before the controller got the short TTL (7-day certificate) | Enroll a fresh connector (§7.2) |
| Relay marked `inactive` while running | A Phase A regression, or the relay really missed heartbeats | Capture the controller log, `relay:heartbeat:last:*` values and relay logs. **This is a failure; record it.** |
| `initial Relay CRL fetch failed` on the connector | Controller HTTP address unreachable, or a CRL issuer mismatch | Check `curl http://localhost:8080/relay.crl`. Relay dialing is fail-closed until it recovers. |
| Relayed tunnel never used | The direct path still works, or the snapshot has no relay fields | Confirm the nft rule; the snapshot needs `relay_addr` / `relay_spiffe_id` |
| Client stuck on the relay after unblocking | Direct-path cooldown (KI-5) | `sudo systemctl restart zecurity-client` |
| Snapshot version doesn't change in Stage 6 | The controller was restarted (in-memory version), or the watcher hasn't run yet | Wait 120 s; don't restart the controller mid-run |
