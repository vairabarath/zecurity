---
type: bug-fix-plan
status: pending
sprint: 8.5
tags:
  - invitation
  - auth
  - frontend
  - go
  - react
---

# Invitation Flow — Remaining Bug Fixes

> Bug 1 (missing `workspace_members` table) is already fixed and merged.
> This document covers the two remaining bugs to implement next.

---

## Context (Read This First)

The invitation flow has three actors:

1. **Admin** — sends invite via dashboard UI or CLI `zecurity-client invite`
2. **Controller** — creates invitation row + `workspace_members` row, sends email
3. **Invited user** — clicks email link → OAuth login → frontend calls accept endpoint

The full sequence after Bug 1 fix:

```
Admin invites alice@example.com
  → invitations row created (status='pending')
  → workspace_members row created (user_id=NULL, status='invited')
  → email sent with link: {baseURL}/invite/{token}

Alice clicks link
  → InviteAccept.tsx stores token in sessionStorage
  → calls initiateAuth mutation → OAuth starts

Alice authenticates with Google
  → CallbackHandler: bootstrap checks workspace_members by email
  → finds pending invite → runInvitedUserTransaction
  → user row created (role='member', tenant_id=invited_workspace_id)
  → workspace_members.user_id = new_user_id (still status='invited')
  → JWT issued (role='member')

Frontend AuthCallback.tsx
  → extracts JWT, stores in Zustand
  → reads invite token from sessionStorage
  → calls POST /api/invitations/{token}/accept
  → AcceptInvitation: marks invitation 'accepted', workspace_members status='active'
  → role check: role !== 'ADMIN' → navigate('/client-install')  ← PAGE MISSING
```

---

## Bug 2 — Missing Email Validation in AcceptInvitation

### Why This Is a Security Issue

`AcceptInvitation` in `handler.go:127` only checks:
- Is the JWT valid?
- Does JWT `tenant_id` match the invitation's `workspace_id`?

It does NOT verify that the authenticated user's email matches the invitation's email.

**Attack scenario:**
```
Admin invites alice@example.com, token = "abc123"
Bob is already a member of the same workspace (has a valid JWT)
Bob somehow gets the token (guesses it, intercepts the email, etc.)
Bob calls: POST /api/invitations/abc123/accept  with his own JWT
→ Bob's user_id gets linked to Alice's workspace_members row
→ Alice's invite token is consumed — she can never accept it
→ Bob gains nothing but Alice is permanently locked out of her invite
```

With 256-bit random tokens this is hard to exploit in practice, but it is
a correctness bug — only the person the invite was sent to should be able
to accept it.

---

### Root Cause

The JWT currently carries: `tenant_id`, `role`, `sub` (user_id).
**Email is not in the JWT.**

`TenantContext` (in `controller/internal/tenant/context.go`) carries:
`TenantID`, `UserID`, `Role`. **No Email field.**

So the handler cannot check email without either:
- A) Adding email to the JWT and TenantContext (recommended), OR
- B) Doing an extra DB query to look up the user's email by user_id

**Recommended: Option A** — email belongs in the JWT anyway, it's useful
for other handlers and logging. One change propagates cleanly.

---

### Files to Change

#### 1. `controller/internal/auth/session.go`

Add `Email` to `jwtClaims` struct:

```go
// BEFORE
type jwtClaims struct {
    TenantID string `json:"tenant_id"`
    Role     string `json:"role"`
    jwt.RegisteredClaims
}

// AFTER
type jwtClaims struct {
    TenantID string `json:"tenant_id"`
    Role     string `json:"role"`
    Email    string `json:"email"`      // ← add this
    jwt.RegisteredClaims
}
```

Add email parameter to `issueAccessToken`:

```go
// BEFORE
func (s *serviceImpl) issueAccessToken(userID, tenantID, role string) (string, error) {
    claims := jwtClaims{
        TenantID: tenantID,
        Role:     role,
        ...
    }

// AFTER
func (s *serviceImpl) issueAccessToken(userID, tenantID, role, email string) (string, error) {
    claims := jwtClaims{
        TenantID: tenantID,
        Role:     role,
        Email:    email,               // ← add this
        ...
    }
```

Also update the public wrapper `IssueAccessToken` to pass email through:

```go
// BEFORE
func (s *serviceImpl) IssueAccessToken(userID, tenantID, role string) (string, int64, error) {
    token, err := s.issueAccessToken(userID, tenantID, role)

// AFTER
func (s *serviceImpl) IssueAccessToken(userID, tenantID, role, email string) (string, int64, error) {
    token, err := s.issueAccessToken(userID, tenantID, role, email)
```

