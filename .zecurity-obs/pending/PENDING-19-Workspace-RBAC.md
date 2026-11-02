# PENDING-19: Workspace admin-console roles (RBAC)

**Status:** approved 2026-11-02. **R1 (backend) built** on local branch `feat/workspace-rbac-r1` (not pushed); build gate green. O-6/O-7 decided as R-12/R-13 and built. **R2 (admin UI) built** on the same branch. **R3 live run 2026-11-02: PASS** (41/41 API checks, UI pass; one R2 fix, one follow-up). Branch local, not pushed.
**Date:** 2026-10-31
**Base:** `fixed-pendings` (`origin/main` is stale; it has 41 gated operations, `fixed-pendings` has 73).
**Scope:** its own epic, outside Sprint 22. Sprint 22's "don't modify `admin/`" rule is why it waits; this epic does change `admin/`.

---

## 1. Problem

The workspace admin console has one effective role. `workspace_members.role` and `users.role` allow `admin`, `member` and `viewer`, but:

- all 73 console operations (23 queries, 50 mutations) carry `@hasRole(roles: [ADMIN])`, so `viewer` gets exactly what `member` gets: nothing;
- the three admin REST routes (`POST /api/invitations`, `/api/connectors/{id}/token`, `/api/shields/{id}/token`) use `RequireRole("admin")`;
- the check reads the `role` claim from the access token (`graph/resolvers/directives.go`), so a role change would only apply when the token is re-minted;
- there is no mutation to change a workspace member's role.

Teams want to give people part of the console (network operators, security staff, helpdesk) without making them full Admins.

## 2. Decisions taken (2026-10-31, Sathiya)

| # | Decision |
|---|---|
| R-1 | **One role per person**, picked from a radio list ("Manage Role" dialog). |
| R-2 | **Role set:** Admin, Viewer, DevOps, Security, Helpdesk, Member (§3). Billing, Internet Security and Access Reviewer are not mapped: Zecurity has no matching feature. |
| R-3 | **Roles are set by hand in the console.** No SCIM / IdP group → role mapping in v1. |
| R-4 | **DevOps may create connector and shield install tokens.** |
| R-5 | **A role change applies on the next request** (live role check, §5). It doesn't log the user out and doesn't touch their Zecurity client. |
| R-6 | **A reason is required** for every role change and is stored in the audit log. |
| R-7 | **Keep the Security role** (groups, access policies, device posture), although Twingate has no equivalent (was O-5). |
| R-8 | **Helpdesk can't revoke devices.** It approves and unapproves only; `revokeDevice` (permanent) is Admin-only (was O-2). |
| R-9 | **No self group changes for non-Admins** (2026-11-02, was O-1): the server refuses `addGroupMember` / `removeGroupMember` on yourself unless you are Admin. Collusion between two people remains; the audit log covers it. |
| R-10 | **Invites stay Admin-only** (2026-11-02, was O-3): `createInvitation` and `POST /api/invitations` need `MEMBERS_ADMIN`. |
| R-12 | **No resource access changes on your own group** (2026-11-02, was O-6): `assignResourceToGroup` / `unassignResourceFromGroup` are refused when the caller is a member of the group, unless Admin. |
| R-13 | **No self device approval** (2026-11-02, was O-7): `approveDevice` / `unapproveDevice` on your own device are refused unless Admin. |
| R-11 | **Never zero active Admins, from any path** (2026-11-02, was O-4). The last active Admin can't make themselves Member (no self-change, §7). SCIM is refused only when the change would leave zero active Admins; it may disable an Admin while another active Admin remains. Covered: `DirectoryService.Deprovision` (DELETE / suspend), `DirectoryService.Update` with `active=false` (PATCH), and the connection-disable bulk suspend (`idp/store.go suspendSCIMUsers`). Refusal is SCIM 409 plus an audit event, under the same Admin-row lock as `setMemberRole`. |

## 3. Roles

