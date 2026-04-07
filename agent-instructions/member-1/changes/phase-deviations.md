# Member 1 — Implementation Changes Log

This document tracks all deviations from the original plan documents in `agent-instructions/member-1/`. Every change is recorded here so future phases can follow the adapted APIs.

---

## Phase 1 — Project Setup

### 1.1 shadcn/ui CLI init skipped (interactive CLI non-functional)

**Plan:** Run `npx shadcn@latest init` interactively with answers (Style: Default, Base color: Slate, CSS variables: Yes), then `npx shadcn@latest add button card badge avatar dropdown-menu separator skeleton toast alert`.

**Reality:** The shadcn CLI v4.1.2 was fully interactive and did not accept piped input or `--style` flags. It hung or errored.

**What was done instead:**
- Created `components.json` manually with the correct config (style: default, baseColor: slate, cssVariables: true).
- Manually wrote all shadcn/ui component files:
  - `src/components/ui/button.tsx`
  - `src/components/ui/card.tsx`
  - `src/components/ui/badge.tsx`
  - `src/components/ui/avatar.tsx`
  - `src/components/ui/separator.tsx`
  - `src/components/ui/dropdown-menu.tsx`
  - `src/components/ui/skeleton.tsx`
  - `src/components/ui/toast.tsx`
  - `src/components/ui/toaster.tsx`
  - `src/components/ui/alert.tsx`
- Manually created `src/components/ui/use-toast.ts` (the toast hook that shadcn normally generates).

**Files affected:**
- `admin/components.json` — created manually
- `admin/src/components/ui/*.tsx` — all 10 components written by hand
- `admin/src/components/ui/use-toast.ts` — toast hook written by hand

### 1.2 `src/index.css` — replaced default Vite CSS with shadcn/ui CSS variables

**Plan:** shadcn/ui CLI init would have written CSS variables and Tailwind directives into `src/index.css` automatically.

**Reality:** Since CLI init was skipped, the file still contained default Vite template styles (purple theme, centered layout, heading styles).

**What was done instead:**
- Replaced the entire file with proper shadcn/ui `@layer base` CSS variables (`--background`, `--foreground`, `--primary`, `--card`, `--destructive`, `--muted`, `--accent`, `--border`, `--ring`, etc.) plus `@import "tailwindcss"`.

**Files affected:**
- `admin/src/index.css` — overwritten

### 1.3 `@types/node` installed for `path` in `vite.config.ts`

**Plan:** Not explicitly mentioned, but `vite.config.ts` uses `import path from 'path'`.

**Reality:** TypeScript needed `@types/node` for the `path` module to resolve.

**What was done:** `@types/node` was already installed as a devDependency by the Vite scaffold. No extra action needed.

**Files affected:** None (dependency already present).

### 1.4 `tsconfig.app.json` — added `baseUrl` and `paths` for `@` alias

**Plan:** The plan specifies `'@': path.resolve(__dirname, './src')` in Vite alias and `@/` imports throughout all code.

**Reality:** Vite scaffold's `tsconfig.app.json` did not include `baseUrl` or `paths`, so TypeScript would not resolve `@/` imports.

**What was done:** Added `"baseUrl": "."` and `"paths": { "@/*": ["./src/*"] }` to `compilerOptions`.

**Files affected:**
- `admin/tsconfig.app.json`

### 1.5 codegen output path: file → directory

**Plan:** `codegen.yml` specified `generates: src/generated/graphql.ts` with `preset: client`.

**Reality:** The `client` preset in `@graphql-codegen/client-preset` v5 requires a **directory** path (must end with `/`), not a file path. It errored with: *"target output should be a directory, ex: 'src/gql/'"*.

**What was done:** Changed to `generates: src/generated/` (directory). Codegen now produces 4 files:
- `src/generated/graphql.ts` — types, documents, enums
- `src/generated/gql.ts` — graphql() helper
- `src/generated/fragment-masking.ts` — fragment masking utilities
- `src/generated/index.ts` — barrel re-export

