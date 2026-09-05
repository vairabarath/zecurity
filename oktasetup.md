# Okta → Zecurity Setup Guide

> Working record of connecting an Okta tenant (`trial-3724025`, org `gomail-edu-trial-3724025`) to a
> locally-running Zecurity (Go controller on `:8080`, admin UI on `:5173`) for Sprint 17
> (Enterprise Directory Sync / PENDING-05).
>
> Written from an actual end-to-end session on 2026-08-28, including the errors hit and how each was
> resolved. Where something is **still broken or unverified**, it says so explicitly rather than
> presenting an idealised flow.
>
> **Secrets are redacted.** Client secrets and SCIM tokens are shown as `<REDACTED>` with a note on
> where to obtain them.

---

## 0. Key concept: two separate Okta apps, one Zecurity connection

This is the single most confusing part, and the source of several errors below.

| Purpose | Okta app | Talks to Zecurity via |
|---|---|---|
| **Directory sync** (push users/groups into Zecurity) | `SCIM 2.0 Test App (OAuth Bearer Token)` from the App Catalog | SCIM 2.0 REST API + bearer token |
| **Login / SSO** (users authenticate to Zecurity) | A hand-built `OIDC – OpenID Connect / Web Application` | OIDC authorization-code flow |

### Why two apps and not one — this is an Okta platform limitation

