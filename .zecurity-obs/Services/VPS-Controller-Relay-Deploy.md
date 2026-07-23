---
type: runbook
scope: deployment
status: draft
date: 2026-07-06
branch: integration/relay-merge
tags:
  - deployment
  - controller
  - relay
  - vps
  - tls
---

# VPS Deploy — Controller + Relay (domain: zecurity.in)

> Step-by-step deploy of the **controller** and **relay** on a single Hostinger VPS,
> using the domain **zecurity.in** (which is also the platform SPIFFE trust domain —
> `appmeta/identity.go:28`, `relay/src/appmeta.rs:1`). Companion:
> [Relay-WAN-Test-Plan](Relay-WAN-Test-Plan.md) (the end-to-end test this enables).

## Placeholders used below
- `<VPS_IP>` — the VPS public IPv4.
- `zecurity.in` — your domain, DNS A-record → `<VPS_IP>`.
- `<ADMIN_EMAIL>` — an email for Let's Encrypt/Caddy.

---

## 0. Exposure model (read once)

Three listeners, three different exposure rules — getting this wrong is the #1 failure:

| Listener | Port | Exposure | Notes |
|---|---|---|---|
| Controller gRPC | 9090/tcp | **Direct** (firewall open) | mTLS+SPIFFE; controller mints its own cert (SAN from `CONTROLLER_ADDR`); peers trust the platform CA, not public roots. **Never behind a TLS proxy.** |
| Relay QUIC | 9093/udp | **Direct** (firewall open) | UDP/QUIC, mTLS. |
| Controller HTTP | 8080/tcp | **Bind localhost**, front with Caddy on 443 | Plaintext; Caddy adds public HTTPS for OAuth + admin + `/ca.crt`. |

The relay is on the same VPS and fetches `/ca.crt` over **plaintext `http://127.0.0.1:8080`**
(hardcoded `http://`, fingerprint-pinned — `relay/src/provision.rs:98,129`), so it does
**not** use Caddy.

---

## 1. DNS

Create an **A record**: `zecurity.in` → `<VPS_IP>` (and `www` if you like). Verify:

```bash
dig +short zecurity.in    # → <VPS_IP>
```

Wait for propagation before requesting the Caddy cert (step 5), or the ACME challenge fails.

---

## 2. Firewall (ufw)

```bash
sudo ufw allow 22/tcp        # SSH
sudo ufw allow 80/tcp        # ACME HTTP-01 challenge (Caddy)
sudo ufw allow 443/tcp       # Caddy HTTPS (controller HTTP endpoints)
sudo ufw allow 9090/tcp      # controller gRPC (direct)
sudo ufw allow 9093/udp      # relay QUIC (direct)
sudo ufw enable
sudo ufw status verbose
```

**Do not** open 8080 — Caddy reaches it on localhost.

---

## 3. Backing stores (Postgres + Valkey)

Migrations auto-apply on first Postgres init (they're mounted into the container —
`controller/docker-compose.yml:13`).

```bash
sudo apt-get update && sudo apt-get install -y docker.io docker-compose-plugin
cd ~/zecurity/controller
sudo docker compose up -d
sudo docker compose ps        # postgres + valkey healthy
```

Default dev credentials (from the compose file): user `ztna`, password `ztna_dev_secret`,
db `ztna_platform`. **Change the password for anything but a throwaway test.**

---

## 4. Controller

### 4.1 Build

```bash
cd ~/zecurity/controller
go build -o /usr/local/bin/zecurity-controller ./cmd/server
```

### 4.2 Google OAuth (needed for client + admin login)

In Google Cloud Console create **OAuth 2.0 Web application** credentials with authorized
redirect URI:

```
https://zecurity.in/auth/callback
```

The controller needs **two** client pairs (main.go:85-86, 321-322) — a web pair and a CLI
pair. You may reuse one Google client for both in a test.

### 4.3 Environment file

`sudo mkdir -p /etc/zecurity && sudo tee /etc/zecurity/controller.env` with:

