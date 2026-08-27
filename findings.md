# Findings: Twingate + Okta Setup, and Its Implications for Zecurity

**Status:** Reference / investigation document only. **Nothing described in §9–§13 has been
implemented.** No Zecurity source, migrations, GraphQL schema, or configuration was modified while
producing this document.

**Context:** During this session, an Okta ↔ Twingate identity-provider integration was set up live
(a third-party ZTNA product, unrelated codebase) via the Orca CLI browser-automation tool, on the
Okta trial org `trial-3724025` and the Twingate network `sthiyaseelank326.twingate.com`. This
document reconstructs exactly what was done, then uses it as a concrete reference point to inventory
Zecurity's *existing* (already-implemented) equivalent capability and identify real gaps — Zecurity
already has a substantial generic-OIDC-login and SCIM-provisioning engine; this is not a
build-from-scratch situation.

---

## 1. The Actual Twingate + Okta Setup — Step by Step (Observed/Confirmed)

Reconstructed in the order performed, from the session transcript.

1. **Restored the Orca embedded-browser session** by copying live Google-account cookies from a
   local Firefox profile and injecting them into the Orca browser via `orca-ide cookie set`
   (unrelated to Okta/Twingate mechanics — session bootstrapping only, out of scope for this
   document beyond noting it's how the browser ended up authenticated as the operator's Google
   account).
2. **Navigated to Twingate Admin → Settings → Identity Providers**
   (`https://sthiyaseelank326.twingate.com/settings/idp`). Observed state: 1 existing "Social Login"
   provider (Google/Microsoft/LinkedIn/GitHub), 1 user provisioned through it.
3. **Clicked "Add Identity Provider"** → a dropdown menu appeared listing: **Okta**, Google
   Workspace, Entra ID, OneLogin, JumpCloud. Selected **Okta**.
4. Twingate opened a **"Connect Okta" dialog** with three input fields — **Okta Domain**, **Client
   ID**, **Client Secret** — and a disabled **"Sign in with Okta"** button (enabled only once all
   three fields are non-empty), plus a link to Twingate's own "Okta Configuration Guide"
   documentation.
5. Followed that guide (`twingate.com/docs/okta-configuration` → `okta-app-configuration`) to
   configure the **Okta side first**:
   - In the Okta Admin Console (`trial-3724025-admin.okta.com`), the **Twingate app was already
     present** under Applications, in **Active** status, with 3 users/groups already assigned
     (this had evidently been done in an earlier, unobserved part of the session — not reconstructed
     here as a "step performed," just the state found).
   - Opened the Twingate app's **Sign On** tab, which exposed a **Client ID** and **Client Secret**
     field.
   - Read the org's **Okta Domain** by inspecting the page (`trial-3724025.okta.com` — distinct from
     the admin-console hostname `trial-3724025-admin.okta.com`).
6. **Filled the Twingate "Connect Okta" dialog**: Okta Domain, Client ID, Client Secret — all three
   values copied **from Okta's Sign On tab into Twingate's dialog**, i.e. Okta was the source of
   truth for all three credentials; Twingate only consumed them.
7. Clicked **"Sign in with Okta"**. This triggered an OAuth/OIDC redirect through Okta and back; it
   completed automatically because the browser already held a valid Okta admin session. Twingate then
   reported the connection as verified and advanced the wizard to a second dialog.
