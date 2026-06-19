---
type: code-study
flow: connector-enrollment
created: 2026-05-05
---

# Code Study 04 — Connector Enrollment Flow

> Trace creating a connector: admin clicks "Add Connector" → JWT install token minted → admin runs curl pipe bash on a server → Rust connector enrolls via gRPC → workspace CA signs a cert → connector enters operational state with heartbeats and ACL snapshots.

---

## What Is "Connector Enrollment"?

A **Connector** is a Rust binary running on a server inside the customer's network. It's the controller's outpost — proxies device traffic, applies ACL snapshots, and hosts Shields. To exist, it needs:

1. A row in the `connectors` table (created when admin generates the install command)
2. A signed certificate from the workspace CA (issued during enrollment)
3. A persistent state file (`state.json`) on the connector host

Two halves:

**Half A — Admin half** (instant, GraphQL via admin UI)
- Admin creates the connector → backend reserves a `connectors` row in `status='pending'` and mints a single-use JWT enrollment token
- Returns an install command (curl pipe to bash) the admin copies and runs on the server

**Half B — Server half** (Rust connector boots, gRPC)
- Install script writes the token to `/etc/zecurity/connector.conf`, installs the binary, starts systemd
- Connector starts → sees no `state.json` → runs the 10-step enrollment
- Connector generates a keypair, builds CSR, calls `Enroll` gRPC
- Controller burns the JTI, verifies CSR, workspace CA signs the cert
- Connector saves cert + state, transitions into normal heartbeat operation

---

## High-Level Flow

```
[ADMIN]                                  [BACKEND]                                  [SERVER]
admin → AllConnectors                       │
clicks "Add Connector"                      │
fills name + network                        │
  ─GenerateConnectorToken mutation────────▶│
                                            │ resolver: verify network active
                                            │ INSERT connectors (status=pending)
                                            │ compute SHA-256 of intermediate CA
                                            │ sign enrollment JWT (HS256, 24h)
                                            │ Redis SET enrollment:jti:<jti>=conn_id
                                            │ UPDATE connectors SET enrollment_token_jti
                                            │ build curl install command
  ◀──── { connectorId, installCommand } ───│
                                            │
admin sees install cmd on detail page       │
copies + runs on server ─────────────────────────────────────────────────────────▶ install script
                                            │                                     writes /etc/zecurity/connector.conf
                                            │                                     installs binary
                                            │                                     systemd start
                                            │                                                  │
                                            │                                                  ▼
                                            │                                           main.rs loads config
                                            │                                           no state.json → enroll()
                                            │                                                  │
                                            │                                           parse JWT (no verify)
                                            │◀──GET /ca.crt over plain HTTP──────────────────│
                                            │ ──intermediate CA PEM────────────────────────▶ │
                                            │                                           SHA-256 match ✓ MITM defense
                                            │                                           generate EC P-384 keypair
                                            │                                           save mode 0600
                                            │                                           build CSR with SPIFFE SAN
                                            │                                           open gRPC TLS channel
                                            │                                                  │
                                            │◀──Enroll(token, csr, hostname, version)────────│
                                            │ verify JWT signature (HS256+iss+exp)
                                            │ Redis GETDEL enrollment:jti:<jti> ← atomic burn
                                            │ verify connector row status=pending
                                            │ verify workspace status=active
                                            │ parse CSR, CheckSignature, SPIFFE URI match
                                            │ PKIService.SignConnectorCert
                                            │   load+decrypt workspace CA private key
                                            │   x509.CreateCertificate
                                            │ UPDATE connectors SET status=active
                                            │ ────EnrollResponse(cert + CA chain)──────────▶ │
                                            │                                           save connector.crt
                                            │                                           save workspace_ca.crt
                                            │                                           save state.json
                                            │                                           best-effort clean config
                                            │                                                  │
                                            │                                           main resumes:
                                            │                                             build mTLS channel
                                            │                                             spawn Shield server :9091
                                            │                                             spawn device tunnels :9092
                                            │                                             refresh CRL + spawn 5min loop
                                            │                                             notify systemd READY=1
                                            │                                             spawn watchdog
                                            │◀──Heartbeat (every 15s)─────────────────────────│
                                            │ ──HeartbeatAck + ACLSnapshot───────────────────▶│
                                            │                                           PolicyCache populated
                                            │                                           devices can authorize
```

---

## Study Tracker

> Review **one stage at a time** against the live code. Tick a box when you've walked that stage and confirmed the doc matches the source. Record anything you find **inline in that stage's section** (add a `**Findings**` block under it) — this tracker stays just a map of what's been reviewed. No findings are recorded yet.

**Half A — Admin Generates Token**
- [x] Stage 1 — Admin opens /connectors, clicks Add Connector ✅ findings (server gate confirmed real; allow-by-default guard test added)
- [x] Stage 2 — Apollo sends the mutation
- [x] Stage 3 — Middleware → gqlgen → resolver
- [x] Stage 4 — Verify remote network is active
- [x] Stage 5 — INSERT INTO connectors (status='pending')
- [x] Stage 6 — Compute the CA fingerprint
- [x] Stage 7 — Sign the enrollment JWT
- [x] Stage 8 — Store the JTI in Redis
- [x] Stage 9 — UPDATE connector row + build install command
- [x] Stage 10 — Frontend shows install command ⚠️ findings (REST regen path, double-mint; ~~role asymmetry~~ **retracted** — both paths are admin-gated)

**Half B — Server Side (connector enrolls)**
- [ ] Stage 11 — Admin runs the install command
- [ ] Stage 12 — Connector boots, enters enrollment flow
- [ ] Stage 13 — Parse JWT payload (no signature verify)
- [ ] Stage 14 — Fetch /ca.crt over plain HTTP
- [ ] Stage 15 — Verify CA fingerprint (MITM defense)
- [ ] Stage 16 — Generate EC P-384 keypair, save mode 0600
- [ ] Stage 17 — Build CSR with CN + SPIFFE SAN URI
- [ ] Stage 18 — Open gRPC TLS channel rooted in verified CA
- [ ] Stage 19 — Call Enroll RPC

**Controller-Side Enrollment Handler**
- [ ] Stage 20 — Verify the JWT
- [ ] Stage 21 — Atomic BurnEnrollmentJTI
- [ ] Stage 22 — Verify connector row is pending, tenant matches
- [ ] Stage 23 — Verify workspace is active
- [ ] Stage 24 — Parse CSR, verify signature, verify SPIFFE SAN
- [ ] Stage 25 — Workspace CA signs the connector cert
- [ ] Stage 26 — UPDATE connectors → active
- [ ] Stage 27 — Return EnrollResponse

**Back On the Connector**
- [ ] Stage 28 — Save cert, CA chain, state.json
- [ ] Stage 29 — Best-effort clean up connector.conf

**Stage 30 — Connector Becomes Operational**
- [ ] 30.0 — Re-read state.json
- [ ] 30.1 — Build controller mTLS channel
- [ ] 30.2 — ShieldRegistry + spawn :9091 server
- [ ] 30.3 — Auto-updater spawn
- [ ] 30.4 — Cert store + empty PolicyCache
- [ ] 30.5 — LAN IP for QUIC advertise
- [ ] 30.6 — CRL manager + spawn refresh
- [ ] 30.7 — TLS + QUIC listeners on :9092
- [ ] 30.8 — Notify systemd READY + spawn watchdog
- [ ] 30.9 — run_control_stream (heartbeats + ACL snapshots, blocks forever)

---

# Identity Reference (SPIFFE)

> Consolidated quick-reference for the identities this flow issues and verifies — pulled together from the per-stage SPIFFE builders so you don't have to reconstruct them. Definitions live in [appmeta/identity.go](controller/internal/appmeta/identity.go) (Go) and [connector/src/appmeta.rs](connector/src/appmeta.rs) (Rust).

| Identity | Format | Go | Rust |
|---|---|---|---|
| Vendor trust domain | `zecurity.in` | `identity.go:28` | `appmeta.rs` |
| Workspace trust domain | `ws-{slug}.zecurity.in` | `identity.go:66-68` | — |
| Controller | `spiffe://zecurity.in/controller/global` | `identity.go:32` | `appmeta.rs:39` |
| Connector | `spiffe://{trust_domain}/connector/{connector_id}` | `identity.go:77-79` | `appmeta.rs:95-100` |
| Shield | `spiffe://{trust_domain}/shield/{shield_id}` | `identity.go:82-84` | — |

**Chain:** connector leaf ← **Workspace CA** (per-tenant, `workspace_ca_keys`) ← **Intermediate CA** (platform, `ca_intermediate`) ← Root.
**Key type everywhere:** EC **P-384** (ECDSA P-384 / SHA-384).
**Connector cert TTL:** 7 days (`CONNECTOR_CERT_TTL`). The connector leaf carries **both** `ClientAuth` (it is a client to the controller) **and** `ServerAuth` (it is a TLS server to shields on `:9091`) EKUs — which is why the same leaf works in Stage 30's mTLS channel and the `:9091` server.

---

# Half A — Admin Generates Token

## Stage 1 — Admin Opens /connectors, Clicks Add Connector

