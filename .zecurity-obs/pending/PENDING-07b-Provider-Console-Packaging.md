---
type: adr
status: pending
id: PENDING-07b
domain: operator
priority: P1
created: 2026-07-03
related:
  - PENDING-07-Provider-Dashboard-Vision
  - PENDING-07a-Provider-Identity-and-Authorization
tags: [pending, adr, operator, provider, frontend, deployment]
---

# Pending ADR 07b — Provider Console Packaging (separate app vs shared)

> **Status: PENDING — for team discussion.** Frontend/deployment half of
> [[PENDING-07-Provider-Dashboard-Vision]]. Depends on 07a (the backend tier is separated
> regardless of this choice).

## Context / Current State

The tenant admin app (`admin/`, React + Vite + Apollo) is per-workspace. There is **no relay/
provider UI** today (only unrelated `shield_relay` chips in `AccessLog.tsx`). The provider console
is the most privileged surface in the product and will eventually host partner/reseller delegated
admin — so isolation and network lockdown are load-bearing.

**Key framing:** the backend tier is separated no matter what (07a). This ADR is only about the
*frontend packaging and deployment surface*.

## Problem — Decision Needed

One React app with role-based routing, or a separate app? And what do we ship for the alpha?

## Options

### Option A — Separate app (`provider.zecurity.in`), recommended for prod
Own build, own domain, network-locked (VPN/allowlist), shares a component library with `admin/`.
- **Pros:** blast-radius isolation (a vuln in tenant-facing code can't reach provider capability);
  can be taken off the public tenant domain; clean auth boundary; provider code never ships to
  tenant browsers. **Cons:** more boilerplate + a second deploy pipeline.

### Option B — Shared app + provider role (feature-flagged)
Reuse `admin/` with a gated provider area.
- **Pros:** fastest to build; one codebase. **Cons:** provider code ships to tenant browsers unless
  rigorously code-split; every screen must gate role+scope (one miss = cross-tenant exposure);
  can't network-isolate the provider surface. Poor fit for the highest-privilege surface.

### Option C — CLI-first for the alpha (recommended *for alpha*)
No React yet. A small CLI drives the secured `/provider` API. Build the React app (Option A) after
the model is proven.
- **Pros:** ships a secure alpha in a fraction of the time; de-risks the data/authz model before
  investing in UI; nothing to expose on the web. **Cons:** not clickable for demos; internal-only.

## Recommendation (non-binding)
- **Alpha:** Option C (CLI against the `/provider` API) if the alpha is internal — prove the secure
  model first. Use B only if you specifically need something clickable to demo *and* accept the
  code-split discipline.
- **Beta → GA:** Option A (separate, network-locked app sharing a component library). The partner
  ambition makes isolation non-optional, so don't invest in a shared-app path you'll unwind.

## Open Questions
- Is the alpha internal-only? (Yes → C is clearly right. Partners in alpha → jump to A sooner.)
- Deployment/network model for the console (VPN, IP-allowlist, separate domain, or localhost+tunnel
  for alpha)?
- Component-library sharing between `admin/` and the provider app (design system reuse) vs full
  independence?
- White-label tenant admin ever on the roadmap? (If yes, A is settled.)

## Rough Effort / Priority
**Alpha (CLI): S. Separate app: M.** P1 for the path decision; the app build is Beta.
