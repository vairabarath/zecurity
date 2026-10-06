# Zecurity Provider Console

The provider operators' console (Sprint 21, Decision Record D-18): its own Vite + React app, with its own build and its own domain. It is separate from the tenant admin dashboard in `admin/`, which it never touches or imports from. It talks only to the controller's `/provider/*` API.

What it does today (Phase C):
- email + password sign-in;
- the forced first password change and voluntary password changes;
- sign-out;
- a session countdown;
- role-aware navigation (`super-admin`, `relay-ops`);
- a Home page;
- **Provider users** (`/users`, super-admin only): list operators, add an operator, change a role, disable, enable and reset a password.

Still to come:
- **Phase P:** the read-only fleet pages (relays, tenants, audit, certificates).

## Operator management (Provider users)

- **Who:** super-admins only. relay-ops has no nav entry, `/users` shows Forbidden, and the controller answers 403 anyway.
- **Every change is confirmed in a dialog first.** After a success the list is re-read from the controller; nothing is updated optimistically.
- **Your own row has no controls.** The controller refuses changes to yourself (`cannot_modify_self`) and never allows zero active super-admins (`last_super_admin`). Change your own password from the account menu.
- **Temporary passwords** (after Add or Reset password):
  - shown once, in a dialog, and discarded from memory when it closes;
  - never stored, logged or put in the URL;
  - copied only when you click Copy (the browser Clipboard API). If copying fails, the password stays visible so you can copy it by hand. The console doesn't touch the clipboard afterwards.
- **Enable clears the password.** An enabled operator can't sign in until you reset their password. After Enable, the console offers **Reset password now**, but never resets by itself.
- **Error codes** from the controller (`already_exists`, `exists_disabled`, `account_disabled`, `last_super_admin`, `cannot_modify_self`, …) are shown as plain-language messages (`src/lib/operator-errors.ts`).
- **Effect on the operator:** a role change, disable or reset signs that operator out everywhere on their next request.

## Development

Prerequisites:
- Node 22 or newer (CI uses 24);
- the local controller stack.

1. **Start the database and the controller.** Follow [`docs/database-development.md`](../docs/database-development.md).
   - The first provider super-admin comes from the create-only bootstrap. Set these in `controller/.env`, then start the controller on `:8080`:

     ```
     PROVIDER_JWT_SECRET=<32+ random bytes, different from JWT_SECRET>
     PROVIDER_BOOTSTRAP_EMAIL=you@example.com
     PROVIDER_BOOTSTRAP_PASSWORD=<temporary, 12–128 characters>
     ```

   - Remove `PROVIDER_BOOTSTRAP_PASSWORD` once the account exists. It is ignored from then on.
2. **Run the console:**

   ```bash
   cd provider-console
   npm ci
   npm run dev          # http://localhost:5174
   ```

   - The dev server proxies `/provider` to `http://localhost:8080`, so the browser sees a single origin in development. No CORS setup is needed.
3. **Sign in** with the bootstrap email and temporary password. You will be asked to set a new password before anything else is reachable.

| Script | What it does |
|---|---|
| `npm run dev` | Dev server on port 5174 (fails if the port is taken) |
| `npm run lint` | ESLint |
| `npm test` | Vitest (jsdom), single run |
| `npm run test:watch` | Vitest in watch mode |
| `npm run build` | Type-check (`tsc -b`) and production build into `dist/` |
| `npm run preview` | Serve `dist/` locally |

The build gate is `npm ci && npm run lint && npm test && npm run build`.

## Production deployment

1. **Build with the controller's origin:**

   ```bash
   VITE_PROVIDER_API_BASE=https://controller.example.com npm run build
   ```

   `VITE_PROVIDER_API_BASE` is baked in at build time. Leave it empty only if the console and the controller's `/provider/*` routes are served from the same origin.