**Files affected:**
- `admin/codegen.yml` — output path changed from `src/generated/graphql.ts` to `src/generated/`

---

## Phase 3 — Apollo Client + Links

### 3.1 Apollo Client v4 — breaking API changes throughout

**Plan:** Written for Apollo Client v3 API (`@apollo/client` v3.x).

**Reality:** `@apollo/client` v4.1.6 was installed. Multiple v3 APIs were removed or renamed.

All changes below are in **Phase 3 files** only:

#### 3.1a `from()` removed from top-level exports

**Plan:** `import { from } from '@apollo/client'` → `link: from([errorLink, authLink, httpLink])`

**Reality:** `from` is no longer exported from `@apollo/client`. Use `ApolloLink.from()` instead.

**Change:** `ApolloLink.from([errorLink, authLink, httpLink])`

**File:** `admin/src/apollo/client.ts`

#### 3.1b `onError()` deprecated, `ErrorLink` class is the new API

**Plan:** `import { onError } from '@apollo/client/link/error'` → `export const errorLink = onError(({ graphQLErrors, ... }) => { ... })`

**Reality:** `onError` is deprecated in v4. The new API is `new ErrorLink(callback)`.

**Change:** `import { ErrorLink } from '@apollo/client'` → `export const errorLink = new ErrorLink(({ error, operation, forward }) => { ... })`

**File:** `admin/src/apollo/links/error.ts`

#### 3.1c `fromPromise()` removed — use `Observable` instead

**Plan:** `import { fromPromise } from '@apollo/client'` → `return fromPromise(refreshAccessToken()).filter(...).flatMap(...)`

**Reality:** `fromPromise` was completely removed in Apollo Client v4.

**Change:** Replaced with `new Observable((observer) => { refreshAccessToken().then(...).catch(...) })` and manually subscribe to `forward(operation)` on success.

**File:** `admin/src/apollo/links/error.ts`

#### 3.1d GraphQL errors detection: `graphQLErrors` array → `CombinedGraphQLErrors.is()`

**Plan:** `onError(({ graphQLErrors }) => { for (const err of graphQLErrors) { err.extensions?.code === 'UNAUTHORIZED' ... } })`

**Reality:** Apollo Client v4 wraps all GraphQL errors in a `CombinedGraphQLErrors` instance. The `graphQLErrors` array is no longer provided to the error handler. The error is provided as `error` instead.

**Change:** Check `CombinedGraphQLErrors.is(error)` then iterate `error.errors` array to find `UNAUTHORIZED` code in extensions.

**File:** `admin/src/apollo/links/error.ts`

#### 3.1e Error handler signature changed

**Plan (v3):** `onError(({ graphQLErrors, networkError, operation, forward }) => { ... })`

**Reality (v4):** `new ErrorLink(({ error, result, operation, forward }) => { ... })` — `graphQLErrors` and `networkError` are gone, replaced by a single `error` field that can be `CombinedGraphQLErrors`, `Error`, or other error types.

**File:** `admin/src/apollo/links/error.ts`

---

## Phase 5 — Auth Pages

### 5.1 Generated React hooks no longer available from codegen

**Plan:** Import `useInitiateAuthMutation` from `@/generated/graphql` (codegen-generated React hook).

**Reality:** The `@graphql-codegen/client-preset` v5 does not generate React hooks when used with Apollo Client v4. The `useMutation`/`useQuery` hooks from Apollo v4 have a different type signature than v3, and the codegen preset doesn't produce compatible hooks.

**Change:** Use `useMutation` from `@apollo/client` directly with the generated `InitiateAuthDocument`:
```ts
// Plan (v3):
const [initiateAuth, { loading, error }] = useInitiateAuthMutation()

// Actual (v4):
import { useMutation } from '@apollo/client'
import { InitiateAuthDocument } from '@/generated/graphql'
const [initiateAuth, { loading, error }] = useMutation<
  InitiateAuthMutation,
  InitiateAuthMutationVariables
>(InitiateAuthDocument)
```

