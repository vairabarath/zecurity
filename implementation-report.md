# ZECURITY — Implementation Report
## Full Workflow, Logic & Security Verification

**Date:** 2026-04-07
**Status:** All core systems operational. End-to-end flow verified with live data.

---

## Current State Summary

Everything described in `fullplan.md`, the four member plans, and `member1-updated-plan.md` is implemented and working. Two bugs were found and fixed during integration testing:

1. **Callback redirect loop** — `callback.go` redirected to `/auth/callback#token=JWT` as a relative path, which hit the same Go handler again (missing `code`/`state` on second hit caused `missing_params`). Fixed by redirecting to `ALLOWED_ORIGIN + "/auth/callback#token=..."` so the browser goes to the React app at `:5173`, not back to Go at `:8080`.

2. **Enum case mismatch** — Database stores `role = 'admin'` and `status = 'active'` (lowercase), but GraphQL enums are `ADMIN`/`ACTIVE` (uppercase). The `me` query resolver did `graph.Role("admin").IsValid()` which returned `false`, causing `session_failed`. Fixed by adding `strings.ToUpper()` in both the Role and Status resolvers before enum validation.

---

## Live Database State (Verified)

```
WORKSPACES
--------------------------------------------------------------
id: acbdfaf6-57f7-418b-a78d-ebfea9bfe115
slug: barath | name: Barath | status: active
created: 2026-04-07 11:44:38

id: c8281e1c-dd9f-450d-8151-b733039b6ed7
slug: zero | name: zero | status: active
created: 2026-04-07 11:58:34

USERS
--------------------------------------------------------------
momsfaithbarath@gmail.com  | workspace: barath  | role: admin | active
  last_login: 2026-04-07 11:57:34 (returned and logged in again)

bairavasanthoshs@gmail.com | workspace: zero    | role: admin | active
  last_login: NULL (first login only, hasn't returned yet)

PKI HIERARCHY
--------------------------------------------------------------
Root CA:         1 row | 10-year validity (2026-2036) | EC P-384
Intermediate CA: 1 row |  5-year validity (2026-2031) | EC P-384
Workspace CAs:   2 rows (one per workspace) | 2-year validity | EC P-384
```

---

## Complete Authentication Flow (As Implemented)

### New User — Signup Flow