```ini
# --- backing stores ---
DATABASE_URL=postgres://ztna:ztna_dev_secret@localhost:5432/ztna_platform
VALKEY_URL=redis://localhost:6379

# --- ports ---
GRPC_PORT=9090
PORT=8080                       # HTTP; localhost-only via Caddy

# --- serving-cert SANs (must include the host peers dial for gRPC) ---
CONTROLLER_ADDR=zecurity.in:9090
CONTROLLER_HTTP_ADDR=zecurity.in:443

# --- client (CLI) OAuth service wiring ---
CONTROLLER_HOST=zecurity.in:9090
CONTROLLER_HTTP_URL=https://zecurity.in

# --- secrets / OAuth ---
JWT_SECRET=<generate: openssl rand -hex 32>
GOOGLE_CLIENT_ID=<web oauth client id>
GOOGLE_CLIENT_SECRET=<web oauth client secret>
GOOGLE_REDIRECT_URI=https://zecurity.in/auth/callback
CLIENT_GOOGLE_CLIENT_ID=<cli oauth client id>
CLIENT_GOOGLE_CLIENT_SECRET=<cli oauth client secret>

# --- admin UI origin (for CORS / redirects); set to your admin URL ---
APP_BASE_URL=https://zecurity.in
```

> `CONTROLLER_ADDR=zecurity.in:9090` is what puts `zecurity.in` into the gRPC server
> cert's SAN list (`main.go:702-704`); the client does hostname verification
> (`client/src/grpc.rs:15`) plus a SPIFFE check, so the SAN must match the dialed host.

### 4.4 systemd unit

`sudo tee /etc/systemd/system/zecurity-controller.service`:

```ini
[Unit]
Description=Zecurity Controller
After=network-online.target docker.service
Wants=network-online.target

[Service]
EnvironmentFile=/etc/zecurity/controller.env
ExecStart=/usr/local/bin/zecurity-controller
Restart=on-failure
RestartSec=3
User=root

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now zecurity-controller
journalctl -u zecurity-controller -f
```

**Expect:** `listening on :8080` and the gRPC server up on `:9090`.

---

## 5. Caddy (public HTTPS for the HTTP endpoints)

```bash
sudo apt-get install -y debian-keyring debian-archive-keyring apt-transport-https curl
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/gpg.key' | sudo gpg --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
curl -1sLf 'https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt' | sudo tee /etc/apt/sources.list.d/caddy-stable.list
sudo apt-get update && sudo apt-get install -y caddy
```

`sudo tee /etc/caddy/Caddyfile`:

```caddy
zecurity.in {
    encode gzip
    reverse_proxy 127.0.0.1:8080
}
```

```bash
sudo systemctl restart caddy
# Caddy auto-fetches the Let's Encrypt cert (needs 80+443 open + DNS live)
curl -fsS https://zecurity.in/ca.crt | openssl x509 -noout -subject   # → platform CA subject
```

That last command succeeding proves: DNS + Caddy TLS + controller HTTP are all wired.

---

## 6. Relay (same VPS)

### 6.1 Build

```bash
cargo build --release --manifest-path ~/zecurity/relay/Cargo.toml
```

### 6.2 Provider pre-registers the relay row (required)

```bash
# Get a provider JWT first, then:
curl -sS -H "Authorization: Bearer $PROVIDER_TOKEN" -X POST https://zecurity.in/provider/relays \
  -d '{"name":"relay-vps","dns_allowlist":["zecurity.in"],"ip_allowlist":["<VPS_IP>"]}' | jq .
# note the returned relay_id and provisioning_token
```

Do not skip this step. Authenticated provisioning rejects unregistered relay IDs and requires the
single-use token on the first boot.

### 6.3 Pin the CA fingerprint (from localhost, plaintext)

```bash
RELAY_CA_FINGERPRINT=$(curl -fsS http://127.0.0.1:8080/ca.crt \
    | openssl x509 -outform DER | sha256sum | awk '{print $1}')
echo "$RELAY_CA_FINGERPRINT"
```

### 6.4 Install & run

