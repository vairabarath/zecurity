---
type: code-study
flow: invite-user
created: 2026-05-05
---

# Code Study 02 — Invite Flow End-to-End

> Trace the full invite flow: from an admin clicking "Invite" in TeamUsers, through SMTP, to the invited user landing on `/client-install` as a fully active workspace member.

---

## High-Level Flow

```
[ADMIN]                          [BACKEND]                         [INVITED USER]
admin → TeamUsers.tsx                │
clicks "Invite", types email         │
  ─CreateInvitation mutation────────▶│
                                     │ resolver: role check (admin only)
                                     │ store: INSERT invitations + workspace_members
                                     │ goroutine: emailer sends link
  ◀──────── mutation ack ────────────│
                                     │                                  email arrives
                                     │                                  user clicks /invite/<token>
                                     │◀──GET /api/invitations/<token>───│ (public)
                                     │ ─{ email, workspace_name }──────▶│
                                     │                                  user clicks "Sign in with Google"
                                     │◀──initiateAuth mutation──────────│
                                     │ ──{ redirectUrl }───────────────▶│
                                     │                                  → Google OAuth → /auth/callback
                                     │ CallbackHandler:
                                     │   Bootstrap → Branch B (invited)
                                     │   runInvitedUserTransaction
                                     │   JWT(tenant_id=inviter's workspace)
                                     │ ─302 /auth/callback#token=──────▶│
                                     │                                  AuthCallback reads hash, runs me
                                     │◀──POST /api/invitations/<token>/accept─│
                                     │ store: status='accepted', joined_at=NOW
                                     │ ────────200 OK──────────────────▶│
                                     │                                  navigate('/client-install')
```

---

# Stage 1 — Admin Opens TeamUsers, Clicks Invite

