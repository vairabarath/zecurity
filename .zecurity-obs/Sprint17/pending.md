---
type: reference
status: pending-review
sprint: 17
tags:
  - sprint17
  - identity
  - scim
  - oidc
  - okta
  - reference-integration
---

# Pending: Okta Integration Reference vs. Zecurity's Current Identity Stack

> **Correction (post-publication, verified against source):** every "Finding A" reference below was
> written from `path.md`'s notes without independently re-checking current source, and hedged
> accordingly at the time ("Marked unchecked in `path.md` at last read — treat as still open unless
> re-confirmed"). Finding A has since been directly re-checked against
> `controller/internal/scim/directory_service.go` and `controller/internal/identity/revocation.go` and
> is **already fixed** (commit `b5c1bce`, with a passing regression test) — `path.md`'s checklist was
> simply never updated after the fix landed. Only Finding C (the mapping round-trip probe) is still
> genuinely open. See `Member1-Go/Phase10-Reference-Integration-Hardening.md`, "Verified status," for
> the full evidence trail and the corrected, actionable scope. Left uncorrected below to preserve the
> record of what was actually checked when this document was written.

**Status:** Reference / investigation document only. **Nothing described in §8–§12 has been
implemented.** No Zecurity source, migrations, GraphQL schema, or configuration was modified while
producing this document.

**Context:** A working Okta ↔ Twingate identity-provider integration (Twingate is a third-party ZTNA
product, unrelated codebase) was configured end-to-end on an Okta trial org
(`trial-3724025.okta.com`) and a Twingate network (`sthiyaseelank326.twingate.com`), covering both
OIDC login and SCIM provisioning. This document reconstructs exactly what was configured and uses it
as a concrete reference point to compare against Zecurity's *existing* identity/SCIM engine — which
already covers most of the same ground (Sprint 17 / ADR-025). This is not a build-from-scratch
situation; the real gaps are narrow and mostly already tracked in `path.md`.

---

## 1. The Reference Setup — What Was Configured, In Order

1. **Twingate Admin → Settings → Identity Providers**
   (`https://sthiyaseelank326.twingate.com/settings/idp`). Starting state: one existing "Social
   Login" provider (Google/Microsoft/LinkedIn/GitHub), one user provisioned through it.
2. **"Add Identity Provider" → Okta** selected from a dropdown also listing Google Workspace, Entra
   ID, OneLogin, JumpCloud.
3. A **"Connect Okta" dialog** opened with three required fields — **Okta Domain**, **Client ID**,
   **Client Secret** — and a **"Sign in with Okta"** button that stays disabled until all three are
   filled, plus a link to Twingate's own Okta configuration guide.
4. On the **Okta side** (`trial-3724025-admin.okta.com`), the Twingate application was present under
   Applications, status **Active**, with several users/groups already assigned.
5. The application's **Sign On** tab exposed the **Client ID** and **Client Secret** needed for step
   3. The org's **Okta Domain** was confirmed as `trial-3724025.okta.com` — distinct from the
   admin-console hostname `trial-3724025-admin.okta.com`; the two must not be conflated when filling
   in an OIDC connection.
6. Okta Domain, Client ID, and Client Secret were entered into Twingate's "Connect Okta" dialog —
   **all three values originate from Okta**; Twingate only consumes them.
7. **"Sign in with Okta"** was submitted. This is a live login round-trip through Okta's OIDC
   authorization endpoint, not a passive discovery check — Twingate reported the connection verified
   and advanced to a second step automatically.
8. **"Configure SCIM Provisioning" dialog** (Twingate) appeared as the next step of the same wizard:
   - **SCIM Endpoint**: `https://sthiyaseelank326.twingate.com/api/scim/v2/`
   - **SCIM Token**: an opaque bearer token, explicitly labeled **shown once** — Twingate's own UI
     warns it cannot be retrieved again once the dialog is closed.
9. **Okta → Twingate app → Provisioning tab**: initially read "Provisioning is not enabled." Enabling
   it opened a form with an **"Enable API integration"** checkbox, an **API Token** field, and an
   **Import Groups** checkbox (pre-checked). Notably **no separate Base URL field was present** —
   Okta's pre-built Twingate app (an Okta Integration Network / OIN listing) has the SCIM base URL
   built into the app template; only the bearer token needed to be supplied.
10. The **SCIM Token from step 8** was entered as the **API Token**, "Test API Credentials" returned
    **"Twingate was verified successfully!"**, and Save was submitted.
11. **Post-save confirmation on the Provisioning tab is not verified** — a page reload immediately
    afterward triggered an Okta re-authentication redirect, and the resulting state was not confirmed
    before this record was compiled. Treat SCIM provisioning as *configured but not independently
    re-confirmed active* until the Provisioning tab is checked again.

No errors were encountered on the Twingate side. The only anomaly was the reload-triggered Okta
re-auth in step 11, which is ordinary Okta admin-console session behavior, not a configuration
failure.

---

## 2. Okta-Side Configuration (What Was Set Up)

| Item | Value / Observation |
|---|---|
| Okta org | `trial-3724025` (Free Trial plan; 3 of 10 active users) |
| Okta admin console domain | `trial-3724025-admin.okta.com` |
| **Okta org/API domain** (entered into Twingate) | `trial-3724025.okta.com` — differs from the admin console hostname; conflating the two would misconfigure the integration |
| Application source | Okta Integration Network (OIN) pre-built app — added via *Applications → Browse App Catalog → search "Twingate"*, per Twingate's own documented instructions, rather than a hand-built generic OIDC app |
| Application type | OIDC-based OIN app ("OIDC – OpenID Connect", "Web Application" per Twingate's docs) |
| Application status | Active |
| Client ID | `0oa16w8p8wiwsBeIP698` — an Okta application ID, not a secret |
| Client Secret | `<CLIENT_SECRET_REDACTED>` — obtained from the app's Sign On tab; entered into Twingate's Client Secret field; never written to this document |
| Assignments observed | 3 identities: 1 individual assignment, 2 group-based assignments. Not verified whether these pre-existed or were made as part of this configuration pass. |
| Grant type | Not directly inspected; documentation-derived as Authorization Code + PKCE, consistent with Twingate's stated "SP-Initiated SSO via OIDC." |
| Sign-in redirect URI | Not verified — Twingate's docs state the OIN app template pre-fills this; not read from the Okta UI. |
| Issuer / authorization / token / JWKS / discovery endpoints | Not verified directly against Okta's own `/.well-known/openid-configuration`; standard Okta layout is `https://trial-3724025.okta.com/oauth2/v1/...`, inferred from general product documentation, not observed here. |
| Scopes / claims | Not verified — the exact scope string on the Okta app side was not inspected. |
| SCIM (Provisioning tab) | API-token-based, not OAuth — a single bearer token (the SCIM Token minted by Twingate, §3) entered into Okta's "API Token" field. Base URL not manually entered (built into the OIN app template). "Import Groups" left checked. Credentials tested successfully; **post-save state not independently re-confirmed** (§1.11). |

---

## 3. Twingate-Side Configuration (What Twingate Required From Okta)

### 3.1 "Connect Okta" dialog (OIDC login)

| Field | Purpose | Source |
|---|---|---|
| Okta Domain | Org hostname, used to reach the OIDC discovery document | Okta admin console |
| Client ID | OIDC client identifier | Okta app's Sign On tab |
| Client Secret | OIDC client secret, for server-side token exchange | Okta app's Sign On tab |

"Sign in with Okta" performs an **active login round-trip** to verify the credentials — stronger than
a passive discovery-endpoint probe.

### 3.2 "Configure SCIM Provisioning" dialog (directory sync)

Twingate generates and displays, one-time:

| Field | Value | Notes |
|---|---|---|
| SCIM Endpoint | `https://sthiyaseelank326.twingate.com/api/scim/v2/` | Workspace-scoped by subdomain |
| SCIM Token | opaque bearer token | **Shown exactly once** — Twingate warns it cannot be retrieved again after the dialog closes |

Twingate treats login (OIDC) and provisioning (SCIM) as two capabilities of a single IdP connection
object, configured back-to-back in one wizard, backed by two independent credential sets (OIDC
client id/secret vs. a SCIM bearer token).

### 3.3 Login/authentication flow after configuration

Not independently exercised end-to-end as a non-admin user login. The one live login that did run
(§1.7) succeeded without a credential prompt, since the browser already held a valid Okta session.

---

## 4. End-to-End Authentication Flow

### 4.1 Confirmed
- Redirect through Okta → Twingate reported the connection verified and advanced to the SCIM dialog
  automatically.

### 4.2 Documentation-derived (not independently packet-verified)
```
User (browser)
  ↓ clicks "Sign in with Okta" / launches Twingate Client
Twingate
  ↓ redirects to Okta's OIDC authorization endpoint (SP-initiated)
Okta
  ↓ authenticates the user (existing session or credential prompt)
  ↓ checks the user is assigned to the Twingate app
  ↓ issues an authorization code, redirects back to Twingate's callback
Twingate
  ↓ exchanges the code for tokens at Okta's token endpoint
  ↓ validates the id_token (issuer, audience, signature via JWKS)
User authenticated into Twingate
```

### 4.3 Inferred, not verified
- Whether Twingate uses PKCE and/or a nonce on this flow.
- Exact claim(s) Twingate maps to its internal user identity (email vs. sub).
- Whether Twingate also reads a `groups` claim from the id_token in addition to SCIM sync.

---

## 5. Current Zecurity Identity Architecture (Inspected, Not Modified)

Zecurity's controller is a single Go service (`controller/`) internally organized into narrow
packages under `controller/internal/`, each with a clear ownership boundary (per Sprint 17's own
"Conflict Zones" table). Relevant to identity/auth:

| Package | Responsibility |
|---|---|
| `controller/internal/auth` | Session/login service: OIDC login initiation (`oidc.go`), callback handling (`callback.go`), token exchange (`exchange.go`), id_token verification (`idtoken.go`), refresh (`refresh.go`), discovery (`discovery.go`), Google-specific adapter (`google_provider.go`), generic-provider factory (`idp_adapter.go`), config (`config.go`). |
| `controller/internal/auth/providers` | Protocol adapters behind a common interface: `providers/oidc.go` is a **generic, provider-agnostic OIDC adapter** (discovery + PKCE + nonce + JWKS-cached id_token verification), used for Okta/Entra/JumpCloud/Keycloak/any-OIDC connections alike. |
| `controller/internal/idp` | The identity connection store: `idp.Connection` (`store.go`) models one persisted OIDC/SAML IdP connection per workspace (or platform-global), with `Provider`, `Issuer`, `ClientID`, `ClientSecret` (encrypted at rest), `DiscoveryURL`, `Scopes`, `DomainHint`, `SubjectClaim`, `ScimIdentifier`, `ScimEnabled`, `LastSyncAt`. `Provider` field comment literally lists `"google","github","okta","entra","oidc"` as example values. |
| `controller/internal/identity` | The identity lifecycle/session-effect boundary: `resolver.go`/`linker.go`, `lifecycle.go`, `revocation.go` (`Revoker`), `device_trust.go` (the `SideEffectSink`/outbox event contract), `principal.go`, `service.go`. Shared by both the login path (`auth`) and the SCIM provisioning path (`scim`). |
| `controller/internal/scim` | The SCIM 2.0 provisioning engine (Sprint 17 / ADR-025): `directory_service.go`, `provisioner.go`, `users.go`, `groups.go`, `conflict.go`, `mapping.go`, `profiles.go` (built-in provider profiles — Okta/Entra/JumpCloud/Keycloak/Generic — plus per-connection overrides), `token_store.go`, `sync_instance.go`, `validation.go` (the `MappingGate`), `middleware.go`. |
| `controller/internal/outbox` | Durable outbox (Sprint 18, merged); SCIM only calls `outbox.Enqueue`. |
| `controller/internal/permission` | Fine-grained permission primitives (e.g. `identity.mapping.break_glass`), deliberately not role-based. |
| `controller/internal/tenant` | Workspace/tenant context propagation. |
| `controller/internal/policy` | ACL compilation/caching + `Notifier.NotifyPolicyChange(workspaceID)`. |
| `controller/internal/audit` | Audit event recording — applied inconsistently on identity paths (see §6/§7). |
| `controller/graph` | GraphQL API: `idp.graphqls`, resolvers under `graph/resolvers/idp*.go`. |
| `admin/src/pages/IdpConnections.tsx`, `admin/src/components/idp/CreateIdpConnectionDialog.tsx` | Admin UI: connection list page and the "Add Identity Provider"-equivalent dialog, asking for Issuer/Client ID/Client Secret/discoveryUrl/scopes/domainHint, with `okta`/`entra`/`jumpcloud`/`keycloak`/`oidc` as provider-type options. |

No monolithic identity service exists or is implied anywhere in the codebase or planning docs —
identity work is already split across `auth`, `idp`, `identity`, and `scim`, plus the `graph` API
layer and `admin` frontend. Any Okta-related work belongs inside this existing boundary set — see §8.

---

## 6. Current Zecurity Capabilities Inventory

| Capability | Current Zecurity Status | Location | Notes |
|---|---|---|---|
| Password/session auth | Present | `controller/internal/auth` (JWT access+refresh via Valkey/Redis) | HS256, `JWT_SECRET` ≥32 bytes enforced at boot |
| Google OIDC login (platform IdP) | Present | `auth/google_provider.go`, `auth/oidc.go` | The original/bootstrap login path |
| **Generic enterprise OIDC login** (Okta/Entra/JumpCloud/Keycloak/any-OIDC) | **Present** | `auth/providers/oidc.go`, `auth/idp_adapter.go`, `idp.Connection` | Full PKCE + nonce + signed-state CSRF + per-issuer discovery/JWKS caching + `email_verified` enforcement — functionally equivalent to the reference Okta OIDC connection in §3.1 |
| Multi-connection selection at login | Present | `InitiateAuth(ctx, provider, workspaceName, connectionID)` | `connectionID` selects a specific workspace IdP connection; falls back to the platform IdP |
| OIDC connection admin CRUD | Present (backend + in-progress frontend) | `graph/idp.graphqls` (`createIdpConnection`, `updateIdpConnection`), `admin/src/components/idp/CreateIdpConnectionDialog.tsx` | Frontend dialog is uncommitted/new on this branch |
| Connection health/status probing | Present, partial | `testIdpConnection` mutation, `OIDCProvider.Probe()` | Runs an OIDC discovery probe + mapping-config validation; does not run a full login round-trip the way the reference "Sign in with Okta" button does |
| SSO / SCIM provider profiles | Present | `controller/internal/scim/profiles.go` | Okta/Entra/JumpCloud/Keycloak/Generic built-ins + per-connection overrides |
| SCIM 2.0 Users provisioning | Present | `scim/provisioner.go`, `scim/users.go` | RFC 7644 envelopes, `meta.version`, filtering; known gap: `users` table lacks `name`/`displayName`/`title`/`department` columns (tracked in `path.md` Phase 5) |
| SCIM 2.0 Groups sync | Present | `scim/groups.go` | Origin-scoped (`origin='scim'`), connection-scoped; out-of-order member → `404` |
| SCIM deprovisioning | Present, with a documented open defect | `scim/directory_service.go` (`Deprovision`) | Per `path.md` Finding A: `Revoker.BumpGenerationTx` does not call `afterBump`, so live sessions may not be proactively killed and no audit event is emitted on the SCIM deprovision path; `DELETE` does not purge `group_members`. Marked unchecked in `path.md` at last read — treat as still open unless re-confirmed. |
| SCIM bearer token management | Present | `scim/token_store.go` | HMAC-SHA256 hashed, dual-token rotation, GraphQL mint/rotate/revoke, plaintext shown once — directly analogous to the reference Twingate SCIM token UX in §3.2 |
| SCIM base URL exposure to admin | Planned, frontend not confirmed shipped | Sprint 17 FE-1 | Described as "buildable now" in `path.md`, not confirmed landed |
| Identity conflict handling (JIT/manual vs SCIM collision) | Present | `scim/conflict.go`, `scim_identity_conflicts` table | Keyed on canonical identity key, never email; admin queue is Sprint 17 FE-4 |
| Break-glass permission for identity mapping override | Present | `controller/internal/permission`, `identity.mapping.break_glass` | Dedicated permission, ADMIN alone insufficient |
| Connection lifecycle (disable/delete) | Present | `scim/directory_service.go` (M1-9) | Uses `Revoker.BumpGeneration` (non-Tx variant), which does call `afterBump` — not affected by Finding A |
| Identity Health surfacing | Present | `DirectoryService.IdentityHealth` | Healthy/Delayed/Disconnected/Disabled derivation from `last_sync_at` |
| Mapping-gate active probe (POST→GET→verify→DELETE round-trip) | **Not wired** (`path.md` Finding C) | `scim/validation.go` `MappingGateResult.WithRoundTrip` | No production caller as of `path.md`'s last audit; SCIM can only be enabled via break-glass override |
| Tenant-scoping of SCIM writes | Present, fixed after a prior defect | `scopedProvisioningOwner`, `Revoker.bump` (`AND tenant_id = $2`) | `path.md` Finding B — cross-tenant generation-bump bug, fixed 2026-08-26, with a regression test |
| Groups (Zecurity-native, non-SCIM) | Present | `admin/src/components/groups/GroupOriginLabel.tsx` (new/uncommitted) | Origin-labeling UI for SCIM vs Local vs System groups (Sprint 17 FE-5) |
| RBAC / roles | Present | `@hasRole(roles:[ADMIN])` directives; `controller/internal/permission` | Two-tier: coarse role directives + fine-grained permission table |
| Audit logging | Present, inconsistently applied on identity paths | `controller/internal/audit` | Per `path.md`: only `conflict.go` and `provisioner.go` call `Publish`/`RecordTx` in `internal/scim`; `Deprovision` does not |
| Config/secrets management | Present | `Config` struct pattern, `ClientSecret` marked "encrypted at rest… NEVER expose outward" | Encryption-at-rest primitive itself not independently inspected |
| SCIM conformance testing | Absent | — | `path.md`: "No conformance suite, runner, or fixture exists anywhere in the repo" |
| Live Okta/Entra interop testing | Absent / previously blocked | — | `path.md`: "PENDING tenant access" — access to a live Okta trial org (as used for the reference setup in this document) is the kind of access previously missing, though it was not used to test Zecurity itself |

---

## 7. Reference Integration vs. Zecurity — Gap Analysis

| Reference Requirement (Okta/Twingate) | Current Zecurity Support | Gap | Required Zecurity Change | Relevant Package | Priority |
|---|---|---|---|---|---|
| Generic OIDC login connection (issuer, client id/secret, discovery URL, scopes) | **Supported** | None functionally — `idp.Connection` + `providers/oidc.go` already model this | — | `idp`, `auth/providers` | — |
| Admin UI to create an OIDC connection ("Connect Okta"-equivalent dialog) | **Supported (in progress, uncommitted on this branch)** | Confirm `CreateIdpConnectionDialog.tsx` is finished/merged and wired to `createIdpConnection` | Verify + finish FE-0 (already tracked) | `admin/src/components/idp` | Low |
| Live "Sign in with Okta"-style login verification in the admin UI | **Partial** — `testIdpConnection` runs a discovery probe, not a full login | The admin console never drives a real login the way the reference dialog does | Decide if worth building, or rely on discovery probe + first real user login | `graph/resolvers/idp*.go`, `auth` | Medium — UX gap, not correctness gap |
| SCIM endpoint + shown-once bearer token, minted from the identity-provider admin UI | **Supported** | None functionally | Confirm FE-1's token-mint panel is live | `scim/token_store.go`, FE-1 | Low |
| SCIM Users provisioning (create/update via directory push) | **Supported**, with a documented schema gap — `users` lacks `name`/`displayName`/`title`/`department` | Directory-owned attribute writes for those fields will `400` | Extend `users` schema (already tracked: ADR-025 §5) | `scim/users.go`, migrations | Medium |
| SCIM deprovision must kill live sessions + emit audit event synchronously with the status change | **Documented as broken** (`path.md` Finding A) | `Revoker.BumpGenerationTx` does not invoke `afterBump`; SCIM-driven suspend/delete may leave sessions alive until next token refresh, no audit trail | Wire the post-commit `afterBump` call on the SCIM deprovision path; purge `group_members` on hard delete | `scim/directory_service.go`, `identity/revocation.go` | **High** — already flagged as a live defect in the repo's own docs |
| Mapping validated by an active round-trip probe before SCIM can be "proven" | **Not wired** (`path.md` Finding C) | `MappingGateResult.WithRoundTrip` has no caller; SCIM can only be turned on via break-glass | Wire the round-trip probe into `TestIdpConnection` | `scim/validation.go`, `graph/resolvers/idp.resolvers.go` | High (already flagged as a sprint-completion blocker) |
| Distinct redirect/callback per connection (reference IdP app has its own registered redirect URI) | **Different design, not a gap** — Zecurity uses one process-wide `cfg.RedirectURI` and disambiguates the in-flight login via `connection_id` stored server-side against the CSRF `state` token | None — intentional, arguably cleaner than per-app redirect URI management across many IdPs | — | `auth/oidc.go` | — |
| Pre-built vendor app in the IdP's own app catalog (so the customer only supplies a token, no base URL) | **Not applicable to a self-hosted/enterprise product** | Admins configuring Zecurity in Okta would use a generic SCIM app template, requiring both a base URL and a token | Onboarding copy must tell the customer to enter both — not a code gap | FE-1 (help text), docs | Low |
| Group import via SCIM ("Import Groups") | **Supported** | None | — | `scim/groups.go` | — |
| Endpoint + token surfaced together, generated automatically after OIDC verification succeeds, in a single guided wizard | **Different UX sequencing** — OIDC connection creation and SCIM token minting are two separate admin actions/mutations | UX sequencing difference, not a functional gap | Optionally combine into a guided setup wizard for parity | `admin/src/components/idp` (not yet planned) | Low — nice-to-have, not tracked |
| Cross-tenant isolation on every SCIM-token-scoped write | **Fixed** (`path.md` Finding B) | None currently known | — | `scim/*` | — |
| Nonce/state/PKCE on the login flow | **Supported**, arguably more rigorous than what was directly observed of the reference flow | None | — | `auth/oidc.go` | — |

**Summary judgment:** the functional gap between the reference Okta/Twingate requirements and what
Zecurity already has is small — Zecurity's `idp`/`auth/providers`/`scim` stack already covers the same
ground (generic OIDC login + SCIM provisioning with token-based auth) at a comparable or greater level
of protocol rigor. The real, current gaps are the ones already identified in Sprint 17's own
verification pass (`path.md` Findings A and C) — not new gaps surfaced by this reference exercise. The
walkthrough is useful mainly as a UX/sequencing reference and as independent confirmation that the
field set Zecurity already asks for (issuer/domain, client id, client secret, discovery URL, scopes)
matches what a real enterprise IdP integration needs.

---

## 8. What Zecurity Would Need to Implement (By Service Boundary)

**Not implemented. Pending plan only.** Organized by existing package/service boundary — no new
service is proposed.

### `controller/internal/identity` (+ `internal/scim/directory_service.go`)
- Fix `Revoker.BumpGenerationTx` (or its caller in `scim/directory_service.go`) to invoke `afterBump`
  post-commit, so SCIM-driven suspend/delete actually kills live sessions and emits the audit event —
  closes `path.md` Finding A.
- Add `group_members` purge on hard delete, per ADR-025 §5.

### `controller/internal/scim` (`validation.go`, `graph/resolvers/idp.resolvers.go`)
- Wire `MappingGateResult.WithRoundTrip` into `TestIdpConnection` (or the Phase 5 provisioning path)
  so a POST→GET→verify→DELETE probe user round-trip actually runs and can move `MappingState` to
  `proven` — closes `path.md` Finding C.

### `controller/internal/scim` (`users.go`, migrations)
- Extend the `users` table with `name`/`displayName`/`title`/`department` (or equivalent) if
  directory-owned attribute writes for those fields are required — already tracked in ADR-025 §5.

### `admin/src/components/idp`, `admin/src/pages/IdpConnections.tsx`
- Confirm/complete FE-0 (Create OIDC connection dialog) and FE-1 (SCIM config: mapping fields,
  base-URL box, token mint/rotate/revoke panel) per Sprint 17's own tracked plan.
- Optional, not currently tracked anywhere: a single guided multi-step wizard combining OIDC
  connection creation and SCIM token minting into one flow, if product wants that sequencing.

### Documentation / admin-facing onboarding copy (no code)
- Because Zecurity is not an Okta-catalog (OIN) app, any customer-facing "how to connect Okta to
  Zecurity" guide must explicitly instruct the admin to create a generic SCIM app in Okta and enter
  both the SCIM base URL and the bearer token.

No changes are proposed to `controller/internal/auth`, `controller/internal/idp`,
`controller/internal/permission`, `controller/internal/policy`, `controller/internal/outbox`, or
`controller/internal/tenant` — the reference walkthrough did not surface any gap attributable to those
packages.

---

## 9. Configuration Contract

### What configuring Okta itself requires
- Okta Domain / org hostname (e.g. `trial-3724025.okta.com` — not the `-admin` console hostname)
- An OIDC app (or, if pursued, an Okta-catalog app) with:
  - Client ID (generated by Okta)
  - Client Secret (generated by Okta) — `<CLIENT_SECRET_REDACTED>` pattern in any future logs/docs
  - Sign-in redirect URI (Zecurity's process-wide callback URL — see §7, a single value across all
    connections, not per-connection)
- If SCIM is desired: a bearer token entered as Okta's "API Token" for the Provisioning integration

### What already exists as Zecurity-side configuration (verified in `idp.Connection` / `CreateIdpConnectionInput`)
| Field | Generated by | Entered by |
|---|---|---|
| `provider` | — | Admin selects (`okta`/`entra`/`jumpcloud`/`keycloak`/`oidc`) |
| `displayName` | — | Admin |
| `issuer` | Okta (the org's issuer URL) | Admin, copied from Okta |
| `clientId` | Okta | Admin, copied from Okta |
| `clientSecret` | Okta | Admin, copied from Okta — write-only, never re-displayed |
| `discoveryUrl` | Optional; Zecurity derives `<issuer>/.well-known/openid-configuration` if omitted | Admin (optional) |
| `scopes` | Zecurity default `"openid email profile"` if omitted | Admin (optional) |
| `domainHint` | — | Admin (optional) |
| `subjectClaim` / `scimIdentifier` | — | Admin, via `UpdateScimConfigInput` (separate mutation) |
| SCIM token (plaintext) | Zecurity generates, shown once | Admin copies it into Okta, not the reverse |

Values that should never be manually entered if discovery can provide them: authorization endpoint,
token endpoint, JWKS URI — all already derived automatically from `discoveryUrl` /
`<issuer>/.well-known/openid-configuration` by `OIDCProvider.discover()`. This matches the reference
dialog's own behavior (it never asked for those three endpoints directly either — only issuer/domain,
client id, client secret).

---

## 10. Security Requirements

- **Client secret protection**: consistent pattern on both sides — Okta shows it once per rotation on
  its Sign On tab; Zecurity's `idp.Connection.ClientSecret` doc comment states "decrypted; empty for
  managed; NEVER expose outward" and `CreateIdpConnectionInput.clientSecret` is documented
  write-only/encrypted-at-rest. The actual encryption-at-rest primitive was not independently
  verified.
- **HTTPS**: enforced throughout the reference setup; Zecurity's `RedirectURI` doc comment likewise
  assumes `https://`.
- **State (CSRF) validation**: Zecurity already implements HMAC-signed state (`generateSignedState`/
  `verifySignedState`, `auth/oidc.go`).
- **Nonce validation**: Zecurity's `providers/oidc.go` generates and can enforce a nonce; not
  verified whether the reference flow does the same.
- **PKCE**: Zecurity generates a 64-byte verifier/S256 challenge and enforces it; not verified whether
  the reference flow uses PKCE.
- **Issuer/audience validation**: Zecurity's `verify()` in `providers/oidc.go` explicitly checks
  `jwt.WithAudience(p.clientID)` and `jwt.WithIssuer(p.issuer)`, plus rejects unverified emails.
- **JWKS/key rotation**: Zecurity caches JWKS per-issuer with a 1-hour TTL and re-fetches on unknown
  `kid`.
- **Token expiration**: `jwt.WithExpirationRequired()` enforced in Zecurity's verifier.
- **Redirect URI validation**: implicit on the IdP side; not stress-tested against a mismatched
  redirect URI in this exercise.

---

## 11. Testing Requirements

### Unit tests
- OIDC adapter: issuer mismatch on discovery document → rejected (already covered per source read;
  not independently re-run).
- id_token verification: expired token, wrong audience, wrong issuer, unverified email, missing `kid`
  — confirm existing coverage in `providers/oidc_test.go`.
- SCIM mapping gate: `WithRoundTrip` states (unproven/proven/failed) once wired (§8).

### Integration tests
- SCIM deprovision → session actually killed + audit event actually recorded, once Finding A is
  fixed.
- Full mapping-gate round-trip against a mapping-validation fake IdP.
- Provider-not-assigned equivalent: a user authenticated by the IdP but not assigned to the Zecurity
  app — verify Zecurity's login path fails closed.

### End-to-end tests
- Successful login through a real (or realistically faked) generic-OIDC IdP connection, exercising
  the full `InitiateAuth` → redirect → callback → session-issuance path.
- Invalid state / invalid nonce / tampered id_token rejected end-to-end.
- SCIM provision → conflict (JIT collision) → admin Accept-Link → subsequent SCIM update succeeds.
- SCIM deprovision → subsequent authenticated request with the old session is rejected (requires
  Finding A fixed first).
- Live interop against a real Okta (and ideally Entra) tenant — `path.md` already flags this as
  ungated now that live tenant access exists.

---

# Pending Zecurity Implementation

- **Nothing from this investigation has been implemented.** No Zecurity source file, GraphQL schema,
  migration, or configuration was changed while producing this document.
- This document is a **findings/reference document** only, meant to save a future implementer from
  re-deriving the same field-by-field comparison.
- The Okta/Twingate integration described here is used purely as an **external reference
  implementation** — a concrete example of what a real enterprise customer's Okta-side setup and
  admin-facing OIDC/SCIM configuration look like — not as a spec Zecurity must copy.
- **Zecurity's existing microservice-internal architecture must be preserved.** Identity-related work
  already lives across four distinct packages (`auth`, `idp`, `identity`, `scim`) plus the `graph` API
  layer and `admin` frontend; nothing here proposes collapsing that into a new monolithic identity
  service.
- **Future implementation should follow the existing service boundaries and patterns** documented in
  §5 and used throughout Sprint 17's own `path.md` (one engine + provider profiles, not per-provider
  handler types; `SideEffectSink` as the seam to the outbox; fine-grained permissions over role
  overloading; canonical-identity-key resolution never keyed on email).
- **The bulk of what the reference setup required is already implemented in Zecurity.** The concrete,
  actionable gaps identified are the same two the repository's own Sprint 17 verification pass already
  flagged as open (`path.md` Finding A — SCIM deprovision session/audit gap; Finding C — unwired
  mapping round-trip probe), plus one already-tracked schema extension and some UX/documentation
  nice-to-haves (§7, §8). No item in §8 should be treated as scoped, estimated, or scheduled by this
  document — **the actual implementation should be planned/reviewed separately, by the team, before
  any code is written.**