2. **Host `dist/` on a static host, on the console's own domain** (for example `provider-console.example.com`). Don't put it on a tenant-facing domain.
   - Rewrite unknown paths to `index.html`, so that `/login` and `/change-password` work on reload.
   - Serve `index.html` with `Cache-Control: no-cache`. The hashed files under `assets/` can be cached long-term.
3. **Network-lock the console origin (D-18).** Make it reachable only over the provider VPN or from an IP allowlist. Lock the controller's `/provider/*` routes the same way where your network allows it.
4. **Set the controller's `PROVIDER_CONSOLE_ORIGIN` to exactly the deployed console origin:** scheme, host and port, with no trailing slash (for example `https://provider-console.example.com`).
   - The controller then allows CORS from that origin only, for `/provider/*` only, and without credentials.
   - The console never uses cookies; it sends a bearer token.

## Security model

- **Tokens:**
  - The provider token is held in memory and in this tab's `sessionStorage`. It is never stored in `localStorage`, cookies or the URL, and never logged.
  - Closing the tab ends the local session.
  - There is no refresh token. A full session lasts 15 minutes, a password-change-only token 10 minutes, and the operator then signs in again.
- **Refresh:** a stored token is not trusted until `GET /provider/me` accepts it. If the controller rejects it or can't be reached, the session is dropped.
- **Passwords** exist only in form state. They are cleared on submit and never stored or logged.
- **Server responses:**

  | Response | What the console does |
  |---|---|
  | `401 invalid_credentials` from sign-in or change password | Shows a form error |
  | Any other `401` | Ends the session and shows Login with a notice |
  | `403 not a provider user` (disabled account) | Ends the session |
  | `403 password_change_required` | Opens Change password |
  | Any other `403` | Shows Forbidden and keeps the session |

  All of this lives in `src/api/client.ts`.
- **Authorization happens on the server.** Route guards and the role-filtered nav only hide what the controller would refuse. The controller authorizes every request.
- **Allowed writes:** the console's only non-GET calls are login, change password and logout, plus the five operator-management calls (create, change role, disable, enable, reset password). `src/api/api-surface.test.ts` fails if a new write appears.

## Layout

```text
src/
  api/          client.ts (the only fetch), auth.ts, operators.ts, types.ts
  auth/         session.ts (zustand store), flows.ts (sign in/out, change password, refresh),
                guards.tsx, roles.ts (the single role matrix)
  components/   Layout, SessionCountdown, RoleBadge, AuthCard, ProviderBrand, OperatorDialogs,
                TemporaryPasswordDialog; ui/ (primitives: copied from admin/, plus table and select)
  pages/        Login, ChangePassword, Home, ProviderUsers, Forbidden, NotFound
  lib/          formatting, password-policy hints, operator error messages
  test/         setup, fetch stub, render helpers, source guards
```

**Adding a page:**
1. Add an entry to `SECTIONS` in `src/auth/roles.ts`; the nav picks it up.
2. Add its route in `src/App.tsx` under `RequireSession`, wrapped in `<RequireRole roles={...}>`.

## Tests

- Vitest + Testing Library on jsdom, with `fetch` stubbed (`src/test/fetch.ts`).
- Beyond the behaviour tests, two guard tests protect the invariants:
  - **`src/test/static-guards.test.ts`** checks that only the API client calls `fetch`, nothing uses `localStorage` or the console, only the session store touches `sessionStorage`, only the temporary-password dialog uses the clipboard, and there are no tenant endpoints.
  - **`api-surface.test.ts`** checks the allowed writes.
- `vitest.config.ts` runs the test workers with `--no-experimental-webstorage`. Without it, Node's own `localStorage` global would hide jsdom's.
- `src/test/setup.ts` stubs the pointer-capture and `scrollIntoView` APIs that jsdom lacks and Radix Select calls (tests only).
- jsdom doesn't do layout, so narrow-screen behaviour is checked in a real browser: no page-level horizontal scroll at 375, 640, 768 and 1024px.
