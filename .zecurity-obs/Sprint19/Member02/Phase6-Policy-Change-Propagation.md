---
type: phase
member: M02
sprint: 19
phase: 6
title: Policy Change Propagation
status: planned
depends_on: [5]
tags: [resource-policy, cache, acl-push, controller, connector, pending-16]
---

# Phase 6 — Policy Change Propagation

## Goal

Every Resource Policy or Device Profile binding change must converge through the existing:

```text
mutation
  ↓
NotifyPolicyChange
  ↓
invalidate cache
  ↓
recompile
  ↓
push
  ↓
Connector
```

path.

## Required work

- [ ] Verify create/update/delete Resource Policy mutations call policy notification.
- [ ] Verify Resource → Policy assignment changes call policy notification.
- [ ] Verify Policy → Device Profile binding changes call policy notification.
- [ ] Preserve per-workspace policy version behavior.
- [ ] Preserve cache epoch protection against stale in-flight compilation.
- [ ] Preserve immediate ACL push to live Connectors.
- [ ] Preserve heartbeat/version reconciliation as fallback.
- [ ] Verify no stale ACL can survive a successful policy mutation after convergence.
- [ ] Verify a removed Device Profile causes affected access to be removed after propagation.
- [ ] Verify adding a Device Profile grants access only to devices satisfying it.
- [ ] Verify changing a profile requirement/revision invalidates affected authorization.

## Race tests

- [ ] Policy changes while ACL compilation is in flight.
- [ ] Old compilation cannot overwrite a newer cache epoch.
- [ ] Connector temporarily disconnected during push.
- [ ] Connector catches up through heartbeat/version reconciliation.
