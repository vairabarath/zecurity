---
type: task
status: done
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

- [x] `pub async fn setup(interface_addr: &str, connector_addr: &str) -> anyhow::Result<()>`
  - `interface_addr`: e.g. `"100.64.0.1/32"` — assigned by Controller at enrollment
  - `connector_addr`: e.g. `"192.168.1.10:9091"` — used to extract Connector IP for nftables

### setup_tun_interface()

- [x] `async fn setup_tun_interface(interface_addr: &str) -> anyhow::Result<()>`
- [x] Use `rtnetlink` crate to:
  1. Create TUN interface named `"zecurity0"` (`RTM_NEWLINK`, kind `"tun"`, mode tun)
  2. Assign `interface_addr` (parse CIDR, add address via `RTM_NEWADDR`)
  3. Bring interface UP (`RTM_NEWLINK` with `IFF_UP`)
- [ ] Shell equivalent (for reference):
  ```bash
  ip tuntap add dev zecurity0 mode tun
  ip addr add 100.64.0.1/32 dev zecurity0
  ip link set zecurity0 up
  ```
- [x] If `zecurity0` already exists (Shield restart): log warning, skip creation, proceed
- [x] On failure: return `Err` (logged in enrollment.rs as `warn!` — non-fatal for now)

### setup_nftables()

- [x] `async fn setup_nftables(connector_addr: &str) -> anyhow::Result<()>`
- [x] Extract connector IP from `connector_addr` (strip `:9091` port)
- [x] Use the `nftables` crate to build the ruleset document in Rust:
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
- [x] If table already exists (restart): delete and recreate (idempotent)
- [x] Log `info!("nftables table 'zecurity' created, connector_ip={}", connector_ip)`

### Current crate note

The implementation now builds the nftables ruleset with the `nftables` crate instead of hand-writing shell command strings.

However, the current `nftables` crate version in this repo still applies that ruleset by invoking the system `nft` executable through its helper API. So the code is Rust-native at the ruleset-construction layer, but the host still needs `nft` installed at runtime.

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
- This phase no longer shells out to `ip`; interface lookup, address assignment, and link-up use `rtnetlink`.

---

## Related

- [[Sprint4/Member4-Rust-Shield-CI/Phase3-Enrollment]] — calls network::setup()
- [[Sprint4/Member4-Rust-Shield-CI/Phase6-Updater-Systemd]] — systemd unit grants CAP_NET_ADMIN