```
Browser                     React (:5173)                  Go (:8080)                    Google                    Redis                  PostgreSQL
   |                            |                              |                            |                        |                        |
   |  GET /signup               |                              |                            |                        |                        |
   |--------------------------->|                              |                            |                        |                        |
   |  Step1Email renders        |                              |                            |                        |                        |
   |  User enters email +       |                              |                            |                        |                        |
   |  selects Home/Office       |                              |                            |                        |                        |
   |                            |                              |                            |                        |                        |
   |  Click Continue            |                              |                            |                        |                        |
   |--------------------------->|                              |                            |                        |                        |
   |                            | store email + accountType    |                            |                        |                        |
   |                            | in Zustand signup store      |                            |                        |                        |
   |                            | navigate /signup/workspace   |                            |                        |                        |
   |                            |                              |                            |                        |                        |
   |  Step2Workspace renders    |                              |                            |                        |                        |
   |  Auto-suggest workspace    |                              |                            |                        |                        |
   |  name from email domain    |                              |                            |                        |                        |
   |  (acme.com -> "Acme")      |                              |                            |                        |                        |
   |  Live slug preview updates |                              |                            |                        |                        |
   |  on every keystroke        |                              |                            |                        |                        |
   |                            |                              |                            |                        |                        |
   |  Click Continue            |                              |                            |                        |                        |
   |--------------------------->|                              |                            |                        |                        |
   |                            | store workspaceName in       |                            |                        |                        |
   |                            | signup store                 |                            |                        |                        |
   |                            | navigate /signup/auth        |                            |                        |                        |
   |                            |                              |                            |                        |                        |
   |  Step3Auth renders         |                              |                            |                        |                        |
   |  Shows summary card:       |                              |                            |                        |                        |
   |  email + workspace name    |                              |                            |                        |                        |
   |                            |                              |                            |                        |                        |
   |  Click "Sign in with       |                              |                            |                        |                        |
   |   Google"                  |                              |                            |                        |                        |
   |--------------------------->|                              |                            |                        |                        |
   |                            | POST /graphql                |                            |                        |                        |
   |                            | mutation InitiateAuth(       |                            |                        |                        |
   |                            |   provider: "google",        |                            |                        |                        |
   |                            |   workspaceName: "Acme"      |                            |                        |                        |
   |                            | )                            |                            |                        |                        |
   |                            | X-Public-Operation:          |                            |                        |                        |
   |                            |   initiateAuth               |                            |                        |                        |
   |                            |----------------------------- >|                            |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            |                              | 1. Generate PKCE pair:     |                        |                        |
   |                            |                              |    code_verifier = 64      |                        |                        |
   |                            |                              |      random bytes,         |                        |                        |
   |                            |                              |      base64url (86 chars)  |                        |                        |
   |                            |                              |    code_challenge =        |                        |                        |
   |                            |                              |      SHA256(code_verifier),|                        |                        |
   |                            |                              |      base64url             |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            |                              | 2. Generate signed state:  |                        |                        |
   |                            |                              |    nonce = 32 random bytes |                        |                        |
   |                            |                              |    sig = HMAC-SHA256(      |                        |                        |
   |                            |                              |      nonce, JWT_SECRET)    |                        |                        |
   |                            |                              |    state = nonce.sig       |                        |                        |
   |                            |                              |      (base64url)           |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            |                              | 3. Store in Redis -------->| SET pkce:<state>       |                        |
   |                            |                              |                            | { code_verifier,       |                        |
   |                            |                              |                            |   workspaceName }      |                        |
   |                            |                              |                            | TTL = 5 minutes        |                        |
   |                            |                              |                            |                        |                        |
   |                            |                              | 4. Build Google OAuth URL  |                        |                        |
   |                            |                              |    with code_challenge +   |                        |                        |
   |                            |                              |    state                   |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            | { redirectUrl, state }       |                            |                        |                        |
   |                            |<-----------------------------|                            |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            | sessionStorage.setItem(      |                            |                        |                        |
   |                            |   'oauth_state', state)      |                            |                        |                        |
   |                            | signupStore.reset()          |                            |                        |                        |
   |                            | window.location.href =       |                            |                        |                        |
   |                            |   redirectUrl                |                            |                        |                        |
   |                            |                              |                            |                        |                        |
   |  Full browser redirect --->|----------------------------------------------------->    |                        |                        |
   |                            |                              |                            |                        |                        |
   |  Google login screen       |                              |                            |                        |                        |
   |  User authenticates        |                              |                            |                        |                        |
   |                            |                              |                            |                        |                        |
   |  Google redirects:         |                              |                            |                        |                        |
   |  GET /auth/callback?code=X&state=Y -------------------->  |                            |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            |                              | === CALLBACK HANDLER ===   |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            |                              | Step 1: Read code + state  |                        |                        |
   |                            |                              |   from URL query params    |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            |                              | Step 2: Verify state HMAC  |                        |                        |
   |                            |                              |   split nonce.sig          |                        |                        |
   |                            |                              |   recompute HMAC(nonce,    |                        |                        |
   |                            |                              |     JWT_SECRET)            |                        |                        |
   |                            |                              |   constant-time compare    |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            |                              | Step 3: Retrieve + delete  |                        |                        |
   |                            |                              |   PKCE state from Redis -->| Pipeline:              |                        |
   |                            |                              |                            | GET pkce:<state>       |                        |
   |                            |                              |                            | DEL pkce:<state>       |                        |
   |                            |                              |                            | (atomic, single-use)   |                        |
   |                            |                              | <-- code_verifier +        |                        |                        |
   |                            |                              |     workspaceName           |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            |                              | Step 4: Exchange code ---->|                        |                        |
   |                            |                              |   POST googleapis.com/     |                        |                        |
   |                            |                              |     token                  |                        |                        |
   |                            |                              |   { code, code_verifier,   |                        |                        |
   |                            |                              |     client_id,             |                        |                        |
   |                            |                              |     client_secret,         |                        |                        |
   |                            |                              |     redirect_uri }         |                        |                        |
   |                            |                              | <-- { id_token,            |                        |                        |
   |                            |                              |       access_token }       |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            |                              | Step 5: Verify id_token    |                        |                        |
   |                            |                              |   a) Fetch Google JWKS     |                        |                        |
   |                            |                              |   b) Verify RS256 sig      |                        |                        |
   |                            |                              |   c) aud == CLIENT_ID      |                        |                        |
   |                            |                              |   d) iss == accounts.       |                        |                        |
   |                            |                              |        google.com          |                        |                        |
   |                            |                              |   e) exp > now             |                        |                        |
   |                            |                              |   f) email_verified == true |                        |                        |
   |                            |                              |   g) sub non-empty         |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            |                              | Step 6: Extract identity   |                        |                        |
   |                            |                              |   email, sub, name         |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            |                              | Step 7: Bootstrap -------->|                        |----------------------->|
   |                            |                              |   (direct Go function      |                        | BEGIN TRANSACTION      |
   |                            |                              |    call, no network)       |                        |                        |
   |                            |                              |                            |                        | Check returning user:  |
   |                            |                              |                            |                        |   SELECT FROM users    |
   |                            |                              |                            |                        |   WHERE provider_sub   |
   |                            |                              |                            |                        |                        |
   |                            |                              |                            |                        | NOT FOUND (new user):  |
   |                            |                              |                            |                        |                        |
   |                            |                              |                            |                        | INSERT workspace       |
   |                            |                              |                            |                        |   status='provisioning'|
   |                            |                              |                            |                        |   -> tenant_id (UUID)  |
   |                            |                              |                            |                        |                        |
   |                            |                              |                            |                        | INSERT user            |
   |                            |                              |                            |                        |   role='admin'         |
   |                            |                              |                            |                        |   -> user_id (UUID)    |
   |                            |                              |                            |                        |                        |
   |                            |                              |                            |                        | Generate WorkspaceCA:  |
   |                            |                              |                            |                        |   EC P-384 keypair     |
   |                            |                              |                            |                        |   CSR with SAN:        |
   |                            |                              |                            |                        |     URI:tenant:<id>    |
   |                            |                              |                            |                        |   Sign with            |
   |                            |                              |                            |                        |     Intermediate CA    |
   |                            |                              |                            |                        |   Encrypt private key: |
   |                            |                              |                            |                        |     HKDF(master,       |
   |                            |                              |                            |                        |       tenantID)        |
   |                            |                              |                            |                        |     -> AES-256-GCM     |
   |                            |                              |                            |                        |   Zero key from memory |
   |                            |                              |                            |                        |                        |
   |                            |                              |                            |                        | INSERT workspace_ca_   |
   |                            |                              |                            |                        |   keys (encrypted)     |
   |                            |                              |                            |                        |                        |
   |                            |                              |                            |                        | UPDATE workspace       |
   |                            |                              |                            |                        |   status='active'      |
   |                            |                              |                            |                        |   ca_cert_pem=<cert>   |
   |                            |                              |                            |                        |                        |
   |                            |                              |                            |                        | COMMIT                 |
   |                            |                              | <-- { tenant_id,           |                        |                        |
   |                            |                              |       user_id, role }      |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            |                              | Step 8: Issue access JWT   |                        |                        |
   |                            |                              |   alg: HS256               |                        |                        |
   |                            |                              |   claims: {                |                        |                        |
   |                            |                              |     sub: user_id,          |                        |                        |
   |                            |                              |     tenant_id: tenant_id,  |                        |                        |
   |                            |                              |     role: "admin",         |                        |                        |
   |                            |                              |     iss: "zecurity-        |                        |                        |
   |                            |                              |       controller",         |                        |                        |
   |                            |                              |     exp: now + 15min,      |                        |                        |
   |                            |                              |     iat: now               |                        |                        |
   |                            |                              |   }                        |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            |                              | Step 9: Issue refresh token |                       |                        |
   |                            |                              |   random 256-bit value ---> | SET refresh:<user_id> |                        |
   |                            |                              |                             | TTL = 7 days          |                        |
   |                            |                              |   Set-Cookie:               |                       |                        |
   |                            |                              |     refresh_token=<token>   |                       |                        |
   |                            |                              |     Path=/auth/refresh      |                       |                        |
   |                            |                              |     HttpOnly=true           |                       |                        |
   |                            |                              |     SameSite=Strict         |                       |                        |
   |                            |                              |     Secure=true             |                       |                        |
   |                            |                              |                            |                        |                        |
   |                            |                              | Step 10: Redirect          |                        |                        |
   |  302 Location:             |                              |                            |                        |                        |
   |  http://localhost:5173/    |                              |                            |                        |                        |
   |    auth/callback#token=JWT |                              |                            |                        |                        |
   |<----------------------------------------------------------|                            |                        |                        |
   |                            |                              |                            |                        |                        |
   |  GET /auth/callback        |                              |                            |                        |                        |
   |  (hash NOT sent to server) |                              |                            |                        |                        |
   |--------------------------->|                              |                            |                        |                        |
   |                            | AuthCallback.tsx:            |                            |                        |                        |
   |                            | 1. Read window.location.hash |                            |                        |                        |
   |                            |    extract JWT after #token= |                            |                        |                        |
   |                            | 2. replaceState to clear     |                            |                        |                        |
   |                            |    hash from URL             |                            |                        |                        |
   |                            | 3. Store JWT in Zustand      |                            |                        |                        |
   |                            |    (memory only)             |                            |                        |                        |
   |                            | 4. apolloClient.query(Me)    |                            |                        |                        |
   |                            |    Authorization: Bearer JWT |                            |                        |                        |
   |                            |---------------------------->  |                            |                        |                        |
   |                            |                              | AuthMiddleware:             |                        |                        |
   |                            |                              |   Verify JWT (HS256,       |                        |                        |
   |                            |                              |     iss, exp)              |                        |                        |
   |                            |                              |   Extract: sub, tenant_id, |                        |                        |
   |                            |                              |     role                   |                        |                        |
   |                            |                              |   Inject TenantContext     |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            |                              | WorkspaceGuard:            |                        |                        |
   |                            |                              |   SELECT status            |                        |----------------------->|
   |                            |                              |   FROM workspaces          |                        |                        |
   |                            |                              |   WHERE id = tenant_id     |                        |                        |
   |                            |                              |   status == 'active' -> OK |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            |                              | Me Resolver:               |                        |                        |
   |                            |                              |   SELECT FROM users ------>|                        |----------------------->|
   |                            |                              |   WHERE id = user_id       |                        |                        |
   |                            |                              |     AND tenant_id          |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            | <-- { id, email, role,       |                            |                        |                        |
   |                            |       provider, createdAt }  |                            |                        |                        |
   |                            |                              |                            |                        |                        |
   |                            | 5. setUser(result)           |                            |                        |                        |
   |                            | 6. navigate('/dashboard')    |                            |                        |                        |
   |                            |                              |                            |                        |                        |
   |  Dashboard renders         |                              |                            |                        |                        |
   |  User info + workspace     |                              |                            |                        |                        |
```

