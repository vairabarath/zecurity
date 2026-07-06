---
type: adr
status: pending
id: PENDING-08
domain: zero-trust
priority: P2
created: 2026-07-03
related:
  - PENDING-09-Continuous-Authorization
tags: [pending, adr, zero-trust, posture, device]
---

# Pending ADR 08 — Device Posture & Health Checks

> **Status: PENDING — for team discussion.** On adoption, promote to the next free `ADR-0NN`.

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
