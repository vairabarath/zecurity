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

### Option A — TPM 2.0 / platform secure element, key never leaves hardware
Generate the keypair inside the TPM (Linux TPM2, Windows CNG TPM-backed key storage provider,
macOS Secure Enclave via Keychain). Signing operations (CSR signing at enrollment, and — after
ADR-028 Track 3 ships — the renewal CSR) are delegated to the hardware; the private key material
is never exported, never in process memory, never on disk in any form.
- **Pros:** the actual industry best-practice bar for device identity (this is what Windows Hello
  for Business, most enterprise ZTNA agents, and Tailscale's device attestation lean toward).
  Closes the "root/backup/same-user disk access = full impersonation" gap completely.
- **Cons:** a different API per OS, no unified Rust story as clean as `rcgen`'s in-memory
  `KeyPair`. Requires re-architecting the signing flow around "ask the platform to sign" instead
  of "hold a keypair and sign locally" — this interacts directly with ADR-028 Track 3's renewal
  CSR-building step, which currently assumes an in-memory key it can reload via
  `KeyPair::from_pem`.

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

Option B as a near-term improvement (small, no impact on ADR-028 Track 3's design), with Option A
as the real target once there's appetite for the platform-specific engineering — likely its own
multi-week track, not a drop-in change, since it changes the fundamental signing model that Track
3's renewal scheduler is currently built around (reload key from PEM, build CSR, sign locally).

## Open Questions
- Does Track 3's renewal design (same key, reloaded via `rcgen::KeyPair::from_pem`) need to be
  built with an abstraction seam now, so a later move to Option A doesn't require re-touching the
  scheduler? Or is that premature abstraction for a feature that may not land for a while?
- Minimum viable cross-platform target — is Linux-only TPM2 acceptable for a v1, given macOS/Windows
  clients would fall back to Option B?
- Does this block anything security-review-relevant before the platform is sold to a customer who
  audits device-identity storage specifically?

## Rough Effort / Priority
**Option B: S.** Drop-in per-OS secret-store swap, no protocol change.
**Option A: L, cross-cutting.** Touches enrollment, renewal (ADR-028 Track 3), and re-enroll; a
different implementation per target OS. **P2** — real gap, not urgent pre-production.