### Returning User — Login Flow

```
Browser -> /login -> "Sign in with Google" button
  -> initiateAuth(provider: "google")          [no workspaceName]
  -> PKCE + state generated, stored in Redis
  -> Redirect to Google
  -> Google authenticates
  -> GET /auth/callback?code=X&state=Y
  -> Steps 1-6 same as above
  -> Step 7: Bootstrap finds existing user by provider_sub
     -> UPDATE last_login_at = NOW()
     -> Return existing { tenant_id, user_id, role }
  -> Steps 8-10 same (issue JWT, refresh cookie, redirect)
  -> AuthCallback reads token, loads user, navigates to /dashboard
```

### Page Reload — Silent Refresh

```
Browser reloads any protected route
  -> useRequireAuth() fires
  -> No accessToken in Zustand (memory cleared on reload)
  -> POST /auth/refresh
     credentials: 'include' (sends httpOnly cookie automatically)
  -> Go reads refresh_token from cookie
  -> Looks up in Redis: refresh:<user_id>
  -> Validates match
  -> Issues new access JWT
  -> Returns { access_token: "<new JWT>" }
  -> React stores in Zustand
  -> isReady = true, page renders

If cookie expired or invalid:
  -> 401 response
  -> navigate('/login')
```

### Token Refresh on 401 (Mid-Session)

