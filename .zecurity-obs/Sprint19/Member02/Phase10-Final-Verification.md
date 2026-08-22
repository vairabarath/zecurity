---
type: phase
member: M02
sprint: 19
phase: 10
title: Final Verification / Cleanup
status: planned
depends_on: [9]
tags: [verification, documentation, cleanup, pending-16]
---

# Phase 10 — Final Verification / Cleanup

## Goal

Prove that Sprint 19 implements the complete PENDING-16 architecture and leaves no partial path.

## Checklist

- [ ] Resource Policy is a real persistent entity.
- [ ] Resource has exactly one Resource Policy.
- [ ] Resource Policy supports zero or more Device Profiles.
- [ ] Multiple Device Profiles use OR.
- [ ] Empty Device Profile list means Any Device.
- [ ] Device Profiles do not carry an enforcement mode in the target path.
- [ ] Device Profile posture visibility works without Resource Policy binding.
- [ ] Linux Device Profile works end-to-end.
- [ ] Existing posture evaluation remains correct.
- [ ] ACL compilation remains correct.
- [ ] ACL snapshot contract remains compatible.
- [ ] Connector remains the identity enforcement point.
- [ ] Shield remains network/firewall enforcement.
- [ ] Policy changes invalidate stale ACL state.
- [ ] ACLs are recompiled and pushed.
- [ ] Heartbeat/version fallback still converges.
- [ ] Existing legacy bindings are safely migrated/coexist as planned.
- [ ] No customer access silently changes during migration.
- [ ] No unrelated pending/ADR work was implemented.
- [ ] Only Member02 changes are included in the Sprint 19 work.
- [ ] Source code, migration, API, UI, and tests are all documented.
- [ ] PENDING-16 status/checklist is updated only after all acceptance criteria pass.

## Final architecture evidence

Produce a short implementation note showing:

```text
Admin
  ↓
Resource Policy
  ↓
Device Profile
  ↓
Linux Posture
  ↓
Controller evaluation
  ↓
ACL Snapshot
  ↓
Connector
  ↓
Resource
```

and the corresponding actual code paths/functions.

## Final build gate

All backend, frontend, integration, and Linux end-to-end tests must pass.