The decisive detail: **`CONTROLLER_ADDR` points at the public host `zecurity.in`** (so the
controller observes the VPS *public* IP as the relay's peer and advertises it), while
**`CONTROLLER_HTTP_ADDR` uses localhost** (plaintext `/ca.crt`, off the proxy):

```bash
sudo CONTROLLER_ADDR=zecurity.in:9090 \
     CONTROLLER_HTTP_ADDR=127.0.0.1:8080 \
     RELAY_ID=<RELAY_UUID> \
     RELAY_CA_FINGERPRINT=$RELAY_CA_FINGERPRINT \
     RELAY_BIND=0.0.0.0:9093 \
     RELAY_IP_SANS=<VPS_IP> \
     RELAY_DNS_SANS=zecurity.in \
     RELAY_MAX_CONNECTIONS=64 \
     LOG_LEVEL=info \
     ~/zecurity/scripts/relay-local-install.sh ~/zecurity/relay/target/release/zecurity-relay
```

**Expect** in `journalctl -u zecurity-relay -f`:
```
relay provisioning successful cert_serial=... cert_not_after=...
relay heartbeat ok
```

### 6.5 CHECKPOINT — verify the relay is advertised with the PUBLIC IP

This is make-or-break for the WAN path. In the controller Postgres:

```bash
sudo docker exec -it ztna_postgres psql -U ztna -d ztna_platform -c \
 "SELECT id,status,public_addr,address_scope,observed_ip,last_heartbeat_at FROM relays ORDER BY last_heartbeat_at DESC LIMIT 3;"
```

**Expect:** `status=active`, `address_scope=public`, `public_addr=<VPS_IP>:9093`.

**If it shows loopback/private or empty `public_addr`:** the controller saw the wrong
source IP (co-located gotcha). Confirm the relay's `CONTROLLER_ADDR` is `zecurity.in`
(not `localhost`/`127.0.0.1`), and that Hostinger puts the public IP directly on the VPS
interface (not 1:1 NAT). If it persists, run the relay on a separate host. Do not proceed
to the WAN test until `public_addr=<VPS_IP>:9093`.

---

## 7. Smoke test the VPS side

```bash
dig +short zecurity.in                                   # <VPS_IP>
curl -fsS https://zecurity.in/ca.crt | openssl x509 -noout -subject   # platform CA
nc -vz zecurity.in 9090                                  # gRPC reachable
# relays row shows public_addr=<VPS_IP>:9093 (step 6.5)
```

The LAN connector + client bring-up and the actual client→relay→connector WAN validation
continue in [Relay-WAN-Test-Plan](Relay-WAN-Test-Plan.md) §Phase 3 onward. The connector
just needs `CONTROLLER_ADDR=zecurity.in:9090`; the client uses
`zecurity-client setup --workspace <ws> --controller-address zecurity.in`
(and reaches HTTP via `https://zecurity.in`).

---

## 8. Why not nginx / why the split

| Component | Handling | Reason |
|---|---|---|
| gRPC :9090 | Direct, no proxy | mTLS+SPIFFE client-cert auth (`main.go:283-289`) — a TLS-terminating proxy strips the client cert; peers trust the platform CA (from `/ca.crt`), not Let's Encrypt, so a public cert adds nothing; and a proxy rewrites the relay's observed source IP (§6.5). |
| Relay :9093 | Direct, no proxy | UDP/QUIC. |
| HTTP :8080 | Caddy → 443 | OAuth requires an HTTPS redirect URI; controller HTTP is plaintext. Caddy = auto Let's Encrypt + auto-renew (nginx would need certbot + a renewal cron for no benefit). |
| Relay `/ca.crt` | Direct localhost plaintext | Relay fetch is hardcoded `http://` and fingerprint-pinned; never proxied. |

## 9. Teardown

```bash
sudo systemctl disable --now zecurity-relay zecurity-controller caddy
cd ~/zecurity/controller && sudo docker compose down          # add -v to wipe data
sudo ufw delete allow 9090/tcp; sudo ufw delete allow 9093/udp
sudo ufw delete allow 80/tcp;   sudo ufw delete allow 443/tcp
```

---

## Appendix — controller env var reference (from `cmd/server/main.go`)

| Var | Required | Purpose |
|---|---|---|
| `DATABASE_URL` | yes | Postgres DSN (`internal/db/pool.go:18`) |
| `VALKEY_URL` | yes | Valkey/Redis URL |
| `JWT_SECRET` | yes | Signs admin/session JWTs |
| `GOOGLE_CLIENT_ID/SECRET` | yes | Web (admin) OAuth |
| `GOOGLE_REDIRECT_URI` | yes | `https://zecurity.in/auth/callback` |
| `CLIENT_GOOGLE_CLIENT_ID/SECRET` | yes | CLI client OAuth (`main.go:321-322`) |
| `CONTROLLER_HOST` | yes | `zecurity.in:9090` (client OAuth svc, `main.go:323`) |
| `CONTROLLER_HTTP_URL` | yes | `https://zecurity.in` (`main.go:324`) |
| `CONTROLLER_ADDR` | rec. | gRPC serving-cert SAN (`main.go:702`) |
| `CONTROLLER_HTTP_ADDR` | rec. | adds host to cert SAN (`main.go:708`) |
| `GRPC_PORT` / `PORT` | no (9090/8080) | listener ports |
| `APP_BASE_URL` | no | admin UI origin (default `localhost:5173`) |
| `SMTP_*` | no | invitation emails |
| `ENV` | no | `development` relaxes some checks; leave unset for the test |