[admin/src/pages/AllConnectors.tsx](admin/src/pages/AllConnectors.tsx) at `/connectors`. Admin-only route gated by [`AdminLayout`](admin/src/App.tsx#L43).

Local state:
```tsx
const [showAdd, setShowAdd] = useState(false)
const { data } = useQuery(GetRemoteNetworksDocument, {
  fetchPolicy: 'cache-and-network',
  pollInterval: 30000,
})
```

The page doesn't have its own list query — it reuses `GetRemoteNetworks` and flattens connectors from each network:
```tsx
const allConnectors = networks.flatMap((network) =>
  (network.connectors ?? []).map((c) => ({ ...c, networkId: network.id, networkName: network.name })),
)
```

A connector always belongs to exactly one remote network. The "Add Connector" button is **disabled when there are no networks** — you can't have a connector without a network to assign it to.

Clicking opens [InstallCommandModal](admin/src/components/InstallCommandModal.tsx) — shared between Connector and Shield variants. Three form fields:
- **Connector Name** — required, free text
- **Remote Network** — required, dropdown from `GetRemoteNetworks`
- **Platform** — Linux vs Docker toggle (UI-only at this stage)

`canSubmit = !!agentName.trim() && !!networkId && !loading` — submit button disabled until both required fields filled.

> **Findings** (verified 2026-06-16) — Security analysis of Stage 1's auth posture:
>
> 1. **Server-side admin gate is real.** `AdminLayout` (client-side React routing) hides `/connectors` from non-admins, but the actual enforcement lives at the GraphQL layer: `generateConnectorToken` carries `@hasRole(roles: [ADMIN])` ([connector.graphqls:30](controller/graph/connector.graphqls#L30)), enforced by `HasRole` ([directives.go:23](controller/graph/resolvers/directives.go#L23)). A non-admin who bypasses the UI and calls the mutation directly gets `"forbidden"`. The client-side gate is UX, not security.
>
> 2. **V2 — AdminLayout fails open on null user → FIXED.** The check was `if (user && user.role !== 'ADMIN')` — false when `user` is null → rendered `<AppShell />`. Now default-deny: `if (!user || user.role !== 'ADMIN')` ([App.tsx:47](admin/src/App.tsx#L47)). Not exploitable (server is the real gate), but no longer relies on the `useRequireAuth` set-user-before-ready invariant.
>
> 3. **Systemic risk: allow-by-default schema → FIXED with a guard test.** gqlgen does not require `@hasRole`. Today every Query/Mutation field is correctly annotated — but nothing enforced that a *new* field MUST be annotated. The real risk isn't this page; it's the next field someone adds and forgets to annotate. **Fix shipped:** [`schema_authz_test.go`](controller/graph/schema_authz_test.go) walks every Query/Mutation field (across both `type` in `schema.graphqls` **and** `extend type` in the other 7 files — gqlparser splits these into `doc.Definitions` vs `doc.Extensions`, so both must be walked) and fails CI unless each field carries `@hasRole` or is on an explicit `publicAllowlist`. Turns "remember to annotate" into "the build won't pass unless you do." A `minExpectedFields` floor prevents the test from silently going vacuous. **It immediately caught two fields a manual audit missed** — `Query.workspace` and `Query.myDevices` — both correctly *member-scoped* (not admin), so they were added to the allowlist (authenticated, non-admin tier) rather than gated. Net: the guard found real coverage gaps on its first run, and there are now **no ungated fields outside the justified allowlist**.
>
> 4. **No dedicated connectors list query.** `AllConnectors` flattens connectors from `GetRemoteNetworks` because `GetConnectors` requires a `$remoteNetworkId` — there's no "all connectors across all networks" query. Works fine at current scale; would need pagination at 100+ networks.
>
> **Fixes applied (2026-06-16), batched after the stage review:**
>
> - **I1 — name validation (connector + shield).** New `validateAgentName(kind, raw)` ([validation.go](controller/graph/resolvers/validation.go)): trim, require non-empty, cap 64 runes, reject control chars. Wired into `GenerateConnectorToken` and `GenerateShieldToken`; client `maxLength={64}` on both modal inputs. Not an XSS vector (React escapes; never reaches a shell) — this is length/DB-hygiene + trimming.
> - **G1 — dead Platform toggle → honest.** Linux/Docker was cosmetic (state never sent; no builder branches on it; no Docker install path exists — both builders hardcode the Linux `curl | bash`). Docker is now `disabled` + "Soon" in the modal (both variants) and the `ConnectorDetail`/`ShieldDetail` install headers. Not wired through (Docker install remains a future feature).
> - **I2 — internal error leakage → server-side mask.** Resolvers wrapped errors with `fmt.Errorf("...: %w", err)`; gqlgen serialized the full chain (e.g. Postgres `…connectors_tenant_id_name_key`) to clients. Added fail-closed `ErrorPresenter` ([presenter.go](controller/graph/resolvers/presenter.go), wired in [main.go](controller/cmd/server/main.go)): only `apperr.UserError` ([internal/apperr](controller/internal/apperr/apperr.go)) and gqlgen parse/validation errors reach clients; everything else is logged server-side and returned as `"an unexpected error occurred"` (`code=INTERNAL`). User-facing messages were marked across `directives.go` (auth/authz), `validation.go`, the connector/shield resolvers (incl. PG `23505` → friendly "already exists"), and `resource/store.go` ("no shield…"). Covered by [presenter_test.go](controller/graph/resolvers/presenter_test.go).
> - **I3 — list pages mask load failures as empty state.** `AllConnectors` (and siblings) destructured only `{ data, loading }`, so a failed `GetRemoteNetworks` rendered "create a remote network first" — a failure disguised as an empty workspace. Added reusable `ErrorState` ([console.tsx](admin/src/lib/console.tsx)) and an error branch with Retry in [AllConnectors.tsx](admin/src/pages/AllConnectors.tsx). **Siblings also fixed:** `RemoteNetworks.tsx` and `AllShields.tsx` (gated on `error && networks.length === 0`), and `Resources.tsx` (gated on `error && resources.length === 0`, keying off its `GetAllResources` query — the bespoke case). All list pages now distinguish a load failure from an empty workspace.

## Stage 2 — Apollo Sends the Mutation

[admin/src/graphql/mutations.graphql line 22](admin/src/graphql/mutations.graphql#L22):
```graphql
mutation GenerateConnectorToken($remoteNetworkId: ID!, $connectorName: String!) {
  generateConnectorToken(remoteNetworkId: $remoteNetworkId, connectorName: $connectorName) {
    connectorId
    installCommand
  }
}
```

[InstallCommandModal.tsx line 99](admin/src/components/InstallCommandModal.tsx#L99):
```tsx
const result = await generateConnectorToken({
  variables: { remoteNetworkId: networkId, connectorName: agentName.trim() },
  refetchQueries: [{ query: GetRemoteNetworksDocument }],
  awaitRefetchQueries: true,
})

const connectorId = result.data?.generateConnectorToken.connectorId
handleClose()
if (connectorId) navigate(`/connectors/${connectorId}`)
```

- **`refetchQueries`** — Apollo re-runs `GetRemoteNetworks` after success so the new connector appears in lists
- **`awaitRefetchQueries: true`** — wait for refetch to complete BEFORE resolving the await; ensures the detail page finds the new connector

> **✅ RESOLVED (2026-06-19, F3 — data-ownership refactor).** The `awaitRefetchQueries: true` above (and on the shield path) was a workaround for `ConnectorDetail` having no source of its own — it `.find()`-ed the connector *inside* the workspace-wide `GetRemoteNetworks`, so the create flow had to block on that full refetch before navigating. Fixed by making the detail pages **own their data via by-id queries**: `ConnectorDetail` now uses `GetConnector(id)` (+ `GetRemoteNetwork(remoteNetworkId)` for the name, `GetShields(remoteNetworkId)` for siblings), with **zero dependency on the `GetRemoteNetworks` cache** — mirroring `ShieldDetail`, which already worked this way (`GetShield(id)`). `awaitRefetchQueries` was removed on both create paths; the `refetchQueries` list refresh is now **background-only**, so create navigates immediately. `revokeConnector` refetches `GetConnector(id)` (not the workspace query the page no longer reads). No `cache.modify`/`writeFragment`/fabricated entries.

[authLink](admin/src/apollo/links/auth.ts) attaches `Authorization: Bearer <admin's JWT>` when a token exists, and nothing else. (As of ADR-010 the client no longer declares public operations — the `X-Public-Operation` header and the `PUBLIC_OPERATIONS` list were removed; the server classifies public-vs-protected by parsing the request itself.)

## Stage 3 — Middleware → gqlgen → Resolver

[main.go line 156](controller/cmd/server/main.go#L156):
```go
mux.Handle("/graphql", routeGraphQL(protected, gqlSrv))
```

`routeGraphQL` ([main.go](controller/cmd/server/main.go)) now **parses the request body server-side** and routes to `public` only if it is a single query/mutation whose every root field is in the server's `publicRootFields` allowlist (`initiateAuth`, `lookupWorkspace`, `lookupWorkspacesByEmail`); anything else (incl. all of `GenerateConnectorToken`) falls through to `protected`. No client header is involved (ADR-010).

`protected` chain:
```go
AuthMiddleware → WorkspaceGuard → gqlSrv
```

[AuthMiddleware](controller/internal/middleware/auth.go#L27): verifies JWT (HMAC + iss + exp), extracts claims, injects `tenant.TenantContext` via `tenant.Set(ctx, ...)`.

[WorkspaceGuard](controller/internal/middleware/workspace.go#L21): `SELECT status FROM workspaces WHERE id = $1`, rejects if not `'active'`.

gqlgen dispatches to [connector.resolvers.go line 72](controller/graph/resolvers/connector.resolvers.go#L72):
```go
func (r *mutationResolver) GenerateConnectorToken(ctx context.Context, remoteNetworkID string, connectorName string) (*graph.ConnectorToken, error) {
    tc := tenant.MustGet(ctx)
    ...
}
```

**No role check here.** Unlike `CreateInvitation`, any workspace member can create connectors. Likely intentional — DevOps engineers who aren't admins can still spin up infrastructure.

## Stage 4 — Verify Remote Network Is Active

[connector.resolvers.go line 75](controller/graph/resolvers/connector.resolvers.go#L75):
```go
var rnStatus string
err := r.TenantDB.QueryRow(ctx,
    `SELECT status FROM remote_networks WHERE id = $1 AND tenant_id = $2`,
    remoteNetworkID, tc.TenantID,
).Scan(&rnStatus)
if err != nil { return error("not found") }
if rnStatus != "active" { return error(...) }
```

**`WHERE id = $1 AND tenant_id = $2`** — defense in depth. Even though `remote_networks.id` is globally unique, also filter by `tenant_id` so cross-tenant probes return zero rows (look like "not found" rather than leaking existence).

`pgx.ErrNoRows` → wrapped as "remote network not found." `%w` preserves the original for logs.

Status whitelist (single allowed value: `'active'`). Adding connectors to soft-deleted networks would create orphan infrastructure.

## Stage 5 — INSERT INTO connectors with status='pending'

[connector.resolvers.go line 87](controller/graph/resolvers/connector.resolvers.go#L87):
```go
var connectorID string
err = r.TenantDB.QueryRow(ctx,
    `INSERT INTO connectors (tenant_id, remote_network_id, name)
     VALUES ($1, $2, $3) RETURNING id`,
    tc.TenantID, remoteNetworkID, connectorName,
).Scan(&connectorID)
```

Only three columns explicitly set. Everything else uses DB defaults:
- `status` → `'pending'` (default)
- `hostname`, `version`, `cert_serial`, `cert_not_after` → NULL (filled at enrollment)
- `enrollment_token_jti` → NULL (filled in Stage 9)
- `last_heartbeat_at` → NULL (set at enrollment)
- `created_at`, `updated_at` → `NOW()`

**`RETURNING id`** is Postgres-specific. One round-trip instead of `INSERT` then `SELECT WHERE just_inserted`.

What `'pending'` means downstream:
- [Enroll handler](controller/internal/connector/enrollment.go#L103) refuses to enroll if status ≠ `'pending'`. A connector can only enroll once
- UI shows "not installed" badge for pending connectors
- Pending connectors don't count against `RemoteNetwork.NetworkHealth` calculations
- `DeleteRemoteNetwork` allows deletion only when no active/disconnected connectors exist; pending ones don't count

**Important gap**: Stages 5-9 are NOT wrapped in a single transaction. If anything between INSERT and Stage 9 UPDATE fails (Redis down, etc.), the connectors row exists with `status='pending'` but no usable token. Admin must delete and retry. Simplicity over atomicity.

## Stage 6 — Compute the CA Fingerprint

The MITM defense's seed. [connector.resolvers.go line 107](controller/graph/resolvers/connector.resolvers.go#L107):
```go
var certPEM string
r.Pool.QueryRow(ctx, `SELECT certificate_pem FROM ca_intermediate LIMIT 1`).Scan(&certPEM)

block, _ := pem.Decode([]byte(certPEM))
if block == nil { return error("decode intermediate CA PEM") }
fingerprint := sha256.Sum256(block.Bytes)
caFingerprint := hex.EncodeToString(fingerprint[:])
```

`r.Pool` (raw, not tenant-scoped) — the intermediate CA is **global**, not per-tenant.

`LIMIT 1` is defensive — only one row, but future rotation might add more.

PEM → DER → SHA-256 → hex:
- PEM is `-----BEGIN/END CERTIFICATE-----` wrapped base64
- `pem.Decode` strips wrappers and base64-decodes; returns `.Bytes` (DER) plus rest
- `_` discards the rest (we only want the first cert)
- `sha256.Sum256` over DER → 32 bytes
- `hex.EncodeToString` → 64 lowercase hex chars

**Why fingerprint over DER, not PEM?** Different PEM tools format headers/line-breaks differently → different hashes. DER is canonical. Matches `openssl x509 -fingerprint -sha256 -in cert.pem` exactly so operators can verify manually.

**Why intermediate, not workspace CA?** Because `/ca.crt` endpoint serves the intermediate. The connector will download it in Stage 14; the fingerprint must match what the controller serves. Intermediate is also the trust anchor for the controller's gRPC server cert.

> **Findings** (verified 2026-06-15) — Between the Stage 5 INSERT and this fingerprint step, the resolver also runs an unlabeled lookup `SELECT slug FROM workspaces WHERE id = $1` (connector.resolvers.go:98-105) on `r.Pool`. That `workspaceSlug` is the source of the `workspaceSlug` argument that appears in Stage 7's `GenerateEnrollmentToken(...)` — the doc previously used it without saying where it came from. The slug feeds `appmeta.WorkspaceTrustDomain(slug)` → the JWT's `trust_domain` claim. No bug; doc-provenance gap only.

## Stage 7 — Sign the Enrollment JWT

[connector.resolvers.go line 122](controller/graph/resolvers/connector.resolvers.go#L122):
```go
tokenString, jti, err := connector.GenerateEnrollmentToken(
    r.ConnectorCfg,
    connectorID,
    tc.TenantID,
    workspaceSlug,
    caFingerprint,
)
```

Returns the JWT string AND the JTI separately — both embedded but extracted for Stage 8's Redis store.

Inside [token.go line 26](controller/internal/connector/token.go#L26):
```go
jti = uuid.NewString()
now := time.Now()
trustDomain := appmeta.WorkspaceTrustDomain(workspaceSlug)

claims := EnrollmentClaims{
    RegisteredClaims: jwt.RegisteredClaims{
        ID:        jti,
        Issuer:    appmeta.ControllerIssuer,
        ExpiresAt: jwt.NewNumericDate(now.Add(cfg.EnrollmentTokenTTL)),  // 24h default
        IssuedAt:  jwt.NewNumericDate(now),
    },
    ConnectorID:   connectorID,
    WorkspaceID:   workspaceID,
    TrustDomain:   trustDomain,
    CAFingerprint: caFingerprint,
}

token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
tokenString, _ = token.SignedString([]byte(cfg.JWTSecret))
```

What each claim carries:

| Claim | Source | Used by |
|---|---|---|
| `jti` | UUID | Controller burns in Redis (single-use) |
| `iss` | `"ztna-controller"` | Controller verifies during Enroll |
| `exp` | `now + 24h` | Connector ignores; controller validates |
| `iat` | `now` | Audit only |
| `connector_id` | from INSERT | Connector builds SPIFFE URI; controller verifies row |
| `workspace_id` | `tc.TenantID` | Connector saves to state.json; controller verifies row tenant |
| `trust_domain` | derived from slug | Connector builds SPIFFE URI; controller verifies CSR SAN |
| `ca_fingerprint` | from Stage 6 | Connector verifies downloaded CA matches |

Why UUID for JTI? Different from `connectorID` because admin can regenerate tokens (new JTI per mint) without invalidating old ones until they're burned. Also unguessable.

Signed with HS256 + `JWT_SECRET`. Symmetric. Same secret used by user-auth tokens. Never leaves the controller.

## Stage 8 — Store the JTI in Redis

The JWT alone can't enforce single-use. Redis enforces it via atomic GETDEL.

[connector.resolvers.go line 133](controller/graph/resolvers/connector.resolvers.go#L133):
```go
err = connector.StoreEnrollmentJTI(ctx, r.Redis, jti, connectorID, r.ConnectorCfg.EnrollmentTokenTTL)
```

[token.go line 79](controller/internal/connector/token.go#L79):
```go
const enrollmentJTIPrefix = "enrollment:jti:"

func StoreEnrollmentJTI(ctx context.Context, rdb valkeycompat.Cmdable, jti, connectorID string, ttl time.Duration) error {
    return rdb.Set(ctx, enrollmentJTIPrefix+jti, connectorID, ttl).Err()
}
```

Single Redis `SET key value EX ttl`. Key namespace `enrollment:jti:` prevents collision with refresh tokens (`refresh:`), PKCE state (`pkce:`), etc.

**Why store `connector_id` as the value, not just a flag?** Defense in depth. Stage 21's burn checks that `Redis-value == claim-connector_id`. If an attacker forges a JWT with a different `connector_id` but the same JTI, the mismatch is detected.

**TTL matches JWT exp** (24h). Two reasons:
- Storage hygiene — abandoned attempts auto-cleanup
- No bypass via TTL mismatch — both the JWT exp check and Redis check fire at the same time

`valkeycompat.Cmdable` is the `go-redis` v9 compatible interface — depends on the smallest interface (testability + future flexibility).

## Stage 9 — UPDATE Connector Row + Build Install Command

### Part A — UPDATE
[connector.resolvers.go line 138](controller/graph/resolvers/connector.resolvers.go#L138):
```go
err = r.TenantDB.Exec(ctx,
    `UPDATE connectors SET enrollment_token_jti = $1, updated_at = NOW()
      WHERE id = $2 AND tenant_id = $3`,
    jti, connectorID, tc.TenantID,
)
```

Stamps the row from Stage 5 with the JTI from Stage 7. Why? UI can show "token outstanding" by checking this column; audit trail; supports regeneration UX.

### Part B — Resolve controller addresses
[connector.resolvers.go line 147](controller/graph/resolvers/connector.resolvers.go#L147):
```go
controllerAddr := os.Getenv("CONTROLLER_ADDR")
if controllerAddr == "" {
    controllerAddr = "localhost:" + r.ConnectorCfg.GRPCPort
}
controllerHTTPAddr := os.Getenv("CONTROLLER_HTTP_ADDR")
if controllerHTTPAddr == "" {
    if colon := strings.LastIndex(controllerAddr, ":"); colon != -1 {
        controllerHTTPAddr = controllerAddr[:colon] + ":8080"
    }
}
```

Two addresses baked into the install command:
- `CONTROLLER_ADDR` — gRPC endpoint (`:9090`) — used by `Enroll` RPC, Control stream
- `CONTROLLER_HTTP_ADDR` — HTTP endpoint (`:8080`) — used for `/ca.crt`, `/ca.crl`

Env vars take precedence in production. Falls back to deriving from gRPC host + `:8080` for dev.

### Part C — Assemble the install command
[connector.resolvers.go line 159](controller/graph/resolvers/connector.resolvers.go#L159):
```go
installCmd := fmt.Sprintf(
    "curl -fsSL https://raw.githubusercontent.com/.../connector-install.sh | \\\n"+
        "  sudo CONTROLLER_ADDR=%s \\\n"+
        "  CONTROLLER_HTTP_ADDR=%s \\\n"+
        "  ENROLLMENT_TOKEN=%s \\\n"+
        "  bash",
    controllerAddr, controllerHTTPAddr, tokenString,
)
```

Rendered:
```bash
curl -fsSL https://raw.githubusercontent.com/vairabarath/zecurity/main/connector/scripts/connector-install.sh | \
  sudo CONTROLLER_ADDR=controller.example.com:9090 \
  CONTROLLER_HTTP_ADDR=controller.example.com:8080 \
  ENROLLMENT_TOKEN=eyJhbGci... \
  bash
```

Token interpolated **literally** into the command. Single-use + 24h TTL mitigates leak risk.

### Part D — Return
```go
return &graph.ConnectorToken{
    ConnectorID:    connectorID,
    InstallCommand: installCmd,
}, nil
```

Apollo resolves the promise → `refetchQueries` runs → modal closes → navigates to `/connectors/<id>`.

## Stage 10 — Frontend Shows Install Command

`ConnectorDetail` page renders the connector record + an `installCommand` in a copyable code block. Connector still has `status='pending'` so the page shows a "Connector registered, not installed" banner ([ConnectorDetail.tsx:314-328](admin/src/pages/ConnectorDetail.tsx#L314)).

Admin copies + SSHs into target server + pastes.

> **Findings** (verified 2026-06-15) — The install command the admin actually copies does **NOT** come from the Stage 2–9 GraphQL mutation. Three things the original write-up missed:
>
> 1. **The mutation's `installCommand` is discarded.** `InstallCommandModal.handleSubmit` reads only `result.data?.generateConnectorToken.connectorId` and navigates — it never touches the returned `installCommand` ([InstallCommandModal.tsx:108-112](admin/src/components/InstallCommandModal.tsx#L108)). The detail page mints a *fresh* one: on mount, if `status==='pending'`, `useEffect` calls `fetchInstallCommand()`, a REST **`POST /api/connectors/{id}/token`** that reads `result.install_command` ([ConnectorDetail.tsx:152-182](admin/src/pages/ConnectorDetail.tsx#L152)). Handler is [`RegenerateTokenHandler`](controller/internal/connector/token_handler.go) — it re-runs the *entire* Stage 4–9 logic (status='pending' recheck → slug → CA fingerprint → `GenerateEnrollmentToken` → `StoreEnrollmentJTI` → `UPDATE enrollment_token_jti`).
>
> 2. **Two tokens are minted in the create-via-modal path.** Token #1 by the GraphQL mutation (Stages 2–9); token #2 by the REST endpoint when the detail page loads. The `UPDATE` at [token_handler.go:95-99](controller/internal/connector/token_handler.go#L95) overwrites `enrollment_token_jti` with #2, so the connector row points at #2; #1's JTI stays in Redis (orphaned, harmless — burns out at its 24h TTL). Only #2 is ever displayed. Not a bug, but the first token is wasted work and the `installCommand` GraphQL field is effectively dead for this UI.
>
> **✅ RESOLVED (2026-06-19, ADR-008 S1+S2).** Minting is now **lazy**: `GenerateConnectorToken`/`GenerateShieldToken` only validate + reserve the `pending` row and return the ID — no JWT, no Redis write, no `enrollment_token_jti` at create. `installCommand` was removed from both mutations + the `ConnectorToken`/`ShieldToken` GraphQL types. **All** token issuance now happens once, on demand, via the REST endpoints (`POST /api/{connectors,shields}/{id}/token`) when the detail page loads. Net: one token per agent, never in the Apollo cache. (Findings #1 and #2's "mint at create" description above is now historical — that logic lives only in the REST handler / shield service.)
>
> 3. **~~Role asymmetry~~ RETRACTED (2026-06-16).** The original finding claimed `GenerateConnectorToken` (GraphQL) had no role gate while the REST regenerate route required admin. This was wrong. The GraphQL mutation **does** carry `@hasRole(roles: [ADMIN])` ([connector.graphqls:30](controller/graph/connector.graphqls#L30)), enforced by the `HasRole` directive ([directives.go:23](controller/graph/resolvers/directives.go#L23)). Both paths are admin-gated. The error was checking the resolver for an inline `RequireRole` call and concluding "ungated" — but the project's RBAC convention is directive-based, not inline. The directive IS the gate.
>
> Minor: the REST builder emits a **single-line** install command via `netutil.DetectLANAddr` + `IsLocalhost` ([token_handler.go:105-117](controller/internal/connector/token_handler.go#L105)); the GraphQL builder (shown in Stage 9 Part C) emits the **multi-line** `\`-continued form with plain env/localhost fallback. The doc's Stage 9 rendering is the form the user *doesn't* see.

---

# Half B — Server Side

## Stage 11 — Admin Runs the Install Command

Curl pipes [connector-install.sh](connector/scripts/connector-install.sh) into bash with env vars set.

The script:
1. Create `zecurity` system user (no login, no shell) — connector runs as this user, NOT as root
2. Download connector binary from GitHub release → `/usr/local/bin/zecurity-connector`
3. Write `/etc/zecurity/connector.conf` mode **0660** `root:zecurity` with env vars persisted as KEY=VALUE — group-writable so the connector (running as `zecurity`) can rewrite it after enrollment to strip `ENROLLMENT_TOKEN` (Stage 29)
4. Create `/var/lib/zecurity-connector/` state directory, chown to `zecurity` user
5. Install systemd units — `zecurity-connector.service` + `zecurity-connector-update.timer`
6. `systemctl daemon-reload && systemctl enable --now zecurity-connector`

Systemd launches the binary. Install done.

**Deployment hardening details** (additive to the 6-step summary above):
- **Binary integrity** — the script downloads the arch-matched release (`connector-linux-amd64` / `connector-linux-arm64`) and **verifies its SHA-256 against `checksums.txt`** before installing; a mismatch aborts.
- **CA pre-seed** — it also fetches `/ca.crt` to `/etc/zecurity/ca.crt` during install (the enrollment flow re-fetches and pins it in Stage 14–15 regardless).
- **systemd sandboxing** — [zecurity-connector.service](connector/systemd/zecurity-connector.service) runs `ProtectSystem=strict`, `NoNewPrivileges`, `SystemCallFilter=@system-service`, grants only `CAP_NET_ADMIN` / `CAP_NET_RAW`, and limits writes to `ReadWritePaths=/var/lib/zecurity-connector /etc/zecurity`.
- **Docker alternative** — [docker-compose.yml](connector/docker-compose.yml) runs the published image with `network_mode: host` + `NET_ADMIN` / `NET_RAW`, persisting `/var/lib/zecurity-connector`; same env vars, with `ENROLLMENT_TOKEN` dropped after first run.

## Stage 12 — Connector Boots, Enters Enrollment Flow

Systemd's `EnvironmentFile=/etc/zecurity/connector.conf` directive parses the file and exports each line as an env var. `User=zecurity` means non-root.

[main.rs line 81](connector/src/main.rs#L81):
```rust
rustls::crypto::ring::default_provider().install_default()
    .expect("failed to install default crypto provider");
```

Process-global crypto provider for rustls. Must happen before any TLS code.

[main.rs line 85](connector/src/main.rs#L85): `--check-update` path skipped on first boot.

[main.rs line 96](connector/src/main.rs#L96):
```rust
let cfg = ConnectorConfig::load()?;
let env_filter = tracing_subscriber::EnvFilter::try_new(&cfg.log_level)
    .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));
tracing_subscriber::fmt().with_env_filter(env_filter).init();
```

[figment](https://docs.rs/figment) reads env vars into `ConnectorConfig`. Tracing subscriber publishes to stdout; systemd captures to `journalctl`.

[main.rs line 110](connector/src/main.rs#L110):
```rust
let state_path = Path::new(&cfg.state_dir).join("state.json");

let enrollment_state: EnrollmentState = if state_path.exists() {
    // returning connector — load saved state
    let state_json = fs::read_to_string(&state_path)?;
    serde_json::from_str(&state_json)?
} else {
    info!("no state found — starting enrollment");
    let result = enrollment::enroll(&cfg).await?;
    let state_json = fs::read_to_string(&state_path)?;
    serde_json::from_str(&state_json)?
};
```

The presence/absence of `state.json` is the entire "have I enrolled?" marker. First boot → file missing → call `enrollment::enroll(...)`. Re-reads the file it just wrote (5-field `EnrollmentState`, not 2-field `EnrollmentResult`).

## Stage 13 — Parse JWT Payload (No Signature Verify)

[enrollment.rs line 76](connector/src/enrollment.rs#L76):
```rust
pub async fn enroll(cfg: &ConnectorConfig) -> Result<EnrollmentResult> {
    let token = cfg.enrollment_token.as_deref()
        .context("ENROLLMENT_TOKEN is required for first-run enrollment")?;

    let claims = parse_jwt_payload(token)?;
    info!(connector_id = %claims.connector_id, workspace_id = %claims.workspace_id, "parsed enrollment token");
    ...
}
```

[parse_jwt_payload line 288](connector/src/enrollment.rs#L288):
```rust
fn parse_jwt_payload(token: &str) -> Result<JwtClaims> {
    let parts: Vec<&str> = token.split('.').collect();
    if parts.len() != 3 { bail!("invalid JWT format"); }

    let payload_bytes = URL_SAFE_NO_PAD.decode(parts[1])?;
    let claims: JwtClaims = serde_json::from_slice(&payload_bytes)?;
    Ok(claims)
}
```

JWT = `<header>.<payload>.<signature>`. Split on `.` → three segments. Base64URL-decode middle → JSON. Deserialize.

**No HMAC verification.** The connector doesn't have `JWT_SECRET`. By design — the secret never leaves the controller. So the connector parses optimistically.

Why is this safe? Stage 15's fingerprint check retroactively authenticates the entire token. If `ca_fingerprint` matches the CA bytes we'll fetch, the token must have been minted by the real controller (only the real controller knew that fingerprint to embed).

```rust
#[derive(Debug, Deserialize)]
struct JwtClaims {
    #[serde(rename = "jti")]
    _jti: String,           // deserialized but unused
    connector_id: String,
    workspace_id: String,
    trust_domain: String,
    ca_fingerprint: String,
}
```

`_jti` prefix marks it intentionally unused (compiler suppresses warning). Other JWT claims (`iss`, `iat`, `exp`) are dropped silently by serde — connector doesn't need them.

## Stage 14 — Fetch /ca.crt Over Plain HTTP

[enrollment.rs line 91](connector/src/enrollment.rs#L91):
```rust
let http_addr = cfg.controller_http_addr.clone()
    .unwrap_or_else(|| derive_http_addr(&cfg.controller_addr));

let ca_pem = fetch_ca_cert(&http_addr).await?;
```

[fetch_ca_cert line 308](connector/src/enrollment.rs#L308):
```rust
async fn fetch_ca_cert(http_addr: &str) -> Result<String> {
    let client = Client::builder().build()?;
    let url = format!("http://{}/ca.crt", http_addr);
    let resp = client.get(&url).send().await?;
    if !resp.status().is_success() { bail!("HTTP {}", resp.status()); }
    let pem = resp.text().await?;
    if !pem.contains("-----BEGIN CERTIFICATE-----") { bail!("not a PEM"); }
    Ok(pem)
}
```

**Plain HTTP, not HTTPS.** The connector has no CA to validate TLS yet — we're bootstrapping that.

Controller-side handler [ca_endpoint.go line 13](controller/internal/connector/ca_endpoint.go#L13):
```go
SELECT certificate_pem FROM ca_intermediate LIMIT 1
```

Serves the intermediate CA as PEM, `Content-Type: application/x-pem-file`. No auth — public bootstrap endpoint.

**Vulnerability surface here**: a MITM could swap the body. Stage 15 closes this.

## Stage 15 — Verify CA Fingerprint (MITM Defense)

[verify_ca_fingerprint line 347](connector/src/enrollment.rs#L347):
```rust
fn verify_ca_fingerprint(ca_pem: &str, expected_fingerprint: &str) -> Result<()> {
    let certs = certs(&mut ca_pem.as_bytes())
        .collect::<Result<Vec<_>, _>>()?;
    if certs.is_empty() { bail!("no certificates in PEM"); }

    let der_bytes = &certs[0];
    let hash = Sha256::digest(der_bytes);
    let fingerprint = hex::encode(hash);

    if fingerprint != expected_fingerprint {
        warn!("CA FINGERPRINT MISMATCH — possible MITM attack, aborting!");
        bail!("CA fingerprint mismatch! Expected {}, got {}", expected_fingerprint, fingerprint);
    }
    Ok(())
}
```

Mirror image of Stage 6:
1. PEM → DER via `rustls_pemfile::certs` (strips headers + base64-decodes)
2. First cert only — same as controller's `pem.Decode` which returns the first block
3. SHA-256 over DER (canonical form, matches `openssl x509 -fingerprint`)
4. Hex-encode lowercase
5. String compare against `claims.ca_fingerprint`

**Why this defense holds**: an attacker would need to (1) intercept HTTP — easy, (2) substitute different CA — easy, (3) forge a JWT with matching fingerprint — **HARD**, requires `JWT_SECRET` which never leaves the controller.

String comparison is NOT constant-time. That's fine — the "secret" here is the fingerprint of a PUBLIC certificate. Nothing to time-leak.

After this passes, the connector **trusts the downloaded CA**. Everything from here builds on this.

## Stage 16 — Generate EC P-384 Keypair, Save Mode 0600

[enrollment.rs line 104](connector/src/enrollment.rs#L104):
```rust
let key_pair = crypto::generate_keypair()?;
let key_path = Path::new(&cfg.state_dir).join("connector.key");
crypto::save_private_key(&key_pair, &key_path)?;
```

[crypto.rs line 28](connector/src/crypto.rs#L28):
```rust
pub fn generate_keypair() -> Result<KeyPair> {
    KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384)
}
```

**EC P-384** — same curve as the workspace CA. Reasons over alternatives:
- Faster than RSA at equivalent security
- More headroom than P-256 (192-bit security level vs 128-bit)
- NSA Suite B / CNSA-1.0 compatible — enterprise friendly
- TLS-compatible across all major libraries

Behind the scenes `rcgen` calls `ring`'s ECDSA. Reads from OS CSPRNG (Linux `getrandom`). Generates random 384-bit scalar `d` (private), computes public point `Q = d × G`.

[save_private_key line 35](connector/src/crypto.rs#L35):
```rust
OpenOptions::new()
    .write(true).create(true).truncate(true)
    .mode(0o600)   // owner read/write only
    .open(path)?
    .write_all(pem.as_bytes())?;
```

**`.mode(0o600)` applied atomically via `open(2)`.** Without it, there's a race window where the file exists with default umask perms (0644 — world-readable). With atomic mode, the file NEVER exists with anything but 0600.

Resulting file: `-rw------- zecurity zecurity ... connector.key`.

The **private key never leaves this machine**. Only the public key (inside the CSR) goes over the network.

## Stage 17 — Build CSR with CN + SPIFFE SAN URI

[enrollment.rs line 110](connector/src/enrollment.rs#L110):
```rust
let cn = format!("{}{}", appmeta::PKI_CONNECTOR_CN_PREFIX, claims.connector_id);
let spiffe_uri = appmeta::connector_spiffe_id(&claims.trust_domain, &claims.connector_id);
let csr_der = crypto::build_csr(&key_pair, &cn, &spiffe_uri)?;
```

**CN**: `connector-01HX27M5K2...` — informational, used for log lines

**SPIFFE URI**: `spiffe://acme.zecurity.in/agent/01HX27M5K2...` — the cryptographic identity

Why SPIFFE instead of hostname-based identity? Workloads have no fixed hostname (rotating IPs in cloud envs, no DNS), but they need stable identities. SPIFFE solves this: URI-shaped identity, trust-domain scoped, path identifies the specific workload.

`build_csr` uses `rcgen` to construct a PKCS#10 CertificateRequest:
- Set Distinguished Name (CN only)
- Set Subject Alternative Names: single URI SAN
- `is_ca = NoCa` — defensive; this is a leaf, can't sign other certs
- Sign with the private key (proof-of-possession)
- Serialize to DER (binary, no PEM headers, ~30% smaller)

The CSR's contents:
```
CertificationRequest {
    info { version, subject, subjectPKInfo, attributes (SAN) },
    signatureAlgorithm,
    signature  ← over `info` using private key
}
```

The signature is **proof-of-possession** — proves whoever generated the CSR holds the matching private key.

What the CSR does NOT carry:
- Validity dates (CA decides)
- Serial number (CA generates)
- KeyUsage / ExtKeyUsage (CA decides)
- Issuer (CA adds when signing)

The CSR is a **proposal**. The CA reviews and decides what to grant.

## Stage 18 — Open gRPC TLS Channel Rooted in Verified CA

[enrollment.rs line 119](connector/src/enrollment.rs#L119):
```rust
let grpc_host = controller_host(&cfg.controller_addr);
let grpc_addr = format!("https://{}", cfg.controller_addr);
let channel = Endpoint::from_shared(grpc_addr.clone())?
    .tls_config(
        ClientTlsConfig::new()
            .ca_certificate(Certificate::from_pem(ca_pem.as_bytes()))
            .domain_name(grpc_host.clone()),
    )?
    .connect().await?;
let mut client = proto::connector_service_client::ConnectorServiceClient::new(channel);
```

`controller_host` strips the port: `controller.example.com:9090` → `controller.example.com`. TLS validates hostname; TCP connects to host:port.

**`.ca_certificate(...)` is the critical line**: trust ONLY this CA. System root store is NOT used. Any other CA — including legitimate public ones — rejected. This is intentional: our intermediate CA is private; letting public CAs through would actually weaken security.

**`.domain_name(...)`** — SNI extension + hostname verification. The controller's cert must have `controller.example.com` in its SAN.

`.connect().await` does:
1. DNS resolution via system resolver
2. TCP three-way handshake
3. **TLS handshake** — server cert chain → walk to our trusted CA → verify SANs → verify signatures → verify validity

Not mTLS — server-authenticated only. Connector doesn't have a cert yet. Application-layer auth via JWT in `EnrollRequest`.

## Stage 19 — Call Enroll RPC

[enrollment.rs line 137](connector/src/enrollment.rs#L137):
```rust
let hostname = util::read_hostname();
let request = tonic::Request::new(proto::EnrollRequest {
    enrollment_token: token.to_string(),
    csr_der,
    version: env!("CARGO_PKG_VERSION").to_string(),
    hostname,
});

let response = client.enroll(request).await?.into_inner();
info!("enrollment successful");
```

Four fields:
- **`enrollment_token`** — full JWT string from config
- **`csr_der`** — DER bytes from Stage 17, moved (consumed)
- **`version`** — `env!("CARGO_PKG_VERSION")` resolved at COMPILE TIME from Cargo.toml
- **`hostname`** — `gethostname(2)` system call

`tonic::Request::new(...)` wraps in metadata-bearing envelope. We don't add custom metadata.

The request hits `/connector.v1.ConnectorService/Enroll` over the TLS channel. Protobuf-encoded, HTTP/2 framed.

`.await` parks the task. While parked, the controller runs Stages 20-27 — total ~30-100ms typical.

`.into_inner()` extracts `EnrollResponse` from the metadata envelope.

Error wrapping: `.context("Enroll RPC call failed")?` builds the error chain so `journalctl` shows:
```
Enroll RPC call failed: status: PermissionDenied, message: "token expired or already used"
```

---

# Controller-Side Enrollment Handler

## Stage 20 — Verify the JWT

Request lands at `/connector.v1.ConnectorService/Enroll`. The SPIFFE interceptor SKIPS this method — the connector has no cert yet ([spiffe.go bypass logic](controller/internal/connector/spiffe.go)). Bootstrap exception.

[enrollment.go line 65](controller/internal/connector/enrollment.go#L65):
```go
claims, err := VerifyEnrollmentToken(h.Cfg, req.EnrollmentToken)
if err != nil {
    return nil, status.Errorf(codes.Unauthenticated, "invalid enrollment token: %v", err)
}

jti := claims.ID
connectorID := claims.ConnectorID
workspaceID := claims.WorkspaceID
trustDomain := claims.TrustDomain

if jti == "" || connectorID == "" || workspaceID == "" || trustDomain == "" {
    return nil, status.Error(codes.InvalidArgument, "enrollment token missing required claims")
}
```

[VerifyEnrollmentToken line 58](controller/internal/connector/token.go#L58):
```go
jwt.ParseWithClaims(tokenString, &EnrollmentClaims{}, func(t *jwt.Token) (interface{}, error) {
    if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
        return nil, fmt.Errorf("unexpected signing method")
    }
    return []byte(cfg.JWTSecret), nil
}, jwt.WithIssuer(appmeta.ControllerIssuer), jwt.WithExpirationRequired())
```

Five checks bundled:

| Check | Defends Against |
|---|---|
| Structural parse | Garbage tokens |
| HMAC algo enforced | `alg=none`, RS256-confusion attacks |
| HMAC signature with `JWT_SECRET` | Forgery — only this controller minted |
| `iss == "ztna-controller"` | Tokens from other internal services |
| `exp` required + in future | Replay of old tokens |

The `*jwt.SigningMethodHMAC` type-assertion defense is critical. Some JWT libraries default-accept `alg: "none"`. By requiring HMAC explicitly, we close that path.

After verification succeeds, claims are trusted because only this controller has `JWT_SECRET`.

Final blank-check defends against malformed-but-valid tokens (e.g., from a buggy future internal tool).

## Stage 21 — Atomic BurnEnrollmentJTI

[enrollment.go line 82](controller/internal/connector/enrollment.go#L82):
```go
burnedConnectorID, found, err := BurnEnrollmentJTI(ctx, h.Redis, jti)
if err != nil { return Internal }
if !found { return PermissionDenied("token expired or already used") }
if burnedConnectorID != connectorID { return PermissionDenied("token connector mismatch") }
```

[BurnEnrollmentJTI line 84](controller/internal/connector/token.go#L84):
```go
val, err := rdb.GetDel(ctx, enrollmentJTIPrefix+jti).Result()
if err == valkeycompat.Nil { return "", false, nil }
if err != nil { return error }
return val, true, nil
```

**`GETDEL` is atomic** — single Redis command that reads AND deletes in one operation. No race window. Two concurrent enrollments with the same token: only one gets the value; the other gets nil.

Without atomicity:
```
GET key    // both succeed
DEL key    // both no-op the second
→ both proceed thinking they're first
```

Single-use enforced cryptographically.

Why this requires Redis 6.2+ / Valkey 7.0+: GETDEL was added in 6.2. Controller refuses to boot on older Redis ([main.go line 386](controller/cmd/server/main.go#L386)).

Three error branches:
- **Internal** — Redis broken, retry might work
- **PermissionDenied "expired or already used"** — same message for both cases (don't leak which); operator action = delete connector + recreate
- **PermissionDenied "connector mismatch"** — should never happen (would require both forged JWT + tampered Redis); defense in depth

**JTI burn is irreversible.** If Stage 25 later fails (e.g., PKI broken), the token is already spent. Admin must mint a new one.

## Stage 22 — Verify Connector Row Is Pending, Tenant Matches

[enrollment.go line 94](controller/internal/connector/enrollment.go#L94):
```go
var connStatus, connTenantID string
err = h.Pool.QueryRow(ctx,
    `SELECT status, tenant_id FROM connectors WHERE id = $1`,
    connectorID,
).Scan(&connStatus, &connTenantID)
if err != nil { return NotFound }
if connStatus != "pending" { return PermissionDenied("status is %q") }
if connTenantID != workspaceID { return PermissionDenied("tenant mismatch") }
```

**No `WHERE tenant_id` filter** — deliberate. We WANT to detect tenant mismatch as a distinct case (rather than masking it as "not found"). Distinct error code → operator sees the security signal.

Status check: only `'pending'` allows enrollment.
- `'active'` — already enrolled, refuse re-enrollment
- `'disconnected'` — still has a cert; use renewal instead
- `'revoked'` — admin explicitly invalidated
- `'deleted'` — soft-deleted

`%q` quotes the value so error reads: `connector status is "active", expected pending`. Diagnostic-friendly.

Tenant mismatch should be impossible given Stage 9's UPDATE used `tc.TenantID`. Belt-and-suspenders for unusual cases (manual DB tampering, JWT forgery somehow passing Stage 20).

## Stage 23 — Verify Workspace Is Active

[enrollment.go line 111](controller/internal/connector/enrollment.go#L111):
```go
var wsStatus string
err = h.Pool.QueryRow(ctx, `SELECT status FROM workspaces WHERE id = $1`, workspaceID).Scan(&wsStatus)
if err != nil { return Internal }
if wsStatus != "active" {
    return nil, status.Errorf(codes.FailedPrecondition, "workspace status is %q", wsStatus)
}
```

Workspace states: `provisioning`, `active`, `suspended`, `deleted`. Only `active` permits enrollment.

**`FailedPrecondition`, not `PermissionDenied`** — semantic distinction:
- `PermissionDenied` — caller isn't authorized
- `FailedPrecondition` — caller IS authorized but system state prevents action

Defense in depth — workspace might have been suspended between mint and enrollment.

## Stage 24 — Parse CSR, Verify Signature, Verify SPIFFE SAN

[enrollment.go line 123](controller/internal/connector/enrollment.go#L123):
```go
csr, err := x509.ParseCertificateRequest(req.CsrDer)
if err != nil { return InvalidArgument("parse CSR: %v", err) }

if err := csr.CheckSignature(); err != nil {
    return InvalidArgument("CSR signature invalid: %v", err)
}

expectedSPIFFE := appmeta.ConnectorSPIFFEID(trustDomain, connectorID)
if !csrHasSPIFFEURI(csr, expectedSPIFFE) {
    return PermissionDenied("SPIFFE ID in CSR does not match token")
}
```

Three checks:

1. **Parse** — ASN.1 PKCS#10 decode. Failures: malformed DER, unsupported key types
2. **`CheckSignature()`** — **proof-of-possession**. The CSR's signature is over `tbsCertRequest` using the connector's private key. Verifies the signature using the public key embedded IN the CSR. Proves the sender HOLDS the private key matching the public key
3. **SPIFFE URI match** — `csrHasSPIFFEURI` scans `csr.URIs` for exact match

```go
func csrHasSPIFFEURI(csr *x509.CertificateRequest, expectedURI string) bool {
    for _, uri := range csr.URIs {
        if uri.String() == expectedURI { return true }
    }
    return false
}
```

Why check the URI if we'll sign anyway? Because the CSR could claim ANY URI. Without this check, a connector could request a cert for `spiffe://.../<some-other-connector-id>` — identity confusion. The token-bound `(trustDomain, connectorID)` is authoritative.

## Stage 25 — Workspace CA Signs the Connector Cert

[enrollment.go line 144](controller/internal/connector/enrollment.go#L144):
```go
certResult, err := h.PKIService.SignConnectorCert(ctx, workspaceID, connectorID, trustDomain, csr, h.Cfg.CertTTL)
```

[pki/workspace.go::SignConnectorCert](controller/internal/pki/workspace.go) does 9 steps:

### 25.1 Load workspace CA material
```sql
SELECT encrypted_private_key, nonce, certificate_pem FROM workspace_ca_keys WHERE tenant_id = $1
```

### 25.2 Decrypt CA private key
```go
caPrivKey, err := decryptPrivateKey(encryptedKey, nonce, s.masterSecret, tenantID)
defer caPrivKey.D.SetInt64(0)   // zero the private scalar after use
```
AES-GCM with master secret + tenant-scoped context.

`defer caPrivKey.D.SetInt64(0)` — **scalar zeroing**. Defense against memory dumps. If `/proc/<pid>/maps` is read later, key isn't sitting in memory.

### 25.3-25.5 Parse CA cert, generate serial, build SPIFFE URL
```go
caCert, _ := parseCertFromPEM(caCertPEM)
serial, _ := newSerialNumber()                          // crypto/rand 16 bytes
spiffeURI, _ := url.Parse(appmeta.ConnectorSPIFFEID(trustDomain, connectorID))
```

### 25.6 Validity window
```go
notBefore := now.Add(-1 * time.Hour)   // clock-skew tolerance
notAfter  := now.Add(certTTL)           // 7 days default
```
Short-lived certs limit blast radius. Renewal via `RenewCert` RPC before expiry.

### 25.7 Cert template
```go
template := &x509.Certificate{
    SerialNumber:          serial,
    Subject:               pkix.Name{CommonName: "connector-<id>", Organization: [workspace]},
    NotBefore:             notBefore,
    NotAfter:              notAfter,
    KeyUsage:              x509.KeyUsageDigitalSignature,
    ExtKeyUsage:           []x509.ExtKeyUsage{ClientAuth, ServerAuth},
    BasicConstraintsValid: true,
    IsCA:                  false,            // ← leaf, cannot sign sub-certs
    URIs:                  []*url.URL{spiffeURI},
}
```

- **`KeyUsage: DigitalSignature`** only — no KeyEncipherment (legacy RSA-KEX, irrelevant for ECDSA)
- **`ExtKeyUsage: ClientAuth + ServerAuth`** — connector acts as both. Client (calling controller), server (Shield gRPC + device tunnels)
- **`IsCA: false`** + `BasicConstraintsValid: true`** — critical defense, prevents accidental sub-cert minting

### 25.8 Sign
```go
certDER, err := x509.CreateCertificate(
    rand.Reader,
    template,
    caCert,
    csr.PublicKey,    // connector's public key from CSR
    caPrivKey,        // workspace CA's decrypted private key
)
```
ECDSA-SHA384 signature (matches the P-384 keys).

### 25.9 Encode + return
```go
certPEM := encodeCertToPEM(certDER)
return &ConnectorCertResult{
    CertificatePEM: certPEM,
    Serial:         serial.Text(16),   // hex string for DB column
    NotBefore:      notBefore,
    NotAfter:       notAfter,
}, nil
```

## Stage 26 — UPDATE Connectors → Active

[enrollment.go line 150](controller/internal/connector/enrollment.go#L150):
```go
_, err = h.Pool.Exec(ctx,
    `UPDATE connectors
        SET status = 'active',
            trust_domain = $1,
            cert_serial = $2,
            cert_not_after = $3,
            hostname = $4,
            version = $5,
            last_heartbeat_at = NOW(),
            enrollment_token_jti = NULL,
            updated_at = NOW()
      WHERE id = $6`,
    trustDomain, certResult.Serial, certResult.NotAfter,
    req.Hostname, req.Version, connectorID,
)
```

Eight columns updated:

| Column | Source | Purpose |
|---|---|---|
| `status` | hardcoded `'active'` | Real connector |
| `trust_domain` | from JWT | SPIFFE interceptor lookups |
| `cert_serial` | from PKI | CRL generation |
| `cert_not_after` | from PKI | Renewal scheduling |
| `hostname` | from `EnrollRequest` | UI display |
| `version` | from `EnrollRequest` | Fleet management |
| `last_heartbeat_at` | `NOW()` | First heartbeat; disconnect watcher starts counting |
| `enrollment_token_jti` | `NULL` | Token consumed |

No `WHERE tenant_id` — Stage 22 already verified tenant.

## Stage 27 — Return EnrollResponse

[enrollment.go line 173](controller/internal/connector/enrollment.go#L173):
```go
workspaceCAPEM, intermediateCAPEM, err := h.loadCACerts(ctx, workspaceID)
if err != nil { return Internal }

return &pb.EnrollResponse{
    CertificatePem:    []byte(certResult.CertificatePEM),
    WorkspaceCaPem:    []byte(workspaceCAPEM),
    IntermediateCaPem: []byte(intermediateCAPEM),
    ConnectorId:       connectorID,
}, nil
```

[loadCACerts line 189](controller/internal/connector/enrollment.go#L189):
```sql
SELECT certificate_pem FROM workspace_ca_keys WHERE tenant_id = $1
SELECT certificate_pem FROM ca_intermediate LIMIT 1
```

Four pieces flow back over gRPC:

| Field | Used for |
|---|---|
| `certificate_pem` | Connector's own cert — mTLS to controller, Shield server, device tunnels |
| `workspace_ca_pem` | Trust anchor for Shield certs (Shields enroll into the connector) |
| `intermediate_ca_pem` | Trust anchor for controller's gRPC cert during operations |
| `connector_id` | Saved in `state.json` |

`bytes` in proto3 maps to `Vec<u8>` in Rust.

---

# Back On the Connector

## Stage 28 — Save Cert, CA Chain, state.json

[enrollment.rs line 152](connector/src/enrollment.rs#L152):
```rust
fs::create_dir_all(state_dir)?;  // mkdir -p

// connector.crt = leaf + workspace CA concatenated
let leaf_cert = String::from_utf8(response.certificate_pem.clone())?;
let workspace_ca_for_chain = String::from_utf8(response.workspace_ca_pem.clone())?;
let full_chain = format!("{}\n{}", leaf_cert, workspace_ca_for_chain);
fs::write(&cert_path, &full_chain)?;
let cert_not_after = parse_cert_not_after(&response.certificate_pem)?;

// workspace_ca.crt = workspace CA + intermediate CA concatenated
let ca_chain = format!("{}\n{}", workspace_ca, intermediate_ca);
fs::write(&ca_chain_path, &ca_chain)?;

// state.json
let enrolled_at = OffsetDateTime::now_utc().format(&Rfc3339)?;
let state = EnrollmentState {
    connector_id: claims.connector_id.clone(),
    trust_domain: claims.trust_domain.clone(),
    workspace_id: claims.workspace_id.clone(),
    enrolled_at,
    cert_not_after,
};
let state_json = serde_json::to_string_pretty(&state)?;
fs::write(&state_path, state_json)?;
```

Three files in `/var/lib/zecurity-connector/`:

| File | Contents | Used by |
|---|---|---|
| `connector.crt` | Leaf cert + workspace CA chain | mTLS to controller (chain self-contained) |
| `workspace_ca.crt` | Workspace CA + intermediate CA | Verify Shield certs + controller gRPC cert |
| `state.json` | `{connector_id, trust_domain, workspace_id, enrolled_at, cert_not_after}` | Boot marker + identity record |

The cert chain matters: the controller's `x509.Verify()` builds `Connector cert ← Workspace CA ← Intermediate CA`. Bundling avoids round-trip to fetch the workspace CA.

## Stage 29 — Best-Effort Clean Up connector.conf

[enrollment.rs line 231](connector/src/enrollment.rs#L231):
```rust
const CONFIG_PATH: &str = "/etc/zecurity/connector.conf";

fn cleanup_config_after_enrollment(connector_id: &str) {
    let content = match fs::read_to_string(path) {
        Ok(c) => c,
        Err(_) => { warn!("could not read config"); return; }
    };

    let mut lines: Vec<String> = content.lines()
        .filter(|line| !line.trim().starts_with("ENROLLMENT_TOKEN="))
        .map(String::from)
        .collect();

    if !lines.iter().any(|l| l.trim().starts_with("CONNECTOR_ID=")) {
        lines.push(format!("CONNECTOR_ID={}", connector_id));
    }

    let new_content = lines.join("\n") + "\n";
    if let Err(_) = fs::write(path, new_content) {
        warn!("could not write config — consider manually removing ENROLLMENT_TOKEN");
    }
}
```

Two transformations:
1. Remove `ENROLLMENT_TOKEN=...` line — JWT is single-use and burned, but raw JWT on disk leaks claims if file ever exposed
2. Add `CONNECTOR_ID=<uuid>` if missing — useful for ops debugging

**Best-effort.** The config file is typically `root:zecurity` mode 0640 — the connector running as `zecurity` may not have write permission. On failure, log warning and continue. Enrollment ALREADY SUCCEEDED at this point.

---

# Stage 30 — Connector Becomes Operational

Returns to `main.rs`. Everything else from here runs the connector's steady state.

## 30.0 — Re-read state.json

`enroll()` returned `EnrollmentResult { connector_id, trust_domain }` — only 2 fields. We need 5-field `EnrollmentState`. Re-reading from disk keeps the load path identical to the "returning connector" branch.

## 30.1 — Build Controller mTLS Channel

[main.rs line 139](connector/src/main.rs#L139):
```rust
let cert_pem = fs::read(state_dir.join("connector.crt"))?;
let key_pem = fs::read(state_dir.join("connector.key"))?;
let ca_pem = fs::read(state_dir.join("workspace_ca.crt"))?;

let tls = ClientTlsConfig::new()
    .identity(Identity::from_pem(&cert_pem, &key_pem))
    .ca_certificate(Certificate::from_pem(&ca_pem));

let controller_channel = Channel::from_shared(grpc_addr)?
    .tls_config(tls)?
    .connect().await?;
```

Compared to Stage 18 (server-only TLS), this is **mutual TLS**.

**`Identity::from_pem(cert, key)`** is the new piece — makes the handshake mutual. The connector sends its cert chain alongside the server-cert verification. The controller's SPIFFE interceptor extracts the SAN URI from the leaf and identifies the connector.

Single TLS connection, reused. HTTP/2 multiplexes all subsequent gRPC calls (Control stream, RenewCert, etc.).

## 30.2 — ShieldRegistry + Spawn :9091 Server

[main.rs line 163](connector/src/main.rs#L163):
```rust
let (ack_tx, ack_rx) = mpsc::channel(128);

let shield_registry = agent_server::ShieldRegistry::new(
    controller_channel,
    enrollment_state.trust_domain.clone(),
    enrollment_state.connector_id.clone(),
    ack_tx,
);

let reg_for_serve = shield_registry.clone();
let shield_state_dir = cfg.state_dir.clone();
let shield_addr: SocketAddr = "0.0.0.0:9091".parse().unwrap();
tokio::spawn(async move {
    if let Err(e) = reg_for_serve.serve(shield_addr, &shield_state_dir).await {
        error!(error = %e, "Shield gRPC server on :9091 failed");
    }
});
```

**The ack channel** (`mpsc::channel(128)`) bridges two tasks:
- Resource acks from Shields land in `ack_tx` (sender)
- Control stream task drains `ack_rx` and forwards to controller

128 capacity = backpressure-buffered.

**`ShieldRegistry`** holds:
- `controller_channel` — to proxy RenewCert back to controller (connector doesn't have workspace CA private key)
- `trust_domain` + `connector_id` — for verifying Shield SPIFFE URIs + identifying ourselves upstream
- `ack_tx` — sender for resource acks
- Plus internal state: connected-Shields map, AgentTunnelHub, workspace CA for mTLS verification

**Why `:9091`, not `:9090`?** `:9090` is controller's. `:9091` is the connector's own Shield-facing gRPC. Shields never talk directly to controller — connector is the relay.

The spawn uses `async move` and takes a clone of `shield_registry`. Cloning is cheap (Arc-internal). Main keeps its own handle for later use.

If `serve` errors (port conflict, etc.), task ends but rest of connector keeps running. Degraded mode.

## 30.3 — Auto-Updater Spawn

[main.rs line 183](connector/src/main.rs#L183):
```rust
if cfg.auto_update_enabled {
    let upd_cfg = cfg.clone();
    tokio::spawn(async move {
        if let Err(e) = updater::run_update_loop(&upd_cfg).await {
            error!(error = %e, "auto-updater failed");
        }
    });
}
```

If enabled, spawns a task that polls GitHub releases. Downloads, verifies checksums, signals systemd to restart.

Separate from `zecurity-connector-update.timer` (which runs `--check-update` periodically). Belt-and-suspenders.

## 30.4 — Cert Store + Empty PolicyCache

[main.rs line 195](connector/src/main.rs#L195):
```rust
let policy_cache = Arc::new(policy::PolicyCache::new());
let cert_store = tls::cert_store::CertStore::load(&cfg.state_dir)?;
```

**`PolicyCache`** holds the ACL snapshot delivered by controller. Empty at this stage; populated on first heartbeat reply. Empty cache = **default-deny** — devices connecting during the gap get rejected. Safer than starting with a stale on-disk snapshot.

`Arc<PolicyCache>` — Control stream writes; listeners read; many handles via `Arc::clone`. Internally uses `Arc<RwLock<...>>` so concurrent reads don't block.

**`CertStore::load`** reads the three PKI files into a struct. Same files Stage 30.1 read for controller channel; reading again is intentional — separate consumers, separate purposes.

Why hold in memory? No I/O on hot path (every TLS handshake); single source of truth; mode-0600 perms verified once at startup.

## 30.5 — LAN IP for QUIC Advertise

[main.rs line 202](connector/src/main.rs#L202):
```rust
let lan_ip = net_util::lan_ip().map(|ip| ip.to_string()).unwrap_or_default();
let quic_advertise = format!("{}:9092", lan_ip);
```

[net_util.rs line 5](connector/src/net_util.rs):
```rust
pub fn lan_ip() -> anyhow::Result<IpAddr> {
    let socket = UdpSocket::bind("0.0.0.0:0")?;
    socket.connect("8.8.8.8:53")?;
    Ok(socket.local_addr()?.ip())
}
```

**The OS-asks-itself trick.** UDP is connectionless, so `socket.connect("8.8.8.8:53")` doesn't actually send packets. It triggers a kernel route lookup, which binds the socket to the local interface IP that would be used to reach 8.8.8.8.

Why? Because device clients need to know where to find the QUIC version. NAT/LB/multi-homed setups mean the address used for TLS isn't always the right address for QUIC (UDP). By advertising the connector's actual interface IP, clients talk directly.

`8.8.8.8:53` is a convention — any reachable external address works. No packet sent. The kernel just figures out which interface would have been used.

The result is stored in a `OnceLock<String>` via `device_tunnel::set_quic_advertise_addr()` and included in every `TunnelResponse` so clients can pre-warm QUIC.

## 30.6 — CRL Manager + Spawn Refresh

[main.rs line 213](connector/src/main.rs#L213):
```rust
let http_base = cfg.controller_http_addr.clone().unwrap_or_else(|| {
    let host = cfg.controller_addr.split(':').next().unwrap_or("localhost").to_string();
    format!("http://{}:8080", host)
});
let crl_url = format!("{}/ca.crl?workspace_id={}", http_base, enrollment_state.workspace_id);

let crl_manager = crl::CrlManager::new();
if let Err(e) = crl_manager.refresh(&crl_url).await {
    tracing::warn!("initial CRL fetch failed (using empty cache): {e}");
}
crl_manager.clone().spawn_refresh(crl_url, 300);   // every 5 min
```

CRL URL includes `?workspace_id=<uuid>` — scoped to this workspace. Connectors in workspace A never see revocations for workspace B.

HTTP (not HTTPS) because the CRL is **self-authenticating** — signed by the workspace CA. Integrity by signature, not by transport.

[CrlManager](connector/src/crl.rs):
```rust
pub struct CrlManager {
    revoked: Arc<RwLock<HashSet<Vec<u8>>>>,
}
```

HashSet of revoked cert serials. O(1) membership check on the hot path.

`refresh()` fetches DER bytes, parses via `x509-parser::parse_x509_crl` (verifies CA signature), extracts serials, atomic-swaps the set.

`spawn_refresh(url, 300)` spawns a 5-min interval loop.

**Failure is non-fatal.** If initial fetch fails, log warning + start with empty cache. Better than refusing to boot. Fail-open trade-off (vs. fail-closed): connectors lost > stale revocation briefly.

## 30.7 — TLS + QUIC Listeners on :9092

[main.rs line 230](connector/src/main.rs#L230):
```rust
let (ctrl_tx, _ctrl_rx) = tokio::sync::mpsc::channel::<ControlMessage>(128);

// TLS/TCP on :9092
{
    let store       = cert_store.clone();
    let acl         = acl.clone();
    let hub         = tunnel_hub.clone();
    let reg         = agent_registry.clone();
    let crl         = crl_manager.clone();
    let cid         = connector_id.clone();
    let tx          = ctrl_tx.clone();
    tokio::spawn(async move {
        if let Err(e) = device_tunnel::listen("0.0.0.0:9092", store, acl, hub, reg, crl, cid, tx).await {
            error!(error = %e, "device tunnel (TLS) on :9092 failed");
        }
    });
}

// QUIC/UDP on :9092
{
    // (identical clones)
    tokio::spawn(async move {
        if let Err(e) = quic_listener::listen("0.0.0.0:9092", &quic_advertise, ...).await { ... }
    });
}
```

**Block scopes** (`{ ... }`) let each spawn shadow names without conflict. After each block, originals are still in scope for the next.

**Same port for both protocols.** TCP and UDP use different kernel demux paths; port 9092 carries both.

The data-plane wiring:
- `acl` — devices authorized against PolicyCache
- `tunnel_hub` — protected resources routed through Shields
- `agent_registry` — Shield lookup for which Shield owns which resource
- `crl` — `is_revoked(serial)` per connection
- `cid` — for access log entries
- `tx` — sends access logs upstream (M4 wiring in progress)

`_ctrl_rx` is dropped — `run_control_stream` doesn't currently accept it. Sprint 9 M4 will close this loop.

[device_tunnel::handle_stream](connector/src/device_tunnel.rs) (after M4 merge):
1. TLS handshake (mTLS — requires device cert signed by workspace CA)
2. Extract cert serial → `crl.is_revoked(serial)` → reject if true
3. Read JSON `TunnelRequest`
4. Extract client SPIFFE ID from peer cert
5. `acl.authorize(destination, port, protocol, spiffe_id)` → deny if None
6. Route decision via `decision.protected` flag:
   - `true` — `AgentTunnelHub::open_relay_session(shield_id)` → traffic through Shield
   - `false` — `TcpStream::connect(resource)` → `copy_bidirectional`
7. Emit access log

QUIC listener (`quic_listener.rs`) uses Quinn library. Each QUIC connection multiplexes many bidirectional streams; each stream gets a `device_tunnel::handle_stream` task. Same handler logic, different transport.

## 30.8 — Notify Systemd READY + Spawn Watchdog

[main.rs line 270](connector/src/main.rs#L270):
```rust
watchdog::notify_ready();
watchdog::spawn_watchdog();
```

[watchdog.rs](connector/src/watchdog.rs):
```rust
pub fn notify_ready() { sd_notify("READY=1\n"); }

fn sd_notify(msg: &str) {
    let Ok(sock_path) = env::var("NOTIFY_SOCKET") else { return };
    let _ = std::os::unix::net::UnixDatagram::unbound()
        .and_then(|s| s.send_to(msg.as_bytes(), &sock_path));
}

pub fn spawn_watchdog() {
    let Some(usec_str) = env::var("WATCHDOG_USEC").ok() else { return };
    let Ok(usec) = usec_str.parse::<u64>() else { return };
    let interval_ms = usec / 2 / 1000;
    tokio::spawn(async move {
        let mut tick = interval(Duration::from_millis(interval_ms));
        loop {
            tick.tick().await;
            sd_notify("WATCHDOG=1\n");
        }
    });
}
```

**`notify_ready()`** — sends `READY=1` to systemd's notify socket. The unit is `Type=notify`, so the `activating → active` transition fires here. Before this, `systemctl start` blocks; `systemctl status` shows `activating`. After, `active (running)`. Anything `After=zecurity-connector.service` can start.

**`spawn_watchdog()`** — if `WatchdogSec` is set in the unit (`WATCHDOG_USEC` env var present), spawns a task that pings every `WatchdogSec/2` seconds. Half-interval = margin against scheduling delays.

If the runtime is starved (deadlock, CPU-bound loop without `await`), the ticker won't fire, systemd's timer expires, the service is killed + restarted. The whole point of watchdog: catch stuck processes.

Both functions are no-ops if env vars missing — same code runs in dev (cargo run) without systemd.

## 30.9 — run_control_stream (Heartbeats + ACL Snapshots, Blocks Forever)

[main.rs line 274](connector/src/main.rs#L274):
```rust
control_stream::run_control_stream(&cfg, &enrollment_state, shield_registry, ack_rx, policy_cache).await
```

Main never returns past this line under normal operation.

[control_stream.rs](connector/src/control_stream.rs) — outer reconnect loop:
```rust
loop {
    match try_open_stream(...).await {
        Ok(client) => run_session(client, ...).await,
        Err(e) => warn!("failed to open: {e}"),
    }
    sleep(5s).await;   // backoff
}
```

Inner session uses `tokio::select!` to multiplex three sources:
```rust
loop {
    tokio::select! {
        // Controller → Connector
        msg = incoming.message() => {
            match msg? {
                Some(HeartbeatAck(ack)) => {
                    if let Some(snapshot) = ack.acl_snapshot {
                        policy_cache.update(snapshot);
                    }
                }
                Some(ResourceInstructions(inst)) => {
                    shield_registry.forward_to_shield(&inst.shield_id, inst).await?;
                }
                ...
            }
        }

        // Heartbeat tick (every 15s — HEALTH_INTERVAL_SECS)
        _ = hb_interval.tick() => {
            tx.send(heartbeat_msg(...)).await?;
        }

        // Shield acks forwarded upstream
        Some((shield_id, ack)) = ack_rx.recv() => {
            tx.send(forward_ack(shield_id, ack)).await?;
        }
    }
}
```

What flows on this stream:

| Direction | Messages |
|---|---|
| Connector → Controller | `Heartbeat` (15s — connector's `HEALTH_INTERVAL_SECS`), `Goodbye` (shutdown), `ResourceAck` (forwarded from Shields), `ConnectorLog` (access events) |
| Controller → Connector | `HeartbeatAck` (with `ACLSnapshot`), `ResourceInstructions`, `DiscoveryRequest` |

**ACL snapshots ride on heartbeat replies.** No separate RPC. Saves bandwidth + complexity. First heartbeat reply populates the empty PolicyCache — devices that were getting default-denied can now authorize.

**On stream drop** (network blip, controller restart), the inner loop ends with an error → outer loop sleeps 5s → re-opens stream. Auto-reconnecting.

**State that persists across reconnects:**
- `policy_cache` — last snapshot stays in memory; devices keep being authorized
- `shield_registry` — Shield connections to `:9091` keep their own streams alive

**State that doesn't persist:**
- The bidirectional stream itself
- Pending acks in `ack_rx` that hadn't been forwarded yet

---

# Files Touched

### Frontend
- [admin/src/pages/AllConnectors.tsx](admin/src/pages/AllConnectors.tsx) — list page, "Add Connector" trigger
- [admin/src/pages/ConnectorDetail.tsx](admin/src/pages/ConnectorDetail.tsx) — shows install command
- [admin/src/components/InstallCommandModal.tsx](admin/src/components/InstallCommandModal.tsx) — shared modal for Connector/Shield
- [admin/src/graphql/mutations.graphql](admin/src/graphql/mutations.graphql) — `GenerateConnectorToken`
- [admin/src/apollo/links/auth.ts](admin/src/apollo/links/auth.ts) — Bearer attachment

### Controller — Graph
- [controller/cmd/server/main.go](controller/cmd/server/main.go) — wires routes, registers gRPC server, registers EnrollmentHandler
- [controller/graph/connector.graphqls](controller/graph/connector.graphqls) — schema
- [controller/graph/resolvers/connector.resolvers.go](controller/graph/resolvers/connector.resolvers.go) — `GenerateConnectorToken`, `RevokeConnector`
- [controller/graph/resolvers/helpers.go](controller/graph/resolvers/helpers.go) — `scanConnector`

### Controller — Internal
- [controller/internal/connector/enrollment.go](controller/internal/connector/enrollment.go) — `EnrollmentHandler.Enroll` + `RenewCert`
- [controller/internal/connector/token.go](controller/internal/connector/token.go) — `GenerateEnrollmentToken`, `VerifyEnrollmentToken`, `StoreEnrollmentJTI`, `BurnEnrollmentJTI`
- [controller/internal/connector/ca_endpoint.go](controller/internal/connector/ca_endpoint.go) — `/ca.crt` + `/ca.crl` HTTP handlers
- [controller/internal/connector/spiffe.go](controller/internal/connector/spiffe.go) — gRPC interceptor; bypasses Enroll
- [controller/internal/pki/workspace.go](controller/internal/pki/workspace.go) — `SignConnectorCert`, `RenewConnectorCert`, `GenerateClientCRL`
- [controller/internal/middleware/auth.go](controller/internal/middleware/auth.go) — JWT verification for GraphQL
- [controller/internal/middleware/workspace.go](controller/internal/middleware/workspace.go) — workspace guard
- [controller/internal/tenant/context.go](controller/internal/tenant/context.go) — tenant context

### Connector (Rust)
- [connector/scripts/connector-install.sh](connector/scripts/connector-install.sh) — install script (curl-pipe-bash target)
- [connector/src/main.rs](connector/src/main.rs) — main + post-enrollment startup
- [connector/src/enrollment.rs](connector/src/enrollment.rs) — full enrollment flow
- [connector/src/crypto.rs](connector/src/crypto.rs) — keypair generation, CSR builder, PEM I/O
- [connector/src/config.rs](connector/src/config.rs) — figment-based env var loader
- [connector/src/appmeta.rs](connector/src/appmeta.rs) — SPIFFE constants, identity builders
- [connector/src/net_util.rs](connector/src/net_util.rs) — `lan_ip()` UDP routing trick
- [connector/src/crl.rs](connector/src/crl.rs) — CRL fetcher + cache
- [connector/src/watchdog.rs](connector/src/watchdog.rs) — sd_notify integration
- [connector/src/control_stream.rs](connector/src/control_stream.rs) — bidirectional gRPC loop
- [connector/src/agent_server.rs](connector/src/agent_server.rs) — `ShieldRegistry`, `:9091` gRPC server
- [connector/src/tls/cert_store.rs](connector/src/tls/cert_store.rs) — cert material container
- [connector/src/tls/server_cfg.rs](connector/src/tls/server_cfg.rs) — device tunnel TLS config builder
- [connector/src/device_tunnel.rs](connector/src/device_tunnel.rs) — TLS device tunnel listener + handler
- [connector/src/quic_listener.rs](connector/src/quic_listener.rs) — QUIC device tunnel listener
- [connector/src/agent_tunnel.rs](connector/src/agent_tunnel.rs) — `AgentTunnelHub` for protected-path relays

### Database
- `connectors` table — `(id, tenant_id, remote_network_id, name, status, trust_domain, cert_serial, cert_not_after, hostname, version, last_heartbeat_at, enrollment_token_jti, created_at, updated_at)`
- `remote_networks` table — assigned via FK
- `workspaces` table — tenant
- `workspace_ca_keys` table — encrypted private key + CA cert PEM
- `ca_intermediate` table — global intermediate CA

### Redis
- `enrollment:jti:<jti>` — `connector_id`, TTL=24h, single-use via GETDEL

### Files on connector host
- `/etc/zecurity/connector.conf` — env vars (CONTROLLER_ADDR, CONTROLLER_HTTP_ADDR, ENROLLMENT_TOKEN until enrollment, CONNECTOR_ID after)
- `/var/lib/zecurity-connector/connector.key` — private key, mode 0600
- `/var/lib/zecurity-connector/connector.crt` — leaf cert + workspace CA chain
- `/var/lib/zecurity-connector/workspace_ca.crt` — workspace CA + intermediate
- `/var/lib/zecurity-connector/state.json` — identity record
- `/usr/local/bin/zecurity-connector` — the binary
- `/etc/systemd/system/zecurity-connector.service` — systemd unit

---

# Key Invariants

| Invariant | Where enforced |
|---|---|
| Connector belongs to caller's tenant | `tc.TenantID` from JWT, never from request body |
| Network must be active before connector created | Stage 4 status check |
| Enrollment JWT is single-use | Redis GETDEL in Stage 21 (atomic) |
| Enrollment JWT expires in 24h | `cfg.EnrollmentTokenTTL` + JWT `exp` |
| Connector can only enroll once | Stage 22 `status='pending'` check; UPDATE flips to `'active'` |
| CSR identity matches token claim | Stage 24 SPIFFE URI check (`csrHasSPIFFEURI`) |
| Private key never leaves the connector | Generated in Stage 16, saved 0600, only public key sent in CSR |
| MITM-resistant CA download | Stage 15 SHA-256 fingerprint check |
| HMAC algo enforced on JWT verify | `*jwt.SigningMethodHMAC` type assertion in `keyfunc` |
| Workspace CA private key zeroed after use | `defer caPrivKey.D.SetInt64(0)` in Stage 25 |
| Connector cert cannot sign sub-certs | `IsCA: false` + `BasicConstraintsValid: true` in Stage 25 template |
| Clock-skew tolerance | `notBefore = now - 1h` |
| Default-deny if no policy yet | Stage 30.4 starts with empty PolicyCache |
| Fail-open on initial CRL fetch | Stage 30.6 warning instead of abort |
| Stuck connectors are killed | Watchdog ping every `WatchdogSec/2` |
| State persistence is one JSON file | `state.json` existence = "have I enrolled?" |

---

# Quick-Reference Call Chain

```
Admin browser
  → AllConnectors.tsx → InstallCommandModal → handleSubmit
  → GenerateConnectorToken mutation
  → main.go routeGraphQL → protected chain
  → AuthMiddleware → WorkspaceGuard → gqlgen
  → connector.resolvers.go: GenerateConnectorToken
      → verify network active
      → INSERT connectors (status=pending) RETURNING id
      → SELECT ca_intermediate.certificate_pem
      → SHA-256 of DER → caFingerprint
      → token.go: GenerateEnrollmentToken (HS256 JWT, 24h)
      → token.go: StoreEnrollmentJTI (Redis SET, 24h TTL)
      → UPDATE connectors SET enrollment_token_jti
      → build curl install command
      → return { connectorId, installCommand }
  → modal closes, navigate /connectors/<id>
  → ConnectorDetail renders, admin copies install command

Server (after admin runs install on remote host)
  → curl-pipe-bash → connector-install.sh
      → install zecurity user + binary + systemd unit
      → systemctl enable --now
  → systemd starts zecurity-connector binary

main.rs
  → rustls::install_default()
  → ConnectorConfig::load() (figment from env)
  → check state_path.exists() → false
  → enrollment::enroll(&cfg).await:
      → parse_jwt_payload (no signature verify)
      → fetch_ca_cert (plain HTTP GET /ca.crt)
      → verify_ca_fingerprint (SHA-256 match) ← MITM defense
      → crypto::generate_keypair (EC P-384)
      → crypto::save_private_key (mode 0600 atomic)
      → crypto::build_csr (CN + SPIFFE SAN, signed)
      → Endpoint::from_shared + tls_config(ca_cert) → connect (server-only TLS)
      → client.enroll(EnrollRequest { token, csr_der, version, hostname })
          ─── controller-side ───
          enrollment.go: EnrollmentHandler.Enroll
              → VerifyEnrollmentToken (HS256 + iss + exp)
              → BurnEnrollmentJTI (Redis GETDEL atomic)
              → SELECT connectors status='pending' + tenant match
              → SELECT workspaces status='active'
              → x509.ParseCertificateRequest + CheckSignature
              → csrHasSPIFFEURI match
              → PKIService.SignConnectorCert
                  → SELECT workspace_ca_keys
                  → decryptPrivateKey
                  → x509.CreateCertificate (sign with workspace CA)
              → UPDATE connectors status='active' + cert info + hostname
              → loadCACerts (workspace + intermediate)
              → return EnrollResponse
          ─── back on connector ───
      → fs::write connector.crt (leaf+workspace CA chain)
      → fs::write workspace_ca.crt (workspace+intermediate)
      → fs::write state.json
      → cleanup_config_after_enrollment (best-effort)
      → return EnrollmentResult

main.rs (Stage 30):
  → re-read state.json for full EnrollmentState
  → build controller mTLS channel (Identity::from_pem + ca_certificate)
  → mpsc::channel for Shield acks
  → ShieldRegistry::new + tokio::spawn serve(:9091)
  → if auto_update: tokio::spawn updater::run_update_loop
  → Arc::new(PolicyCache::new) (empty, default-deny)
  → CertStore::load
  → net_util::lan_ip() + format quic_advertise
  → CrlManager::new + refresh + spawn_refresh(300s)
  → tokio::spawn device_tunnel::listen(:9092 TLS)
  → tokio::spawn quic_listener::listen(:9092 QUIC)
  → watchdog::notify_ready (READY=1 to systemd)
  → watchdog::spawn_watchdog (WATCHDOG=1 every WatchdogSec/2)
  → control_stream::run_control_stream:
      outer reconnect loop:
        open bidirectional stream to /ConnectorService/Control
        inner session loop (tokio::select!):
          - send Heartbeat every 15s (HEALTH_INTERVAL_SECS; controller's 30s CONNECTOR_HEARTBEAT_INTERVAL is the disconnect-watcher tick, not the send rate)
          - receive HeartbeatAck (with ACLSnapshot) → policy_cache.update
          - receive ResourceInstructions → forward to Shield
          - drain ack_rx → forward Shield acks upstream
        on stream drop: backoff 5s, retry
```

---

# Next Flows to Study

- **Shield enrollment** — admin generates Shield token from a connector's detail page; Shield runs install; enrolls into THE CONNECTOR (not controller); cert chain is `Shield ← Workspace CA ← Intermediate`
- **Policy creation + ACL push** — admin creates Group + Resource Access Rule; controller compiles to ACLSnapshot; pushed via heartbeat reply
- **Resource creation** — admin creates a Resource; if `shield_id` set, instructions piggyback on heartbeat and Shield applies nftables
- **Network discovery** — connector-side TCP scan from a remote network; results stored in `discovery_results`; promote to resource
- **Cert renewal** — connector's `renewal.rs` triggers when `cert_not_after - now < 48h`; calls `RenewCert` RPC (existing mTLS, no enrollment loop)
- **Client device enrollment + RDE tunnel** — `zecurity setup` from `/client-install`; device cert signed by workspace CA; `zecurity up` → TUN device → traffic through connector to resource
