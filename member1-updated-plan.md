# Member 1 — Updated Flow Plan
## Twingate-Style Signup + Returning User Login + Dashboard

---

## What Is Already Built and Working

Everything from the original plan is implemented and working:

- Vite + React + TypeScript + shadcn/ui + Tailwind + Apollo Client
- Zustand auth store (JWT in memory, never localStorage)
- Apollo auth link + error link (Bearer token, 401 refresh, retry)
- Router with public and protected routes
- useRequireAuth hook (silent refresh on page reload)
- Login.tsx (single "Sign in with Google" button)
- AuthCallback.tsx (reads JWT from URL hash, calls me query)
- Dashboard.tsx (me + workspace queries with skeleton loading)
- Settings.tsx (workspace info)
- AppShell, Sidebar, Header (layout + sign out)

None of this changes. Everything below is additive.

---

## What Is Missing

The current flow jumps straight from "open app" to "sign in with Google."
The workspace name is never chosen by the user — Bootstrap uses Google's
profile name, which is the person's name (e.g. "Alice Chen"), not a
company or network name.

What Twingate actually does before OAuth:
  1. Ask for email and account type (Home / Office)
  2. Suggest a workspace name derived from the email domain
  3. Let the user edit that name freely, show the URL slug live
  4. THEN redirect to OAuth

We are missing steps 1, 2, and 3.

---

## The Complete Flow After This Update

### New user

```
/signup              → Step 1: enter email + choose Home or Office
/signup/workspace    → Step 2: workspace name (suggested, editable) + slug preview
/signup/auth         → Step 3: "Sign in with Google to continue"
                               workspaceName carried into initiateAuth mutation
                               OAuth happens here
/auth/callback       → reads JWT from hash (unchanged)
/dashboard           → workspace active, user is admin (unchanged)
```

### Returning user

```
/login               → "Sign in with Google" (unchanged, no workspace step)
/auth/callback       → reads JWT from hash (unchanged)
/dashboard           → unchanged
```

### Page reload (either user type)

```
any protected route  → useRequireAuth tries silent refresh
success              → stays on page
failure              → /login
```

---

## What Changes on the Frontend

### New files to create

```
src/
  store/
    signup.ts              ← new Zustand store for the 3-step wizard state

  pages/
    signup/
      Step1Email.tsx       ← email input + Home / Office selection
      Step2Workspace.tsx   ← workspace name input + live slug preview
      Step3Auth.tsx        ← sign in with Google (carries workspaceName)
```

### Existing files to update

**src/App.tsx** — add three new routes:
```
/signup              → Step1Email
/signup/workspace    → Step2Workspace
/signup/auth         → Step3Auth
```
The existing `/login` and `/auth/callback` routes stay unchanged.

**src/graphql/mutations.graphql** — add the optional `workspaceName` variable:
```
mutation InitiateAuth($provider: String!, $workspaceName: String)
```
Run codegen after this change. The generated hook gains an optional
`workspaceName` variable. Existing `Login.tsx` still works because
the variable is optional — it just does not pass it.

---

## Signup Store (signup.ts)

Holds the state that flows across all three steps.
Separate from the auth store — completely reset after OAuth redirect.

Fields:
- `email` — what the user typed in Step 1
- `accountType` — "home" or "office", chosen in Step 1
- `workspaceName` — editable in Step 2
- `slug` — derived live from workspaceName (mirrors backend slugify logic)

Actions:
- `setEmail`, `setAccountType`, `setWorkspaceName` — obvious setters
- `setWorkspaceName` also recomputes `slug` on every keystroke
- `reset` — wipes everything; called in Step3Auth right before the OAuth redirect

The slug preview logic (frontend) must mirror Member 3's `slugify()` exactly
so what the user sees in Step 2 is what gets stored.
Rule: lowercase, replace non-alphanumeric with hyphens, trim leading/trailing hyphens.

---

## Step 1 — Email + Account Type

**Route:** `/signup`

**What it shows:**
- Heading: "Create your network"
- Email input field (type=email, autofocus)
- Two toggle cards: "Home" and "Office", each with a one-line description
  - Home: "Personal devices and home lab"
  - Office: "Team and company resources"
- Continue button
- "Already have a network? Sign in" link pointing to `/login`

**Behaviour:**
- Continue is blocked if email is empty or has no `@`
- Continue is blocked if neither Home nor Office is selected
- On Continue: stores email + accountType in signup store, navigates to `/signup/workspace`
- Enter key on the email field triggers Continue
- No backend call happens here — purely local

---

## Step 2 — Workspace Name

**Route:** `/signup/workspace`

**Guard:** if `email` is empty in signup store (user landed here directly),
redirect to `/signup` immediately.

**What it shows:**
- Heading: "Name your network"
- Subheading: "You can always rename it later."
- Workspace name input (text, autofocus)
- Live slug preview box below the input:
  ```
  Your network URL
  ztna.yourapp.com/[slug]
  ```
  Updates on every keystroke.
- Continue button (disabled while workspaceName is empty)
- Back button → `/signup`

**Auto-suggestion logic (runs once on mount):**
- If workspaceName is already set in the store, do not overwrite it
- Otherwise derive a suggestion from the email domain:
  - Take the part before the first dot: `acme.com` → `acme`
  - Title-case it, replace hyphens/underscores with spaces: `my-corp` → `My Corp`
  - Skip generic providers (gmail.com, yahoo.com, hotmail.com, outlook.com,
    icloud.com, proton.me) — leave the field empty for those
- Set the suggestion via `setWorkspaceName` so slug also computes

