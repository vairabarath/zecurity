# Relay — Operational Validation Runbook

Source: Sprint 10.2 M3 Phase 3 ([Phase3-Integration-Security.md](../Sprint10.2/Member3-Rust/Phase3-Integration-Security.md)).

This is the manual end-to-end verification procedure for the Client direct-first + Relay-fallback path. Automated coverage of the security matrix lives in `client/tests/relay_security.rs` and `client/tests/relay_confidentiality.rs`; this runbook covers the **operational** validation that requires the full live stack (Controller, Relay, Connector, Client).

---

## Pre-conditions

- Controller built and running with `RELAY_ADDR` + `RELAY_SPIFFE_ID` set.
- Relay binary built; provisioning token issued via `POST /api/relays`.
- Connector built and registered against the same workspace.
- Client built and configured (`zecurity-client setup`) against the same Controller.
- `journalctl -u zecurity-client` follow window open in a second terminal.

```bash
journalctl -u zecurity-client -f --output=cat
```

## Procedure

### Step 1 — Controller emits Relay discovery

```bash
RELAY_ADDR=relay.local:9093 \
RELAY_SPIFFE_ID=spiffe://zecurity.in/relay/<relay-uuid> \
./controller-server
```

**Expect** in Controller logs:
```
relay discovery enabled relay_addr=relay.local:9093 relay_spiffe_id=spiffe://zecurity.in/relay/<...>
```

### Step 2 — Relay provisions cert and registers

```bash
# Issue provisioning token (admin auth required)
curl -sS -H "Authorization: Bearer $ADMIN_TOKEN" \
     -X POST https://controller.local/api/relays \
     -d '{"name":"relay-1","dns_allowlist":["relay.local"],"ip_allowlist":[]}' \
  | jq -r .provisioning_token > /tmp/relay.token

# Start relay; it consumes the token and provisions
RELAY_ID=<from-response> RELAY_PROVISIONING_TOKEN=$(cat /tmp/relay.token) ./relay
```

**Expect** in Relay logs:
```
relay provisioning successful cert_serial=... cert_not_after=...
relay heartbeat ok
```

### Step 3 — Connector registers

```bash
./connector
```

**Expect** in Controller logs:
```
connector registered id=<uuid> workspace=<workspace>
```

### Step 4 — Client up, direct path selected

```bash
zecurity-client up
zecurity-client sync                   # refresh ACL snapshot
curl -v <some-allowed-resource>:80    # triggers a stream open
```

**Expect** in Client logs within ~50ms of the resource request:
```
INFO zecurity_client::net_stack: new TCP connection — spawning QUIC relay
INFO zecurity_client::tunnel_pool: ...     # direct path (no "relay fallback" warning)
```

The absence of the `direct path failed; used relay fallback` warning is the positive signal.

### Step 5 — Block direct route, fall back to relay

Block UDP/9092 (the Connector QUIC port) on the Client side:

```bash
sudo iptables -A OUTPUT -p udp --dport 9092 -j REJECT
```

Trigger a fresh stream by curl-ing the same resource (after the existing pooled connection times out, or open a different resource so a new dial happens):

```bash
curl <another-allowed-resource>:80
```

**Expect** in Client logs **within 2 seconds** of the dial attempt:
```
WARN zecurity_client::transport: direct path failed; used relay fallback
  direct_err="..." relay_addr="relay.local:9093"
```

### Step 6 — Unblock, return to direct path

```bash
sudo iptables -D OUTPUT -p udp --dport 9092 -j REJECT
```

Open a new stream:

```bash
curl <yet-another-resource>:80
```

**Expect**: no `relay fallback` warning. The next dial succeeds directly within 2s. (Existing relay-bridged streams stay on the relay until they close — that's expected and correct.)

### Step 7 — ACL denial returns without second fallback

Pick a resource that's **not** in the Client's policy. Attempt to reach it:

```bash
curl <denied-resource>:80
```

**Expect** in Client logs:
```
WARN zecurity_client::net_stack: QUIC relay ended  error="tunnel denied for <denied-resource>:80: ..."
```

**Do NOT expect** any `relay fallback` warning — the denial originates from the Connector's `TunnelResponse{ok:false}`, which is downstream of `open_authenticated_stream` and not a fallback trigger.

---

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| Client logs no `relay discovery` mention | Controller env vars not set, or ACL snapshot empty | re-check `RELAY_ADDR`/`RELAY_SPIFFE_ID` on Controller |
| `direct path failed; used relay fallback` appears immediately on first stream | Direct LAN address unreachable from Client | confirm `connector_tunnel_addr` in ACL snapshot; check connector is listening on 9092 |
| `direct path failed; used relay fallback` followed by relay error chain | Relay rejection — token expired, cert wrong | re-provision relay; check `cert_not_after` in Controller `relays` row |
| `identity validation failed` / `TLS alert ...` | Connector/Relay served wrong SPIFFE or wrong workspace CA | should never happen in prod — file as incident; do NOT proceed past this |
| Fallback warning appears > 2 seconds after dial | system clock skew or excessive scheduler latency | check `DIRECT_TIMEOUT` constant in `client/src/transport.rs` |

Useful `tracing` filter:
```bash
RUST_LOG=zecurity_client::transport=debug,zecurity_client::net_stack=info zecurity-client up
```

## Success criterion

All seven `expect:` lines above appear in `journalctl -u zecurity-client -f --output=cat` in the right order with no unexpected warnings or errors. Steps 5 and 6 are the load-bearing operational checks; the rest are setup verification.
