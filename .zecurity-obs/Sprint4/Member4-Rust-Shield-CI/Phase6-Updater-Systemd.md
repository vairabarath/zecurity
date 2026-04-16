---
type: task
status: pending
sprint: 4
member: M4
phase: 6
depends_on:
  - Phase2-Core-Modules (util.rs exists for version comparison)
unlocks:
  - Phase7-CI-Connector-Main (install script needed for CI release)
tags:
  - rust
  - systemd
  - updater
  - install
---

# M4 · Phase 6 — Updater + Systemd Units + Install Script

**Depends on: core modules done. Can run in parallel with Phase 3–5.**

---

## Goal

Implement binary self-update, create systemd service units, and write the install script.

---

## Files to Create

| File | Purpose |
|------|---------|
| `shield/src/updater.rs` | GitHub release binary self-update |
| `shield/systemd/zecurity-shield.service` | Main service unit |
| `shield/systemd/zecurity-shield-update.service` | Update oneshot service |
| `shield/systemd/zecurity-shield-update.timer` | Weekly update timer |
| `shield/scripts/shield-install.sh` | One-line install script |

---

## Checklist

### updater.rs

- [ ] Mirror `connector/src/updater.rs` exactly
- [ ] Change GitHub release tag pattern from `connector-v*` to `shield-v*`
- [ ] Change binary name from `zecurity-connector` to `zecurity-shield`
- [ ] Change install path from `/usr/local/bin/zecurity-connector` to `/usr/local/bin/zecurity-shield`
- [ ] `pub async fn run(cfg: ShieldConfig)` — loop checking for new versions

### zecurity-shield.service

- [ ] `Description=Zecurity Shield — Resource Host Protection`
- [ ] `ExecStart=/usr/local/bin/zecurity-shield`
- [ ] `EnvironmentFile=/etc/zecurity/shield.conf`
- [ ] `User=zecurity` / `Group=zecurity`
- [ ] Full systemd hardening (NoNewPrivileges, ProtectSystem=strict, PrivateTmp, etc.) — mirror connector service
- [ ] **Shield-specific capabilities** (different from connector):
  ```ini
  CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE
  AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW
  ```
- [ ] `StateDirectory=zecurity-shield`
- [ ] `WorkingDirectory=/var/lib/zecurity-shield`
- [ ] `Restart=on-failure` / `RestartSec=3`

### zecurity-shield-update.service

- [ ] `Description=Zecurity Shield Update`
- [ ] `Type=oneshot`
- [ ] `ExecStart=/usr/local/bin/zecurity-shield --update`
- [ ] Mirror connector update service pattern

### zecurity-shield-update.timer

- [ ] `OnCalendar=weekly` (or `OnCalendar=*-*-* 03:00:00` for 3am weekly)
- [ ] `Unit=zecurity-shield-update.service`
- [ ] Mirror connector timer pattern

### shield-install.sh

Key differences from `connector-install.sh`:

- [ ] Binary name: `zecurity-shield`
- [ ] Config file: `/etc/zecurity/shield.conf` (not `connector.conf`)
- [ ] State dir: `/var/lib/zecurity-shield/`
- [ ] Service name: `zecurity-shield.service`
- [ ] Release tag pattern: `shield-v*`
- [ ] Env var written to config: `ENROLLMENT_TOKEN=$ENROLLMENT_TOKEN`
- [ ] System user `zecurity` — check if already exists (connector install may have created it)
- [ ] Script should be idempotent — safe to run twice

Install script minimal structure:
```bash
#!/usr/bin/env bash
set -euo pipefail
: "${ENROLLMENT_TOKEN:?ENROLLMENT_TOKEN is required}"
: "${CONTROLLER_ADDR:?CONTROLLER_ADDR is required}"
: "${CONTROLLER_HTTP_ADDR:?CONTROLLER_HTTP_ADDR is required}"

# 1. Create system user 'zecurity' (if not exists)
# 2. Create /etc/zecurity/ directory
# 3. Write /etc/zecurity/shield.conf
# 4. Download binary from GitHub release (shield-linux-amd64 or arm64)
# 5. Install to /usr/local/bin/zecurity-shield (chmod +x)
# 6. Install systemd units
# 7. systemctl daemon-reload && systemctl enable --now zecurity-shield.service
```

---

## Build Check

```bash
# Verify updater compiles:
cargo build --manifest-path shield/Cargo.toml

# Verify systemd units are valid syntax (if systemd-analyze available):
systemd-analyze verify shield/systemd/zecurity-shield.service

# Verify install script:
bash -n shield/scripts/shield-install.sh  # syntax check
```

---

## Notes

- The `zecurity` system user is shared between Connector and Shield — if connector is already installed, the user exists. The install script must handle this gracefully.
- `CAP_NET_ADMIN` and `CAP_NET_RAW` are required for `network.rs` (zecurity0 TUN creation + nftables). These are NOT on the Connector service unit — do not copy that part blindly.

---

## Related

- [[Sprint4/Member4-Rust-Shield-CI/Phase7-CI-Connector-Main]] — CI workflow uploads these files
