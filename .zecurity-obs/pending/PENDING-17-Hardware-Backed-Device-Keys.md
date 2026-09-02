---
type: adr
status: pending
id: PENDING-17
domain: identity/client
priority: P2
created: 2026-08-27
related:
  - ADR-002-Client-Daemon-Required
  - ADR-028-Client-Device-Lifecycle-and-Cert-Renewal
tags: [pending, adr, client, pki, tpm, key-storage]
---

# Pending ADR 17 — Hardware-Backed Device Key Storage

> **Status: PENDING — for team discussion.** On adoption, promote to the next free `ADR-0NN`.
> Raised while implementing PENDING-13 / ADR-028 Track 3 (cert renewal + fingerprint pinning) —
> that track hardens *who can renew a device's cert*, but doesn't change *where the key itself
> lives*, which is the gap this doc is about.

> **Scoping correction (2026-09-02):** the original Options below assumed a genuine 3-OS client
> needing per-OS backends (Linux TPM2 / Windows CNG / macOS Secure Enclave). Checked the actual
> codebase: this is **not accurate today**. CI only ever builds `ubuntu-latest`
> (`client-release.yml`/`connector-release.yml`/`shield-release.yml`); there is exactly one
> `cfg(target_os = ...)` branch in the entire client crate (a Linux-only `SO_PEERCRED` check in
> `ipc.rs`); and device posture checks are hard-coded Linux mechanisms end to end
> (`linux.disk_encryption.luks` reading `/etc/crypttab`, `nftables`/`iptables`/`ufw`,
> `/sys/firmware/efi/efivars` for secure boot). There is no macOS or Windows code path anywhere in
> this client. **The real near-term scope is Linux TPM 2.0 specifically** — Option A below is
> rewritten around that, with the crate choice and file-by-file plan now concrete rather than
> theoretical. See "Implementation plan" below.

## Context / Current State

The client device's private key (P-384, generated via `rcgen` at enrollment, `client/src/login.rs`)
is held in daemon process memory during use and persisted to disk **encrypted** — AES-256-GCM,
`client/src/state_store.rs`. But the AES key protecting it is itself just another plaintext file
on disk (`.{workspace}.key`, mode `0600`), sitting next to the ciphertext it protects. The real
security boundary today is therefore "OS file permissions for this user account," not hardware
secrecy: root, a backup that captures both files together, or anyone with the same OS-user's disk
access can decrypt everything and impersonate the device indefinitely.

This is a reasonable software-only baseline (better than plaintext-on-disk, key never touches disk
unencrypted, wiped on revoke/re-enroll per Track 2), but it is not what serious device-identity
products do for a long-lived credential that is the device's entire proof of identity on the
network.

## Problem — Decision Needed

Should the device private key move to hardware-backed storage (TPM / platform secure element),
and if so, how much of the cross-platform complexity is worth taking on now vs. later?

## Options