| Role | Console access |
|---|---|
| **Admin** | Everything, including identity (IdP, SCIM), members and roles. |
| **Viewer** | Read-only: every console query. Reads already return no secrets (§6). |
| **DevOps** | Viewer + write: remote networks, connectors, shields, resources, discovery/scan, connector/shield install tokens. |
| **Security** | Viewer + write: groups, group membership, resource↔group assignment, device posture profiles. |
| **Helpdesk** | Viewer + write: device approve / unapprove. Revoking a device (permanent) stays with Admin (R-8). |
| **Member** | No console access. Client app and self-service queries only (`me`, `myDevices`, …). |

Splitting DevOps and Security means DevOps decides **what is reachable** (resources, protect/unprotect) and Security decides **who can reach it** (groups, assignments, posture). Neither can do the whole job alone. Admin can.

## 4. Capabilities (the enforcement model)

Each operation names a **capability**. One Go table maps role → capabilities. Roles are not listed on fields.

The word is "capability", not "permission", on purpose: ADR-025 already has *permissions* (`workspace_permissions`, `grantPermission`, e.g. `identity.mapping.break_glass`). These are explicit per-user grants. **No role implies an ADR-025 permission, Admin included**, and this epic doesn't change that.

| Capability | Admin | Viewer | DevOps | Security | Helpdesk | Member |
|---|:-:|:-:|:-:|:-:|:-:|:-:|
| `CONSOLE_READ` | ✓ | ✓ | ✓ | ✓ | ✓ | |
| `NETWORK_WRITE` | ✓ | | ✓ | | | |
| `ACCESS_WRITE` | ✓ | | | ✓ | | |
| `POSTURE_WRITE` | ✓ | | | ✓ | | |
| `DEVICE_WRITE` | ✓ | | | | ✓ | |
| `DEVICE_ADMIN` | ✓ | | | | | |
| `IDENTITY_ADMIN` | ✓ | | | | | |
| `MEMBERS_ADMIN` | ✓ | | | | | |

### 4.1 Every operation

**`CONSOLE_READ` (23 queries)**
- client: `clientDevices`
- connector: `remoteNetworks`, `remoteNetwork`, `connectors`, `connector`
- shield: `shields`, `shield`
- resource: `resources`, `allResources`
- discovery: `getDiscoveredServices`, `getScanResults`
- policy: `groups`, `group`
- posture: `supportedPostureChecks`, `deviceProfiles`, `devicePostureVisibility`
- idp: `idpConnections`, `platformLoginEnabled`, `scimTokens`, `scimConflicts`, `scimProviderProfiles`
- log: `connectorLogs`
- schema: `users`

**`NETWORK_WRITE` (16 mutations + 2 REST routes)**
- connector: `createRemoteNetwork`, `deleteRemoteNetwork`, `generateConnectorToken`, `revokeConnector`, `deleteConnector`
- shield: `generateShieldToken`, `revokeShield`, `deleteShield`
- resource: `createResource`, `updateResource`, `protectResource`, `unprotectResource`, `deleteResource`, `forceDeleteResource`
- discovery: `triggerScan`, `promoteDiscoveredService`
- REST: `/api/connectors/{id}/token`, `/api/shields/{id}/token`

**`ACCESS_WRITE` (7 mutations)**
- policy: `createGroup`, `updateGroup`, `deleteGroup`, `addGroupMember`, `removeGroupMember`, `assignResourceToGroup`, `unassignResourceFromGroup`

**`POSTURE_WRITE` (8 mutations)**
- posture: `createDeviceProfile`, `updateDeviceProfileMode`, `updateDeviceProfileManualTrust`, `addProfileRequirement`, `removeProfileRequirement`, `deleteDeviceProfile`, `bindResourceToProfile`, `unbindResourceFromProfile`

**`DEVICE_WRITE` (2 mutations)**
- client: `approveDevice`, `unapproveDevice`

**`DEVICE_ADMIN` (1 mutation)**
- client: `revokeDevice` (permanent; R-8)

