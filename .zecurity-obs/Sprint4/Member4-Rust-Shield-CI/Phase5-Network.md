---
type: task
status: pending
sprint: 4
member: M4
phase: 5
depends_on:
  - Phase1-Crate-Scaffold (Cargo.toml has rtnetlink + nftables deps)
unlocks:
  - Phase3-Enrollment calls network::setup()
tags:
  - rust
  - network
  - nftables
  - tun
  - linux
---

# M4 · Phase 5 — network.rs: zecurity0 + nftables

**Fully independent — no dependency on other members. Can be done in parallel with any phase after Phase 1.**

---

## Goal

Implement `network::setup()` which creates the `zecurity0` TUN interface and base nftables table. Called once after successful enrollment. Requires `CAP_NET_ADMIN`.

---

## File to Create

`shield/src/network.rs`

---

## Checklist

### Public API

- [ ] `pub async fn setup(interface_addr: &str, connector_addr: &str) -> anyhow::Result<()>`
  - `interface_addr`: e.g. `"100.64.0.1/32"` — assigned by Controller at enrollment
  - `connector_addr`: e.g. `"192.168.1.10:9091"` — used to extract Connector IP for nftables

### setup_tun_interface()

- [ ] `async fn setup_tun_interface(interface_addr: &str) -> anyhow::Result<()>`
- [ ] Use `rtnetlink` crate to:
  1. Create TUN interface named `"zecurity0"` (`RTM_NEWLINK`, kind `"tun"`, mode tun)
  2. Assign `interface_addr` (parse CIDR, add address via `RTM_NEWADDR`)
  3. Bring interface UP (`RTM_NEWLINK` with `IFF_UP`)
- [ ] Shell equivalent (for reference):
  ```bash
  ip tuntap add dev zecurity0 mode tun
  ip addr add 100.64.0.1/32 dev zecurity0
  ip link set zecurity0 up
  ```
- [ ] If `zecurity0` already exists (Shield restart): log warning, skip creation, proceed
- [ ] On failure: return `Err` (logged in enrollment.rs as `warn!` — non-fatal for now)

### setup_nftables()

- [ ] `async fn setup_nftables(connector_addr: &str) -> anyhow::Result<()>`
- [ ] Extract connector IP from `connector_addr` (strip `:9091` port)
- [ ] Use `nftables` crate (or `std::process::Command` as fallback) to create:
  ```
  table inet zecurity {
    chain input {
      type filter hook input priority 0; policy accept;
      iif "lo" accept
      ip saddr <connector_ip> accept
      iif "zecurity0" drop
    }
  }
  ```
- [ ] If table already exists (restart): flush and recreate (idempotent)
- [ ] Log `info!("nftables table 'zecurity' created, connector_ip={}", connector_ip)`

### nftables crate fallback

If no good `nftables` crate is available, use `std::process::Command`:

```rust
// Write rules to a temp file, then apply with nft -f
let rules = format!(r#"
table inet zecurity {{
  chain input {{
    type filter hook input priority 0; policy accept;
    iif "lo" accept
    ip saddr {} accept
    iif "zecurity0" drop
  }}
}}
"#, connector_ip);
tokio::fs::write("/tmp/zecurity-rules.nft", rules).await?;
tokio::process::Command::new("nft")
    .args(["-f", "/tmp/zecurity-rules.nft"])
    .status().await?;
```

---

## Test Plan

```bash
# After enrollment, on the resource host:
ip link show zecurity0
# Expected: <POINTOPOINT,UP,...> mtu 1500

ip addr show zecurity0
# Expected: inet 100.64.0.1/32 scope global zecurity0

nft list ruleset
# Expected:
# table inet zecurity {
#   chain input { ... }
# }
```

---

## Notes

- This function requires `CAP_NET_ADMIN`. The systemd unit sets `AmbientCapabilities=CAP_NET_ADMIN`.
- Network setup failing should NOT abort enrollment — log `warn!` and continue. The Shield can still heartbeat; network rules are idempotent and can be re-applied on next startup.
- Sprint 5 will ADD resource-specific DROP rules to this table. The base table set up here is the foundation.

---

## Related

- [[Sprint4/Member4-Rust-Shield-CI/Phase3-Enrollment]] — calls network::setup()
- [[Sprint4/Member4-Rust-Shield-CI/Phase6-Updater-Systemd]] — systemd unit grants CAP_NET_ADMIN