```
Any GraphQL query returns 401 UNAUTHORIZED
  -> Apollo errorLink intercepts
  -> Check isRefreshing flag (prevent concurrent calls)
  -> POST /auth/refresh with httpOnly cookie
  -> Receive new JWT
  -> Update Zustand store
  -> Retry the original failed GraphQL operation
  -> If refresh also fails: clearAuth() + redirect to /login
```

---

## PKI Certificate Chain (As Implemented)

```
Root CA (self-signed)
  Algorithm:  EC P-384
  Validity:   10 years (2026-04-07 to 2036-04-07)
  Storage:    Encrypted with AES-256-GCM
              Key derived via HKDF(master_secret, context="root-ca")
              Stored in ca_root table
  Memory:     Private key zeroed after Intermediate CA signing
              Never loaded again
  |
  +-- Intermediate CA (signed by Root CA)
        Algorithm:  EC P-384
        Validity:   5 years (2026-04-07 to 2031-04-07)
        Storage:    Encrypted with AES-256-GCM
                    Key derived via HKDF(master_secret, context="intermediate-ca")
                    Stored in ca_intermediate table
        Memory:     Private key kept in memory for WorkspaceCA signing
        |
        +-- WorkspaceCA-acbdfaf6 (workspace: "Barath")
        |     Algorithm:  EC P-384
        |     Validity:   2 years (2026-04-07 to 2028-04-07)
        |     SAN:        URI:tenant:acbdfaf6-57f7-418b-a78d-ebfea9bfe115
        |     Storage:    Encrypted with AES-256-GCM
        |                 Key derived via HKDF(master_secret, context=tenantID)
        |                 Stored in workspace_ca_keys table
        |     Memory:     Private key zeroed after encryption
        |
        +-- WorkspaceCA-c8281e1c (workspace: "zero")
              Algorithm:  EC P-384
              Validity:   2 years (2026-04-07 to 2028-04-07)
              SAN:        URI:tenant:c8281e1c-dd9f-450d-8151-b733039b6ed7
              Storage:    Same as above
              Memory:     Private key zeroed after encryption
```