**`IDENTITY_ADMIN` (15 mutations)**
- idp: `createIdpConnection`, `updateIdpConnection`, `setIdpConnectionStatus`, `deleteIdpConnection`, `testIdpConnection`, `updateScimConfig`, `setPlatformLoginEnabled`, `mintScimToken`, `rotateScimToken`, `revokeScimToken`, `acceptScimConflict`, `rejectScimConflict`, `reopenScimConflict`, `grantPermission`, `enableScimBreakGlass`
- `enableScimBreakGlass` still also needs the explicit ADR-025 row, as today.

**`MEMBERS_ADMIN` (1 mutation + 1 REST route + 1 new mutation)**
- client: `createInvitation`; REST `POST /api/invitations`
- new: `setMemberRole` (§7)

Total: 23 + 16 + 7 + 8 + 2 + 1 + 15 + 1 = **73**, plus 3 REST routes and the new mutation.

### 4.2 Schema shape

- Replace `@hasRole(roles: [ADMIN])` with `@requires(capability: X)` on every field. `@hasRole` is removed, so there is one mechanism.
- Add `enum Capability` and `User.capabilities: [Capability!]!` (on `me`), computed from the **live** role. The admin UI uses it to show tabs; the server still checks every call.
- Extend `enum Role` with `DEVOPS`, `SECURITY`, `HELPDESK`.
- The REST routes swap `RequireRole("admin")` for the same capability check.

## 5. Live role check (R-5)

- **Source of truth:** `users.role`. Login reads it (`identity/resolver.go`) and puts it in the token. `workspace_members.role` is written at invite/join with the same value; `setMemberRole` updates both in one transaction.
- **On every console request** (`/graphql` field with `@requires`, and the three REST routes): after the JWT is validated, look up the caller's current `role` and `status` by `(tenant_id, user_id)`.
- **Cache:** process-local map, allowed by D-02 (single controller instance). Entries are dropped on `setMemberRole`, user disable/removal and SCIM deprovision, and expire after 30 s as a backstop for direct SQL or bootstrap changes.
- **Fail closed:** a failed lookup, a missing row, or a non-`active` user denies the call.
- **The JWT `role` claim becomes a display hint only.** No authorization path reads it.
- **No `identity_generation` bump.** A bump only takes effect at `/auth/refresh` (the access token lives up to 15 min) and would also force the user's client to log in again (`client/issuance_authz.go`). The live check is immediate and leaves the client alone.
- Demoting someone to Member keeps `me`, `myDevices` and their client working.

## 6. Secrets

Today's reads already return no secrets: IdP `clientSecret` is write-only, `scimTokens` lists metadata, invitation reads have no token. Secrets come back only from mutations:

- connector/shield install tokens: `NETWORK_WRITE` (Admin, DevOps; R-4);
- SCIM tokens (`mintScimToken`, `rotateScimToken`): `IDENTITY_ADMIN` (Admin).

So Viewer can read every query. A test pins this: any new read field that returns a token or secret fails the build unless it is moved to a write capability.

## 7. Changing a role: `setMemberRole(userId, role, reason!)`

- `MEMBERS_ADMIN` only.
- **No self-change:** an Admin can't change their own role.
- **Never zero Admins:** in one transaction, lock every active Admin row of the workspace (`… FOR UPDATE`, id order) and the target row, then refuse to demote the last active Admin. This is the same pattern as `lockActiveSuperAdmins` in `provider/operators.go`.
- **Reason required** (R-6): trimmed, non-empty, length-capped; stored in the audit row, never logged elsewhere.
- **Audit:** actor, target, from-role, to-role, reason.
- A no-op change (same role) succeeds without an audit row, as on the provider side.
- Clears the target's cache entry after commit.

## 8. Decisions still open

O-1, O-3 and O-4 were decided on 2026-11-02 as R-9, R-10 and R-11 (§2). The O-4 code check found that SCIM could disable the last Admin through three paths with no guard; R-11 closes them.

O-6 and O-7 (raised by the automated security review of the R1 commit) were decided on 2026-11-02 as R-12 and R-13 and built in R1.

## 9. Database

- One new file, the next free number at merge time (currently `047_`; `043_` is reserved for Phase D): widen the CHECK on `users.role` and `workspace_members.role` to `('admin','viewer','devops','security','helpdesk','member')`.
- PR label `[schema: reset DB]`; everyone recreates their local DB. No migration framework.