**File:** `admin/src/pages/Login.tsx`

---

## Phase 7 — Dashboard + Settings

### 7.1 Generated React hooks no longer available (same as Phase 5)

**Plan:** Import `useMeQuery` and `useGetWorkspaceQuery` from `@/generated/graphql`.

**Reality:** Same issue as Phase 5 — codegen client preset v5 doesn't generate React hooks for Apollo v4.

**Change:** Use `useQuery` from `@apollo/client` with the generated documents:
```ts
// Plan (v3):
const { data, loading } = useMeQuery()
const { data, loading } = useGetWorkspaceQuery()

// Actual (v4):
import { useQuery } from '@apollo/client'
import { MeDocument, GetWorkspaceDocument } from '@/generated/graphql'
const { data: meData, loading: meLoading } = useQuery<MeQuery>(MeDocument)
const { data: wsData, loading: wsLoading } = useQuery<GetWorkspaceQuery>(GetWorkspaceDocument)
```

**Files:** `admin/src/pages/Dashboard.tsx`, `admin/src/pages/Settings.tsx`

---

## Build Fixes (discovered during `npm run build`)

### B.1 Apollo Client v4 — sub-package imports required

**Plan:** All Apollo imports from `@apollo/client` (top-level).

**Reality:** Apollo Client v4 splits exports into sub-packages. Top-level `@apollo/client` doesn't re-export `useQuery`, `useMutation`, `ApolloProvider`, `ErrorLink`.

**Change:**

| Import | Plan (`@apollo/client`) | Actual (v4) |
|--------|------------------------|-------------|
| `ApolloProvider` | `@apollo/client` | `@apollo/client/react` |
| `useQuery` | `@apollo/client` | `@apollo/client/react` |
| `useMutation` | `@apollo/client` | `@apollo/client/react` |
| `ErrorLink` | `@apollo/client` | `@apollo/client/link/error` |
| `CombinedGraphQLErrors` | `@apollo/client` | `@apollo/client/errors` |
| `ApolloLink`, `ApolloClient`, `HttpLink`, `InMemoryCache` | `@apollo/client` | `@apollo/client/core` |

**Files affected:**
- `admin/src/main.tsx` — `ApolloProvider` from `@apollo/client/react`
- `admin/src/apollo/client.ts` — all from `@apollo/client/core`
- `admin/src/apollo/links/auth.ts` — `ApolloLink` from `@apollo/client/core`
- `admin/src/apollo/links/error.ts` — `ErrorLink` from `@apollo/client/link/error`, `CombinedGraphQLErrors` from `@apollo/client/errors`, `ApolloLink` type from `@apollo/client/core`
- `admin/src/pages/Login.tsx` — `useMutation` from `@apollo/client/react`
- `admin/src/pages/Dashboard.tsx` — `useQuery` from `@apollo/client/react`
- `admin/src/pages/Settings.tsx` — `useQuery` from `@apollo/client/react`

### B.2 TypeScript `erasableSyntaxOnly` + `verbatimModuleSyntax` incompatible with codegen output

**Plan:** Vite scaffold defaults include `erasableSyntaxOnly: true` and `verbatimModuleSyntax: true`.

**Reality:** `@graphql-codegen/client-preset` generates:
- Non-type-only imports for `ResultOf`, `DocumentTypeDecoration`, `TypedDocumentNode`, `FragmentDefinitionNode`, `Incremental`
- `export enum` declarations (not allowed with `erasableSyntaxOnly`)

**Change:** Removed `erasableSyntaxOnly` and `verbatimModuleSyntax` from `tsconfig.app.json`. Kept all other strict checks (`noUnusedLocals`, `noUnusedParameters`, `noFallthroughCasesInSwitch`).

**File:** `admin/tsconfig.app.json`

### B.3 TypeScript `baseUrl` deprecation warning

