---
type: phase
sprint: 16
stage: 2
phase: 10
title: Admin UI — FQDN Resources
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase9-Client-Binding-Registry-Synthetic-Routing
status: code-complete — Gate 2 outstanding
tags: [sprint16, admin, frontend, react, graphql, fqdn, gate2]
---

# Sprint 16 · Phase 10 — Admin UI: FQDN Resources

> Goal: an operator can create and edit a name-addressed resource, and can see which delivery mode a
> resource uses and whether its resolver is healthy — without writing SQL.
> Depends on **Phase 9** (there is no point exposing a resource type the data plane can't serve).
> This phase closes **GATE 2**, the Stage 2 merge point.

## Current state (verified)

The GraphQL contract already exists from Phase 5 — this phase is UI only, no schema work:

```graphql
type Resource {
  host:        String!   # "" (never null) when name-addressed — kept non-null for existing clients
  hostname:    String    # null for classic IP-pinned
  resolver:    String    # raw JSON as stored: {"type":"dns","config":{…}}
  localTarget: String    # shield-delivered only
}

input CreateResourceInput {
  host:     String       # relaxed from String! in Sprint 16 — exactly one of host/hostname
  hostname: String
  resolver: String
  localTarget: String
}
```

Pages: [admin/src/pages/Resources.tsx](../../../admin/src/pages/Resources.tsx),
[ResourceDetail.tsx](../../../admin/src/pages/ResourceDetail.tsx).

⚠️ **`resolver` is a raw JSON string on the wire, not a typed input.** The UI must serialize/parse it.
Do not surface a free-text JSON box to operators as the primary control — that pushes a validation
burden onto them and the only guard is a server-side "must contain a type key".

## Tasks

### 10.1 — Create / edit a name-addressed resource
- [x] **Addressing mode is a choice, not two optional fields.** Present a radio/segmented control —
      *IP address* vs *Hostname* — and show only the relevant input. The server enforces exactly-one
      (`validateAddressing`); the UI should make violating it unrepresentable rather than surfacing
      `"host and hostname are mutually exclusive"` after a failed submit.
- [x] Resolver: a **type dropdown** (`dns` | `static`) plus type-specific config fields. Serialize to
      `{"type":"…","config":{…}}`. An "advanced / raw JSON" escape hatch is fine as a secondary control.
- [x] Resolver is only shown for the **hostname** mode. It is meaningless for a pinned IP, and offering
      it there invites the misconception that a resolver chooses delivery.
- [x] ⚠️ **Do not offer `shield` as a resolver type.** Delivery is derived server-side from
      `(status, shield_id)`; `resolver.type` only answers *how the connector finds the endpoint*. A
      dropdown containing `shield` would encode the exact conflation Phase 7 warns about, and users would
      then expect it to work.
- [x] `localTarget` is editable **only** for shield-delivered resources, with the allowed values made
      explicit (`127.0.0.1` or the shield's LAN IP). Anything else is rejected by the shield with
      `status: "failed"` — surface that as a validation hint, not as a mysterious later failure.
- [x] Regenerate typed hooks: `cd admin && npm run codegen`.

### 10.2 — Show delivery type + resolver health
- [x] A **delivery** column/badge: *Protected* (shield) vs *Connector-reachable*. This is the
      Protected/ConnectorReachable discriminator (`route_type`) surfaced for the first time — today an
      operator cannot tell them apart in the UI.
- [x] For name-addressed resources: show the **hostname** as the primary address and, where available,
      the resolver type. Never show a synthetic IP — those are client-local, the controller does not have
      them, and displaying one would imply the server knows something it does not.
- [ ] ⛔ **Not shipped — deliberate, per this task's own scope check (see below).** Resolver health /
      last error: display the typed failure classes from Phase 6 distinctly —
      `NXDOMAIN` (config error) · `resolver unavailable` (infra) · `no A record` · `dial failed`
      (resource down, resolution fine). Collapsing them into "unreachable" sends operators to the wrong
      system, which is the entire reason Phase 6 types them.
      > ⚠️ **Scope check:** there is no transport today for connector→controller resolver health. If
      > exposing it requires a new field or RPC, that is **out of this sprint** (standing rule: no new
      > RPCs; ACL/heartbeat piggyback only). Ship 10.2 without live health rather than inventing a
      > channel — show the resolver *config* and last known state, and note the gap.

## Build gate

```bash
cd admin && npm run codegen && npx tsc --noEmit
```

## 🚩 GATE 2 — E2E, Stage 2 merge point

- [ ] An FQDN resource created **through the UI** (not SQL) is reachable end-to-end. This closes the
      Phase 5 known gap: *"the GraphQL `createResource` path with `hostname` has never been executed
      end-to-end."*
- [ ] **Changing the backend IP requires no controller action, bumps no ACL version, and restarts no
      tunnel.** Verify by watching the ACL version across a DNS change:
      ```bash
      zecurity-client status      # note acl_snapshot_version
      # change the DNS A record / static resolver config target
      # re-connect through the tunnel — traffic reaches the NEW backend
      zecurity-client status      # acl_snapshot_version MUST be unchanged
      ```
      This is *the* acceptance criterion of the whole sprint. An ACL version bump here means resolution
      leaked into the control plane, and every tunnel in the fleet restarted
      (`restart_tunnel_if_running`).
- [ ] Existing IP resources produce **identical** effective ACLs (diff a snapshot before/after).
- [ ] Protected resources are still delivered via the shield and fail closed when every shield is
      offline.
- [ ] `cd relay && cargo build` still green — proves no accidental coupling to the relay.

## Verify (UI)

- [ ] Creating a resource with both an IP and a hostname is not expressible in the form.
- [ ] Creating with neither is blocked client-side, and the server error is still handled gracefully.
- [ ] A malformed resolver JSON (via the raw escape hatch) shows the server's readable message, not a
      raw constraint violation.
- [ ] Editing `hostname` or `resolver` bumps the ACL version (they *are* ACL-relevant); editing
      `localTarget` does **not** (Shield-only) — the UI should not imply otherwise.
- [ ] Existing IP resources render exactly as before.

## Notes

- ⚠️ **Stage 3 is not part of Gate 2.** After this phase an operator creates an FQDN resource and it
  works — but reaching it still needs a `hosts` entry on the client. **Typing `react.app` in a browser
  does not work until Phase 11–12.** Do not let the UI imply name resolution already happens; if a hint
  is shown, say "reachable by name once client DNS is enabled".
- Stopping here leaves a coherent, useful system: the capability (backend IPs can move freely) ships in
  Stage 2; Stage 3 only buys UX.

## Implementation Notes

**`admin/src/lib/resourceAddressing.ts` (new) is the whole contract.** Every page and both modals go
through it, so addressing rules live in one place instead of being re-derived per component:

```ts
export type Addressing =
  | { mode: 'ip';       host: string;     localTarget: string }
  | { mode: 'hostname'; hostname: string; resolver: ResolverDraft }
```

A draft carrying **both** a host and a hostname is **not constructible** — the discriminated union makes
the server's `validateAddressing` rule unrepresentable in the UI rather than surfacing
*"host and hostname are mutually exclusive"* after a failed submit. That satisfies 10.1's first bullet
at the type level, which is stronger than the radio-plus-conditional-render the task asked for.

`ResolverDraft` is `dns | static | raw`. **`shield` is deliberately absent**, and the reason is written
into the type's doc comment so it survives the next edit: delivery (`route_type`, derived server-side
from `(status, shield_id)`) and resolution (`resolver.type`) are orthogonal axes, and a dropdown
containing `shield` would encode exactly the conflation Phase 7 warns about. `raw` is the escape hatch,
**not** a resolver type — it carries JSON straight through for shapes the structured form can't express.

`parseResolverJson` round-trips anything it cannot model through `raw` rather than dropping it — a config
carrying unmodelled keys included. Losing an operator's stored value on an unrelated edit is worse than
showing them JSON.

**Delivery is derived, never inferred from `resolver`.** `deliveryOf()` mirrors the server exactly: a
bound shield plus a protected-ish status ⇒ Shield-delivered, else connector-dialed.

**A blank-address bug fixed in three places.** `host` is `""` (never null) for name-addressed resources,
so pages rendering it raw showed an empty cell. `addressOf()` falls back to the hostname; applied in
`Resources.tsx`, `GroupDetail.tsx` (×2) and `ShieldDetail.tsx`. The client CLI had the same bug — see the
Phase 9.5 post-phase fix.

**Never shows a synthetic IP.** Those are client-local and the controller does not have them; the admin
UI showing one would imply the server knows something it does not.

### Deliberate deviations

- **`localTarget` in the *create* form is offered for `ip` mode, not gated on shield-delivery.** The task
  says *editable only for shield-delivered*, which `EditResourceModal` implements literally
  (`shieldDelivered = !!resource.shield`). At **create** time no shield is bound yet, so that gate would
  hide the field permanently. It is instead offered only in `ip` mode — a name-addressed resource can
  never be shield-delivered, because `MarkProtecting` joins on `shields.lan_ip = resources.host` and
  `host` is NULL for those rows — and constrained to the two legal values by `allowedLocalTargets(host)`
  (`127.0.0.1`, or the host being entered). Returning the legal set rather than validating free text is
  what keeps an illegal value out of the form.
- **Resolver health (10.2, third bullet) is not shipped.** There is no connector→controller transport for
  it, and inventing one would breach the standing no-new-RPCs rule. This is the outcome the task's own
  scope check prescribes: *"Ship 10.2 without live health rather than inventing a channel."* The resolver
  **config** is shown; live state is not. Gap noted here rather than papered over.

### Server-side doc fix made in this phase

`controller/graph/resource.graphqls` carried a `{"config":{"server":"…"}}` example for `resolver` — but
`dns_query_name` **rejects** a `server` key outright (`InvalidResolverConfig`), because a per-resource
nameserver is not a thing the connector implements. Replaced with the three valid shapes and an explicit
note on why `server` is absent. `compiler_fqdn_test.go`'s fixture moved from `server` to `name` to match.

### Verification

`npx tsc --noEmit` clean. Lint: **19 problems project-wide, identical to HEAD** with the same per-file
distribution — zero new. There is no test runner in `admin/`, so `resourceAddressing.ts` was verified by
compiling it standalone and running **31 behavioural assertions** under node.

⚠️ The **Verify (UI)** and **Gate 2** checklists below are still open — they need a browser and a live
stack. Do not mark this phase `done` until Gate 2 passes.

---

## Post-Phase Fixes

_(none yet)_