## 10. Tests (build gate)

- **Completeness:** extend `controller/graph/schema_authz_test.go`. Every Query/Mutation field must carry `@requires` or be in the public/self-service allowlist. Unannotated means deny.
- **Matrix:** role × operation for all 73 fields, the 3 REST routes and `setMemberRole`, generated from the capability table.
- **DB-backed** (must run, not skip):
  - two concurrent demotions of the last two Admins: exactly one succeeds;
  - self-change refused;
  - a demotion applies on the caller's very next request;
  - cache invalidation on role change, disable and deprovision;
  - SCIM DELETE, PATCH `active=false` and connection disable on the last active Admin: refused (409, audited); on an Admin with another active Admin: allowed (R-11);
  - a non-Admin adding or removing themselves from a group: refused (R-9);
  - fail closed when the lookup errors;
  - a Member keeps `me` / `myDevices` / client issuance.
- **Secrets:** no read field of a `CONSOLE_READ` query returns a token or secret.

## 11. Phases

1. **R1 — backend:** migration, capability table, `@requires`, live check (GraphQL and REST), `setMemberRole`, audit, tests. **Built 2026-11-02** (`feat/workspace-rbac-r1`): migration `047_workspace_roles.sql`, `internal/rbac` (table, cached `Checker` with race-safe invalidation, last-Admin lock), `graph/resolvers/directives.go` (`NewRequires`), `middleware.RequireCapability` (replaces `RequireRole`, removed), `member_role.go` (`setMemberRole`, R-9), SCIM/IdP guards (R-11). The authz test now globs every `*.graphqls`: the old hand-kept list missed `idp.graphqls` and `posture.graphqls`. Known gap: `/auth/refresh` re-mints the JWT with the old claim role, so the **display** hint stays stale until re-login (no authz effect).
2. **R2 — admin UI:** Manage Role dialog (radio list + required reason), tabs and buttons driven by `me.capabilities`, read-only views for Viewer. **Built 2026-11-02:** `admin/src/lib/capabilities.ts` (mutation → capability map mirroring `@requires`, `canUseConsole`, `useCan`), console guard on `CONSOLE_READ` (Members → `/install`), every write control hidden per capability (pages and modal triggers; install tokens never minted without `NETWORK_WRITE`), R-9/R-12/R-13 reflected in the UI, `ManageRoleDialog` on Users. Tests: parity with `controller/graph/*.graphqls`, console guard, dialog, per-role page rendering, Viewer sweep over 18 page states (25 files, 195 tests). Known: the console reads `me.capabilities` once per load, so a user demoted mid-session keeps their controls until reload; the server refuses every call (R3 expectation). Build and typecheck pass; lint unchanged (17 pre-existing problems).
3. **R3 — live acceptance on the lab:** one account per role; each role tries an allowed and a refused action; a demotion mid-session applies at once; the last-Admin guard.

   **R3 result (2026-11-02, local stack, controller and console from `feat/workspace-rbac-r1`; local test login):**
   - Setup through the product paths: workspace via the bootstrap code, users invited with `createInvitation` and joined by first login as Member, every role then set with `setMemberRole` (with reason).
   - **API, 41/41 PASS:**
     - each role's allowed and refused GraphQL calls;
     - the REST token and invitation routes (200/403);
     - R-9 (also with an upper-cased own ID), R-10, R-12, R-13 and R-8;
     - Member keeps `me` and `myDevices`;
     - a mid-session demotion refused the same token's next write, and a re-promotion restored it without re-login (the JWT role claim stayed `member` throughout: display only);
     - self-change refused, also by an upper-cased ID; a missing reason refused; demoting one of two Admins allowed;
     - 8 audit rows for 8 changes, none for refusals.
   - Helpdesk device checks used `client_devices` rows inserted by SQL, since there was no real client in this run.
   - SCIM last-Admin paths (R-11) were not live-checked: no SCIM users in this workspace. They are covered by the DB tests.
   - **UI:**
     - Viewer pages show no write controls;
     - a Member lands on `/install`;
     - the Admin changed a role through the Manage Role dialog, and the server and audit row agree;
     - Helpdesk sees approve only on another user's device and no revoke.
   - **Fix found by R3** (R2): the sidebar showed Identity Providers, Access Log, Devices, Policies and Install Client only to `ADMIN`. Fixed: shown to every console role, with a `Sidebar.test.tsx` that fails on the old code.
   - **Follow-up (not fixed):** after a mid-session demotion, a write from a page that is still open is refused by the server, but some pages show no message. `RemoteNetworks.handleCreate` doesn't catch the mutation error, so the form just stays open. The console reads `me.capabilities` once per load, and a reload shows the new role's controls. Options: show mutation errors on every page, or re-probe `me` when a call is refused.