Okta **does not support enabling SCIM provisioning on a custom OIDC app integration.** Straight from
[Okta's own support article](https://support.okta.com/help/s/article/configure-scim-for-a-custom-oidc-app?language=en_US):

> *"Okta does not currently support enabling System for Cross-domain Identity Management (SCIM)
> provisioning with a custom OpenID Connect (OIDC) integration."*

There is **no** "Enable SCIM provisioning" toggle to look for on a custom OIDC app — the Provisioning
tab simply does not exist there. Okta's documented workaround is exactly the two-app split above:
one app for OIDC auth, a second app for SCIM provisioning only. The article also notes the SSO half
of the provisioning-only app **does not need to work** — which is why §3.1 says to accept the SCIM
app's SAML defaults and ignore them.

This applies to both Okta Identity Engine and Classic Engine, and to any custom OIDC app without a
pre-built OIN listing (i.e. Zecurity's situation — it is not an OIN-listed vendor).

### On the Zecurity side, though, it is one connection

Both Okta apps map onto **one** `identity_connections` row: `issuer`, `clientId`, `clientSecret`,
`subjectClaim` (login) *and* `scimIdentifier` / `scimEnabled` (SCIM) all live together. There is a
unique constraint on `(workspace_id, issuer)` — so you get **one Zecurity connection per Okta org**,
serving both purposes. You cannot create a second connection for the same issuer to separate login
from SCIM.

**Net effect:** two apps in Okta → one connection in Zecurity. The SCIM app supplies the bearer
token; the OIDC app supplies the Client ID/Secret. Both point at the same `issuer`.

---

## 1. Prerequisite: expose the local controller

Okta must reach the controller over public HTTPS. `localhost:8080` is not reachable from Okta.

```bash
cloudflared tunnel --url http://localhost:8080
```

It prints a random `https://<words>.trycloudflare.com` URL. Verify before continuing:

```bash
curl -i https://<tunnel-host>/scim/v2/Users     # expect: 401 (auth required, server reachable)
curl -i http://localhost:8080/scim/v2/Users     # expect: 401 (controller itself is up)
```

> ### ⚠️ This tunnel died twice during the session
> `cloudflared tunnel --url` creates an **ephemeral quick tunnel** tied to that terminal process.
> Close the terminal, sleep the machine, or lose the process → the hostname is **permanently gone**
> and a restart gives a **different** random hostname. Symptom in Okta:
> `Error authenticating: null` on Test API Credentials, and `curl: (6) Could not resolve host`
> locally. Any time Okta suddenly can't reach Zecurity, check this first.

---

## 2. Zecurity side — create the connection

In the admin UI (`http://localhost:5173/idp-connections`):

1. **Add Identity Provider** → provider popup → **Okta**
2. In the **Connect Okta** dialog:
   - **Display Name**: anything, e.g. `Okta`
   - **Okta Domain**: `https://trial-3724025.okta.com`
     *(the org domain — **not** the `-admin` console hostname `trial-3724025-admin.okta.com`)*
   - **Client ID** / **Client Secret**: from the OIDC login app (§4). If you haven't built that yet,
     you can use placeholders and update later — SCIM does not use these.
3. **Create Connection** → the wizard advances to SCIM config
4. **Step 2 — SCIM configuration**: leave the Okta preset defaults (`sub` / `externalId`), flip the
   **SCIM toggle on**
5. **Step 3** — **Mint token**, label it (e.g. `okta-scim`), and **copy the plaintext immediately**
   — it is shown once and cannot be retrieved again
6. **Finish**

### What to expect
- SCIM enabling should succeed **without break-glass**. The Phase 12 mapping probe actively proves
  the OIDC↔SCIM canonical-key equivalence, so `MappingGate` reaches `proven` on its own. If it
  *does* demand break-glass, the mapping probe failed — check the reason it reports rather than
  reaching for the override.
- Connection then reads `Active · SCIM On`, health `Disconnected` (nothing has synced yet — expected).

### ⚠️ Known UI bug — do not trust the SCIM base URL box (Phase 7 / F7-2)
Step 3 displays `http://localhost:5173/scim/v2` — the **admin SPA's origin**, not the controller's.
Okta can never reach that. Use your **tunnel URL** instead:
`https://<tunnel-host>/scim/v2`. Tracked in
`.zecurity-obs/Sprint17/Member1-Frontend/Phase7-SCIM-Config-Missing-Fields.md`.

---

## 3. Okta side — the SCIM app (directory sync)

### 3.1 Add the app

**Applications → Browse App Catalog** → search `SCIM 2.0`.

> ### ⚠️ There is no plain "SCIM 2.0" app in the catalog
> All 22 search results are either vendor-specific (Cisco Webex, Egnyte, Envoy…) or named
> **"SCIM 2.0 Test App"**. Despite the name, the *Test App* family **is** Okta's generic,
> vendor-neutral SCIM connector. It comes in three auth variants — pick the one matching Zecurity's
> `AuthMiddleware`, which expects `Authorization: Bearer <token>`:
>
> **→ `SCIM 2.0 Test App (OAuth Bearer Token)`** ← use this one
>
> (Not Basic Auth, not Header Auth, not the SCIM **1.1** variants.)
>
> Also: **Create App Integration** does *not* offer a standalone SCIM option (only OIDC / SAML /
> SWA / API Services). The catalog app is the only route.

Click it → **Add Integration** → name it e.g. `Zecurity SCIM` → **Next** → **Done**.
(The Sign-On Options step is SAML config you don't need — leave defaults.)

### 3.2 Configure the API integration

**Provisioning** tab → **Configure API Integration** → check **Enable API integration**:

| Field | Value |
|---|---|
| SCIM 2.0 Base Url | `https://<tunnel-host>/scim/v2` |
| OAuth Bearer Token | `<REDACTED>` — the token minted in §2 step 5 |
| Import Groups | ✅ leave checked |

Click **Test API Credentials** → expect
*"SCIM 2.0 Test App (OAuth Bearer Token) was verified successfully!"* → **Save**.

> ### ⚠️ Two errors hit here
> **`SCIM 2.0 Base Url: Does not match required pattern`**
> Caused by a **trailing slash**. `.../scim/v2/` is rejected; `.../scim/v2` is accepted.
>
> **`Error authenticating: null`**
> Not a credential problem — the cloudflared tunnel had died (see §1). Verify with `curl` before
> assuming the token or Zecurity config is wrong.

### 3.3 Enable provisioning actions

**Provisioning → To App → Edit**:

| Setting | Value |
|---|---|
| Create Users | ✅ |
| Update User Attributes | ✅ |
| Deactivate Users | ✅ |
| Sync Password | ❌ — Zecurity does not consume passwords over SCIM |

**Save**.

### 3.4 Assign users

**Assignments → Assign → Assign to People** → pick the users → **Assign** → a profile form appears →
**Save and Go Back** → **Done**.

**Assignment is what actually triggers the SCIM push.** Nothing syncs until this step.

### 3.5 Verify on the Zecurity side

Reload the connection detail page. It should flip to:

```
Active · SCIM On · Healthy · last synced <n>s ago
```

The SCIM token row should show `Last used <n>s ago`, confirming Okta authenticated with it.

`identityHealth` derivation (`internal/scim/directory_service.go`):
`Healthy` ≤24h · `Delayed` ≤72h · `Disconnected` >72h, null, or connection not active/scim-enabled.

If it stays `Disconnected`: check the tunnel is alive, the token isn't stale, and Okta's
**Reports → System Log** (filtered to the app) for the outbound SCIM call and its response code.

---

## 4. Okta side — the OIDC login app (SSO)

**Applications → Create App Integration → OIDC – OpenID Connect → Web Application → Next**

| Field | Value |
|---|---|
| App integration name | `Zecurity` |
| Grant type | **Authorization Code** (default — correct; Refresh Token not required) |
| **Sign-in redirect URI** | `http://localhost:8080/auth/callback` |
| Sign-out redirect URI | `http://localhost:8080` (optional) |
| Controlled access | **Limit access to selected groups** *or* **Skip group assignment for now** |
| Enable immediate access with Federation Broker Mode | **UNCHECK IT** — see warning below |

> ### ⚠️ Redirect URI: Okta's default is wrong for Zecurity
> Okta pre-fills `http://localhost:8080/authorization-code/callback`. Zecurity's actual callback is
> **`http://localhost:8080/auth/callback`** (from `GOOGLE_REDIRECT_URI` / `PROVIDER_GOOGLE_REDIRECT_URI`
> in `controller/.env`, consumed at `cmd/server/main.go:169`). This is a **single process-wide
> callback shared by every connection** — Zecurity disambiguates the in-flight login via a
> `connection_id` stored server-side against the CSRF `state` token, not via per-app redirect URIs.

> ### ⚠️ Federation Broker Mode will block user assignment — this was a real mistake in the session
> Selecting *"Allow everyone in your organization to access"* leaves
> **"Enable immediate access with Federation Broker Mode"** checked by default. With it on, the
> Assignments tab shows only:
>
> *"This app is implicitly assigned to users — access is determined by app sign-on policies."*
>
> …and **you cannot assign individual people or groups at all.**
>
> **Fix:** General tab → *Federation Broker Mode (optional)* → **Edit** → **Disable Federation
> Broker Mode** → **Continue**, then **hard-reload the page and confirm it reads `Disabled`.**
>
> In this session the disable appeared to succeed but **did not persist** — after a real page load it
> still read `Enabled`. The Okta SPA caches this view aggressively. Always re-verify after a full
> reload; if it reverts, do it manually rather than trusting the optimistic UI state.

Then: **General tab → CLIENT SECRETS → Generate new secret** (max 2 per app) → reveal and copy it.
Copy the **Client ID** from the same tab.

Feed both into Zecurity's Connect Okta dialog (§2 step 2), or update the existing connection.

> ### ⚠️ `400 Bad Request — User is not assigned to the client application`
> Seen on `/oauth2/v1/authorize` with `Error Code: access_denied`. The signed-in Okta user isn't
> assigned to *that specific* app. Fix on the app's **Assignments** tab (requires Federation Broker
> Mode off, above).

---

## 5. Group sync — use Import, not Push

> ### ⚠️ Okta "Push Groups by name" used to 400 — now derived, with known caveats
> Old error: `Unable to update Group Push mapping target App group <name>: Bad Request. Errors reported
> by remote server: externalId is required`
>
> **What was wrong:** Okta's Push Groups *"By name"* sends only `displayName`, no `externalId`.
> The reject happened **before** `CreateGroup` — the `groupHandler.handlePost` HTTP layer
> (`internal/scim/users.go:426`) hard-rejected `externalId == ""` with `400 "externalId is required"`
> and returned, so the earlier `groups.go` derivation patch was never reached. Restarting the
> controller changed nothing because the guard lived one layer up. The tell was the string: Okta's
> message said `externalId is required` (the *handler's* text, not `externalId is required for scim
> groups` from `groups.go:113`).
>
> **Fixed (controller restarted, serving):** the handler now derives a fallback key via the shared
> `DeriveGroupExternalID(displayName)` helper (`internal/scim/groups.go:62-113`) and only rejects when
> *that* also derives to empty (i.e. the displayName itself is empty/whitespace). The handler guard and
> `CreateGroup`'s fallback call the **same** helper, so they cannot disagree ("!!!" is non-empty but
> derives to `""`). `DeriveGroupExternalID` collapses any run of non-alphanumerics to a single hyphen
> and trims, so `R&D / Ops #1` → `r-d-ops-1` (path-safe; no stray `/`, `&`, `#` in `/Groups/{id}`).
> Push Groups *by name* therefore now creates the group with a derived externalId (verified: `hermes`
> → externalId `hermes`).
>
> **These two behaviors are intentional, known, and pinned by tests** — they apply ONLY when the IdP
> sends no `externalId` (push-by-name). Import Groups (§3.2), which supplies `externalId`, is unaffected.
> 1. **Rename changes the derived key.** Renaming a group in Okta produces a new slug, so the old
>    group keeps its existing memberships (keyed on the old externalId) and a *new, empty* group
>    appears under the new slug — they do not merge. This is covered by
>    `TestDeriveGroupExternalID_NotStableAcrossRename`.
> 2. **Slug collisions are real collisions.** Two different display names that normalize to the same
>    slug collide against `UNIQUE (workspace_id, connection_id, external_id)` — SCIM returns a
>    `409`-class conflict on the second. Covered by
>    `TestDeriveGroupExternalID_CollidesOnEquivalentNames`.
> Both are documented in code, not surprises.
>
> **Use instead / still recommended:** the **Import Groups** checkbox (§3.2) remains the cleaner path
> when Okta can supply a real `externalId` — it sidesteps both caveats above. Push Groups is now
> supported but carries the rename/collision semantics above by design.

---

## 6. Error quick-reference

| Symptom | Cause | Fix |
|---|---|---|
| `SCIM 2.0 Base Url: Does not match required pattern` | Trailing slash | Use `.../scim/v2`, no trailing `/` |
| `Error authenticating: null` | cloudflared tunnel dead | Restart tunnel, update Base URL to the new host |
| `curl: (6) Could not resolve host` | Same as above | Same as above |
| `400 access_denied — User is not assigned` | User not assigned to that Okta app | Assign on the app's Assignments tab |
| Assignments tab says *"implicitly assigned"* | Federation Broker Mode on | Disable it (§4), verify after reload |
| `externalId is required` on group push | Okta Push Groups *by name* sends only `displayName` AND the name itself is empty/whitespace (derivation also yields `""`) | Name the group; or use Import Groups so Okta supplies a real `externalId` (§5) |
| `duplicate key … idx_idp_conn_ws_issuer` | A connection for that issuer already exists in the workspace | One connection per (workspace, issuer) — update the existing one rather than creating a second |
| SCIM base URL box shows `localhost:5173` | Phase 7 / F7-2 UI bug | Use the tunnel URL manually |
| Connection stuck `Disconnected` | Nothing pushed yet | Assign a user in Okta; check Okta System Log |

---

## 7. State at the end of the session (2026-08-28)

**Working and verified:**
- SCIM directory sync end-to-end. Okta pushed users; Zecurity flipped
  `Disconnected → Healthy`, SCIM token showed `Last used` immediately after assignment.
- The Twingate-style Zecurity onboarding wizard (popup → Connect Okta → SCIM config → mint token).
- SCIM enable **without break-glass**, confirming the Phase 12 mapping probe works in production.

**Not finished / known open:**
- **OIDC login is not usable end-to-end.** The Okta app exists with the correct redirect URI, but
  Federation Broker Mode is still `Enabled` (disable did not persist), so users cannot be assigned.
- **Separately, there is no "Sign in with Okta" button in Zecurity's UI.** `admin/src/pages/Login.tsx`
  hardcodes `provider: 'google'` on every `initiateAuth` call. The backend fully supports enterprise
  connections (`InitiateAuth(ctx, provider, workspaceName, connectionID)`), but no frontend control
  passes a `connectionId`. Tracked as **F7-5** in Phase 7. Until built, enterprise login is only
  reachable via a direct GraphQL call.
- **The tunnel is currently down**, and the SCIM app in Okta still holds a token belonging to a
  Zecurity connection that was deleted and recreated. Both must be refreshed before SCIM syncs again.
- Push Groups *by name* now works via a displayName-derived externalId (§5), with the documented
  rename-changes-key and slug-collision caveats. Import Groups remains the cleaner path.

---

## 8. Related documents

| Document | Contents |
|---|---|
| `findings.md` | The original Twingate + Okta reference walkthrough this work was modelled on |
| `.zecurity-obs/Sprint17/pending.md` | Same comparison, scoped to Sprint 17 |
| `.zecurity-obs/Sprint17/Member1-Frontend/Phase7-SCIM-Config-Missing-Fields.md` | F7-1…F7-6 UI defects found during this live run |
| `.zecurity-obs/Sprint17/Member1-Go/Phase10…12` | Backend mapping-probe / subjectClaim / equivalence work that makes the no-break-glass enable possible |

### External references

| Source | Why it matters |
|---|---|
| [Okta — Configure SCIM for a custom OIDC app](https://support.okta.com/help/s/article/configure-scim-for-a-custom-oidc-app?language=en_US) | Okta's own confirmation that SCIM **cannot** be enabled on a custom OIDC integration, and that two separate apps is the supported workaround. This is the authoritative basis for §0. |

---

## 9. Navigating the Okta Admin Console (browser-automation notes)

Recorded 2026-08-29 while driving the console through Orca's embedded browser
(`browser.clientHost.automation.v1`). The single most important finding: **how you
land on a page determines whether Okta re-prompts for MFA.**

### ⚠️ Direct URL navigation re-triggers MFA — click navigation does not

- Pasting the admin URL into the browser (**`goto https://trial-3724025-admin.okta.com`**)
  redirects through `https://trial-3724025.okta.com/oauth2/v1/authorize?...` and lands on
  the **Okta Verify MFA** screen — *"Enter a code from Okta Verify app"* for
  `xiyo@gomail.edu.pl`, with a **Verify** button and a code textbox (`e8`). It does **not**
  reuse the already-authenticated session, so you're stuck unless you can complete MFA.
- **Once a session exists in the browser profile**, navigating by **clicking links/buttons
  inside the console** moves between sections with **no MFA re-prompt**. The MFA prompt
  resolved itself the moment an in-console click happened (the push was presumably approved
  on the device, or the SSO token was already valid for same-tab navigation).

**DO / DON'T**

| | Action | Result |
|---|---|---|
| ✅ DO | Click the **"Okta Admin Console"** logo/link (top of page) to reach `/admin/dashboard` | Lands on the dashboard, no MFA |
| ✅ DO | Use the left **Main navigation** buttons (Directory, Applications and Resources, Security, Reports…) to move around once inside | No MFA |
| ❌ DON'T | `goto` the admin root or any `/admin/...` deep link cold (no session) | Bounces to Okta Verify MFA |
| ❌ DON'T | Assume the **Dashboard** nav button is a route | It's a section expander; from an app-instance page it stayed put. Use the logo link for the home dashboard |
| ⚠️ NOTE | The MFA screen's **"Back to sign in"** link may fail to resolve as a clickable element (`role=link name=Back to sign in` not found) even though the page is interactive | Prefer approving the push / entering the code over clicking that link |

### Console layout as observed (trial-3724025, 2026-08-29)

- **Plan banner:** Free Trial — **3 of 10** active users (limit), `Updated Aug 29, 2026, 12:56:54 PM`.
  Signed in as **xiyo yan** / `gomail-edu-trial-3724025`.
- **Overview** (updated Aug 28, 2026, 5:30 PM): **Users: 3**, **Groups: 1**, **SSO Apps: 9**,
  Okta service **OPERATIONAL**.
- **Main navigation:** Dashboard (→ Dashboard / Tasks / Integration Agents / Notifications /
  Getting Started), Directory, Customizations, Applications and Resources, Security, Workflow,
  Reports, Settings.
- This matches the setup in §3–§4: the 1 group + the Zecurity SCIM `Test App` and Zecurity OIDC
  `Web Application` are among the 9 SSO apps; the 3 users are the assigned people.

### Practical recipe to open the dashboard without MFA

1. If cold, `goto` the admin URL → complete Okta Verify (approve push / enter code).
2. After the session is live, **never `goto` again** — click the logo/link or nav buttons.
3. To reach the dashboard specifically from any sub-page, click **"Okta Admin Console"** (the
   home link), which routes to `/admin/dashboard`.

---

## 10. How the Okta console was driven (Orca CLI skill)

The navigation in §9 was performed by an agent through **Orca's embedded browser**, not a
human at a real browser. Recorded so it can be repeated without re-deriving the toolchain.

### Toolchain

- **Skill:** `orca-cli` (discovery stub). It deliberately omits command syntax and points at
  the binary's own guide so it can't drift.
- **Executable resolution (per skill rules):** no `ORCA_CLI_COMMAND` env, no `ORCA_DEV_REPO_ROOT`
  → used **`orca-ide`**. The skill explicitly warns *never* to run bare `orca` on Linux outside
  an Orca terminal, because it resolves to the GNOME screen reader (`/usr/bin/orca`) and would
  start speech on the user's machine.
- **App check:** `orca-ide status --json` → confirmed running (pid 121372, appVersion 1.4.191)
  and exposing `browser.clientHost.automation.v1` + `browser.screencast.v1`.
- **Guide:** `orca-ide skills get orca-cli` → printed the version-matched reference; grepped for
  the browser command set (`goto`, `snapshot`, `click`, `tab`, `wait`, `eval`). The guide
  prescribes a **snapshot → interact → re-snapshot** loop.

### Command sequence used

1. `orca-ide goto --url https://trial-3724025-admin.okta.com --json`
   → redirected to `/oauth2/v1/authorize` and landed on the Okta Verify MFA screen.
2. `orca-ide snapshot --json`
   → returned the accessibility tree with element refs (`e1…eN`), each carrying `role` + `name`.
     Used to locate clickable targets without pixel coordinates.
3. `orca-ide click --element e16 --json` ("Okta Admin Console" home link)
   → navigated to `/admin/dashboard` with no MFA re-prompt (session already live in the profile).
4. `orca-ide snapshot --json` (again)
   → extracted the dashboard counts: Users 3, Groups 1, SSO Apps 9, Okta service OPERATIONAL.

### Notes / gotchas for next time

- **Always pass `--json`** for agent-driven calls; the structured `result.refs` map is what lets
  you address elements by ref instead of fragile selectors.
- `snapshot` output is large; pipe through `python3 -c "import sys,json; print(json.load(sys.stdin)['result']['snapshot'])"`
  to get just the readable tree.
- Clicking by **ref** (e.g. `e16`) is more reliable than any text/CSS selector; refs are stable
  within a single snapshot but change after navigation, so re-snapshot before each click.
- This is the supported path for "control the browser inside Orca." For desktop UI / app chrome
  outside the embedded browser, the skill says to use `orca computer ...` instead — not these
  browser commands.
