# Certificate Audit Fixes — Live Test Runbook

**For:** Sathiya (M1). **Code:** `fixed-pendings` at `61ec3ac` or later (bugs 1–10 merged through PR #130).
**Goal:** confirm on a real stack (controller, relay, connector, shield, client) that the certificate audit fixes hold, and record the results in the run sheet at the end (§14).

Everything so far was verified with local automated tests only. This run is the first live check of bugs 1–10 and of F33 (bug 3).

## Contents

1. What this run proves (and what it doesn't)
2. Prerequisites
3. Setup
4. Helper commands
5. Stage A — bring-up baseline
6. Stage B — bug 1: tampered `/ca.crt` is refused
7. Stage C — bugs 9 and 10 (part 1): renewals, history, same key
8. Stage D — bug 10 (part 2): renewal with a different key is refused
9. Stage E — bug 6: swapped CRLs are refused
10. Stage F — bug 8: re-issued enrollment tokens
11. Stage G — bugs 4 and 5: an inactive user loses device access
12. Stage H — bug 3 / F33: revoking a shield mid-session
13. Stage I — bugs 7 and 9 (part 2): revoking and deleting a connector
14. Run sheet

---

## 1. What this run proves (and what it doesn't)

| Stage | Bug | What you check live |
|---|---|---|
| B | 1 | Connector, shield and client refuse a `/ca.crt` with an extra certificate (MITM) and write nothing |
| C | 9, 10 | Every connector renewal is recorded in history; connector and shield keep the same key across renewals; the shield's key is pinned |
| D | 10 | A renewal CSR for a different key is refused for connector and shield |
| E | 6 | A connector given swapped CRLs (relay CRL as `/ca.crl` and vice versa) rejects them and fails closed |
| F | 8 | An older enrollment token stops working as soon as a newer one is issued; a used token can't be reused |
| G | 4, 5 | A suspended user's device loses access, and the user can't get a new device certificate |
| H | 3 (F33) | Revoking a shield ends its sessions, refuses its reconnect and makes its resources unreachable |
| I | 7, 9 | A revoked connector can't renew or call Goodbye; every certificate it ever held is on the CRL, even after it's deleted |

**Not covered live:**
- **Bug 2** (relay picking a CRL from a forged issuer copy): needs a hand-forged CA certificate; covered by the relay's automated tests (`b7bf4a6`, `d80dedb`). Skip.
- **Bug 9 for client devices across renewals:** the client certificate lives 7 days and there's no command to force a renewal. Stage G checks a device's single serial; multi-serial device history is covered by automated tests.
- **Bug 4's "token still valid after suspension" window:** hard to hit by hand; covered by automated tests. Stage G checks the observable result.

**Known open issues — record what you see, but they are not failures of this run:**
- **F34 (High): a revoked connector keeps serving tunnels.** Connector-side enforcement is only at Phase A. In Stage I, write down whether existing tunnels through the revoked connector keep working.
- **F32 (Low):** a revoked relay keeps reconnecting its heartbeat.
- The shield itself checks no revocation (only the connector does).

---

## 2. Prerequisites

### 2.1 Pull and reset the database (required)

Bugs 9 and 10 added migrations `039_certificate_history.sql` and `040_shield_public_key_fingerprint.sql`. Migrations only run when the Postgres volume is created, so **you must reset**:

```bash
git checkout fixed-pendings && git pull
git log --oneline -1                       # 61ec3ac or later
cd controller && docker compose down -v && docker compose up -d && cd ..
```

This deletes all local data. Afterwards:
- Re-create the workspace and admin user (tenant sign-in) and the remote network you test with.
- Set `PROVIDER_BOOTSTRAP_EMAIL` and `PROVIDER_BOOTSTRAP_PASSWORD` in `controller/.env` so the controller seeds the provider super-admin on start (needed to create the relay).
- Everything (relay, connector, shield, client) enrolls fresh.

If a shield or connector install from before the reset is still running on a lab machine, stop it; its identity no longer exists.

### 2.2 Lab machines

The commands use `localhost`, as if everything runs on the controller's machine. On the lab hosts (e.g. connector on inkyank, shield on nika, as in the 2026-10-20 AT-K.2 run), use the controller's LAN address for `CONTROLLER_ADDR`, `CONTROLLER_HTTP_ADDR` and the proxy (Stages B and E run the proxy on the controller's machine and point the agent at `<controller-ip>:18080` / `:18081`). The LAN addresses change (DHCP), so check them first. Disable the update timers on the lab hosts so they don't replace the test binaries mid-run.

### 2.3 Tools

`psql`, `openssl` 3.x, `curl`, `jq`, `python3`, `grpcurl`, `nft`, and sudo for the shield.

### 2.4 Build

```bash
buf generate                                # add --exclude-path .worktrees (and any other checkout under the repo) or it fails on duplicate files
(cd controller && go build ./...)
cargo build --manifest-path connector/Cargo.toml
cargo build --manifest-path relay/Cargo.toml
cargo build --manifest-path shield/Cargo.toml
(cd client && ./scripts/dev-build.sh)      # dev client with this controller's root embedded (bug 1)
```

### 2.5 Free the ports

```bash
sudo systemctl stop zecurity-connector zecurity-shield zecurity-relay 2>/dev/null || true
ss -ltnup | grep -E ':(8080|9090|9091|9092|9093|18080|18081)\b'   # nothing should be listening yet
```

| Port | Used by |
|---|---|
| 8080 / 9090 | controller HTTP / gRPC |
| 9091 | connector, shield-facing |
| 9092 | connector device tunnel (TLS + QUIC) |
| 9093 | relay |
| 18080 / 18081 | the test proxy (Stages B and E) |

### 2.6 Run directory

```bash
export RUN=/tmp/cert-live
rm -rf "$RUN" && mkdir -p "$RUN"/{logs,connector,shield,evil-conn,evil-shield,tok,certs}
export DB='postgres://ztna:ztna_dev_secret@localhost:5432/ztna_platform?sslmode=disable'
```

---

## 3. Setup

### 3.1 Controller with short certificate lifetimes

Short lifetimes make renewals happen during the run. Exported variables override `controller/.env`.

```bash
cd controller
export CONNECTOR_CERT_TTL=15m CONNECTOR_RENEWAL_WINDOW=10m   # connector renews ~5 min after each issue
export SHIELD_CERT_TTL=15m SHIELD_RENEWAL_WINDOW=10m         # shield renews ~9 min after each issue (window clamps to 6 min)
export RELAY_CERT_TTL=1h
go run ./cmd/server 2>&1 | tee "$RUN/logs/controller.log"
```

Expect `gRPC server listening on :9090` and `listening on :8080`. Leave it running.

### 3.2 Provider token (to create the relay)

```bash
curl -s -X POST http://localhost:8080/provider/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"<bootstrap email>","password":"<bootstrap password>"}' | tee "$RUN/plogin.json" | jq .
```

The first login after a reset returns `password_change_required: true` and a password-change token. Change it:

```bash
curl -s -X POST http://localhost:8080/provider/auth/password -H "Authorization: Bearer $(jq -r .token "$RUN/plogin.json")" \
  -H 'Content-Type: application/json' -d '{"current_password":"<bootstrap password>","new_password":"<new password>"}' | jq .
```

Since Sprint 22 (Phase T) a super-admin must use TOTP. The password change (and every later login) returns an MFA-stage token with `"mfa": "enroll"` (first time) or `"mfa": "verify"`; only the token from the TOTP step can call `/provider/relays`.

```bash
MFA_TOKEN=$(curl -s -X POST http://localhost:8080/provider/auth/login -H 'Content-Type: application/json' \
  -d '{"email":"<bootstrap email>","password":"<new password>"}' | tee "$RUN/plogin.json" | jq -r .token)
jq -r .mfa "$RUN/plogin.json"     # enroll | verify
# mfa = enroll: get a secret, add it to an authenticator app (TOTP, SHA1, 6 digits, 30 s), then confirm with a code
curl -s -X POST http://localhost:8080/provider/auth/totp/enroll -H "Authorization: Bearer $MFA_TOKEN" | jq -r .otpauth_uri
export PTOKEN=$(curl -s -X POST http://localhost:8080/provider/auth/totp/enroll/confirm -H "Authorization: Bearer $MFA_TOKEN" \
  -H 'Content-Type: application/json' -d '{"code":"<6-digit code>"}' | jq -r .token)
# mfa = verify (already enrolled):
export PTOKEN=$(curl -s -X POST http://localhost:8080/provider/auth/totp/verify -H "Authorization: Bearer $MFA_TOKEN" \
  -H 'Content-Type: application/json' -d '{"code":"<6-digit code>"}' | jq -r .token)
```

Lost the password or the TOTP device? On the controller host: `zecurity-controller provider recover-admin --email <email>` prints a one-time temporary password, clears TOTP and ends sessions.

### 3.3 Workspace ID

```bash
export WS=$(psql "$DB" -Atc "SELECT id FROM workspaces WHERE slug = '<your workspace slug>'")
export TD=$(psql "$DB" -Atc "SELECT trust_domain FROM workspaces WHERE id = '$WS'")
echo "$WS $TD"
```

### 3.4 The test proxy

Save this as `$RUN/proxy.py`. It forwards to the real controller HTTP port and tampers with exactly one thing:
- `append-ca`: adds an extra certificate to `/ca.crt` (Stage B);
- `swap-crl`: serves the relay CRL for `/ca.crl` and the workspace CRL for `/relay.crl` (Stage E).

```python
#!/usr/bin/env python3
"""Test-only proxy: python3 proxy.py <append-ca|swap-crl> <port> <workspace-id> [extra.pem]"""
import http.server, sys, urllib.request

MODE, PORT, WS = sys.argv[1], int(sys.argv[2]), sys.argv[3]
EXTRA = open(sys.argv[4], "rb").read() if MODE == "append-ca" else b""
UP = "http://localhost:8080"

class Proxy(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        path = self.path
        if MODE == "swap-crl":
            if path.startswith("/ca.crl"):
                path = "/relay.crl"
            elif path.startswith("/relay.crl"):
                path = "/ca.crl?workspace_id=" + WS
        with urllib.request.urlopen(UP + path) as r:
            body, ctype = r.read(), r.headers.get("Content-Type", "application/octet-stream")
        if MODE == "append-ca" and path.startswith("/ca.crt"):
            body = body.rstrip(b"\n") + b"\n" + EXTRA
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
        print(f"{MODE}: GET {self.path} -> {path}", flush=True)

http.server.ThreadingHTTPServer(("0.0.0.0", PORT), Proxy).serve_forever()
```

The "evil" certificate used by `append-ca`:

```bash
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 1 \
  -subj /CN=evil-ca -keyout "$RUN/evil.key" -out "$RUN/evil.pem"
```

---

## 4. Helper commands

```bash
# Serial and public-key fingerprint of a certificate file (first certificate in the file)
certinfo() { openssl x509 -in "$1" -noout -serial -enddate; \
  openssl x509 -in "$1" -pubkey -noout | openssl pkey -pubin -outform DER | sha256sum | cut -c1-64; }

# Serials on the workspace CRL / the relay CRL (lowercase hex, as stored in the database)
wscrl()    { curl -s "http://localhost:8080/ca.crl?workspace_id=$WS" | openssl crl -inform DER -noout -text \
             | awk '/Serial Number:/{print tolower($3)}'; }
relaycrl() { curl -s http://localhost:8080/relay.crl | openssl crl -inform DER -noout -text \
             | awk '/Serial Number:/{print tolower($3)}'; }

# Certificate history of a connector (bug 9)
connhist() { psql "$DB" -c "SELECT serial, not_after, revoked_at FROM connector_certificates WHERE connector_id = '$1' ORDER BY issued_at"; }

# A renewal CSR (DER, base64) for a SPIFFE ID, signed by a given key
csr_b64() { openssl req -new -key "$1" -subj "/CN=renew-test" -addext "subjectAltName=URI:$2" -outform DER | base64 -w0; }

# grpcurl as the connector (mTLS with its own certificate); -insecure skips only the server-name check
asconn() { grpcurl -insecure -cert "$RUN/connector/connector.crt" -key "$RUN/connector/connector.key" \
             -import-path . "$@"; }
# Calls pass -proto proto/connector/v1/connector.proto (or proto/shield/v1/shield.proto): connector.proto
# imports proto/client/v1/client.proto, so the import path must be the repo root.
```

> `openssl crl` prints serials without leading zeros; the database stores `SerialNumber.Text(16)`, also without them, so they compare directly.

---

## 5. Stage A — bring-up baseline

### 5.1 Relay

```bash
curl -s -X POST http://localhost:8080/provider/relays -H "Authorization: Bearer $PTOKEN" -H 'Content-Type: application/json' \
  -d '{"name":"cert-live-relay","dns_allowlist":[],"ip_allowlist":["127.0.0.1"]}' | tee "$RUN/relay.json"
RELAY_ID=$(jq -r .relay_id "$RUN/relay.json") RELAY_PROVISIONING_TOKEN=$(jq -r .provisioning_token "$RUN/relay.json") \
  LOG_LEVEL=info scripts/run-relay-local.sh cert-live-relay 9093 2>&1 | tee "$RUN/logs/relay.log"
```

**Expect:** `Relay provisioned`, `Relay heartbeat acknowledged`.

> **Relay on a LAN address** (connector on another machine): start the controller with `RELAY_DEV_ALLOW_PRIVATE_ADDR=true` (honoured only with `ENV=development`), put the relay's LAN IP in `ip_allowlist` / `RELAY_IP_SANS`, and point the relay at the controller's LAN address (`CONTROLLER_ADDR=<lan-ip>:9090`, `CONTROLLER_HTTP_ADDR=<lan-ip>:8080`). Otherwise the relay is never published and connectors log `LabelledRelayList is empty`.

### 5.2 Connector (genuine enrollment)

Admin UI → Remote Network → add connector → copy the token.

```bash
CONTROLLER_ADDR=localhost:9090 CONTROLLER_HTTP_ADDR=localhost:8080 ENROLLMENT_TOKEN='<jwt>' \
  STATE_DIR="$RUN/connector" AUTO_UPDATE_ENABLED=false LOG_LEVEL=info \
  connector/target/debug/zecurity-connector 2>&1 | tee "$RUN/logs/connector.log"
export CONN=$(psql "$DB" -Atc "SELECT id FROM connectors WHERE tenant_id = '$WS' AND status = 'active' ORDER BY created_at DESC LIMIT 1")
```

**Expect:** `enrollment complete`, `Shield gRPC server starting on :9091`, `Connector registered with Relay`, and **no** `initial CRL fetch failed` or `CA bundle does not yield the CRL issuers`.

### 5.3 Shield (genuine enrollment)

Admin UI → add shield → copy the token.

```bash
sudo env CONTROLLER_ADDR=localhost:9090 CONTROLLER_HTTP_ADDR=localhost:8080 ENROLLMENT_TOKEN='<jwt>' \
  STATE_DIR="$RUN/shield" AUTO_UPDATE_ENABLED=false LOG_LEVEL=info \
  shield/target/debug/zecurity-shield 2>&1 | tee "$RUN/logs/shield.log"
export SHIELD=$(psql "$DB" -Atc "SELECT id FROM shields WHERE tenant_id = '$WS' AND status = 'active' ORDER BY created_at DESC LIMIT 1")
```

Protect a resource on the shield's host (admin UI) so Stage H has something to cut.

### 5.4 Client

```bash
zecurity-client setup --workspace <slug> --controller localhost:9090 --http-base http://localhost:8080
zecurity-client login && zecurity-client up && zecurity-client resources
```

Open a long-lived session to the **shield-protected** resource (e.g. `ssh <user>@<resource-ip>`) and keep it open for Stage H.

**Record (run sheet A):** connector ID, shield ID, `certinfo $RUN/connector/connector.crt` (serial `c0`, key fingerprint `kC`), `sudo cat $RUN/shield/shield.crt > $RUN/certs/shield-0.pem; certinfo $RUN/certs/shield-0.pem` (serial `s0`, key fingerprint `kS`).

---

## 6. Stage B — bug 1: tampered `/ca.crt` is refused

```bash
python3 "$RUN/proxy.py" append-ca 18080 "$WS" "$RUN/evil.pem" 2>&1 | tee "$RUN/logs/proxy-ca.log" &
```

### 6.1 Connector

Admin UI → add a **second** connector → copy its token. Enroll it **through the proxy**:

```bash
CONTROLLER_ADDR=localhost:9090 CONTROLLER_HTTP_ADDR=localhost:18080 ENROLLMENT_TOKEN='<jwt>' \
  STATE_DIR="$RUN/evil-conn" AUTO_UPDATE_ENABLED=false LOG_LEVEL=info \
  connector/target/debug/zecurity-connector 2>&1 | tee "$RUN/logs/evil-conn.log"
ls -la "$RUN/evil-conn"
```

**Expect:** enrollment aborts with `/ca.crt must contain exactly one certificate (the Platform Intermediate), got 2 — possible MITM, aborting`; the proxy log shows `GET /ca.crt`; `$RUN/evil-conn` contains **no** `connector.key`, `connector.crt` or `state.json`; the second connector stays `pending` in the admin UI.

### 6.2 Shield

Same with a second shield token: `sudo env … CONTROLLER_HTTP_ADDR=localhost:18080 … STATE_DIR="$RUN/evil-shield" shield/target/debug/zecurity-shield`.

**Expect:** aborts with the same `/ca.crt must contain exactly one certificate … possible MITM` error, nothing written to `$RUN/evil-shield`, the shield stays `pending`.

### 6.3 Client

```bash
zecurity-client logout
zecurity-client setup --workspace <slug> --controller localhost:9090 --http-base http://localhost:18080
zecurity-client login
```

**Expect:** login refuses: `/ca.crt must contain exactly one certificate … possible MITM`. Then restore it:
`zecurity-client setup … --http-base http://localhost:8080 && zecurity-client login && zecurity-client up`, and reopen the Stage H session.

Stop the proxy (`kill %1`). Delete the two pending objects in the admin UI.

---

## 7. Stage C — bugs 9 and 10 (part 1): renewals, history, same key

Wait about **12 minutes** after the connector enrolled; it renews about 5 minutes after each certificate is issued. The shield renews about 9 minutes after its enrollment.

```bash
grep -E "cert renewed|renewal" "$RUN/logs/connector.log" | tail
certinfo "$RUN/connector/connector.crt"                 # serial changes, fingerprint == kC
sudo cat "$RUN/shield/shield.crt" > "$RUN/certs/shield-1.pem"; certinfo "$RUN/certs/shield-1.pem"   # fingerprint == kS
connhist "$CONN"
psql "$DB" -Atc "SELECT public_key_fingerprint FROM shields WHERE id = '$SHIELD'"   # == kS
```

**Expect:**
- the connector renewed at least twice: `connhist` shows **3 rows** (enrollment + 2 renewals), all with `revoked_at` empty;
- the connector's key fingerprint is the same as `kC` after every renewal (bug 10: it reuses its key);
- the shield renewed and its key fingerprint is still `kS`, and `shields.public_key_fingerprint` equals `kS` (bug 10 pin);
- `wscrl` lists none of these serials yet (nothing is revoked).

**Record (run sheet C):** the three connector serials `c0`, `c1`, `c2` (from `connhist`).

---

## 8. Stage D — bug 10 (part 2): renewal with a different key is refused

```bash
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-384 -out "$RUN/attacker.key"
```

### 8.1 Connector

```bash
asconn -proto proto/connector/v1/connector.proto \
  -d "{\"public_key_der\":\"$(csr_b64 "$RUN/attacker.key" "spiffe://$TD/connector/$CONN")\"}" \
  localhost:9090 connector.v1.ConnectorService/RenewCert
```

**Expect:** `PermissionDenied` — `renewal key does not match the presented certificate`. `connhist "$CONN"` has **no new row**.

### 8.2 Shield (through the connector's identity, as the connector proxies it)

```bash
asconn -proto proto/shield/v1/shield.proto \
  -d "{\"shield_id\":\"$SHIELD\",\"public_key_der\":\"$(csr_b64 "$RUN/attacker.key" "spiffe://$TD/shield/$SHIELD")\"}" \
  localhost:9090 shield.v1.ShieldService/RenewCert
```

**Expect:** `PermissionDenied` — `renewal key does not match the shield's enrolled key`. The shield's `cert_serial` in the database is unchanged.

---

## 9. Stage E — bug 6: swapped CRLs are refused

```bash
python3 "$RUN/proxy.py" swap-crl 18081 "$WS" 2>&1 | tee "$RUN/logs/proxy-crl.log" &
```

Stop the connector (Ctrl-C) and start it again from the same state directory, fetching its CRLs **through the swapping proxy**:

```bash
CONTROLLER_ADDR=localhost:9090 CONTROLLER_HTTP_ADDR=localhost:18081 \
  STATE_DIR="$RUN/connector" AUTO_UPDATE_ENABLED=false LOG_LEVEL=info \
  connector/target/debug/zecurity-connector 2>&1 | tee "$RUN/logs/connector-swapped.log"
```

**Expect:**
- the proxy log shows `GET /ca.crl… -> /relay.crl` and `GET /relay.crl -> /ca.crl…`;
- the connector logs `initial CRL fetch failed` and `initial Relay CRL fetch failed` with an issuer error (`crl issuer does not match expected ca subject`), and keeps failing on refresh;
- **fail closed:** the client's resources behind this connector are refused (`certificate revocation state unavailable` in the connector log), and the connector does not dial the relay (`relay revocation state unavailable` / relay dialing fail-closed).

Then restart the connector with `CONTROLLER_HTTP_ADDR=localhost:8080`. **Expect:** no CRL errors; resources work again. Stop the proxy.

---

## 10. Stage F — bug 8: re-issued enrollment tokens

### 10.1 Connector

1. Admin UI → add connector `tok-conn` → copy token **A**.
2. Reload the connector's detail page → copy token **B** (a new token is minted).
3. Enroll with **A**:
   ```bash
   CONTROLLER_ADDR=localhost:9090 CONTROLLER_HTTP_ADDR=localhost:8080 ENROLLMENT_TOKEN='<A>' \
     STATE_DIR="$RUN/tok/a" AUTO_UPDATE_ENABLED=false connector/target/debug/zecurity-connector 2>&1 | tee "$RUN/logs/tok-a.log"
   ```
   **Expect:** refused — `token expired or already used` (re-issuing B drops A's jti from Valkey, so A fails the single-use check before the current-token check); nothing written.
4. Enroll with **B** (`STATE_DIR="$RUN/tok/b"`). **Expect:** enrolls; stop it with Ctrl-C afterwards (it would clash on :9091).
5. Enroll with **B** again in a fresh directory (`STATE_DIR="$RUN/tok/b2"`). **Expect:** refused (`token expired or already used`).
6. Optional: on a fresh connector `tok-conn-2`, start two enrollments with the **same** token at the same time (two terminals, two state directories). **Expect:** exactly one succeeds.

Delete `tok-conn` and `tok-conn-2` in the admin UI afterwards (revoke first if active).

### 10.2 Shield

Repeat steps 1–5 with a new shield `tok-shield` (sudo, separate state directories). **Expect:** same results, with `token expired or already used` for A.

---

## 11. Stage G — bugs 4 and 5: an inactive user loses device access

There is no admin action to suspend a user yet. Use one of these:
- **If an IdP connection with SCIM users is set up:** Admin UI → disable the IdP connection. Its SCIM users are suspended and the policy is invalidated in one transaction.
- **Otherwise (SQL):**
  ```bash
  psql "$DB" -c "UPDATE users SET status = 'suspended' WHERE email = '<client user email>'"
  ```
  SQL alone does not notify the policy. Make any policy change for the workspace afterwards (e.g. add and remove a member from a group in the admin UI) so the connector gets a recompiled ACL.

**Expect**, in this order:
1. **bug 5:** the client user's open sessions through the connector end, and new connections to resources are refused.
2. Reactivate (`UPDATE users SET status = 'active' …` plus a policy change, or re-enable the IdP connection). Access comes back without re-enrolling, because the device was suspended, not revoked. **Do this before step 3**: `logout` wipes the device's local state, so after it there is nothing left to come back.
3. Suspend again, then check **bug 4:** `zecurity-client logout && zecurity-client login` does not give the user a new device certificate. `logout` also stops the daemon: run `sudo systemctl start zecurity-client` before `login`.

Then **revoke** the client device in the admin UI and check:
```bash
psql "$DB" -Atc "SELECT serial FROM client_device_certificates d JOIN client_devices c ON c.id = d.device_id WHERE c.workspace_id = '$WS' AND d.revoked_at IS NOT NULL"
wscrl
```
**Expect:** the device's serial is on the workspace CRL (bug 9, device side). Re-enroll the client (`zecurity-client login`) before Stage H.

---

## 12. Stage H — bug 3 / F33: revoking a shield mid-session

Have the long-lived session to the shield-protected resource open (Stage A).

1. Note the time, then Admin UI → revoke the shield.
2. Watch `connector.log` and the session.

**Expect:**
- within seconds the connector logs the revocation list applied for that shield, closes its stream, and **the open session ends**;
- `psql "$DB" -c "SELECT * FROM shield_revocations WHERE shield_id = '$SHIELD'"` shows the tombstone;
- restart the shield (Ctrl-C, run again with the same state directory): **it is refused** and cannot reconnect;
- the resource it protected is unreachable from the client (no fallback to direct routing);
- the shield's renewal is refused if it tries (`shield is revoked`).

**Record (run sheet H):** revoke time, session-end time, reconnect result.

---

## 13. Stage I — bugs 7 and 9 (part 2): revoking and deleting a connector

1. Note `connhist "$CONN"` (should now be 3 or more serials). Admin UI → revoke the connector.
   - If it is the network's last connector and a non-revoked shield is still enrolled, the revoke is refused (`… they would be stranded`, Sprint 20 Fix 16, F46). Add another connector, revoke the shield first, or call `revokeConnector(id: …, alsoRevokeShields: true)` in GraphQL.
2. **Bug 9 — every serial is on the CRL:**
   ```bash
   connhist "$CONN"      # all rows now have revoked_at
   wscrl                 # must list every UNEXPIRED serial (the CRL omits expired ones: not_after > NOW())
   ```
   With 15 min certificates, `c0`–`c2` have usually expired by this stage, so they are on no CRL. Check that every row in `connhist` has `revoked_at`, and that every row with `not_after` in the future is on `wscrl`.
3. **Bug 7 — the revoked connector can't use connector RPCs** (it still has a valid certificate):
   ```bash
   asconn -proto proto/connector/v1/connector.proto \
     -d "{\"public_key_der\":\"$(openssl req -new -key "$RUN/connector/connector.key" -subj /CN=renew-test \
          -addext "subjectAltName=URI:spiffe://$TD/connector/$CONN" -outform DER | base64 -w0)\"}" \
     localhost:9090 connector.v1.ConnectorService/RenewCert
   asconn -proto proto/connector/v1/connector.proto -d '{}' localhost:9090 connector.v1.ConnectorService/Goodbye
   psql "$DB" -Atc "SELECT status, revoked_at IS NOT NULL FROM connectors WHERE id = '$CONN'"
   ```
   **Expect:** both calls fail with `PermissionDenied` (`connector revoked`); the row stays `revoked`. In `connector.log`, any shield renewal it tries to proxy fails (`shield cert renewal proxy failed`).
4. **F34 (known open, record only):** do client sessions through this connector keep working? Write down what you see.
5. **Bug 9 — deleting doesn't drop it from the CRL:** Admin UI → delete the connector, then:
   - The delete is refused while a shield is still attached (`… shield(s) still attached`, F19). Let the shield fail over to another connector, or revoke it first.
   ```bash
   psql "$DB" -Atc "SELECT count(*) FROM connectors WHERE id = '$CONN'"   # 0
   wscrl                                                                    # still lists the unexpired serials
   ```

---

## 14. Run sheet

Fill in one row per check: **PASS / FAIL / BLOCKED**, the clock time, and where the evidence is (log file and line, screenshot, command output). For a FAIL, paste the exact error.

**Run info:** date ______ · tester ______ · commit (`git log --oneline -1`) ______ · machines ______

| ID | Bug | Check | Result | Time | Evidence / notes |
|---|---|---|---|---|---|
| A1 | — | DB reset done; relay, connector, shield, client up; no CRL errors at connector start | | | |
| A2 | — | Recorded: connector `c0` + key `kC`; shield `s0` + key `kS` | | | |
| B1 | 1 | Connector via tampered `/ca.crt`: aborts, nothing written, stays pending | | | |
| B2 | 1 | Shield via tampered `/ca.crt`: aborts, nothing written, stays pending | | | |
| B3 | 1 | Client login via tampered `/ca.crt`: refused (MITM message) | | | |
| C1 | 9 | Connector renewed ≥ 2 times; history has 3 rows, none revoked | | | |
| C2 | 10 | Connector key fingerprint unchanged across renewals (= `kC`) | | | |
| C3 | 10 | Shield renewed; key unchanged (= `kS`); DB pin = `kS` | | | |
| D1 | 10 | Connector RenewCert with a different key: PermissionDenied, no new history row | | | |
| D2 | 10 | Shield RenewCert with a different key: PermissionDenied, serial unchanged | | | |
| E1 | 6 | Swapped CRLs: connector rejects both (issuer error) | | | |
| E2 | 6 | Fail closed: resources refused, relay not dialed while CRLs are rejected | | | |
| E3 | 6 | Back on the real CRLs: recovers | | | |
| F1 | 8 | Connector: older token A refused after B was issued | | | |
| F2 | 8 | Connector: B enrolls; B reused → refused | | | |
| F3 | 8 | (optional) Two simultaneous enrollments with one token → exactly one succeeds | | | |
| F4 | 8 | Shield: A refused, B enrolls, B reused → refused | | | |
| G1 | 5 | Suspended user: sessions end, new connections refused | | | |
| G2 | 4 | Suspended user: no new device certificate | | | |
| G3 | 4/5 | Reactivated: access returns without re-enrolling | | | |
| G4 | 9 | Revoked device: its serial is on the workspace CRL | | | |
| H1 | 3 | Shield revoked: open session ends (record seconds) | | | |
| H2 | 3 | Revoked shield can't reconnect; resource unreachable; tombstone present | | | |
| I1 | 9 | Revoked connector: all its serials (`c0`, `c1`, `c2`, …) on the CRL | | | |
| I2 | 7 | Revoked connector: RenewCert and Goodbye → PermissionDenied; row stays revoked | | | |
| I3 | F34 | (record only) Do sessions through the revoked connector keep working? | | | |
| I4 | 9 | Connector deleted: serials still on the CRL | | | |

**Clean-up after the run:** stop the relay, connector, shield and proxies; `zecurity-client down`; delete the test objects in the admin UI; `rm -rf /tmp/cert-live` if you don't need the logs. Send the filled-in run sheet and the `logs/` directory to Barath.