**Behaviour:**
- User can freely overwrite whatever is suggested
- Slug preview updates live as they type
- On Continue: stores the final workspaceName (and derived slug) in signup store,
  navigates to `/signup/auth`
- Enter key triggers Continue

---

## Step 3 — Sign In With Google

**Route:** `/signup/auth`

**Guard:** if `email` or `workspaceName` is empty in signup store,
redirect to `/signup` immediately.

**What it shows:**
- Heading: "One last step"
- Subheading: "Sign in with Google to verify your identity and create your network."
- A summary card showing what was chosen:
  ```
  Email         alice@acme.com
  Network name  Acme
  ```
- "Sign in with Google" button
- Error message if mutation fails
- Back button → `/signup/workspace`

**Behaviour on button click:**
1. Call `initiateAuth(provider: "google", workspaceName: workspaceName)`
2. Store `state` in sessionStorage (CSRF check on return)
3. Call `reset()` on the signup store — workspace name is now in Redis on the backend,
   it does not need to live in memory anymore
4. `window.location.href = redirectUrl` — full browser redirect to Google

**After this point:** AuthCallback.tsx takes over exactly as before.
Nothing changes in AuthCallback, me query, or dashboard.

---

## Workspace Name Suggestion Rules

These are important to get right so the preview matches what the backend stores.

| Email | Suggested name | Reason |
|---|---|---|
| alice@acme.com | Acme | domain base, title-cased |
| bob@my-corp.io | My Corp | hyphen → space, title-cased |
| carol@gmail.com | (empty) | generic provider, skip |
| dave@outlook.com | (empty) | generic provider, skip |
| eve@big-company.co.uk | Big Company | first segment only, hyphen → space |

The slug shown in the preview is what `slugify()` produces from the name:
- "Acme Corp" → `acme-corp`
- "My Network" → `my-network`
- "Big Company!" → `big-company`

---

## Backend Changes Required (Coordinate Before Testing Step 3)

Member 1 can build and test Steps 1 and 2 immediately — they are pure frontend.

Step 3 end-to-end requires two backend changes:

**Member 4 — schema.graphqls:**
Add `workspaceName: String` (optional) to the `initiateAuth` mutation.
Run gqlgen. Update `auth.resolvers.go` to pass it through to `AuthService.InitiateAuth`.

**Member 2 — oidc.go + redis.go + callback.go:**
`InitiateAuth()` now receives `workspaceName` and stores it in Redis alongside
`code_verifier` (as a small JSON payload instead of just a string).
`callback.go` retrieves `workspaceName` from the Redis payload and passes it
to `Bootstrap()` instead of the Google profile name.
`Bootstrap()` signature does not change — it still takes `name string`.
Member 3 touches nothing.

---

## What Does Not Change

| File | Status |
|---|---|
| AuthCallback.tsx | Unchanged |
| Dashboard.tsx | Unchanged |
| Settings.tsx | Unchanged |
| AppShell.tsx | Unchanged |
| Sidebar.tsx | Unchanged |
| Header.tsx | Unchanged |
| Login.tsx | Unchanged |
| useRequireAuth.ts | Unchanged |
| store/auth.ts | Unchanged |
| apollo/client.ts | Unchanged |
| apollo/links/auth.ts | Unchanged |
| apollo/links/error.ts | Unchanged |

---

## Integration Checklist

```
Step 1 — Email + account type (no backend needed)
  ✓ /signup renders without redirect
  ✓ Continue blocked when email is empty or invalid
  ✓ Continue blocked when neither Home nor Office selected
  ✓ Continue stores email + accountType in signup store
  ✓ Continue navigates to /signup/workspace
  ✓ "Sign in" link goes to /login

Step 2 — Workspace name (no backend needed)
  ✓ Landing directly on /signup/workspace with empty email → redirect to /signup
  ✓ Workspace name auto-suggested from email domain on first mount
  ✓ Generic email domains (gmail, yahoo, etc.) → no suggestion, field empty
  ✓ Slug preview updates on every keystroke
  ✓ Slug preview matches backend slugify logic exactly
  ✓ Continue disabled when workspaceName is empty
  ✓ Continue stores workspaceName in signup store
  ✓ Continue navigates to /signup/auth
  ✓ Back navigates to /signup

Step 3 — Sign in with Google (needs Member 2 + 4 backend changes)
  ✓ Landing directly on /signup/auth with empty email → redirect to /signup
  ✓ Summary card shows correct email and workspace name from store
  ✓ Button calls initiateAuth with provider="google" and workspaceName
  ✓ workspaceName is passed in the mutation variables
  ✓ state stored in sessionStorage before redirect
  ✓ signup store reset() called before redirect
  ✓ window.location.href set to redirectUrl
  ✓ Error state shown if mutation fails
  ✓ Back navigates to /signup/workspace

Full flow (needs full backend running)
  ✓ New user: /signup → /signup/workspace → /signup/auth → OAuth → /dashboard
  ✓ Workspace name on dashboard matches what user typed in Step 2
  ✓ Returning user: /login → OAuth → /dashboard (workspace step not shown)
  ✓ Page reload: silent refresh works, no regression
```

---

## Summary of Changes

```
New files       store/signup.ts
                pages/signup/Step1Email.tsx
                pages/signup/Step2Workspace.tsx
                pages/signup/Step3Auth.tsx

Updated files   App.tsx (3 new routes)
                graphql/mutations.graphql (workspaceName variable added)
                → run codegen after mutations.graphql change

Unchanged       everything else

Backend needed  Step 1 + 2: none
                Step 3 end-to-end: Member 4 schema + Member 2 Redis/callback
                Member 3: nothing changes
```