---

## Security Measures — Full Audit

### Authentication Security

| Measure | Requirement | Implementation | Status |
|---|---|---|---|
| PKCE code_verifier | 64 random bytes, base64url | `rand.Read(64)` -> 86-char string | PASS |
| PKCE code_challenge | SHA256, S256 method | `sha256.Sum256(code_verifier)` | PASS |
| PKCE single-use | Atomic GET+DEL | Redis pipeline `GET` + `DEL` in one call | PASS |
| PKCE TTL | 5 minutes | `SET ... 5*time.Minute` | PASS |
| State CSRF protection | HMAC-SHA256 signed | `nonce.HMAC(nonce, JWT_SECRET)` | PASS |
| State verification | Constant-time compare | `hmac.Equal()` in `verifySignedState()` | PASS |
| Token exchange | Server-to-server | `client_secret` never exposed to browser | PASS |

### ID Token Verification

| Check | Requirement | Implementation | Status |
|---|---|---|---|
| Signature | Against Google JWKS | Fetch JWKS, verify RS256 with matching `kid` | PASS |
| Audience | aud == CLIENT_ID | `containsString(aud, clientID)` | PASS |
| Issuer | iss == accounts.google.com | Accepts both with and without `https://` | PASS |
| Expiry | exp > now | `jwt.WithExpirationRequired()` | PASS |
| Email verified | email_verified == true | `if !claims.EmailVerified` -> error | PASS |
| Subject | sub non-empty | `if claims.Sub == ""` -> error | PASS |

### JWT Security

| Measure | Requirement | Implementation | Status |
|---|---|---|---|
| Algorithm | HS256 | `jwt.SigningMethodHS256` | PASS |
| Claims | sub, tenant_id, role, iss, iat, exp | All present in `issueAccessToken()` | PASS |
| Access TTL | 15 minutes | Default `15m`, configurable via `JWTAccessTTL` | PASS |
| Refresh TTL | 7 days | `168h` via `JWTRefreshTTL` | PASS |
| Delivery | Hash fragment only | `#token=JWT` — never sent to server | PASS |
| Frontend storage | Memory only | Zustand store, zero localStorage usage | PASS |
| Hash clearing | replaceState after read | `window.history.replaceState()` | PASS |

### Cookie Security

| Measure | Requirement | Implementation | Status |
|---|---|---|---|
| HttpOnly | true | `HttpOnly: true` — XSS cannot read it | PASS |
| SameSite | Strict | `SameSite: http.SameSiteStrictMode` — CSRF protection | PASS |
| Secure | true | `Secure: true` — HTTPS only | PASS |
| Path | /auth/refresh | Cookie only sent to refresh endpoint | PASS |

### Middleware Security

