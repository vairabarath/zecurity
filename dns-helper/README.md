# zecurity-dns-helper

The privileged half of **ADR-023 option C**. Applies per-link DNS routing for the Zecurity TUN so managed
FQDNs resolve without a `hosts` entry, while the main client daemon stays **unprivileged**.

## Status: partial — validation only

| | |
|---|---|
| `src/validate.rs` — the whitelist (WHAT) | ✅ complete, revert-tested |
| `src/peer.rs` — `SO_PEERCRED` (WHO) | ✅ complete, tested over a real socket |
| `src/protocol.rs` — closed 2-verb contract | ✅ complete |
| `src/resolved.rs` — host calls via `resolvectl` | ✅ complete, injectable for tests |
| `src/server.rs` — authorize → validate → act | ✅ complete, revert-tested |
| `src/main.rs` — socket activation + args | ✅ complete |
| `systemd/*.socket`, `*.service` | ✅ written, **not installed** |
| daemon-side client + wiring | ⬜ not started |
| installer: group + `@ALLOW_UID@` substitution | ⬜ not started |
| Gate 3 (live, on a real stack) | ⬜ not started |

**42 tests**, zero clippy warnings. The helper is functionally complete but **nothing calls it yet** —
the client daemon has no helper client, and the units are not installed.

The binary refuses to start unless it is root, has `--allow-uid`, and has a socket — it fails on
privilege *before* anything else, so a misconfiguration cannot surface later as a confusing
`resolvectl` permission error.

## Why a library plus a thin binary

The validation whitelist is this component's entire security value. As library API it can be tested and
reviewed with no root process, socket or systemd involved — which is why the 20 tests need none of those.

## Request path — the order is the design

```text
1. WHO   SO_PEERCRED           refused before the body is even read
2. WHAT  the whitelist          backend only ever sees in-range values
3. DO    resolvectl (argv)      no policy of its own
```

Authorizing first means an unauthorized caller never reaches the parser, so
malformed-input handling is not part of its attack surface — and it is told `"unauthorized"`, never the
validation reason, so the policy cannot be probed. An authorized caller *does* get the reason, because
it is their own bug to fix.

## API

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

`libc`, `serde`, `serde_json` — and nothing else. **No D-Bus crate**: host changes go through
`resolvectl` with an **argument vector** (never a shell string), which keeps a substantial dependency
out of a root binary. Task 1 verified those exact invocations. Two independent reasons an injected
argument cannot reach a shell: there is no shell, and `validate` has already constrained every value. Every crate here runs as root, so the list is kept
deliberately short. `libc` is needed only for `SO_PEERCRED`: std's `UnixStream::peer_cred()` is still
unstable (`peer_credentials_unix_socket`, rust-lang/rust#42839).

## Build

```bash
cargo build --manifest-path dns-helper/Cargo.toml
cargo test  --manifest-path dns-helper/Cargo.toml
```