8. **"Configure SCIM Provisioning" dialog** (Twingate side) — appeared automatically as the *next*
   step of the same wizard, not a separately chosen action. It displayed:
   - **SCIM Endpoint**: `https://sthiyaseelank326.twingate.com/api/scim/v2/`
   - **SCIM Token**: a long opaque bearer token, explicitly labeled **shown once** ("Make sure to
     copy the token now. Once you close this dialog, you won't be able to see it again!").
   - A link to Twingate's separate SCIM configuration guide
     (`twingate.com/docs/okta-scim-configuration`) and a **"Close & Finish"** button.
9. **Configured SCIM on the Okta side**, in the same Twingate app, **Provisioning** tab:
   - It initially read **"Provisioning is not enabled"** with a **"Configure API Integration"**
     action.
   - Clicked it → a form appeared with an **"Enable API integration"** checkbox, an **API Token**
     field, and an **Import Groups** checkbox (pre-checked). Notably: **no separate "Base URL" field
     was present** — Okta's pre-built Twingate app (an Okta Integration Network / OIN listing) has
     the SCIM base URL built into the app template; only the bearer token needed to be supplied.
   - Checked "Enable API integration", pasted the **SCIM Token from step 8** into the **API Token**
     field, clicked **"Test API Credentials"** → Okta responded **"Twingate was verified
     successfully!"**.
   - Clicked **Save**.
10. **Verification of the save was inconclusive at the point the session was interrupted.** Reloading
    the Provisioning tab triggered an Okta re-authentication redirect
    (`trial-3724025.okta.com/oauth2/v1/authorize?...`), and the follow-up check of that redirect's
    outcome was not completed before the user interrupted the session with the request that produced
    this document. **This step is explicitly marked "not verified"** — see §5 for the honest
    automation/verification boundary.

No errors were encountered on the Twingate side. On the Okta side, the only anomaly was the
reload-triggered re-auth in step 10, which is common Okta admin-console behavior after a period of
inactivity, not a configuration failure.

---

## 2. Okta-Side Configuration (What Was Actually Set Up in Okta)

| Item | Value / Observation |
|---|---|
| Okta org | `trial-3724025` (Free Trial plan, "gomail-edu-trial-3724025"; 30-day trial; 3 of 10 active users) |
| Okta admin console domain | `trial-3724025-admin.okta.com` |
| **Okta org/API domain** (the one entered into Twingate) | `trial-3724025.okta.com` — **note this differs from the admin console hostname**; conflating the two would misconfigure the integration |
| Application source | **Okta Integration Network (OIN) pre-built app** — "Twingate" was added via *Applications → Browse App Catalog → search "Twingate"*, not built by hand as a generic OIDC app. Not independently re-verified in this session that it was added via the catalog vs. pre-existing; inferred from Twingate's own documented instructions and the absence of a manual "Create App Integration" step in the observed transcript. **Documentation-derived, not directly observed being created.** |
| Application type | OIDC-based OIN app (per Twingate's docs: "OIDC – OpenID Connect", "Web Application") |
| Application status | Active |
| Client ID | `0oa16w8p8wiwsBeIP698` — this is an Okta **application ID**, not a secret; safe to record verbatim |
| Client Secret | `<CLIENT_SECRET_REDACTED>` — obtained from the app's **Sign On** tab in Okta; entered into Twingate's "Connect Okta" dialog **Client Secret** field. Never displayed in the Orca browser snapshot text used for documentation; read via a scoped `get --what value` call, explicitly authorized by the operator before extraction. |
| Assignments observed | 3 identities: 1 individual assignment (`bofeci7824@bocably.com`), 2 group-based assignments (`sathiyaseelank326@gmail.com`, `xiyo@gomail.edu.pl`). **Not verified**: whether these assignments were made during this session or pre-existed — the Assignments tab was only observed, not edited. |
| Grant type | Not directly inspected in the Okta UI (no "General" tab review was performed). **Inferred** (not verified) to be Authorization Code + PKCE, consistent with Twingate's documented flow ("Service Provider Initiated (SP-Initiated) SSO via OpenID Connect (OIDC)"). |
| Sign-in redirect URI | **Not verified.** Twingate's docs state the OIN app template pre-fills this; it was never read from the Okta UI in this session. |
| Issuer / authorization / token / JWKS / discovery endpoints | **Not verified directly** — never inspected Okta's own `/.well-known/openid-configuration` in this session. Standard Okta layout would place them under `https://trial-3724025.okta.com/oauth2/v1/...` and `.../.well-known/openid-configuration`, but this is **inferred from Okta's general product documentation, not observed**. |
| Scopes / claims | **Not verified.** Twingate's OIDC login only requires enough to identify the user (typically `openid profile email`); the exact scope string configured on the Okta app side was never inspected. |
| SCIM (Provisioning tab) | **API integration**, not OAuth — a single **bearer token** (the SCIM Token minted by Twingate, see §3) pasted into Okta's "API Token" field. Base URL not manually entered (built into the OIN app). "Import Groups" left checked (default). Credentials tested successfully ("Twingate was verified successfully!"). Save was clicked; **post-save confirmation not completed** (session interrupted during a page reload that triggered Okta re-auth). |

---

## 3. Twingate-Side Configuration (What Twingate Required From Okta)

Two distinct dialogs, both part of one guided wizard, in this order:

### 3.1 "Connect Okta" dialog (OIDC login)

| Field | Purpose | Source of the value |
|---|---|---|
| Okta Domain | The org's Okta hostname, used to derive/reach the OIDC discovery document | Read from the Okta admin console UI |
| Client ID | OIDC client identifier | Copied from Okta app's Sign On tab |
| Client Secret | OIDC client secret, for server-side token exchange | Copied from Okta app's Sign On tab |

"Sign in with Okta" is disabled until all three are filled — Twingate performs an **active login
round-trip** to verify the credentials work, not just a discovery probe. This is a stronger
verification than a passive discovery-endpoint check.

### 3.2 "Configure SCIM Provisioning" dialog (directory sync)

Twingate **generates and displays**, one-time:

| Field | Value | Notes |
|---|---|---|
| SCIM Endpoint | `https://sthiyaseelank326.twingate.com/api/scim/v2/` | Twingate's own SCIM 2.0 API base, workspace-scoped by subdomain |
| SCIM Token | opaque bearer token | **Shown exactly once** — Twingate's own UI explicitly warns it cannot be retrieved again after the dialog closes. This is the credential pasted into Okta's Provisioning → API Integration → API Token field. |

Twingate's own docs describe two functions delegated to Okta via this one app:
**user authentication via OpenID Connect**, and **user/group synchronization via SCIM** — i.e.
Twingate treats login (OIDC) and provisioning (SCIM) as two capabilities of a *single* IdP
connection object, configured back-to-back in one wizard, but backed by two independent credential
sets (OIDC client id/secret vs. a SCIM bearer token).

### 3.3 Login/authentication flow after configuration

Not independently exercised end-to-end in this session (no test login as a non-admin Twingate user
was performed) — the verification that did run was the admin-initiated "Sign in with Okta" step in
§1.7, which is itself one live login. **Observed/confirmed**: that one login succeeded automatically
because the browser held a valid Okta session (no credential prompt was shown).

---

## 4. End-to-End Authentication Flow

### 4.1 Observed/confirmed (from the one live "Sign in with Okta" click in §1.7)
- Twingate → redirect to Okta (implicit; the request/response was not packet-inspected, only the
  resulting UI state — "verified successfully" — was observed) → Twingate reported success and
  advanced to the SCIM dialog automatically.

### 4.2 Documentation-derived (from Twingate's public docs, not independently verified against a
packet capture in this session)
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
- Whether Twingate uses PKCE and/or a nonce on this flow — not stated in the portion of Twingate's
  docs read, not observed on the wire.
- Exact claim(s) Twingate maps to its internal user identity (email vs. sub) — not documented in the
  pages read.
- Group-claims-based vs. SCIM-only group membership behavior post-connection — the docs show SCIM
  ("Import Groups") as the sync mechanism; nothing suggests Twingate also reads a `groups` claim from
  the id_token, but this was not verified either way.

---

## 5. Orca CLI / Agent Automation — Exactly What Was Automated vs. Manual

### A. Automatically performed by the agent via Orca CLI
| Action | Command shape | System | Output | Reproducible | Auth/permission needed |
|---|---|---|---|---|---|
| List open browser tabs | `orca-ide tab list --json` | Orca browser | tab list incl. `browserPageId`, `url`, `title` | Yes | Orca app running |
| Navigate a tab | `orca-ide goto --url <url> --page <id> --json` | Orca browser | new `url`/`title` | Yes | none beyond Orca reachability |
| Accessibility snapshot (find clickable elements) | `orca-ide snapshot --page <id> --json` | Orca browser | element refs (`e1`, `e2`, …) + roles/names | Yes | none |
| Click an element | `orca-ide click --element <ref> --page <id> --json` | Orca browser | confirmation | Yes, but **refs are only valid until the next snapshot** — every click was preceded by a fresh snapshot in practice after the first stale-ref error | none |
| Fill a text field | `orca-ide fill --element <ref> --value <text> --page <id> --json` | Orca browser | confirmation | Yes | none |
| Check a checkbox | `orca-ide check --element <ref> --page <id> --json` | Orca browser | confirmation | Yes | none |
| Read an element's value | `orca-ide get --element <ref> --what value --page <id> --json` | Orca browser | field value (plaintext) | Yes | **Blocked once by the Claude Code auto-mode permission classifier** for the Client Secret / API Token reads — required explicit operator authorization to proceed (see §5.C) |
| Evaluate arbitrary JS in-page | `orca-ide eval --expression <js> --page <id> --json` | Orca browser | JS return value | Yes | none — used to read a link's `href` and to locate visible-vs-template toast text, since the accessibility snapshot doesn't expose raw HTML attributes |
| Create a new tab | `orca-ide tab create --url <url> --json` | Orca browser | new `browserPageId` | Yes | none |
| Scroll an element into view | `orca-ide scrollintoview --element <ref> --page <id> --json` | Orca browser | confirmation | Yes | none |
| Reload the active tab | `orca-ide reload --page <id> --json` | Orca browser | new tab state | Yes | none — but see the note below: reload triggered an Okta re-auth redirect, which was not resolved before the session was interrupted |

### B. Manually performed in the browser/UI (i.e. by the human operator, not the agent)
- The **initial** creation of the Twingate app inside Okta's App Catalog and its assignment to
  users/groups was already done by the time the agent inspected the Okta tab — **not observed being
  performed**, so it cannot be attributed to either the agent or a specific manual step in this
  document; it is simply the state found.
- The decision to authorize reading the live Client Secret and SCIM API Token (a permission-gated
  action — see C below) was an explicit human decision, made via an interactive confirmation, not
  something the agent inferred or bypassed.

### C. Values obtained from one system and entered into another (agent-mediated)
| Value | Read from | Written to | How |
|---|---|---|---|
| Okta Domain | Okta admin console page text (`eval` querying for `.okta.com` substrings) | Twingate "Connect Okta" dialog, Okta Domain field | `orca-ide fill` |
| Client ID | Okta app's Sign On tab (`get --what value`) | Twingate "Connect Okta" dialog | `orca-ide fill` |
| Client Secret | Okta app's Sign On tab (`get --what value`, **explicitly authorized**) | Twingate "Connect Okta" dialog | `orca-ide fill` |
| SCIM Token | Twingate's "Configure SCIM Provisioning" dialog (read from the rendered accessibility snapshot text, since Twingate displays it as static text, not an input field) | Okta Provisioning → API Integration → API Token field | `orca-ide fill` |

This is the **field-to-field credential transfer pattern**: the agent never displayed the Client
Secret or SCIM Token in its own prose to the user; both were moved directly from a read call's tool
output into a fill call's input, with the read gated behind an explicit one-time authorization.
(Caveat, disclosed here for completeness: the raw tool-call *output* of the two `get`/read calls was
still visible in the session transcript/tool-output stream by nature of how the CLI tools report
results — the agent's own written responses avoided repeating the secret, but the underlying
transport is not a secret vault.)

### D. Things the agent could not automate
- Reading the **Client Secret** and **SCIM API Token** values required breaking past the Claude Code
  auto-mode permission classifier, which denied the first attempt outright. The agent did not attempt
  a workaround; it stopped and asked the human operator, who then explicitly authorized the specific
  action. This is a hard automation boundary by design, not a technical limitation of Orca CLI.
- The agent could not resolve the Okta re-authentication redirect triggered by the final page reload
  in the time available before the session was interrupted for this task — **this is an open,
  unresolved item, not a confirmed success.** It should not be assumed that SCIM provisioning is fully
  active on the Okta side without re-checking the Provisioning tab.
- The agent did not have (and did not attempt to acquire) any way to inspect Okta's or Twingate's
  server-side logs, network traffic, or database state — all findings above come from the rendered
  DOM/accessibility tree of the two admin consoles, which is a UI-level view, not a protocol-level
  one.

---

## 6. Current Zecurity Microservice Architecture (as inspected, not modified)

Zecurity's controller is a single Go service (`controller/`) internally organized into narrow
packages under `controller/internal/`, each with a clear ownership boundary (per Sprint 17's own
"Conflict Zones" table). Relevant to identity/auth:

| Package | Responsibility (verified by reading source / `path.md`) |
|---|---|
| `controller/internal/auth` | Session/login service: OIDC login initiation (`oidc.go`), callback handling (`callback.go`), token exchange (`exchange.go`), id_token verification (`idtoken.go`), refresh (`refresh.go`), discovery (`discovery.go`), Google-specific adapter (`google_provider.go`), generic-provider factory (`idp_adapter.go`), config (`config.go`). This is the **login/session** boundary — it issues Zecurity's own JWTs after a successful upstream OIDC login. |
| `controller/internal/auth/providers` | Protocol adapters behind a common interface: `providers/oidc.go` is a **generic, provider-agnostic OIDC adapter** (discovery + PKCE + nonce + JWKS-cached id_token verification), used for Okta/Entra/JumpCloud/Keycloak/any-OIDC connections alike — "one engine, no per-provider handler types," matching the same design principle the SCIM engine uses. |
| `controller/internal/idp` | The **identity connection store**: `idp.Connection` (`store.go`) models exactly one persisted OIDC/SAML IdP connection per workspace (or platform-global), with `Provider`, `Issuer`, `ClientID`, `ClientSecret` (encrypted at rest, "NEVER expose outward"), `DiscoveryURL`, `Scopes`, `DomainHint`, `SubjectClaim`, `ScimIdentifier`, `ScimEnabled`, `LastSyncAt`. `Provider` field comment literally lists `"google","github","okta","entra","oidc"` as example values. |
| `controller/internal/identity` | The **identity lifecycle/session-effect** boundary: `resolver.go`/`linker.go` (resolve an `AuthenticationContext` to a Zecurity user, link/create as needed), `lifecycle.go`, `revocation.go` (`Revoker` — session kill via `identity_generation` bump), `device_trust.go` (the `SideEffectSink`/outbox event contract), `principal.go`, `service.go` (orchestrates the whole pipeline). Used by **both** the login path (`auth`) and the SCIM provisioning path (`scim`) — this is the shared seam between "a human logged in" and "a directory pushed a user record." |
| `controller/internal/scim` | The **SCIM 2.0 provisioning engine** (Sprint 17 / ADR-025): `directory_service.go`, `provisioner.go`, `users.go`, `groups.go`, `conflict.go`, `mapping.go`, `profiles.go` (built-in provider profiles — Okta/Entra/JumpCloud/Keycloak/Generic — plus per-connection overrides, "one engine"), `token_store.go` (SCIM bearer tokens, HMAC-hashed, dual-token rotation), `sync_instance.go`, `validation.go` (the `MappingGate`), `middleware.go` (Bearer auth for `/scim/v2/*`), `side_effect_sink_outbox.go`. |
| `controller/internal/outbox` | Durable outbox (Sprint 18, merged, out of scope for identity work — SCIM only calls `outbox.Enqueue`). |
| `controller/internal/permission` | Fine-grained permission primitives (e.g. `identity.mapping.break_glass`) — deliberately **not** role-based; a dedicated grant, independent of ADMIN. |
| `controller/internal/tenant` | Workspace/tenant context propagation (`context.go`). |
| `controller/internal/policy` | ACL compilation/caching + `Notifier.NotifyPolicyChange(workspaceID)` — invalidated on every identity-affecting mutation (group membership, deprovision, etc.). |
| `controller/internal/audit` | Audit event recording (`audit.go`) — used inconsistently on the identity paths (see §7/§8, Finding A in `path.md`). |
| `controller/graph` | GraphQL API: `idp.graphqls` (connections, SCIM tokens, conflicts — Sprint 17's admin surface), resolvers under `graph/resolvers/idp*.go`. |
| `admin/src/pages/IdpConnections.tsx`, `admin/src/components/idp/CreateIdpConnectionDialog.tsx` | Admin UI: connection list page and the **"Add Identity Provider"-equivalent dialog** — already built, in progress on this branch (`feat/sprint17-scim`), asking for exactly Issuer/Client ID/Client Secret/discoveryUrl/scopes/domainHint, with `okta`/`entra`/`jumpcloud`/`keycloak`/`oidc` as provider-type options. |

**No monolithic identity service exists or is implied anywhere in the codebase or planning docs** —
identity work is already split across `auth` (login/session), `idp` (connection storage), `identity`
(lifecycle/session-effects, shared), and `scim` (provisioning), matching microservice-style internal
boundaries within the single `controller` binary, plus a dedicated `graph` API layer and `admin`
frontend. Any Okta-equivalent work belongs inside this existing boundary set — see §9.

---

## 7. Current Zecurity Capabilities Inventory

Only claims verifiable by reading the repository are listed; "Not verified" is used where the
control-flow was not traced to completion.

| Capability | Current Zecurity Status | Location | Notes |
|---|---|---|---|
| Password/session auth | Present | `controller/internal/auth` (JWT access+refresh via Valkey/Redis) | HS256, `JWT_SECRET` ≥32 bytes enforced at boot |
| Google OIDC login (platform IdP) | Present | `auth/google_provider.go`, `auth/oidc.go` | The original/bootstrap login path |
| **Generic enterprise OIDC login** (Okta/Entra/JumpCloud/Keycloak/any-OIDC) | **Present** | `auth/providers/oidc.go`, `auth/idp_adapter.go` (factory), `idp.Connection` | Full PKCE + nonce + signed-state CSRF + per-issuer discovery/JWKS caching + `email_verified` enforcement. This is functionally equivalent to what Twingate's "Connect Okta" OIDC half does. |
| Multi-connection selection at login | Present | `InitiateAuth(ctx, provider, workspaceName, connectionID)` in `auth/oidc.go` | `connectionID` selects a specific workspace IdP connection; falls back to the platform IdP for `provider` |
| OIDC connection admin CRUD | Present (backend + in-progress frontend) | `graph/idp.graphqls` (`createIdpConnection`, `updateIdpConnection`), `admin/src/components/idp/CreateIdpConnectionDialog.tsx` | Frontend dialog is uncommitted/new on this branch; not yet confirmed merged |
| Connection health/status probing | Present, **partially wired** — see gap | `testIdpConnection` GraphQL mutation, `OIDCProvider.Probe()` | Runs an OIDC **discovery** probe + mapping-config validation; does **not** run a full login round-trip the way Twingate's "Sign in with Okta" button does (that's a UI-driven live login, not an admin-side probe) |
| SSO / SCIM provider profiles | Present | `controller/internal/scim/profiles.go` | Okta/Entra/JumpCloud/Keycloak/Generic built-ins + per-connection overrides |
| SCIM 2.0 Users provisioning | Present | `scim/provisioner.go`, `scim/users.go` | RFC 7644 envelopes, `meta.version`, filtering; **known gap**: `users` table lacks `name`/`displayName`/`title`/`department` columns (documented in `path.md` Phase 5) |
| SCIM 2.0 Groups sync | Present | `scim/groups.go` | Origin-scoped (`origin='scim'`), connection-scoped; out-of-order member → `404` |
| SCIM deprovisioning | Present, **with confirmed defects** | `scim/directory_service.go` (`Deprovision`) | Per `path.md` Finding A: uses `Revoker.BumpGenerationTx`, which — at the time `path.md` was last updated — never called `afterBump`, so **live sessions were not being proactively killed and no audit event was emitted** on the SCIM deprovision path; also `DELETE` did not purge `group_members`. **Not independently re-verified in this session** whether these are still open; `path.md` marks Finding A as un-checked (`- [ ]`) while a related Finding B is marked fixed, so treat A as still open unless re-confirmed against current source. |
| SCIM bearer token management | Present | `scim/token_store.go` | HMAC-SHA256 hashed, dual-token rotation (≤2 active, configurable grace window), GraphQL mint/rotate/revoke, plaintext shown once — **directly analogous to Twingate's own "shown once" SCIM token UX observed in §3.2** |
| SCIM base URL exposure to admin | Planned, **frontend not confirmed shipped** | Sprint 17 FE-1 (`Member1-Frontend/Phase1-SCIM-Connection-Config.md`) | `path.md` describes this as "buildable now" but does not confirm it landed |
| Identity conflict handling (JIT/manual vs SCIM collision) | Present | `scim/conflict.go`, `scim_identity_conflicts` table, `AcceptScimConflict`/`RejectScimConflict`/`ReopenScimConflict` mutations | Keyed on canonical identity key, never email; admin queue is Sprint 17 FE-4 |
| Break-glass permission for identity mapping override | Present | `controller/internal/permission`, `identity.mapping.break_glass` | Dedicated permission, ADMIN alone insufficient |
| Connection lifecycle (disable/delete) | Present | `scim/directory_service.go` connection lifecycle path (M1-9) | Uses `Revoker.BumpGeneration` (non-Tx variant), which *does* call `afterBump` — this path is **not** affected by Finding A |
| Identity Health surfacing | Present | `DirectoryService.IdentityHealth`, GraphQL `identityHealth`/`lastSyncAt` | Healthy/Delayed/Disconnected/Disabled derivation from `last_sync_at` |
| Mapping-gate active probe (POST→GET→verify→DELETE round-trip) | **Not wired** (confirmed gap, per `path.md` Finding C) | `scim/validation.go` `MappingGateResult.WithRoundTrip` | No production caller as of `path.md`'s last audit; consequence: SCIM can only be enabled via break-glass override, never via a "proven" mapping |
| Tenant-scoping of SCIM writes | Present, **fixed after a confirmed prior defect** | `scopedProvisioningOwner`, `Revoker.bump` (`AND tenant_id = $2`) | `path.md` Finding B describes a cross-tenant generation-bump bug that was fixed 2026-08-26, with a regression test (`TestDeprovision_Integration/cross-tenant_deprovision_is_refused_and_mutates_nothing`) |
| Groups (Zecurity-native, non-SCIM) | Present | `admin/src/components/groups/GroupOriginLabel.tsx` (new/uncommitted) | Origin-labeling UI for SCIM vs Local vs System groups (Sprint 17 FE-5) |
| RBAC / roles | Present | `@hasRole(roles:[ADMIN])` directives across `idp.graphqls`; `controller/internal/permission` for fine-grained grants | Two-tier: coarse role directives + fine-grained permission table |
| Audit logging | Present, **inconsistently applied on identity paths** | `controller/internal/audit` | Per `path.md`: only `conflict.go` and `provisioner.go` call `Publish`/`RecordTx` in `internal/scim`; `Deprovision` does not (part of Finding A) |
| Config/secrets management | Present | `Config` struct pattern (`auth/config.go`), `ClientSecret` marked "encrypted at rest… NEVER expose outward" on `idp.Connection` | Encryption-at-rest mechanism itself not inspected in this session — **not verified** which primitive performs it |
| SCIM conformance testing | **Absent** | — | `path.md`: "No conformance suite, runner, or fixture exists anywhere in the repo" |
| Live Okta/Entra interop testing | **Absent / blocked** | — | `path.md`: "PENDING tenant access" — this session's live Okta trial org is exactly the kind of tenant access that was previously missing, though it was used for an external product's setup (Twingate), not for testing Zecurity itself |

---

## 8. Twingate vs. Zecurity — Gap Analysis

| Twingate/Okta Requirement | Current Zecurity Support | Gap | Required Zecurity Change | Relevant Microservice/Package | Priority |
|---|---|---|---|---|---|
| Generic OIDC login connection (issuer, client id/secret, discovery URL, scopes) | **Supported** | None functionally — `idp.Connection` + `providers/oidc.go` already model this | — | `idp`, `auth/providers` | — |
| Admin UI to create an OIDC connection ("Connect Okta"-equivalent dialog) | **Supported (in progress, uncommitted on this branch)** | Confirm `CreateIdpConnectionDialog.tsx` is finished/merged and wired to `createIdpConnection` | Verify + finish FE-0 (already tracked as Sprint 17 FE-0) | `admin/src/components/idp` | Low (tracking work already exists) |
| Live "Sign in with Okta"-style login verification button in the admin UI | **Partial** — `testIdpConnection` runs a discovery probe, not a full login | The admin console never actually drives a real browser login the way Twingate's dialog does | Decide if this is worth building, or if relying on the discovery probe + first real user login is acceptable | `graph/resolvers/idp*.go`, `auth` | Medium — UX gap, not correctness gap |
| SCIM endpoint + shown-once bearer token, minted from the identity-provider admin UI | **Supported** | None functionally | Confirm FE-1's token-mint panel is live (tracked, not confirmed shipped) | `scim/token_store.go`, FE-1 | Low |
| SCIM Users provisioning (create/update via directory push) | **Supported**, with a **documented schema gap** — `users` lacks `name`/`displayName`/`title`/`department` | Directory-owned attribute writes for those fields will `400` | Extend `users` schema (tracked already: "Schema extension tracked separately, ADR-025 §5") | `scim/users.go`, migrations | Medium (pre-existing tracked item, not newly discovered here) |
| SCIM deprovision **must** kill live sessions + emit audit event synchronously with the status change | **Documented as broken** (`path.md` Finding A, unchecked at last read) | `Revoker.BumpGenerationTx` does not invoke `afterBump`; SCIM-driven suspend/delete may leave sessions alive until next token refresh, and produces no audit trail | Wire the post-commit `afterBump` call (or an equivalent closure) on the SCIM deprovision path; also purge `group_members` on hard delete | `scim/directory_service.go`, `identity/revocation.go` | **High** — this is a live security defect already flagged as such in the repo's own docs, independent of anything from the Twingate exercise |
| Mapping validated by an **active round-trip probe** before SCIM can be "proven" (Twingate verified OIDC login live before proceeding to SCIM setup) | **Not wired** (`path.md` Finding C) | `MappingGateResult.WithRoundTrip` has no caller; SCIM can only be turned on via break-glass, defeating the "proven mapping" design intent | Wire the round-trip probe into `TestIdpConnection` | `scim/validation.go`, `graph/resolvers/idp.resolvers.go` | High (already flagged as a "sprint-completion blocker" in `path.md`, not a new finding from this exercise) |
| Distinct redirect/callback handling per connection (Okta's OIN app has its own registered redirect URI) | **Different design, not a gap** — Zecurity uses one process-wide `cfg.RedirectURI` and disambiguates the in-flight login via `connection_id` stored server-side against the CSRF `state` token in Redis | None — this is an intentional, arguably cleaner architecture (fewer registered redirect URIs to manage across many IdPs) than Twingate/Okta's per-app redirect URI model | — | `auth/oidc.go` | — |
| Pre-built vendor app in the IdP's own app catalog (Okta Integration Network) so the customer only supplies a token, no base URL | **Not applicable to a self-hosted/enterprise product** — Zecurity is not (and likely has no reason to become) an OIN-listed app; admins configuring Zecurity in Okta would use a **generic SCIM app** template, requiring both a base URL and a token | This just means Zecurity's admin-facing SCIM setup instructions must tell the customer to enter **both** the SCIM base URL and the token into Okta (unlike Twingate's one-field flow) | Documentation/onboarding copy only — not a code gap | FE-1 (help text), user-facing docs | Low |
| Group import via SCIM ("Import Groups" checkbox in Okta) | **Supported** | None | — | `scim/groups.go` | — |
| Twingate's SCIM dialog surfaces the endpoint + token together, generated automatically after OIDC verification succeeds | **Zecurity's flow is not sequenced the same way** — OIDC connection creation (`createIdpConnection`) and SCIM token minting are two separate admin actions/mutations, not a single guided wizard | UX sequencing difference, not a functional gap | Optionally combine into a guided setup wizard on the frontend for parity with Twingate's UX | `admin/src/components/idp` (new component, not yet planned) | Low — nice-to-have, not tracked anywhere currently |
| Cross-tenant isolation on every SCIM-token-scoped write | **Fixed** (`path.md` Finding B) | None currently known | — | `scim/*` | — |
| Nonce/state/PKCE on the login flow | **Supported**, arguably more rigorous than what was directly observed of Twingate's flow (nonce+PKCE+HMAC-signed CSRF state vs. Twingate's undocumented internals) | None | — | `auth/oidc.go` | — |

**Summary judgment:** the *functional* gap between "what Twingate+Okta required" and "what Zecurity
already has" is small — Zecurity's `idp`/`auth/providers`/`scim` stack already covers the same ground
(generic OIDC login + SCIM provisioning with token-based auth) at a comparable or greater level of
protocol rigor. The real, current gaps are the ones **already identified in Sprint 17's own
verification pass** (`path.md` Findings A and C) — not new gaps surfaced by the Twingate exercise
itself. The Twingate walkthrough is useful mainly as a **UX/sequencing reference** (how a polished
"Connect an IdP" wizard reads to an admin) and as independent confirmation that the field set Zecurity
already asks for (issuer/domain, client id, client secret, discovery URL, scopes) matches what a real
enterprise IdP integration needs.

---

## 9. What Zecurity Would Need to Implement (By Service Boundary)

**Not implemented. Pending plan only.** Organized by existing package/service boundary — no new
service is proposed.

### `controller/internal/identity` (+ `internal/scim/directory_service.go`)
- Fix `Revoker.BumpGenerationTx` (or its caller in `scim/directory_service.go`) to invoke `afterBump`
  post-commit, so SCIM-driven suspend/delete actually kills live sessions and emits the audit event —
  this closes `path.md` Finding A.
- Add `group_members` purge on hard delete, per ADR-025 §5.

### `controller/internal/scim` (`validation.go`, `graph/resolvers/idp.resolvers.go`)
- Wire `MappingGateResult.WithRoundTrip` into `TestIdpConnection` (or the Phase 5 provisioning path)
  so a POST→GET→verify→DELETE probe user round-trip actually runs and can move `MappingState` to
  `proven` — this closes `path.md` Finding C and is the precondition for `scimEnabledAllowed` ever
  being true outside break-glass.

### `controller/internal/scim` (`users.go`, migrations)
- Extend the `users` table with `name`/`displayName`/`title`/`department` (or equivalent) if
  directory-owned attribute writes for those fields are required by product scope — tracked already
  in ADR-025 §5, not newly discovered here.

### `admin/src/components/idp`, `admin/src/pages/IdpConnections.tsx`
- Confirm/complete FE-0 (Create OIDC connection dialog) and FE-1 (SCIM config: mapping fields,
  base-URL box, token mint/rotate/revoke panel) per Sprint 17's own tracked plan — these already
  cover the Twingate-equivalent "Connect Okta" + "Configure SCIM Provisioning" UX.
- Optional, not currently tracked anywhere: a single guided multi-step wizard combining OIDC
  connection creation and SCIM token minting into one flow, mirroring Twingate's UX (§3), if product
  wants that sequencing.

### Documentation / admin-facing onboarding copy (no code)
- Because Zecurity is not an Okta-catalog (OIN) app, any customer-facing "how to connect Okta to
  Zecurity" guide must explicitly instruct the admin to create a **generic SCIM app** in Okta and
  enter **both** the SCIM base URL and the bearer token (Twingate's flow only required the token,
  since its OIN app pre-fills the URL). This is a documentation task, not an implementation task.

No changes are proposed to `controller/internal/auth`, `controller/internal/idp`,
`controller/internal/permission`, `controller/internal/policy`, `controller/internal/outbox`, or
`controller/internal/tenant` — the Twingate walkthrough did not surface any gap attributable to
those packages.

---

## 10. Configuration Contract

### What an admin configuring **Okta itself** needs to provide (regardless of which service consumes it)
- Okta Domain / org hostname (e.g. `trial-3724025.okta.com` — **not** the `-admin` console hostname)
- An OIDC app (or, if ever pursued, an Okta-catalog app) with:
  - Client ID (generated by Okta)
  - Client Secret (generated by Okta) — `<CLIENT_SECRET_REDACTED>` pattern in any future logs/docs
  - Sign-in redirect URI (Zecurity's process-wide callback URL — see §8, this is a single value across
    all connections, not per-connection)
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
| SCIM token (plaintext) | **Zecurity generates**, shown once | Admin copies it **into Okta**, not the reverse |

Values that **should never be manually entered if discovery can provide them**: authorization
endpoint, token endpoint, JWKS URI — all already derived automatically from `discoveryUrl` /
`<issuer>/.well-known/openid-configuration` by `OIDCProvider.discover()`. This matches how Twingate's
own dialog behaves (it never asked for those three endpoints directly either — only issuer/domain,
client id, client secret).

---

## 11. Security Requirements (Observed/Reinforced by the Real Integration)

- **Client secret protection**: confirmed pattern on both sides — Okta shows it once per rotation on
  its Sign On tab; Zecurity's `idp.Connection.ClientSecret` doc comment states "decrypted; empty for
  managed; NEVER expose outward" and `CreateIdpConnectionInput.clientSecret` is documented
  write-only/encrypted-at-rest. Consistent design intent; the actual encryption-at-rest primitive was
  **not verified** in this session.
- **HTTPS**: both Okta and Twingate enforced HTTPS origins throughout (observed directly — every URL
  visited was `https://`); Zecurity's `RedirectURI` doc comment likewise assumes `https://`.
- **State (CSRF) validation**: Zecurity already implements HMAC-signed state (`generateSignedState`/
  `verifySignedState`, `auth/oidc.go`) — stronger than anything directly observed of Twingate's flow
  (which was not packet-inspected).
- **Nonce validation**: Zecurity's `providers/oidc.go` generates and can enforce a nonce; whether
  Twingate does the same was not verified (§4.3).
- **PKCE**: Zecurity generates a 64-byte verifier/S256 challenge in `oidc.go` and both `providers/oidc.go`
  adapters use it. Not verified whether Twingate's flow uses PKCE.
- **Issuer/audience validation**: Zecurity's `verify()` in `providers/oidc.go` explicitly checks
  `jwt.WithAudience(p.clientID)` and `jwt.WithIssuer(p.issuer)`, plus rejects unverified emails
  (`!claims.EmailVerified`) — this is a stricter check than anything the Twingate/Okta UI surfaced
  directly (that verification happens server-side on Twingate's end, not observable from the admin
  console).
- **JWKS/key rotation**: Zecurity caches JWKS per-issuer with a 1-hour TTL and re-fetches on unknown
  `kid` (`jwksForIssuer`) — handles Okta's routine key rotation correctly by design.
- **Token expiration**: `jwt.WithExpirationRequired()` enforced in Zecurity's verifier.
- **Redirect URI validation**: implicit — Okta/Twingate both validate the redirect URI is registered
  on the OAuth app; Zecurity's side was not stress-tested against a mismatched redirect URI in this
  session.
- **Session security / secret handling on the automation side**: the SCIM Token and Client Secret
  handled during the live Twingate/Okta setup were read only after explicit operator authorization,
  never printed in the agent's own prose, and the temporary cookie-extraction files from the earlier
  (unrelated) session-restore step were deleted after use. This is a good practice worth carrying into
  any future Zecurity-side tooling/runbooks that automate IdP setup, but it is **process guidance, not
  a code requirement**.

---

## 12. Testing Requirements (Derived From the Twingate/Okta Exercise + Existing `path.md` Gaps)

### Unit tests
- OIDC adapter: issuer mismatch on discovery document → rejected (already covered per source read of
  `discover()`, not independently re-run here).
- id_token verification: expired token, wrong audience, wrong issuer, unverified email, missing `kid`
  — each already has corresponding checks in `providers/oidc.go`; confirm existing test coverage
  (`providers/oidc_test.go` exists — not read in this session).
- SCIM mapping gate: `WithRoundTrip` states (unproven/proven/failed) once wired (§9).

### Integration tests
- SCIM deprovision → session actually killed + audit event actually recorded, once Finding A is fixed
  (a regression test analogous to the existing `TestDeprovision_Integration` cross-tenant test, but
  asserting `afterBump` side effects).
- Full mapping-gate round-trip against a mapping-validation fake IdP.
- Provider-not-assigned equivalent: a user authenticated by Okta but not assigned to the Zecurity app
  — verify Zecurity's login path fails closed (mirrors Okta's own app-assignment gate observed in
  §2).

### End-to-end tests
- Successful login through a real (or realistically faked) generic-OIDC IdP connection, exercising
  the full `InitiateAuth` → redirect → callback → session-issuance path.
- Invalid state / invalid nonce / tampered id_token rejected end-to-end.
- SCIM provision → conflict (JIT collision) → admin Accept-Link → subsequent SCIM update succeeds.
- SCIM deprovision → subsequent authenticated request with the old session is rejected (requires
  Finding A fixed first, or this test will pass vacuously / not at all).
- Live interop against a real Okta (and ideally Entra) tenant — `path.md` already flags this as
  ungated now that live tenant access exists (this session's Okta trial org demonstrates the kind of
  access previously missing, though it was not used to test Zecurity itself).

---

# Pending Zecurity Implementation

- **Nothing from this investigation has been implemented.** No Zecurity source file, GraphQL schema,
  migration, or configuration was changed while producing this document.
- This document is a **findings/reference document** only, meant to save a future implementer from
  re-deriving the same field-by-field comparison.
- The Twingate + Okta integration performed in this session is used purely as an **external reference
  implementation** — a concrete example of what a real enterprise customer's Okta-side setup and
  admin-facing OIDC/SCIM wizard look like — not as a spec Zecurity must copy.
- **Zecurity's existing microservice-internal architecture must be preserved.** Identity-related work
  already lives across four distinct packages (`auth`, `idp`, `identity`, `scim`) plus the `graph` API
  layer and `admin` frontend; nothing here proposes collapsing that into a new monolithic identity
  service.
- **Future implementation should follow the existing service boundaries and patterns** documented in
  §6 and used throughout Sprint 17's own `path.md` (one engine + provider profiles, not per-provider
  handler types; `SideEffectSink` as the seam to the outbox; fine-grained permissions over role
  overloading; canonical-identity-key resolution never keyed on email).
- **The bulk of what the Twingate/Okta setup required is already implemented in Zecurity.** The
  concrete, actionable gaps identified are the same two the repository's own Sprint 17 verification
  pass already flagged as open (`path.md` Finding A — SCIM deprovision session/audit gap; Finding C —
  unwired mapping round-trip probe), plus one already-tracked schema extension (user attribute
  columns) and some UX/documentation nice-to-haves (§8, §9). No item in §9 should be treated as
  scoped, estimated, or scheduled by this document — **the actual implementation should be
  planned/reviewed separately, by the team, before any code is written.**
