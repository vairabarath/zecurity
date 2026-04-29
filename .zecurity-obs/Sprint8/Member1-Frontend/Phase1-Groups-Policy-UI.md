---
type: phase
status: pending
sprint: 8
member: M1
phase: Phase1-Groups-Policy-UI
depends_on:
  - M2-Phase1-Policy-Schema
  - M3-Phase1-Policy-Compiler
tags:
  - frontend
  - react
  - policy-engine
  - groups
---

# M1 Phase 1 — Groups + Policy UI

---

## What You're Building

Build the admin UI for group-based resource access.

---

## Required Screens

### Groups Page

- List groups in the current workspace.
- Create group.
- Edit group name/description.
- Delete group with confirmation.

### Members Tab

- Show users in a group.
- Add users to group.
- Remove users from group.

### Resources Tab

- Show resources assigned to a group.
- Assign resources to group.
- Remove resource assignment.
- Clearly show disabled/empty policy states.

### Resources Page Integration

- Show which groups have access to each resource.
- Link from resource to group details where useful.

---

## UX Rules

- This is an admin/workflow UI, not a marketing page.
- Keep tables dense and scannable.
- Show default-deny clearly: no groups assigned means no client access.
- Use existing design system and GraphQL patterns.

---

## Build Check

```bash
cd admin && npm run build
```