### Option A — TPM 2.0 (Linux), key never leaves hardware
Generate the keypair inside the TPM. Signing operations (CSR signing at enrollment, and the
Track 3 renewal CSR) are delegated to the hardware; the private key material is never exported,
never in process memory, never on disk in any form — only an opaque, TPM-sealed blob pair that is
meaningless without that exact chip.
- **Pros:** the actual industry best-practice bar for device identity (this is what Windows Hello
  for Business, most enterprise ZTNA agents, and Tailscale's device attestation lean toward).
  Closes the "root/backup/same-user disk access = full impersonation" gap completely. Scoped to
  Linux (the only platform this client actually ships on), this is a **contained, single-platform
  change**, not the cross-cutting 3-OS lift originally assumed here.
- **The "different API, no unified Rust story" concern is resolved:** `rcgen` (already a
  dependency, already used in `login.rs` and Track 3's `build_renewal_csr`) exposes a
  `RemoteKeyPair` trait built for exactly this — "a private key that is not directly accessible,
  but can be used to sign messages... for example an HSM" (3 methods: `public_key()`, `sign(msg)`,
  `algorithm()`). Wrapping a TPM key handle in a type implementing this trait and passing it to
  `KeyPair::from_remote(...)` plugs directly into the **existing**
  `CertificateParams::serialize_request(&key_pair)` calls in both `login.rs` and
  `build_renewal_csr` — those call sites barely change. Only *how the `KeyPair` is constructed*
  changes, not what's done with it.
- **Remaining cons:** still touches enrollment, renewal, and the revoke/re-enroll wipe path (see
  Implementation plan); requires the system `tpm2-tss` libraries present on the target machine
  (a new deployment-time package dependency, though the same category this client already has for
  `nftables`/`cryptsetup`); graceful fallback needed for machines without a usable TPM (many VMs/
  cloud instances/containers don't expose one).

### Option B — OS-native secret store, software-only
macOS Keychain, Windows DPAPI, Linux `libsecret`/kernel keyring — access gated by the OS
login/session rather than raw file permissions, but no hardware backing requirement.
- **Pros:** meaningfully better than a sibling plaintext key file, much smaller lift than A, still
  a normal exportable-to-memory key so Track 3's renewal flow (CSR from the existing key) needs no
  redesign. **Cons:** doesn't fully close the "full device compromise while unlocked" case any
  better than today; still a software secret at the end of the day.

### Option C — Keep current design (encrypted file + sibling key file)
- **Pros:** zero additional work. **Cons:** this is the status quo the "Context" section above
  already describes as sub-industry-standard for a device identity credential.

## Recommendation (non-binding)

**Option A (Linux TPM2), scoped as above** — now that the client is confirmed Linux-only and the
`RemoteKeyPair` seam removes most of the "cross-cutting per-OS rewrite" cost, this is a contained
enough change to be the actual next step rather than a someday-target. Option B is no longer the
better near-term move on its own merits — it's now just the natural *fallback* for a machine
without a usable TPM (see Implementation plan), not a separate track to build first.

## Implementation plan

**Crate: `tss-esapi`** (Parsec project / Linux Foundation Confidential Computing Consortium,
Apache-2.0, actively maintained — v7.7.0 checked 2026-09-02). The de facto standard Rust binding
to the TCG's ESAPI, via FFI to the system `tpm2-tss` C libraries.

Considered and rejected: pure-Rust, no-`tpm2-tss`-dependency TPM2 protocol implementations
(`tpm2-protocol`, `tpm2-rs`) that talk to `/dev/tpmrm0` directly, avoiding the system library
dependency. Rejected because they are explicitly early-stage ("heavily work in progress" per their
own repos) — for a device's core identity credential, depending on the mature, widely-deployed
stack beats reimplementing TPM2 command marshaling ourselves. This project already assumes
deployment-time system packages exist (`nftables`, `cryptsetup`); `tpm2-tss` isn't a new category
of requirement.

| File | Change |
|------|--------|
| `client/Cargo.toml` | `tss-esapi` under `[target.'cfg(target_os = "linux")'.dependencies]` — never attempts to build/link on a hypothetical future non-Linux target. |
| `client/src/tpm.rs` (new) | `TpmKeyPair` implementing `rcgen::RemoteKeyPair` around a `tss_esapi::Context` opened against `/dev/tpmrm0` (the kernel-arbitrated TPM device, not raw `/dev/tpm0`) + a loaded key handle. Lifecycle: TPM `create` (under the storage hierarchy) produces an opaque, TPM-sealed `(public_blob, private_blob)` pair — meaningless without that exact chip — persisted to disk; `load` reconstructs the in-TPM key handle from that pair on daemon startup/renewal. Also `tpm_available() -> bool` for graceful fallback to software keys (Option B/current design) on machines without a usable TPM (many VMs/cloud instances/containers don't expose one). |
| `client/src/state_store.rs` | `StoredDevice`'s key material needs a backend discriminator — an enum (`Software{private_key_pem}` / `Tpm{public_blob, private_blob}`) rather than a bare `private_key_pem: String`, since a TPM-backed device has no raw PEM to store at all. |
| `client/src/runtime.rs` | `DeviceInfo`'s in-memory key field, same enum. |
| `client/src/login.rs` | Enrollment: detect TPM availability, build the key via the chosen backend, then the **same** `params.serialize_request(&key_pair)` call as today — unchanged. |
| `client/src/daemon.rs` | `build_renewal_csr` (Track 3): reload via the chosen backend before that same CSR call. `react_to_device_directive`'s wipe-on-revoke logic needs backend awareness too — "wiping" a TPM key means discarding/evicting the blob, not blanking a PEM string. |
| `client/src/posture.rs` (optional) | A `linux.tpm.present` check, reusing the existing `CheckStatus` (PASS/FAIL/UNSUPPORTED/UNKNOWN/ERROR) framework already wired end-to-end for LUKS/firewall/secure-boot — gives admins fleet visibility into which devices are hardware-backed. |
| **Controller (Go)** | **Nothing.** The server only ever sees a CSR + signature — it has no idea whether signing happened in software or a TPM. Track 3's fingerprint-pinning design is completely agnostic to this. |

### A broader pattern this previews (not to build speculatively)
Researched how Twingate's cross-platform client is architected (public docs only, verified by
fetching their live docs/changelog directly, 2026-09-02): they don't share one tunnel implementation across OSes — Linux gets
TUN, macOS gets a Network Extension/system extension, Windows gets a *choice* of TunTap (default)
or Wintun (added later, purely for throughput) — but they do share one core client, with the
platform-specific pieces (tunnel, and per Twingate's Linux docs, even an optional TUN-less
userspace HTTP-proxy mode) sitting behind an interface the shared session/auth/routing logic
doesn't know about. The `RemoteKeyPair` split this doc proposes is that same pattern, applied to
key storage. **Do not** pre-emptively draw the equivalent boundaries for tunnel/service-lifecycle/
IPC now, before an actual Windows or macOS port is scheduled — that's designing for a hypothetical
future requirement. Let this ticket be where the pattern gets introduced for real, and let a future
second-platform port learn from it rather than guessing at the shape now.

## Open Questions
- Minimum viable target for v1: TPM-if-available with software fallback (Option B for the
  no-TPM case), or hard-require a TPM and treat its absence as a posture failure? The latter is a
  stricter security posture but would block enrollment on TPM-less machines (common in VMs/cloud
  dev environments) — leaning toward soft fallback + the `linux.tpm.present` posture check for
  visibility, but this is a real policy call, not just an engineering one.
- Does this block anything security-review-relevant before the platform is sold to a customer who
  audits device-identity storage specifically?

## Rough Effort / Priority
**Option A (Linux TPM2), scoped to this platform only: M.** Touches enrollment, Track 3 renewal,
and the revoke/re-enroll wipe path, each a bounded change thanks to the `RemoteKeyPair` seam — not
the "L, cross-cutting, different implementation per OS" estimate this doc originally carried, since
there is only one OS to implement against today. **P2** — real gap, not urgent pre-production.