| Measure | Requirement | Implementation | Status |
|---|---|---|---|
| alg=none prevention | Enforce signing method | Rejects non-HMAC methods | PASS |
| Issuer validation | iss == "zecurity-controller" | `jwt.WithIssuer(appmeta.ControllerIssuer)` | PASS |
| Expiry enforcement | Required | `jwt.WithExpirationRequired()` | PASS |
| Claims completeness | sub, tenant_id, role non-empty | Explicit check before injecting context | PASS |
| Workspace status | Must be 'active' | `SELECT status` + check != 'active' -> 403 | PASS |
| TenantDB enforcer | Panic on missing context | `panic("TenantContext not in context")` | PASS |
| Public route bypass | initiateAuth, /auth/callback, /health | `X-Public-Operation` header + route registration | PASS |

### PKI Security

| Measure | Requirement | Implementation | Status |
|---|---|---|---|
| EC curve | P-384 for all CAs | `elliptic.P384()` everywhere | PASS |
| Key derivation | HKDF with SHA-256 | `hkdf.Key(sha256.New, ...)` | PASS |
| Encryption | AES-256-GCM | 32-byte derived key, random nonce | PASS |
| Root CA validity | 10 years | `certValidity(10)` | PASS |
| Intermediate validity | 5 years | `certValidity(5)` | PASS |
| WorkspaceCA validity | 2 years | `certValidity(2)` | PASS |
| WorkspaceCA SAN | URI:tenant:\<tenantID\> | `URIs: []*url.URL{tenantURI}` | PASS |
| Root key zeroed | After Intermediate signing | `privKey.D.SetInt64(0)` | PASS |
| WorkspaceCA key zeroed | After encryption | `defer privKey.D.SetInt64(0)` | PASS |
| Bootstrap atomicity | Single transaction | `BEGIN` ... all steps ... `COMMIT` / `ROLLBACK` | PASS |

### Frontend Security

| Measure | Requirement | Implementation | Status |
|---|---|---|---|
| JWT in memory only | No localStorage | Zustand store, never persisted | PASS |
| OAuth state | sessionStorage only | `sessionStorage.setItem('oauth_state', ...)` | PASS |
| Concurrent refresh guard | isRefreshing flag | Prevents duplicate POST /auth/refresh | PASS |
| httpLink credentials | same-origin | Sends cookies to same-origin endpoints | PASS |
| Refresh credentials | include | Sends httpOnly cookie on refresh call | PASS |
| Public operation marker | X-Public-Operation header | Bypasses auth middleware for initiateAuth | PASS |

---

## Tenant Isolation Guarantees

Five layers of isolation ensure no cross-tenant data access:

1. **Cryptographic chain** — Each WorkspaceCA cert chains to Intermediate -> Root. Certificate verification confirms the chain is intact.

2. **SAN validation** — Every WorkspaceCA cert contains `URI:tenant:<tenantID>`. Future device/connector certs will be verified against this SAN to ensure they belong to the correct workspace.

3. **JWT scoping** — Every access token contains `tenant_id`. The middleware extracts it and injects it into the request context as `TenantContext`.

4. **DB scoping** — Every query on the `users` table includes `AND tenant_id = $X`. The `workspaces` table uses the workspace UUID as its primary key, which IS the tenant_id.

5. **TenantDB enforcer** — The `TenantDB` wrapper calls `tenant.MustGet(ctx)` before every query. If `TenantContext` is missing (meaning auth middleware was somehow bypassed), it panics. This is a hard programming error, not a runtime condition.

---

## Database Schema

```
ca_root              — Root CA (1 row, created once at startup)
ca_intermediate      — Intermediate CA (1 row, created once at startup)
workspaces           — Tenant root (UUID = tenant_id)
                       status CHECK: provisioning | active | suspended | deleted
users                — Users per workspace
                       role CHECK: admin | member | viewer
                       status CHECK: active | suspended | deleted
                       UNIQUE(tenant_id, provider_sub)
workspace_ca_keys    — Encrypted WorkspaceCA private keys
                       UNIQUE(tenant_id) — one per workspace
```

---

## File Ownership