**Reality:** TypeScript 6.0 deprecates `baseUrl` but it's still required for `paths` to work.

**Change:** Added `"ignoreDeprecations": "6.0"` to `compilerOptions`.

**File:** `admin/tsconfig.app.json`

### B.4 Unused `navigate` import in Login.tsx

**Reality:** The plan's Login.tsx imports `useNavigate` but never calls it (errors go to console, not navigation). `noUnusedLocals: true` catches this.

**Change:** Removed `useNavigate` import.

**File:** `admin/src/pages/Login.tsx`

---

## Summary of All Changed Files

| File | Phase | Change |
|------|-------|--------|
| `admin/components.json` | 1 | Created manually (CLI init skipped) |
| `admin/src/index.css` | 1 | Replaced Vite defaults with shadcn/ui CSS variables |
| `admin/tsconfig.app.json` | 1 | Added `baseUrl` + `paths` for `@/*` alias |
| `admin/codegen.yml` | 1 | Output path: file → directory |
| `admin/src/components/ui/button.tsx` | 1 | Manual shadcn component |
| `admin/src/components/ui/card.tsx` | 1 | Manual shadcn component |
| `admin/src/components/ui/badge.tsx` | 1 | Manual shadcn component |
| `admin/src/components/ui/avatar.tsx` | 1 | Manual shadcn component |
| `admin/src/components/ui/separator.tsx` | 1 | Manual shadcn component |
| `admin/src/components/ui/dropdown-menu.tsx` | 1 | Manual shadcn component |
| `admin/src/components/ui/skeleton.tsx` | 1 | Manual shadcn component |
| `admin/src/components/ui/toast.tsx` | 1 | Manual shadcn component |
| `admin/src/components/ui/toaster.tsx` | 1 | Manual shadcn component |
| `admin/src/components/ui/alert.tsx` | 1 | Manual shadcn component |
| `admin/src/components/ui/use-toast.ts` | 1 | Manual toast hook |
| `admin/src/apollo/client.ts` | 3 | `ApolloLink.from()` instead of removed `from()` |
| `admin/src/apollo/links/error.ts` | 3 | `ErrorLink` class, `Observable` instead of `fromPromise`, `CombinedGraphQLErrors.is()` for error detection |
| `admin/src/pages/Login.tsx` | 5 | `useMutation(InitiateAuthDocument)` instead of removed `useInitiateAuthMutation()` codegen hook; removed unused `useNavigate` |
| `admin/src/pages/Dashboard.tsx` | 7 | `useQuery(MeDocument)` instead of removed `useMeQuery()` codegen hook |
| `admin/src/pages/Settings.tsx` | 7 | `useQuery(GetWorkspaceDocument)` instead of removed `useGetWorkspaceQuery()` codegen hook |
| `admin/src/main.tsx` | B.1 | `ApolloProvider` from `@apollo/client/react` |
| `admin/src/apollo/client.ts` | B.1 | All imports from `@apollo/client/core` |
| `admin/src/apollo/links/auth.ts` | B.1 | `ApolloLink` from `@apollo/client/core` |
| `admin/src/apollo/links/error.ts` | B.1 | `ErrorLink` from `@apollo/client/link/error`, `CombinedGraphQLErrors` from `@apollo/client/errors` |
| `admin/tsconfig.app.json` | B.2,B.3 | Removed `erasableSyntaxOnly` + `verbatimModuleSyntax`; added `ignoreDeprecations: "6.0"` |

## Key Library Versions

| Package | Version in Plan (v3 era) | Actual Installed |
|---------|-------------------------|------------------|
| `@apollo/client` | ~3.x | **4.1.6** |
| `@graphql-codegen/cli` | any | **6.2.1** |
| `@graphql-codegen/client-preset` | any | **5.2.4** |
| `shadcn` | CLI init worked | **4.1.2** (interactive only) |
| `tailwindcss` | v3 | **4.2.2** |
| `react` / `react-dom` | 18.x | **19.2.4** |
