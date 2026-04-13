# Phase 5 — Install Command Modal

## Objective

Build the install flow component used from the connectors page to create a connector token and display the backend-generated install command.

---

## Prerequisites

- Phase 1 complete
- Phase 4 planned or in progress

---

## Files to Create

```
admin/src/components/InstallCommandModal.tsx
```

## Files to Modify

```
admin/src/pages/Connectors.tsx
```

---

## Implementation

This component is a two-step flow.

### Step 1

Collect connector name and trigger:

```txt
generateConnectorToken
```

Inputs:

- connector name
- remote network id is provided by the parent page context

### Step 2

Display:

- the exact `installCommand` string returned by the backend
- copy action/button
- warning text:
  - token expires in 24 hours
  - token works only once

### Important implementation rule

The frontend must not construct or parse the install command.

Display the returned backend string exactly as received.

### Modal/panel constraint

This repo does not currently expose a shared `Dialog` UI component in `admin/src/components/ui/`.

So this phase file should instruct the implementer to use one of:

- an inline expandable panel
- a locally implemented modal pattern
- an existing layout pattern built from available primitives

Do not assume a shared dialog exists.

Use existing primitives where possible:

- `Card`
- `Button`
- `Input`
- `Alert`
- `Separator`

---

## Verification

- Step 1 collects connector name
- mutation trigger is tied to connector creation flow
- Step 2 displays exact backend `installCommand`
- copy interaction exists
- warning text about single-use + 24h expiry is present
- no client-side install command construction exists

---

## Do Not Touch

- backend token generation logic
- backend install command construction
- generated GraphQL files by hand

---

## After This Phase

Proceed to Phase 6: codegen and final wiring.
