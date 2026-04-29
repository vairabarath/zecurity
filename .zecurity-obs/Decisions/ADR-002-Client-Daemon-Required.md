---
type: decision
status: accepted
date: 2026-04-29
related:
  - "[[Sprint7/path]]"
  - "[[Sprint8/path]]"
tags:
  - adr
  - client
  - daemon
  - runtime-state
  - systemd
---

# ADR-002 — Client Daemon Is Required For Active Runtime State

## Context

Sprint 7 moved the client from pure in-memory state to encrypted persisted state so one-shot commands can work:

- `login` saves encrypted workspace/user/device/session state.
- `status` reads saved state.
- `logout` clears saved state.
- `invite` currently runs its own login flow to obtain an admin token.

Sprint 8 adds ACL snapshots. Sprint 9 will add active RDE tunneling.

Active tunnel state cannot live only inside one-shot CLI commands. Once a command exits, memory is gone. A long-running process is required to hold:

- decrypted private key
- active access token
- active ACL snapshot
- active tunnel sessions
- route/TUN state

## Decision

Use a daemon-required client architecture for active runtime state.

Every CLI command that needs active session/tunnel/runtime state must talk to the daemon over a local same-user IPC socket. If the daemon is not running, the CLI starts it through the supported service manager and then retries the IPC call.

There is no direct-state fallback path for daemon-required commands.

IPC uses newline-delimited JSON: one JSON object per line with `\n` as the delimiter.

For Sprint 8.5, the CLI owns the existing OAuth/PKCE/browser callback flow. After login completes, the CLI sends `PostLoginState` to the daemon. The daemon persists encrypted durable state and holds active runtime state in memory.

## Timing

Use **Sprint 8.5** for the M4 daemon foundation.

Sprint 8 remains Policy Engine work for M1/M2/M3. M4 daemon work can happen in parallel or immediately after Sprint 8 schema/proto work, but Sprint 9 RDE must not begin until daemon foundation exists.

Reason:

- Daemon work is a 5-7 day refactor, not a 1-2 day patch.
- It touches every existing client command plus new IPC/service code.
- Running it as a pre-sprint blocker would unnecessarily delay M1/M2/M3 Sprint 8 work.

## Daemonization Mechanism

Use a **system-level service unit** for Linux (`/etc/systemd/system/`), not a user unit.

```text
/etc/systemd/system/zecurity-client.service
```

A user unit (`systemctl --user`) cannot acquire `CAP_NET_ADMIN` via `AmbientCapabilities` for regular users — the capability is not in their inheritable set and PAM configuration would be required. System-level units have no such restriction.

The service file sets `User=<enrolling_user>` so the daemon runs as that user, not root. The installation script sets this value during enrollment.

The CLI uses `systemctl start zecurity-client` (no `--user`) when it needs the daemon and the daemon is not running.

macOS `launchd` support is deferred.

## Privilege Model

Creating a TUN interface and managing routing table entries (Sprint 9 transparent proxy) requires `CAP_NET_ADMIN`. The daemon does not run as root.

Grant the capability in the system-level service unit:

```ini
AmbientCapabilities=CAP_NET_ADMIN
CapabilityBoundingSet=CAP_NET_ADMIN
NoNewPrivileges=yes
```

This is set in `client/zecurity-client.service`.

Alternatives considered and not chosen for Sprint 9:
- `setcap cap_net_admin+ep` on the binary: valid but grants capability to all invocations, not just the daemon.
- User unit + PAM capability grants: non-portable, requires admin configuration per machine.

Sprint 8.5 daemon does not use the capability — it runs user-only. Sprint 9 Phase F activates it when `zecurity up` creates the TUN interface.

A minimal privileged helper binary (creates TUN, passes the fd to the unprivileged daemon via `SCM_RIGHTS`, then exits) is the hardened long-term approach. This is how Twingate handles privilege separation on Linux. Defer to Sprint 11.

## Non-Goals

- No hand-rolled double-fork daemonization.
- No optional fallback path where commands bypass the daemon and read/write active runtime state directly.
- No Sprint 8 RDE tunnel implementation. RDE remains Sprint 9.

## Required Refactor

Client command behavior after daemon foundation:

- `login`: performs OAuth/enrollment in the CLI, sends `PostLoginState` to daemon, and daemon persists encrypted durable state plus active runtime state.
- `status`: queries daemon for active runtime status; can separately show durable login state when daemon is stopped.
- `logout`: tells daemon to drop runtime state and stop active tunnels; clears encrypted durable state.
- `invite`: obtains token through daemon session or runs explicit login flow, but does not create a second direct-state security path.
- future `connect`: daemon owns TUN/routes/tunnel state.

## Security Rules

- Durable state remains encrypted at rest.
- Decrypted private key lives only in daemon memory during active use.
- Access token lives only in daemon memory during active use.
- Refresh token/device identity may remain encrypted on disk for reauth/resume.
- Commands that require active tunnel/session state must fail closed if daemon IPC fails.
- Same-user IPC only. Reject cross-user access.

## Consequences

- Single runtime/security path for active sessions.
- Cleaner Sprint 9 RDE implementation.
- More up-front client refactor work.
- Linux-first client daemon story; macOS support is a later portability task.
