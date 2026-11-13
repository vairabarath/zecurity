---
type: decision
status: accepted
date: 2026-11-13
deciders: Sathiya (M1) — answers given 2026-11-13 (platform switch meaning, Android checks, first platforms)
amends: posture evaluation (device profiles), ADR-030 (server checks keep applying to every platform)
tags:
  - posture
  - device-trust
  - android
  - multi-platform
---

# ADR-031 — Per-platform device profiles and the first Android posture checks

## Context

**A device profile is one flat list of required checks.**
- The checks are platform-specific (`linux.disk_encryption.luks`, …).
- A device fails any check it cannot observe, which shows as "unsupported", unless the requirement has `allowUnsupported`.
- So a profile can serve only one platform: requiring Linux disk encryption *and* an Android screen lock would fail every device.

**Clients now run on Android, with Windows and iOS on the way** (`docs/client-multi-platform.md`).
- Android posture is reported as the OS only, and the controller recognises only `linux.*` checks.
- The live run on an Android 12 phone (2026-11-13) showed the phone can report its screen lock, enrolled biometrics and storage encryption through public APIs, with no root.

**The wanted admin experience** (reference mock-up, 2026-11-13) is a card per platform:
- a switch turns the platform on or off;
- each check is marked "required" or "not required".

## Decision

### D-1 The platform switch allows or denies the platform

- Each profile stores **allowed platforms** (`device_profiles.allowed_platforms`, migration `057_`).
- A device whose platform is not allowed **fails** the profile, with a reason naming the platform.
- A device's platform is `client_devices.os`, fixed at enrollment (`linux`, `android`, `windows`, `ios`). The posture report's OS name is never used, so a device cannot change platform from report to report.
- An unknown or empty platform fails closed.

### D-2 Requirements apply per platform

A device is evaluated against these requirements only:
- the requirements whose check belongs to **its platform**;
- the server-observed checks (`zecurity.device.approved`, `zecurity.network.country`, platform `any`), which apply to every platform.

Further rules:
- A requirement whose check has no known descriptor applies to every device, so it **fails closed**.
- A platform that is allowed but has no applicable requirements **passes**: "Android on, nothing required" means any Android device. The stale-report rule still applies, so the device must report.

### D-3 Existing profiles keep their behaviour (migration `057_`)

- A profile with **at least one requirement** gets `{linux}`. That is what it allowed in practice: other platforms failed their unsupported Linux checks.
  - Verified 2026-11-13: only Linux devices have stored posture reports.
- A profile with **no requirements** gets `{}` (no platform), because today it always fails ("profile has no requirements"). Defaulting it to `{linux}` would have opened it.
- New profiles start with `{}`. The admin turns platforms on.

### D-4 The first Android checks (all `PASS` / `FAIL` / `UNSUPPORTED`)

The check IDs are identical in Go and Rust.

| Check ID | Label | PASS when |
|---|---|---|
| `android.disk_encryption.enabled` | HD Encryption | `DevicePolicyManager.getStorageEncryptionStatus()` is `ACTIVE` or `ACTIVE_PER_USER` (file-based encryption). `ACTIVE_DEFAULT_KEY` (encrypted with a default key, no user credential) is **FAIL**. |
| `android.screen_lock.enabled` | Screen Lock | `KeyguardManager.isDeviceSecure()` |
| `android.biometric.enrolled` | Biometric | `BiometricManager.canAuthenticate(BIOMETRIC_STRONG) == SUCCESS` (Android 11+). Below Android 11: **UNSUPPORTED**, never guessed. |

- They are collected in the app's VPN service process (Kotlin) and handed to the client daemon over JNI. The daemon submits them like the Linux checks.
- No fingerprint data is ever read; only "a strong biometric is enrolled".

### D-5 Platforms in the first version

- The admin UI shows **Linux** and **Android** cards.
- The model and API take any of `linux`, `android`, `windows`, `ios`. Windows and iOS cards come with their collectors.

### D-6 Changing platforms is a policy change

`setProfilePlatforms` takes the same path as adding a requirement:
- capability `POSTURE_WRITE`;
- audit `device_profile.platforms.set`;
- `NotifyPolicyChange`, then workspace re-evaluation.

So turning Android off cuts Android devices without waiting for their next report.

### D-7 Scope note

This epic changes `admin/` (the device profile editor). CLAUDE.md's "don't modify `admin/`" rule belongs to the Sprint 22 provider console work and does not cover this.

## Known limitations (accepted for now)

- **The device declares its own platform and checks.**
  - A modified client could enroll a Linux machine as `android` to avoid Linux checks, or report false `PASS` values.
  - **Android Key Attestation** (hardware-signed boot state, patch level, app identity) closes this. It is deferred; the user may choose it later.
- Security patch age and "developer options off" were offered and **not chosen** for now.

## Consequences

- Admins can build one profile that covers Linux laptops and Android phones, each with its own requirements.
- Turning a platform off is an immediate, audited access change.
- Every new platform needs only check descriptors, a collector and a card. The evaluation rules don't change again.