Also update `VerifyAccessToken` to return email in `AccessTokenClaims`:

```go
// AccessTokenClaims — BEFORE
type AccessTokenClaims struct {
    UserID   string
    TenantID string
    Role     string
}

// AccessTokenClaims — AFTER
type AccessTokenClaims struct {
    UserID   string
    TenantID string
    Role     string
    Email    string   // ← add this
}

// VerifyAccessToken return — AFTER
return &AccessTokenClaims{
    UserID:   claims.Subject,
    TenantID: claims.TenantID,
    Role:     claims.Role,
    Email:    claims.Email,   // ← add this
}, nil
```

---

#### 2. `controller/internal/auth/callback.go`

Pass email to `issueAccessToken` at Step 8 (line ~121).
Email is already available in scope as `email` from Step 6:

```go
// BEFORE (line ~121)
accessToken, err := s.issueAccessToken(result.UserID, result.TenantID, result.Role)

// AFTER
accessToken, err := s.issueAccessToken(result.UserID, result.TenantID, result.Role, email)
```

---

#### 3. `controller/internal/auth/refresh.go`

The refresh handler also calls `issueAccessToken`. It needs to look up the
user's email before issuing:

```go
// After verifying the refresh token and looking up user_id from claims,
// fetch the user's email from DB, then:
accessToken, err := s.issueAccessToken(userID, tenantID, role, email)
```

The user email can be fetched with a simple DB query:
```sql
SELECT email FROM users WHERE id = $1
```

---

#### 4. `controller/internal/tenant/context.go`

Add `Email` to `TenantContext`:

```go
// BEFORE
type TenantContext struct {
    TenantID string
    UserID   string
    Role     string
}

// AFTER
type TenantContext struct {
    TenantID string
    UserID   string
    Role     string
    Email    string   // ← add this
}
```

---

#### 5. Auth middleware (wherever JWT is verified and TenantContext is set)

Find where `tenant.Set(ctx, TenantContext{...})` is called (likely in
`controller/internal/middleware/auth.go` or similar). Add email population:

```go
// AFTER verifying JWT and extracting claims:
tenant.Set(ctx, tenant.TenantContext{
    TenantID: claims.TenantID,
    UserID:   claims.Subject,
    Role:     claims.Role,
    Email:    claims.Email,   // ← add this
})
```

---

#### 6. `controller/internal/invitation/store.go`

Add `email` parameter to `AcceptInvitation` and validate in the UPDATE:

```go
// BEFORE
func (s *Store) AcceptInvitation(ctx context.Context, token, workspaceID, userID string) error {
    tag, err := tx.Exec(ctx,
        `UPDATE invitations
            SET status = 'accepted'
          WHERE token = $1
            AND workspace_id = $2
            AND status = 'pending'
            AND expires_at > NOW()`,
        token, workspaceID,
    )

// AFTER
func (s *Store) AcceptInvitation(ctx context.Context, token, workspaceID, userID, email string) error {
    tag, err := tx.Exec(ctx,
        `UPDATE invitations
            SET status = 'accepted'
          WHERE token = $1
            AND workspace_id = $2
            AND email = $3              -- only the invited person can accept
            AND status = 'pending'
            AND expires_at > NOW()`,
        token, workspaceID, email,      -- pass email as $3
    )
```

---

#### 7. `controller/internal/invitation/handler.go`

Pass `tc.Email` to `AcceptInvitation`:

```go
// BEFORE (line ~127)
if err := h.store.AcceptInvitation(r.Context(), token, tc.TenantID, tc.UserID); err != nil {

// AFTER
if err := h.store.AcceptInvitation(r.Context(), token, tc.TenantID, tc.UserID, tc.Email); err != nil {
```

---

### Build Check

```bash
cd controller && go build ./...
```

No migration needed — this is purely a Go + JWT change.

---

---

## Bug 3 — Missing `/client-install` Page (Frontend)

### Why This Is Broken

`admin/src/pages/AuthCallback.tsx` lines 84–88 already has role-based redirect:

```typescript
if (role === 'ADMIN') {
  navigate('/dashboard')
} else {
  navigate('/client-install')   // ← this route/page does not exist
}
```

When an invited member (non-admin) accepts their invite and logs in,
the app sends them to `/client-install` which hits React Router's
no-match fallback — blank page or 404.

---

### What the Page Needs to Show

