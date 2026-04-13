# Phase 9 — Deployment Infrastructure

Create systemd units, install script, and Docker Compose for connector deployment.

---

## Files to Create

```
connector/systemd/zecurity-connector.service
connector/systemd/zecurity-connector-update.service
connector/systemd/zecurity-connector-update.timer
connector/scripts/connector-install.sh
connector/Dockerfile                ← optional container image
connector/docker-compose.yml        ← connector-side compose example
```

---

## systemd Units

### `zecurity-connector.service`

Main daemon with security hardening:

- `NoNewPrivileges=true`
- `ProtectSystem=strict`
- `PrivateTmp=true`
- `User=zecurity`
- `Group=zecurity`

### `zecurity-connector-update.service`

Oneshot updater service.

### `zecurity-connector-update.timer`

Daily trigger with random delay.

---

## `connector/scripts/connector-install.sh`

- Creates `zecurity` system user
- Fetches `/ca.crt` from `CONTROLLER_HTTP_ADDR`
- Downloads binary from GitHub releases
- Installs systemd units + enables them
- Writes config to `/etc/zecurity/connector.conf` (0600)
- State directory: `/var/lib/zecurity-connector/`
- `-f` flag: force overwrite for re-installation

---

## Docker Compose (connector-side)

```yaml
services:
  zecurity-connector:
    image: ghcr.io/yourorg/zecurity/connector:latest
    restart: unless-stopped
    network_mode: host
    cap_add: [NET_ADMIN, NET_RAW]
    volumes:
      - /var/lib/zecurity-connector:/var/lib/zecurity-connector
      - /etc/zecurity:/etc/zecurity:ro
    environment:
      - CONTROLLER_ADDR=controller.example.com:8443
      - ENROLLMENT_TOKEN=eyJ...
      - AUTO_UPDATE_ENABLED=false
```

**Do NOT modify `controller/docker-compose.yml`.** That's the sprint 1 dev infrastructure (Postgres + Redis). Your Docker Compose file goes in `connector/docker-compose.yml` — a separate file for connector deployment.

---

## Important Rules

1. **State directory permissions matter.** `connector.key` must be 0600 owned by `zecurity`. `connector.conf` must be 0600. The install script sets these permissions. The systemd unit runs as user `zecurity`.
2. **Independent phase** — can be done anytime.

---

## Phase 9 Checklist

```
✓ zecurity-connector.service created with security hardening
✓ zecurity-connector-update.service created (oneshot)
✓ zecurity-connector-update.timer created (daily)
✓ connector-install.sh creates user, downloads binary, installs units
✓ Config file permissions set to 0600
✓ State directory /var/lib/zecurity-connector/ created
✓ -f flag for re-installation works
✓ docker-compose.yml for connector deployment created
✓ Committed and pushed
```

---

## After This Phase

Then proceed to Phase 10 (GitHub Actions CI).
