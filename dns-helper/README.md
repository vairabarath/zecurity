# zecurity-dns-helper

The privileged half of **ADR-023 option C**. Applies per-link DNS routing for the Zecurity TUN so managed
FQDNs resolve without a `hosts` entry, while the main client daemon stays **unprivileged**.

## Status: partial — validation only

| | |
|---|---|
| `src/validate.rs` — the whitelist | ✅ complete, 20 tests, revert-tested |
| socket layer + systemd socket activation | ⬜ not started |
| `SO_PEERCRED` peer authentication | ⬜ not started |
| systemd-resolved D-Bus calls | ⬜ not started |
| daemon-side client + wiring | ⬜ not started |
| `.socket` / `.service` units, group, installer | ⬜ not started |

The binary deliberately **exits non-zero** rather than pretending to work.

## Why a library plus a thin binary

The validation whitelist is this component's entire security value. As library API it can be tested and
reviewed with no root process, socket or systemd involved — which is why the 20 tests need none of those.

## API (when implemented)

```text
apply  { iface, server, domains[] }    # REPLACES the domain list; never appends
revert { iface }                       # idempotent; a no-op on an unconfigured link
```

## What the helper refuses

It validates rather than trusts — the calling daemon is unprivileged and its input is treated as hostile:

- `iface` must be exactly `zecurity0` — **the check that stops this being a general-purpose "set DNS on
  any link" service running as root**. Checked *first*, before the payload is even parsed.
- `server` must be loopback or inside `100.64.0.0/10`
- every domain must be routing-only (`~`-prefixed), an **individual FQDN** (≥ 2 labels), well-formed, and
  unique in the list

## The invariants it enforces mechanically (ADR-023)

1. Never modify global DNS → `~.` is refused outright
2. Never modify another interface's DNS → exact `iface` match
3. Only configure the Zecurity TUN → same check
4. Route managed FQDNs individually → ≥ 2 labels; a bare parent like `~internal` is refused
5. Resolver keeps exact-match + `REFUSED` → Phase 11; invariant 4 is what makes it safe

Invariant 4 is not tidiness. Task 1 showed `~domain` captures the whole **subtree**, so routing a shared
suffix would hand our responder every sibling name — Phase 11 would `REFUSED` the unmanaged ones, and
because the route is link-scoped, resolved has nowhere else to try. That would break names we do not
manage.

## Dependency footprint

`libc`, `serde`, `serde_json` — and nothing else. Every crate here runs as root, so the list is kept
deliberately short. `libc` is needed only for `SO_PEERCRED`: std's `UnixStream::peer_cred()` is still
unstable (`peer_credentials_unix_socket`, rust-lang/rust#42839).

## Build

```bash
cargo build --manifest-path dns-helper/Cargo.toml
cargo test  --manifest-path dns-helper/Cargo.toml
```