This is the landing page for workspace members after first login.
They are NOT admins — they have no access to the admin dashboard.
They need to install the Zecurity client to access resources.

**Layout:**

```
┌─────────────────────────────────────────────────┐
│  Zecurity                                        │
│                                                  │
│  Welcome to {workspaceName}                      │
│                                                  │
│  You've been added as a member. To access        │
│  resources on this network, install the          │
│  Zecurity client on your device.                 │
│                                                  │
│  ── Step 1: Download ─────────────────────────── │
│  [↓ Linux amd64]   [↓ Linux arm64]               │
│  (links to GitHub releases page)                 │
│                                                  │
│  ── Step 2: Install ──────────────────────────── │
│  sudo CONTROLLER_ADDR=... \                      │
│    ./client-local-install.sh zecurity-client     │
│  (copyable code block)                           │
│                                                  │
│  ── Step 3: Authenticate ─────────────────────── │
│  zecurity-client setup --workspace {slug}        │
│  zecurity-client login                           │
│  (copyable code blocks)                          │
│                                                  │
│  Need help? Contact your workspace admin.        │
└─────────────────────────────────────────────────┘
```

---

### Files to Change

#### 1. `admin/src/pages/ClientInstall.tsx` (NEW FILE)

```typescript
import { useAuthStore } from '@/store/authStore'   // or wherever auth state lives

export default function ClientInstall() {
  const { workspaceName, workspaceSlug } = useAuthStore()

  return (
    // Page content per layout above
    // Use existing shadcn/ui components: Card, Button, Badge, etc.
    // Use a <pre> or <code> block for the shell commands
    // Add a copy-to-clipboard button on each code block
  )
}
```

**Key data to read from auth store:**
- `workspaceName` — shown in the welcome header
- `workspaceSlug` — used in `zecurity-client setup --workspace {slug}`
- `email` — can show "Logged in as {email}" at the top

These are available in the JWT claims and should already be in the Zustand
auth store after `AuthCallback.tsx` processes the token. If `workspaceSlug`
is not currently in the store, add it — it is in the JWT `tenant_id` (workspace
UUID), but the slug should be fetched via a `me` query or stored separately.

> **Check `AuthCallback.tsx`** to see exactly what fields it stores in the
> auth store after parsing the JWT. Match what's available.

---

#### 2. `admin/src/App.tsx`

Add the route. Find where other routes like `/dashboard` are defined and add:

```typescript
// BEFORE (somewhere in the route definitions)
<Route path="/dashboard" element={<PrivateRoute><Dashboard /></PrivateRoute>} />

// AFTER — add this route (no PrivateRoute wrapper — user just logged in
// and may not have admin access, so don't gate this behind admin check)
<Route path="/client-install" element={<ClientInstall />} />
```

Import the component at the top of App.tsx:
```typescript
import ClientInstall from '@/pages/ClientInstall'
```

---

### Build Check

```bash
cd admin && npm run build
```

---

## Dashboard Invite Flow — Already Fixed (Confirmation)

The admin dashboard's **Invite** button in `TeamUsers.tsx` calls the
GraphQL `createInvitation` mutation. The resolver at
`controller/graph/resolvers/client.resolvers.go:30` calls:

```go
inv, err := r.InvitationStore.CreateInvitation(ctx, email, tc.TenantID, tc.UserID)
```

This is the **same `store.CreateInvitation` function** that was fixed in
Bug 1. So the dashboard invite flow automatically benefits from that fix:
- Dashboard invite now creates `workspace_members` row (status='invited') ✅
- Invited user bootstrap now joins existing workspace as 'member' ✅
- AcceptInvitation now updates `workspace_members` correctly ✅

No additional changes needed for the dashboard invite path.

---

## Summary Table

| Bug | Owner | Files | Migration? |
|-----|-------|-------|------------|
| Email validation in AcceptInvitation | M3 (Go) | `auth/session.go`, `auth/callback.go`, `auth/refresh.go`, `tenant/context.go`, middleware/auth.go, `invitation/store.go`, `invitation/handler.go` | No |
| `/client-install` page | M1 (React) | `pages/ClientInstall.tsx` (new), `App.tsx` | No |
| Dashboard invite | — | Already fixed by Bug 1 | — |

---

## Implementation Order

1. **M3** implements email validation (Bug 2) — independent, no frontend dependency
2. **M1** implements `/client-install` page (Bug 3) — independent, no backend dependency
3. Both can be done in parallel

Both must be done before the invite flow can be end-to-end tested with a
real invited member who is not an admin.
