---
type: phase
status: pending
sprint: 9
member: M4
phase: Phase1-Client-TUN
depends_on:
  - Sprint8.5-Phase1-Daemon-Scaffold-IPC
  - Sprint9-M4-Phase1-Shield-Tunnel-Relay
  - Sprint9-M3-Phase1-RDE-Device-Tunnel (Connector QUIC listener must be live)
tags:
  - rust
  - client
  - tun
  - smoltcp
  - quic
  - transparent-proxy
---

# M4 Phase 1 — Client Transparent Proxy (TUN + smoltcp + QUIC Pool)

> See [[Decisions/ADR-003-Client-TUN-Transparent-Proxy]] for full architectural rationale.

---

## What You're Building

Replace the missing explicit `zecurity connect` command with a **transparent OS-level proxy**. After `zecurity up`, apps connect to resource IPs normally — the OS routes matching traffic into the daemon's TUN device, and the daemon tunnels it to the Connector without any app being aware.

The daemon already exists (Sprint 8.5) and holds the ACL snapshot in memory. This phase adds the TUN interface, smoltcp TCP/IP stack, and QUIC connection pool on top of that foundation.

---

## New Commands

| Command | IPC message | What happens |
|---------|-------------|-------------|
| `zecurity up` | `{"type":"Up"}` | Daemon creates TUN, installs routes, starts smoltcp loop |
| `zecurity down` | `{"type":"Down"}` | Daemon removes TUN and all routes, stops smoltcp loop |

---

## Files to Create / Modify

### `client/src/tun.rs` (NEW)

TUN interface lifecycle manager.

Responsibilities:
- Create `zecurity0` TUN device using the `tun` crate.
- Assign a `/32` host address (default `100.64.0.1/32`). Do **not** use `/10` — that installs a broad connected route for the CGNAT block which can conflict with enterprise/VPN networks.
- The interface address must be configurable via daemon config.
- Detect conflicts: before `up`, verify no existing route overlaps with any ACL resource IP. Return error if conflict found.
- `add_route(ip: IpAddr)` — add one `/32` host route pointing to `zecurity0` per ACL snapshot resource address.
- `remove_all_routes()` — remove all routes added during this session.
- `destroy()` — bring down and remove the TUN interface.
- Called from daemon's `handle_up()` and `handle_down()`.

```rust
pub struct TunManager {
    dev:    tun::AsyncDevice,
    routes: Vec<IpAddr>,
}

impl TunManager {
    pub fn create() -> Result<Self>;
    pub fn add_route(&mut self, ip: IpAddr) -> Result<()>;
    pub fn into_async_device(self) -> tun::AsyncDevice;
    pub fn cleanup(self) -> Result<()>;  // remove routes + destroy
}
```

Use `ip route add <ip>/32 dev zecurity0` via `std::process::Command`. No external routing crate needed.

---

### `client/src/net_stack.rs` (NEW)

smoltcp integration — the packet processing loop.

Responsibilities:
- Accept the `tun::AsyncDevice` from `TunManager`.
- Create a smoltcp `Interface` wrapping the TUN device.
- Run the poll loop: read packets from TUN → smoltcp processes TCP/UDP.
- For each new TCP connection smoltcp accepts:
  - Extract destination `(ip, port)`.
  - Look up in ACL snapshot → find `resource_id` and Connector address.
  - Call `TunnelPool::open_stream(connector_addr)`.
  - Send `TunnelRequest` JSON, read `TunnelResponse`.
  - `copy_bidirectional(smoltcp_socket, quic_stream)`.
- For each UDP datagram:
  - Look up `(src_ip:port, dst_ip:port)` in UDP session table.
  - New session: open QUIC stream, send `TunnelRequest`.
  - Existing session: forward datagram with 4-byte big-endian length prefix.
  - Session idle timeout: 30 seconds.

```rust
pub async fn run(
    dev: tun::AsyncDevice,
    acl: Arc<AclSnapshot>,
    pool: Arc<TunnelPool>,
) -> Result<()>;
```

Key smoltcp setup:
```rust
let mut config = smoltcp::iface::Config::new(HardwareAddress::Ip);
config.random_seed = rand::random();
let mut iface = Interface::new(config, &mut device, Instant::now());
iface.update_ip_addrs(|addrs| {
    addrs.push(IpCidr::new(IpAddress::v4(100, 64, 0, 1), 10)).ok();
});
```

