---
type: decision
status: accepted
date: 2026-10-28
deciders: Sathiya (M1), Barath (M2) — approved by the team 2026-10-28
supersedes: the unimplemented `DeviceProfile.manualTrust` flag (Sprint 15, migration 031)
tags:
  - posture
  - device-trust
  - byod
---

# ADR-030 — Admin device approval as a per-resource posture requirement

## Context

**OAuth login proves *who* the user is, not *which machine* they use.**
- A stolen password, or a sign-in from a home PC, still gets the user's group access.
- Posture checks (firewall, LUKS, Secure Boot) prove a machine is *healthy*, not that it is *ours*. Any laptop can pass them.

**Sprint 15 added `DeviceProfile.manualTrust`** for a mockup's "Verification Requirements" section.
- It is stored, locked on in the UI, and refused when switched off.
- **No access decision reads it.** It promises an approval step that does not exist (found during the 2026-10-28 posture live test).

**The team needs a mix of access per resource:**
- some resources may be used from personal devices (BYOD);
- some only from company devices an admin has vouched for.

## Decision

1. **Approval is a per-device state, set once by an admin.**
   - `client_devices.approved_at` and `approved_by` (migration `044_device_approval.sql`; `043_` stays reserved for Sprint 22 Phase D).
   - A new or re-created device starts **unapproved**.
   - Admins approve or un-approve with `approveDevice` / `unapproveDevice`. A revoked device can't be approved.
   - Both actions are audited in `audit_logs` (`device.approve` / `device.unapprove`) in the same transaction.
2. **"Approved by an admin" is a posture check that a profile can require**, like the firewall check: `zecurity.device.approved`.
   - Profiles stay per resource, so each resource chooses:
     - no profile, or a posture-only profile → BYOD allowed;
     - a profile that requires `zecurity.device.approved` → company devices only.
   - A resource may bind several ENFORCE profiles. A device needs to satisfy **any one** of them (existing rule), so "company device" **or** "BYOD with strict posture" is expressible.
3. **The check is server-side only.** The controller computes it from `client_devices`.
   - A client-reported observation with that ID is dropped like any unknown check, so a modified client can't approve itself.
   - It is evaluated in the same place as the client checks (`posture.Evaluator`). Stale or missing reports still fail closed, so it inherits AUDIT/ENFORCE and the connector-side enforcement unchanged.
4. **Approve or un-approve re-evaluates the device at once** and notifies the ACL. There's no waiting for the next 5-minute report.
5. **`manualTrust` is retired.** It stays in the schema for now, marked deprecated, and is not read. The admin UI drops the locked switch and offers the "Approved by an admin" requirement in the profile editor instead.

## Consequences

- **Approval happens once per device, not per access.** After approval, only posture is monitored continuously (every 5 min).
- **Lost or stolen laptop:** revoke the device (existing). Un-approving alone keeps BYOD-level access.
- **Admin UI:** a device list with an "Approved" state and Approve / Remove approval actions, plus the new requirement in the profile editor. This is an approved exception to the Sprint 22 "don't modify `admin/`" rule, which is about the provider console's scope.
- **Not in scope:**
  - automatic approval from MDM/EDR ("Connect Trust Methods", still a stub);
  - approval requests raised by the user;
  - headless (browserless) login, which is a separate feature.