```
Member 1 — Frontend (admin/)
  src/App.tsx                           Routes (public + signup + protected)
  src/store/auth.ts                     Zustand auth store (JWT in memory)
  src/store/signup.ts                   Zustand signup wizard state
  src/apollo/client.ts                  Apollo Client (errorLink -> authLink -> httpLink)
  src/apollo/links/auth.ts              Bearer token + X-Public-Operation header
  src/apollo/links/error.ts             401 -> refresh -> retry
  src/hooks/useRequireAuth.ts           Auth guard with silent refresh
  src/pages/Login.tsx                   "Sign in with Google" + link to /signup
  src/pages/AuthCallback.tsx            Read JWT from hash, load user, -> dashboard
  src/pages/Dashboard.tsx               me + workspace queries
  src/pages/Settings.tsx                Workspace info
  src/pages/signup/Step1Email.tsx       Email + Home/Office selection
  src/pages/signup/Step2Workspace.tsx   Workspace name + live slug preview
  src/pages/signup/Step3Auth.tsx        Summary + "Sign in with Google" with workspaceName
  src/components/layout/AppShell.tsx    Sidebar + Header + Outlet
  src/graphql/mutations.graphql         InitiateAuth(provider, workspaceName?)
  src/graphql/queries.graphql           Me, GetWorkspace

Member 2 — Auth + Session (controller/internal/auth/)
  config.go                             Service constructor + Config struct
  oidc.go                               PKCE generation, Google OAuth URL, state signing
  idtoken.go                            Google id_token verification (6 checks)
  session.go                            JWT signing (HS256) + refresh token generation
  callback.go                           /auth/callback handler (10-step flow)
  refresh.go                            /auth/refresh handler
  redis.go                              PKCE state + refresh token storage
  exchange.go                           Google token exchange (server-to-server)
  service.go                            Service interface definition

Member 3 — Bootstrap + PKI (controller/internal/bootstrap/ + pki/)
  bootstrap/bootstrap.go                Atomic workspace + user + CA transaction
  pki/service.go                        PKI service init (master secret, CA startup)
  pki/crypto.go                         EC P-384 keygen, HKDF, AES-256-GCM, cert helpers
  pki/root.go                           Root CA init + encrypted storage
  pki/intermediate.go                   Intermediate CA init + in-memory state
  pki/workspace.go                      WorkspaceCA generation per tenant

Member 4 — Schema + DB + Middleware (controller/)
  graph/schema.graphqls                 THE contract (GraphQL schema)
  graph/resolvers/schema.resolvers.go   me, workspace, initiateAuth resolvers
  graph/resolver.go                     Base resolver struct (TenantDB + AuthService)
  internal/db/pool.go                   pgx connection pool
  internal/db/tenant.go                 TenantDB wrapper (enforcer)
  internal/tenant/context.go            TenantContext struct + ctx keys + MustGet
  internal/middleware/auth.go           JWT verify -> TenantContext inject
  internal/middleware/workspace.go      Workspace status guard
  internal/models/user.go               User model
  internal/models/workspace.go          Workspace model
  migrations/001_schema.sql             All 5 tables
  cmd/server/main.go                    Wiring + HTTP server
  docker-compose.yml                    PostgreSQL + Redis
```

---

## Environment Variables

```
DATABASE_URL=postgres://ztna:ztna_dev_secret@localhost:5432/ztna_platform
REDIS_URL=redis://localhost:6379
PORT=8080
ENV=development
JWT_SECRET=<32+ random bytes>
JWT_ISSUER=zecurity-controller
GOOGLE_CLIENT_ID=<from Google Console>
GOOGLE_CLIENT_SECRET=<from Google Console>
GOOGLE_REDIRECT_URI=http://localhost:8080/auth/callback
PKI_MASTER_SECRET=<64+ random bytes>
ALLOWED_ORIGIN=http://localhost:5173
```

---

## Bugs Found & Fixed During Integration

### Bug 1: Callback Redirect Loop
- **File:** `controller/internal/auth/callback.go`
- **Symptom:** `http://localhost:8080/login?error=missing_params#token=eyJ...` + 404
- **Root cause:** Line 155 redirected to `/auth/callback#token=JWT` (relative path). Since Google's redirect URI points to `:8080`, the browser was already on the Go server. The relative redirect hit the same Go callback handler again, but without `code`/`state` query params, triggering `missing_params`. The `#token=` fragment survived the redirect chain. Then `/login` on `:8080` returned 404 (no such Go route).
- **Fix:** Redirect to `s.cfg.AllowedOrigin + "/auth/callback#token="` (absolute URL to React at `:5173`). Same fix applied to the `fail()` function for error redirects.

