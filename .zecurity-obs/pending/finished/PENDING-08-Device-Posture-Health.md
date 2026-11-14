---
type: adr
status: implemented
verified: 2026-11-14
id: PENDING-08
domain: zero-trust
priority: P2
created: 2026-07-03
related:
  - PENDING-09-Continuous-Authorization
tags: [pending, adr, zero-trust, posture, device]
---

# Pending ADR 08 — Device Posture & Health Checks

> **Status (verified against `main` 25a0cfb5, 2026-11-14): IMPLEMENTED, and wider than first scoped.**
> Since Sprint 15:
> - **Admin approval** check `zecurity.device.approved` ([[ADR-030-Admin-Device-Approval]]).
> - **Country** check `zecurity.network.country` (PENDING-20).
> - **Resource policies** replaced resource↔profile bindings and the enforce/audit modes (PENDING-16, PR #177).
> - **Per-platform profiles** ([[ADR-031-Per-Platform-Device-Profiles-and-Android-Posture]]): `allowed_platforms`
>   (migration `060`). The Android checks (encryption, screen lock, biometric) are PR #203. The Windows checks
>   (BitLocker, screen lock, biometric, firewall, antivirus; `client/src/posture_windows.rs`) and the
>   server-side Minimum OS Version checks (`windows/android/ios.os.minimum`, `controller/internal/posture/osversion.go`)
>   are PR #204.
>
> **Not built:**
> - Option B: MDM/EDR feeds (no Intune, Jamf or CrowdStrike code).
> - Option C: hardware-attested posture. `client/src/tpm.rs` is device-key storage (PENDING-17), not posture.
> - iOS and macOS checks.
>
> Superseded wording below: "ADR promotion deliberately deferred" (ADR-030 and ADR-031 are accepted), and
> resource↔profile "bindings" and "audit/enforce mode" (now resource policies).
>
> **Original status (Sprint 15):** Linux posture collection (OS/version, LUKS, firewall, Secure
> Boot), Device Profiles with requirements and audit/enforce mode, resource↔profile bindings,
> posture-aware ACL compilation (AND within a profile, OR across enforce-mode profiles bound to a
> resource, revision-gated fail-closed evaluation), retention worker, and admin UI (list/create/edit,
> requirements editor, resource-binding picker) are built and merged on `fixed-pendings` — see
> `.zecurity-obs/Sprint15/path.md` (64/65 checkboxes; the one open item is a manual/E2E pass against
> a running backend, not a code gap) and `Sprint15/Member1-Go/Phase1-4`. Options / Recommendation /
> Open Questions below are retained as the original decision record; ADR promotion to `ADR-0NN` was
> deliberately deferred pending real-world validation (see PENDING-09's note on the shared
> revocation-latency mechanism).

## Context / Current State

Access is gated purely by **identity** (device SPIFFE cert + group membership → ACL). There is no
notion of **device posture** — OS/version, disk encryption, screen-lock, EDR/AV present, patch
level, jailbreak/root. This is a core ZTNA differentiator ("never trust, continuously verify");
without it, a compromised-but-enrolled device has the same access as a healthy one.

## Problem — Decision Needed

Do we collect device posture, what signals, and how do they gate access?

## Options

### Option A — Client-daemon-collected posture signals
The client daemon (already resident — ADR-002) reports posture attributes on login/heartbeat;
controller factors them into the ACL decision.
- **Pros:** we already have a trusted daemon; incremental. **Cons:** self-reported (needs
  attestation to be trustworthy); per-OS collectors.

### Option B — Integrate an MDM/EDR signal source
Consume posture from CrowdStrike/Intune/Jamf/etc. rather than collecting first-party.
- **Pros:** authoritative; enterprises already run these. **Cons:** per-vendor integrations;
  coverage gaps for unmanaged devices.

### Option C — Hardware-attested posture (TPM/Secure Enclave)
Bind posture + device identity to hardware attestation.
- **Pros:** strongest anti-spoofing. **Cons:** significant effort; platform variance.

## Recommendation (non-binding)
Start with **Option A** (daemon-reported baseline posture feeding policy), designed so **Option B**
signal sources can be added later. Treat self-reported posture as advisory until attestation
(C) is justified. This is most valuable once **continuous authz (PENDING-09)** exists so posture
changes can revoke live sessions.

## Open Questions
- Which signals are v1 (OS version + disk encryption + EDR present is a common MVP)?
- How does posture combine with ACLs — hard gate, or risk score feeding step-up (PENDING-06)?
- Anti-spoofing stance for self-reported signals?

## Rough Effort / Priority
**M–L, P2.** Differentiator; sequence with PENDING-09.