## 12. Reference: Twingate admin console (looked at 2026-10-31)

Looked at read-only in Sathiya's own Twingate tenant (Orca's built-in browser), plus Twingate's docs ([Admins](https://www.twingate.com/docs/admins), [Admin Roles changelog](https://www.twingate.com/changelog/admin-roles)). Used as a reference for what roles to offer, not to copy.

**Console layout (tab → pages):**

| Tab | Pages |
|---|---|
| Network | Overview, Resources, Remote Networks, Connectors |
| Team | Users, Groups, Services (service accounts) |
| Devices | Devices, Serial Numbers |
| Policies | Resource Policies, Sign In Policy, Device Profiles |
| Internet Security | Secure DNS, Client Configuration |
| Settings | General (incl. Admin Console Security: re-auth interval, admin MFA), Billing, Identity Providers, Device Integrations, Certificate Authorities, Reports, API, Notification Channels, Notifications |

**Roles (docs and the Manage Role dialog):**

| Role | Write | Read |
|---|---|---|
| Admin | everything | everything |
| Viewer (formerly Support) | nothing | everything except Secure DNS, Client Configuration, Billing |
| DevOps | Network tab only. Groups and policies are read-only. | as Viewer |
| Internet Security | Internet Security tab only | everything except Billing |
| Helpdesk | reset user MFA; verify/unverify devices; add/delete/verify/unverify serial numbers | as Viewer |
| Access Reviewer | Access Requests page only (all users and resources) | Access Requests page only |
| Billing | Billing page only | Billing page only |
| Member | no Admin Console access | none |

**Observed in the console:**
- **One role per user.** It is set from the user page with "Manage Role" (a radio list). The Network overview counts users per role ("2 Admin Users", "1 DevOps User", …).
- **IdP-synced users get roles set by hand.** Users from Okta and from social login both show a console-set role, and Okta-synced users can't be edited ("Edit User" is disabled), but "Manage Role" still works. This matches R-3.
- **A limited-role admin sees a badge,** and the console refuses blocked changes with an error (docs).
- **User actions:** Manage Role, Delete, Add to Group, access report and access graph.
- **Device actions:** Verify Device, Archive, Block. Helpdesk has only Verify/Unverify.
- **Not documented by Twingate:** who may change roles, a last-admin rule, whether a role change forces a new login, and IdP/SCIM role mapping. This note decides those itself (§5, §7, R-3).

**Mapping to Zecurity:**

| Twingate | Zecurity |
|---|---|
| Network tab | DevOps / `NETWORK_WRITE` (remote networks, connectors, shields, resources, discovery) |
| Team → Groups, Policies → Resource Policies | `ACCESS_WRITE` (Security, R-7) |
| Policies → Device Profiles | `POSTURE_WRITE` (Security, R-7) |
| Devices → Verify/Unverify | Helpdesk / `approveDevice`, `unapproveDevice` |
| Devices → Block | `revokeDevice` (Admin only, R-8). In Zecurity it is permanent. |
| Helpdesk "reset MFA" | none. User MFA belongs to the customer's IdP. |
| Settings → Identity Providers | `IDENTITY_ADMIN` (Admin) |
| Internet Security, Billing, Access Reviewer, Serial Numbers | no Zecurity feature; not mapped |