### Bug 2: Enum Case Mismatch
- **File:** `controller/graph/resolvers/schema.resolvers.go`
- **Symptom:** `/login?error=session_failed` after successful OAuth
- **Root cause:** Bootstrap inserts `role = 'admin'` (lowercase) into PostgreSQL. GraphQL enum values are `ADMIN` (uppercase). The Role resolver did `graph.Role("admin").IsValid()` which checked against `"ADMIN"` — case-sensitive mismatch returned `false`, causing the `me` query to error. AuthCallback caught the error and redirected to login with `session_failed`.
- **Fix:** Added `strings.ToUpper()` before enum validation in both the Role and Status resolvers.

---

## What Is NOT Built Yet (Future Sprints)

- gRPC service for Linux client communication
- Device/client certificate issuance (signed by WorkspaceCA)
- User invitation flow (admin invites members)
- Policy engine (access rules)
- **Sprint 4: Traffic proxying (WireGuard / tun)** — next sprint
- Workspace management (rename, suspend, delete)
- Multi-workspace support for same Google account

## What Was Added Since This Report (Sprint 5 — Resource Protection)

Sprint 5 implements the core functionality for protecting resources on a shield host. This includes a new `shield` agent, updates to the `controller` and `connector`, a new UI in the `admin` frontend, and new protobuf definitions.

**Controller:**
- **Resource Management:** A new `resource` package (`internal/resource`) was added to manage the lifecycle of resources. This includes a `store.go` with functions to create, read, and update resources in the database.
- **GraphQL API:** The GraphQL API was extended with a new `resource.graphqls` schema, defining the `Resource` type and mutations for creating, protecting, unprotecting, and deleting resources. The corresponding resolvers were implemented in `resource.resolvers.go`.
- **Heartbeat:** The `heartbeat.go` logic was updated to piggyback resource instructions onto the existing heartbeat mechanism, sending instructions to the `connector` and processing acknowledgements back from it.

**Connector:**
- **Heartbeat Relay:** The connector's `heartbeat.rs` and `agent_server.rs` were modified to relay resource instructions from the controller to the shield and to forward acknowledgements from the shield back to the controller.

**Shield:**
- **New Agent:** A new Rust-based `shield` agent was created. This agent is responsible for managing resource protection on the host.
- **Resource Protection:** The `resources.rs` module contains the core logic for applying and removing `nftables` rules to protect resources. It also includes a health checking mechanism to monitor the status of protected services.
- **Heartbeat:** The shield's `heartbeat.rs` communicates with the connector, receiving resource instructions and sending back acknowledgements.

**Admin Frontend:**
- **Resources Page:** A new `Resources.tsx` page was added to the admin UI, allowing users to view and manage protected resources.
- **Create Resource Modal:** A `CreateResourceModal.tsx` component was created to provide a user-friendly way to define new resources.
- **GraphQL:** New queries and mutations were added to `queries.graphql` and `mutations.graphql` to interact with the new resource management backend.

**Proto:**
- **shield.v1:** A new `shield.v1` protobuf package was created with `ResourceInstruction` and `ResourceAck` messages.
- **connector.v1:** The `connector.v1` protobuf was updated to include `shield_resources` in the `HeartbeatResponse` and `resource_acks` in the `HeartbeatRequest`.

## What Was Added Since This Report (Sprint 3 — Cert Renewal)

**Controller:**
- `RenewCert` gRPC RPC handler added to `enrollment.go` (mTLS, proof-of-possession CSR)
- `heartbeat.go` — sets `re_enroll=true` when cert expires within `CONNECTOR_RENEWAL_WINDOW`
- `pki/workspace.go` — `RenewConnectorCert()` method (signs new cert from existing public key)
- `config.go` — `RenewalWindow` field, `CONNECTOR_RENEWAL_WINDOW` env var (default 48h)

**Connector:**
- `src/renewal.rs` — NEW: reads key, extracts public key DER, calls RenewCert RPC, saves cert, rebuilds mTLS channel
- `src/heartbeat.rs` — triggers `renewal::renew_cert()` when `re_enroll=true`
- `src/crypto.rs` — added `extract_public_key_der()` + `parse_cert_not_after()`

**Proto (`proto/connector/v1/connector.proto`):**
- Added `RenewCert` RPC + `RenewCertRequest` / `RenewCertResponse` messages
- Moved to repo root (was `controller/proto/connector/v1/`)

**CI:**
- `cross build --manifest-path connector/Cargo.toml` — runs from repo root so Docker container can access `proto/`
- Released as `connector-v0.3.0`