---

### `client/src/tunnel_pool.rs` (NEW)

QUIC connection pool — one connection per Connector address, N streams per connection.

```rust
pub struct TunnelPool {
    connections: Arc<Mutex<HashMap<SocketAddr, Arc<quinn::Connection>>>>,
    endpoint:    quinn::Endpoint,  // client QUIC endpoint with mTLS cert
}

impl TunnelPool {
    /// Build pool using device cert + private key from daemon RuntimeState.
    pub fn new(cert_pem: &str, key_pem: &str, ca_pem: &str) -> Result<Self>;

    /// Get existing connection or connect new. Never opens duplicate connections.
    pub async fn get_or_connect(&self, addr: SocketAddr) -> Result<Arc<quinn::Connection>>;

    /// Open a new QUIC bidirectional stream on the pooled connection.
    pub async fn open_stream(&self, addr: SocketAddr)
        -> Result<(quinn::SendStream, quinn::RecvStream)>;
}
```

QUIC client config:
- ALPN: `ztna-tunnel-v1` (matches Connector's `quic_listener.rs`)
- TLS: device cert for mTLS — same cert the daemon loaded from `PostLoginState`
- Verify: Connector's cert against workspace CA (from daemon RuntimeState)

---

### `client/src/cmd/up.rs` (NEW)

```rust
pub async fn run() -> Result<()> {
    // Send Up IPC to daemon (start daemon if needed).
    // Print: "Zecurity is up. N resources accessible."
}
```

---

### `client/src/cmd/down.rs` (NEW)

```rust
pub async fn run() -> Result<()> {
    // Send Down IPC to daemon.
    // Print: "Zecurity is down."
}
```

---

### `client/src/daemon.rs` (MODIFY)

Add handlers for `Up` and `Down` IPC messages:

```rust
// Up handler
async fn handle_up(state: &SharedState, ...) -> IpcResponse {
    let acl = state.read().await.acl_snapshot.clone();
    // Create TunManager, install routes for each ACLEntry.address
    // Spawn net_stack::run(dev, acl, pool) as background task
    // Store TunManager in RuntimeState so Down can clean it up
    IpcResponse::ok("up")
}

// Down handler
async fn handle_down(state: &SharedState) -> IpcResponse {
    // Stop net_stack task
    // Call tun_manager.cleanup()
    // Remove from RuntimeState
    IpcResponse::ok("down")
}
```

---

### `client/src/main.rs` (MODIFY)

Add `Up` and `Down` subcommands:

```rust
Commands::Up   => cmd::up::run().await,
Commands::Down => cmd::down::run().await,
```

---

### `client/src/ipc.rs` (MODIFY)

Add `Up` and `Down` to the IPC message enum:

```json
{"type":"Up"}
{"type":"Down"}
```

---

### `client/Cargo.toml` (MODIFY)

```toml
tun     = "0.6"
smoltcp = { version = "0.11", features = ["proto-ipv4", "socket-tcp", "socket-udp"] }
quinn   = "0.11"
```

---

## DNS Constraint

Resources are accessed by **IP address only** in Sprint 9. Routes are installed per `ACLEntry.address` (IP). DNS split-horizon is Sprint 11. Document this in the `--help` text for `zecurity up`:

```
After running `zecurity up`, connect to resources using their IP addresses.
DNS resolution for resource hostnames requires Sprint 11 DNS proxy.
```

---

## Security Rules

- The QUIC connection uses device mTLS cert — same cert stored in daemon memory from Sprint 8.5.
- Verify Connector's certificate against the workspace CA from daemon RuntimeState. Reject on verification failure.
- If ACL snapshot is missing or empty when `Up` is called, return error. Never bring up TUN with an empty route table.
- If `net_stack::run()` exits unexpectedly, log the error and call `handle_down()` automatically. Never leave dangling routes.
- TUN interface and routes must be cleaned up on daemon exit (SIGTERM/SIGINT handler).

---

## Build Check

```bash
cd client && cargo build
```

Manual verification:
```bash
zecurity up
ip addr show zecurity0           # interface exists, default 100.64.0.1/32
ip route show dev zecurity0      # one /32 per ACL resource
curl http://<resource-ip>:<port> # reaches resource transparently
zecurity down
ip addr show zecurity0           # interface gone
ip route show dev zecurity0      # no routes
```