[admin/src/pages/TeamUsers.tsx](admin/src/pages/TeamUsers.tsx). URL is `/users` — admin-only route gated by [`AdminLayout`](admin/src/App.tsx#L43).

The page mounts with two parallel `useQuery` calls (`GetUsersDocument`, `GetGroupsDocument`) populating the table. Local state `showInvite = false` controls modal visibility.

Clicking the "Invite" button:
```tsx
<Button onClick={() => setShowInvite(true)}>Invite</Button>
```

[InviteDialog](admin/src/pages/TeamUsers.tsx#L56) opens. It holds local state for the email input and a `sent` flag, plus `useMutation(CreateInvitationDocument)`. Admin types `bob@acme.com`. On submit:

```tsx
function handleSubmit(e: React.FormEvent) {
  e.preventDefault()
  if (!email.trim()) return
  createInvitation({ variables: { email: email.trim() } })
}
```

---

# Stage 2 — Apollo Sends the Mutation

[admin/src/graphql/mutations.graphql line 111](admin/src/graphql/mutations.graphql#L111):
```graphql
mutation CreateInvitation($email: String!) {
  createInvitation(email: $email) { id email status expiresAt createdAt }
}
```

Auto-generated to `CreateInvitationDocument` by `npm run codegen`.

Apollo link chain: `errorLink → authLink → httpLink`. [authLink](admin/src/apollo/links/auth.ts#L18) attaches `Authorization: Bearer <admin's JWT>`. `CreateInvitation` is **not** in the `PUBLIC_OPERATIONS` allowlist → no `X-Public-Operation` header → server routes it through the protected middleware chain.

Wire request:
```
POST /graphql
Authorization: Bearer eyJhbGci...
{ "operationName": "CreateInvitation", "query": "...", "variables": { "email": "bob@acme.com" } }
```

While in flight: `loading=true`, button shows "Sending…".

---

# Stage 3 — gqlgen Dispatches to CreateInvitation Resolver

[main.go line 156](controller/cmd/server/main.go#L156) routes `/graphql`. `routeGraphQL` checks for `X-Public-Operation` — none → `protected` chain runs:
- [`AuthMiddleware`](controller/internal/middleware/auth.go#L27) — verifies JWT, injects `tenant.TenantContext`
- [`WorkspaceGuard`](controller/internal/middleware/workspace.go#L21) — checks `workspaces.status = 'active'`
- gqlgen handler parses + dispatches

Generated code in [generated.go](controller/graph/generated.go) matches `case "createInvitation"` and calls `ec.Resolvers.Mutation().CreateInvitation(ctx, email)`.

Lands in [client.resolvers.go line 22](controller/graph/resolvers/client.resolvers.go#L22):
```go
func (r *mutationResolver) CreateInvitation(ctx context.Context, email string) (*graph.Invitation, error) {
    tc, ok := tenant.Get(ctx)
    if !ok { return nil, fmt.Errorf("unauthenticated") }
    if tc.Role != "admin" {
        return nil, fmt.Errorf("forbidden: only admins can create invitations")
    }

    inv, err := r.InvitationStore.CreateInvitation(ctx, email, tc.TenantID, tc.UserID)
    if err != nil { return nil, fmt.Errorf("create invitation: %w", err) }

    var workspaceName string
    r.Pool.QueryRow(ctx, `SELECT name FROM workspaces WHERE id = $1`, tc.TenantID).Scan(&workspaceName)
    if workspaceName == "" { workspaceName = "your workspace" }

    go r.InvitationEmailer.SendInvitation(inv, workspaceName)
    return invitationToGQL(inv), nil
}
```

Three moves:
- **Inline role check** — GraphQL has no per-field middleware, so the role check lives in the resolver
- **Workspace from JWT** — `tc.TenantID` decides which workspace, not anything the frontend sent
- **`go ...SendInvitation` is fire-and-forget** — email sending is slow; user shouldn't wait

---

# Stage 4 — Store.CreateInvitation Generates Token + Inserts Rows

[store.go line 40](controller/internal/invitation/store.go#L40):

```go
raw := make([]byte, 32)
rand.Read(raw)
token := hex.EncodeToString(raw)    // 64 hex chars, 256-bit entropy

tx, _ := s.db.Begin(ctx)
defer tx.Rollback(ctx)

// INSERT invitations
tx.QueryRow(ctx,
    `INSERT INTO invitations (email, workspace_id, invited_by, token)
     VALUES ($1, $2, $3, $4)
     RETURNING id, email, workspace_id, invited_by, token, status, expires_at, created_at`,
    email, workspaceID, invitedBy, token,
).Scan(&inv.ID, ...)

// INSERT workspace_members (stub row for the future user)
tx.Exec(ctx,
    `INSERT INTO workspace_members (workspace_id, email, role, status, invited_by)
     VALUES ($1, $2, 'member', 'invited', $3)
     ON CONFLICT (workspace_id, email) DO NOTHING`,
    workspaceID, email, invitedBy,
)

tx.Commit(ctx)
```

Two atomic inserts:

| Row | Why |
|---|---|
| `invitations` | The token + email + 7-day TTL — what the link in the email decodes to |
| `workspace_members` (status=`'invited'`, user_id=NULL) | Stub for the future user. Bootstrap will look this up by email to know which workspace to put the user in |

The `ON CONFLICT DO NOTHING` makes re-inviting the same email a no-op.

DB defaults set `status='pending'` and `expires_at = NOW() + 7 days`.

---

# Stage 5 — Goroutine Sends the Email

[email.go line 34](controller/internal/invitation/email.go#L34):
```go
link := fmt.Sprintf("%s/invite/%s", e.baseURL, inv.Token)

if e.host == "" {
    log.Printf("[INVITE] To: %s | Workspace: %s | Link: %s", inv.Email, workspaceName, link)
    return nil
}
// else: SMTP send
```

Two modes:
- **Production** — `smtp.SendMail` with SMTP_HOST/PORT/FROM/PASSWORD env vars
- **Development** — `SMTP_HOST=""` → logs the link to stdout; copy from terminal

`Emailer` constructed in [main.go line 112](controller/cmd/server/main.go#L112). `envOr` (not `mustEnv`) — SMTP is optional.

---

# Stage 6 — Mutation Returns, Admin Sees Confirmation

Resolver returns `invitationToGQL(inv)` — [helper](controller/graph/resolvers/client_helpers.go#L11) converts internal `*invitation.Invitation` → GraphQL `*graph.Invitation`. gqlgen serializes:
```json
{ "data": { "createInvitation": { "id": "...", "email": "bob@acme.com", "status": "pending", "expiresAt": "...", "createdAt": "..." } } }
```

Apollo resolves the promise → `onCompleted` fires → `setSent(true)` → InviteDialog re-renders into success state ("Invitation sent to bob@acme.com").

Admin's side is done.

---

# Stage 7 — User Receives Email, Clicks the Link

Email contains:
```
Accept here: https://app.zecurity.in/invite/abc123def456...
```

Click → browser GETs `/invite/<token>` → static server returns React `index.html`. React Router matches [App.tsx line 57](admin/src/App.tsx#L57):
```tsx
<Route path="/invite/:token" element={<InviteAccept />} />
```

Public route — outside both `AdminLayout` and `ProtectedLayout`. No JWT required.

---

# Stage 8 — InviteAccept Fetches Invitation Details

[InviteAccept.tsx line 23](admin/src/pages/InviteAccept.tsx#L23):
```tsx
const { token } = useParams<{ token: string }>()
const fetched = useRef(false)

useEffect(() => {
  if (fetched.current || !token) return
  fetched.current = true

  fetch(`/api/invitations/${token}`)
    .then(r => r.ok ? r.json() : Promise.reject(r.status))
    .then((data) => setInvitation(data))
    .catch(() => setError('Invitation not found or has expired.'))
}, [token])
```

- **Plain `fetch`, not Apollo** — Apollo would attach a JWT; there isn't one yet. This endpoint is public.
- **`useRef(false)` guard** — prevents StrictMode double-invoke.

---

# Stage 9 — Backend Serves Public GET /api/invitations/{token}

[main.go line 170](controller/cmd/server/main.go#L170):
```go
mux.Handle("GET /api/invitations/{token}", http.HandlerFunc(inviteHandler.Get))
```

No middleware — public.

[handler.go line 84](controller/internal/invitation/handler.go#L84):
```go
inv, err := h.store.GetByToken(r.Context(), token)
if errors.Is(err, ErrNotFound) { 404; return }
if inv.Status != "pending" || time.Now().After(inv.ExpiresAt) {
    writeJSONError(w, http.StatusNotFound, "invitation not found")
    return
}
json.NewEncoder(w).Encode(toResponse(inv))
```

[`GetByToken`](controller/internal/invitation/store.go#L87) joins `workspaces` to populate `workspace_name`.

Three protections:
- **404 on missing, expired, or accepted** — all indistinguishable to outsiders
- **`toResponse`** omits the `token` field — don't echo it back
- **Limited fields** — `{id, email, status, workspace_name, expires_at, created_at}`

Response:
```json
{ "id": "...", "email": "bob@acme.com", "status": "pending", "workspace_name": "Acme", "expires_at": "...", "created_at": "..." }
```

---

# Stage 10 — User Clicks "Sign in with Google"

[InviteAccept.tsx line 33](admin/src/pages/InviteAccept.tsx#L33):
```tsx
async function handleSignIn() {
  sessionStorage.setItem('ztna_invite_token', token)
  const result = await initiateAuth({
    variables: { provider: 'google', workspaceName: invitation.workspace_name },
  })
  const { redirectUrl, state } = result.data!.initiateAuth
  sessionStorage.setItem('ztna_oauth_state', state)
  window.location.href = redirectUrl
}
```

Two things stashed in `sessionStorage`:
- `ztna_invite_token` — survives the OAuth round trip; AuthCallback reads it after login to finalize the accept
- `ztna_oauth_state` — CSRF nonce

Same `initiateAuth` mutation as signup — no separate endpoint. `workspaceName` gets parked in Redis alongside the PKCE verifier. (This won't actually create a workspace — Bootstrap will detect the pending invite and ignore the name.)

Hard redirect to Google.

---

# Stage 11 — Google Authenticates, Redirects to /auth/callback

Identical to signup. Google → `302 /auth/callback?code=...&state=...`.

---

# Stage 12 — CallbackHandler Runs Steps 1–7 (Same as Signup)

[callback.go line 34](controller/internal/auth/callback.go#L34). Identical execution through Step 6:
1. Read `code`, `state` from query
2. `verifySignedState` — HMAC check
3. `GetAndDeletePKCEState` from Redis — atomic GETDEL, recovers `(codeVerifier, workspaceName)`
4. `exchangeCodeForTokens` — POST to Google
5. `VerifyGoogleIDToken` — 6 checks
6. Extract `email`, `providerSub`, `name`

Then Step 7 — the call to Bootstrap is the same, but Bootstrap takes a different branch:
```go
result, err := s.bootstrapSvc.Bootstrap(ctx, email, "google", providerSub, bootstrapName)
```

---

# Stage 13 — Bootstrap Takes Branch B (Invited User)

[bootstrap.go line 76](controller/internal/bootstrap/bootstrap.go#L76):

```go
// Branch A first: returning user via provider_sub? — no (first login)
// Branch B: pending invite by email?
var pendingWorkspaceID, pendingRole string
err = s.Pool.QueryRow(ctx,
    `SELECT workspace_id, role FROM workspace_members
      WHERE email = $1 AND status = 'invited' AND user_id IS NULL
      LIMIT 1`,
    email,
).Scan(&pendingWorkspaceID, &pendingRole)

if err == nil {
    return s.runInvitedUserTransaction(ctx, email, provider, providerSub, pendingWorkspaceID, pendingRole)
}
```

The pre-created stub row from Stage 4 matches by email. `pendingWorkspaceID` = the inviter's workspace.

**Key invariant:** the `workspaceName` the user typed (or didn't) is **discarded**. Bootstrap uses `pendingWorkspaceID` from the DB. The invited user always lands in the inviter's workspace, regardless of frontend manipulation.

---

# Stage 14 — runInvitedUserTransaction Creates the User

[bootstrap.go line 205](controller/internal/bootstrap/bootstrap.go#L205):
```go
tx, _ := s.Pool.Begin(ctx)
defer tx.Rollback(ctx)

// 1. Create user with tenant_id = invited workspace, role from invite
tx.QueryRow(ctx,
    `INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
     VALUES ($1, $2, $3, $4, $5, 'active')
     RETURNING id`,
    workspaceID, email, provider, providerSub, role,
).Scan(&userID)

// 2. Link the stub workspace_members row to the now-known user_id
tx.Exec(ctx,
    `UPDATE workspace_members
        SET user_id = $1
      WHERE workspace_id = $2 AND email = $3 AND status = 'invited'`,
    userID, workspaceID, email,
)

tx.Commit(ctx)
return &Result{TenantID: workspaceID, UserID: userID, Role: role}, nil
```

Note: status is still `'invited'`. It becomes `'active'` later in Stage 18 (separate `/accept` call has access to the token).

---

# Stage 15 — JWT Issued for the Invited Workspace

Back in [callback.go line 121](controller/internal/auth/callback.go#L121). Same as signup:
- `issueAccessToken` — HS256 JWT, 15min TTL, `tenant_id` = inviter's workspace, `role` = "member"
- `issueRefreshToken` — opaque random bytes, stored in Redis, 7d TTL
- `http.SetCookie` — httpOnly Secure SameSite=Strict refresh cookie
- `http.Redirect → /auth/callback#token=<JWT>`

---

# Stage 16 — Browser Lands on /auth/callback

[AuthCallback.tsx line 38](admin/src/pages/AuthCallback.tsx#L38):
```tsx
const token = window.location.hash.slice('#token='.length)
window.history.replaceState(null, '', window.location.pathname)
setAccessToken(token)

const result = await apolloClient.query<MeQuery>({ query: MeDocument, fetchPolicy: 'network-only' })
setUser(result.data!.me)
```

Same as signup so far. `me` query goes through `AuthMiddleware → WorkspaceGuard → resolver`, returns `role = "MEMBER"`.

Then [line 71](admin/src/pages/AuthCallback.tsx#L71) — the invite-finalization side path runs:
```tsx
const inviteToken = sessionStorage.getItem('ztna_invite_token')
if (inviteToken) {
  sessionStorage.removeItem('ztna_invite_token')
  const jwt = useAuthStore.getState().accessToken
  await fetch(`/api/invitations/${inviteToken}/accept`, {
    method: 'POST',
    headers: { Authorization: `Bearer ${jwt}` },
  })
  // errors ignored — user is already a member, accept can be retried
}
```

`removeItem` runs BEFORE the fetch — defense against retry loops.

---

# Stage 17 — POST /api/invitations/{token}/accept

[main.go line 171](controller/cmd/server/main.go#L171):
```go
inviteAcceptRoute := middleware.AuthMiddleware(JWT_SECRET)(
    middleware.WorkspaceGuard(db.Pool)(
        http.HandlerFunc(inviteHandler.Accept),
    ),
)
mux.Handle("POST /api/invitations/{token}/accept", inviteAcceptRoute)
```

Auth + workspace guard required. No role check — members can accept their own invitations.

[handler.go line 114](controller/internal/invitation/handler.go#L114):
```go
tc, _ := tenant.Get(r.Context())
token := r.PathValue("token")
err := h.store.AcceptInvitation(r.Context(), token, tc.TenantID, tc.UserID, tc.Email)
```

Four values from JWT + URL: token, workspace, user, email. All four must match the DB row.

---

# Stage 18 — Store.AcceptInvitation Marks Status Active

[store.go line 113](controller/internal/invitation/store.go#L113):

```go
tx, _ := s.db.Begin(ctx)
defer tx.Rollback(ctx)

// 1. Mark invitation accepted — must satisfy ALL 5 conditions
tag, _ := tx.Exec(ctx,
    `UPDATE invitations SET status = 'accepted'
      WHERE token = $1
        AND workspace_id = $2
        AND email = $3
        AND status = 'pending'
        AND expires_at > NOW()`,
    token, workspaceID, email,
)
if tag.RowsAffected() == 0 { return ErrNotFound }

// 2. Activate workspace_members
tx.Exec(ctx,
    `UPDATE workspace_members
        SET user_id = $1, status = 'active', joined_at = NOW()
      WHERE workspace_id = $2 AND email = $3 AND status = 'invited'
        AND (user_id IS NULL OR user_id = $1)`,
    userID, workspaceID, email,
)

tx.Commit(ctx)
```

Five-condition `WHERE` on Update 1 is the security boundary. Any mismatch → `RowsAffected = 0` → `ErrNotFound` → handler returns 404. Same 404 for wrong token, wrong email, expired, already accepted — no information leak.

After commit:
- `invitations.status = 'accepted'`
- `workspace_members.status = 'active'`, `joined_at = NOW()`, `user_id = <new user>`

Handler returns 200 OK.

---

# Stage 19 — Role-Based Redirect: Member → /client-install

[AuthCallback.tsx line 84](admin/src/pages/AuthCallback.tsx#L84):
```tsx
if (result.data!.me.role === 'ADMIN') {
  navigate('/dashboard', { replace: true })
} else {
  navigate('/client-install', { replace: true })
}
```

Role is `'MEMBER'` → `/client-install`. [App.tsx line 65](admin/src/App.tsx#L65):
```tsx
<Route element={<ProtectedLayout />}>
  <Route path="/client-install" element={<ClientInstall />} />
</Route>
```

[`ProtectedLayout`](admin/src/App.tsx#L34) (not `AdminLayout`) — any authenticated user, no sidebar. Renders `<ClientInstall />` which shows the user the `zecurity` client install command.

---

# Files Touched

### Frontend
- [admin/src/pages/TeamUsers.tsx](admin/src/pages/TeamUsers.tsx) — invite trigger
- [admin/src/pages/InviteAccept.tsx](admin/src/pages/InviteAccept.tsx) — public invite landing page
- [admin/src/pages/AuthCallback.tsx](admin/src/pages/AuthCallback.tsx) — finalizes accept after OAuth
- [admin/src/pages/ClientInstall.tsx](admin/src/pages/ClientInstall.tsx) — member landing
- [admin/src/graphql/mutations.graphql](admin/src/graphql/mutations.graphql) — `CreateInvitation`, `InitiateAuth`
- [admin/src/graphql/queries.graphql](admin/src/graphql/queries.graphql) — `invitation(token)` (lookup, unused in this flow but exists)
- [admin/src/apollo/links/auth.ts](admin/src/apollo/links/auth.ts) — Bearer attachment
- [admin/src/store/auth.ts](admin/src/store/auth.ts) — token + user state

### Backend
- [controller/cmd/server/main.go](controller/cmd/server/main.go) — route wiring (`/api/invitations`, `/graphql`)
- [controller/graph/resolvers/client.resolvers.go](controller/graph/resolvers/client.resolvers.go) — `CreateInvitation` resolver
- [controller/graph/resolvers/client_helpers.go](controller/graph/resolvers/client_helpers.go) — `invitationToGQL`
- [controller/internal/invitation/handler.go](controller/internal/invitation/handler.go) — `Create`, `Get`, `Accept` HTTP handlers
- [controller/internal/invitation/store.go](controller/internal/invitation/store.go) — DB operations
- [controller/internal/invitation/email.go](controller/internal/invitation/email.go) — SMTP / stdout emailer
- [controller/internal/auth/callback.go](controller/internal/auth/callback.go) — calls Bootstrap
- [controller/internal/bootstrap/bootstrap.go](controller/internal/bootstrap/bootstrap.go) — Branch B / `runInvitedUserTransaction`
- [controller/internal/middleware/auth.go](controller/internal/middleware/auth.go) — JWT verification
- [controller/internal/middleware/workspace.go](controller/internal/middleware/workspace.go) — workspace guard
- [controller/internal/tenant/context.go](controller/internal/tenant/context.go) — tenant context

### Database
- `invitations` table — token, email, workspace_id, invited_by, status, expires_at
- `workspace_members` table — workspace_id, email, user_id, role, status, invited_by, joined_at
- `users` table — created during invited bootstrap
- `workspaces` table — JOIN for workspace_name

---

# Key Invariants

| Invariant | Where enforced |
|-----------|---------------|
| Only admins can create invitations | Role check in [`CreateInvitation` resolver](controller/graph/resolvers/client.resolvers.go#L27) |
| Admin can only invite into their own workspace | `tc.TenantID` from JWT, not from request body |
| Invite tokens are unguessable | `crypto/rand` 32 bytes / hex (256-bit entropy) |
| Tokens expire after 7 days | DB default on `invitations.expires_at` |
| Invitations are single-use | `status='pending'` filter in `AcceptInvitation` UPDATE |
| Public lookup endpoint can't distinguish missing/expired/accepted | All return 404 |
| Public lookup doesn't echo the token | `toResponse` omits the `token` field |
| Invited user always lands in inviter's workspace | Bootstrap Branch B uses `pendingWorkspaceID` from DB |
| Accept verifies token + workspace + email + status + expiry | Five-condition `WHERE` in `AcceptInvitation` |
| Re-invite is idempotent | `ON CONFLICT (workspace_id, email) DO NOTHING` |
| Accept-after-link is idempotent | `user_id IS NULL OR user_id = $1` in workspace_members UPDATE |
| Email failure doesn't break invitation creation | `go r.InvitationEmailer.SendInvitation` is fire-and-forget |

---

# Quick-Reference Call Chains

### Admin creates invitation
```
TeamUsers.tsx → InviteDialog → createInvitation mutation
  → main.go routeGraphQL → protected chain
  → AuthMiddleware → WorkspaceGuard → gqlgen
  → client.resolvers.go: CreateInvitation
      → role check
      → store.CreateInvitation
          → INSERT invitations (pending)
          → INSERT workspace_members (status=invited, user_id=NULL)
      → go InvitationEmailer.SendInvitation (async)
      → invitationToGQL → JSON response
```

### Invited user clicks link → accepts
```
Browser /invite/<token>
  → InviteAccept.tsx → fetch GET /api/invitations/<token>
  → handler.Get → store.GetByToken → JSON
  → click "Sign in" → sessionStorage.setItem('ztna_invite_token')
  → initiateAuth mutation → Google OAuth → /auth/callback
  → callback.go: CallbackHandler
      → bootstrap.go: Bootstrap → Branch B → runInvitedUserTransaction
          → INSERT users (tenant=invited workspace)
          → UPDATE workspace_members SET user_id
      → JWT issued (tenant=invited workspace, role=member)
      → 302 /auth/callback#token=<JWT>
  → AuthCallback.tsx → setAccessToken → me query (role=MEMBER)
  → fetch POST /api/invitations/<token>/accept
      → handler.Accept → store.AcceptInvitation
          → UPDATE invitations SET status='accepted'
          → UPDATE workspace_members SET status='active', joined_at=NOW()
  → navigate('/client-install', { replace: true })
```

---

# Next Flows to Study

- **Connector enrollment** — admin generates install token, connector enrolls, controller signs cert
- **Policy creation + ACL push** — admin creates group + rule, snapshot compiles, connector pulls via heartbeat
- **Client device enrollment** — `zecurity setup` from the client install page, device cert signed
- **RDE tunnel (Sprint 9)** — `zecurity up` → TUN device → traffic through Connector to resource
