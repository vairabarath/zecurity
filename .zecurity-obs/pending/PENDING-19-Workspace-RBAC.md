# PENDING-19: Workspace admin-console roles (RBAC)

**Status:** design note, awaiting approval. Nothing is built.
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

## 3. Roles

| Role | Console access |
|---|---|
| **Admin** | Everything, including identity (IdP, SCIM), members and roles. |
| **Viewer** | Read-only: every console query. Reads already return no secrets (§6). |
| **DevOps** | Viewer + write: remote networks, connectors, shields, resources, discovery/scan, connector/shield install tokens. |
| **Security** | Viewer + write: groups, group membership, resource↔group assignment, device posture profiles. |
| **Helpdesk** | Viewer + write: device approve / unapprove / revoke. |
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

**`DEVICE_WRITE` (3 mutations)**
- client: `approveDevice`, `unapproveDevice`, `revokeDevice`

**`IDENTITY_ADMIN` (15 mutations)**
- idp: `createIdpConnection`, `updateIdpConnection`, `setIdpConnectionStatus`, `deleteIdpConnection`, `testIdpConnection`, `updateScimConfig`, `setPlatformLoginEnabled`, `mintScimToken`, `rotateScimToken`, `revokeScimToken`, `acceptScimConflict`, `rejectScimConflict`, `reopenScimConflict`, `grantPermission`, `enableScimBreakGlass`
- `enableScimBreakGlass` still also needs the explicit ADR-025 row, as today.

**`MEMBERS_ADMIN` (1 mutation + 1 REST route + 1 new mutation)**
- client: `createInvitation`; REST `POST /api/invitations`
- new: `setMemberRole` (§7)

Total: 23 + 16 + 7 + 8 + 3 + 15 + 1 = **73**, plus 3 REST routes and the new mutation.

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

| # | Question | Recommendation |
|---|---|---|
| O-1 | **Security can add itself to a group** (`addGroupMember`) and so gain network access to resources. | Server refuses `addGroupMember` / `removeGroupMember` on yourself unless you are Admin. Collusion between two people remains; the audit log covers it. |
| O-2 | **Helpdesk and `revokeDevice`:** revoke is terminal. | Keep it in Helpdesk (it is the "device verification" job), and say "permanent" in the UI confirmation. |
| O-3 | **Who can invite:** `createInvitation` is Admin-only here. | Keep it. Inviting adds a person to the workspace; that is a membership decision. |
| O-4 | **SCIM deprovision of the last Admin** bypasses `setMemberRole`. | Check during build whether the SCIM path can disable the last active Admin; if it can, apply the same guard or file it separately. |

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
  - fail closed when the lookup errors;
  - a Member keeps `me` / `myDevices` / client issuance.
- **Secrets:** no read field of a `CONSOLE_READ` query returns a token or secret.

## 11. Phases

1. **R1 — backend:** migration, capability table, `@requires`, live check (GraphQL and REST), `setMemberRole`, audit, tests.
2. **R2 — admin UI:** Manage Role dialog (radio list + required reason), tabs and buttons driven by `me.capabilities`, read-only views for Viewer.
3. **R3 — live acceptance on the lab:** one account per role; each role tries an allowed and a refused action; a demotion mid-session applies at once; the last-Admin guard.
