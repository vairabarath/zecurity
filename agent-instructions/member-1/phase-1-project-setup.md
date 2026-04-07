# Phase 1 — Project Setup (No Backend Needed)

Everything here runs before the backend exists.
This phase produces a working Vite dev server with shadcn/ui, Tailwind, codegen config, and all dependencies installed.
No backend required. Start immediately.

---

## Step 1: Scaffold Vite Project

**Path:** `admin/`

```bash
npm create vite@latest admin -- --template react-ts
cd admin
npm install
```

---

## Step 2: Install All Dependencies

```bash
# Apollo Client + GraphQL
npm install @apollo/client graphql

# State management
npm install zustand

# Router
npm install react-router-dom

# shadcn/ui dependencies
npm install tailwindcss @tailwindcss/vite
npm install class-variance-authority clsx tailwind-merge
npm install lucide-react

# shadcn/ui CLI (run once to init)
npx shadcn@latest init

# GraphQL codegen
npm install -D @graphql-codegen/cli
npm install -D @graphql-codegen/client-preset
```

---

## Step 3: shadcn/ui Init

When `npx shadcn@latest init` runs, answer:
- Style: Default
- Base color: Slate
- CSS variables: Yes

Then add the components needed for admin UI:

```bash
npx shadcn@latest add button card badge avatar
npx shadcn@latest add dropdown-menu separator skeleton
npx shadcn@latest add toast alert
```

---

## Step 4: `admin/vite.config.ts`

**Path:** `admin/vite.config.ts`

```typescript
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import path from 'path'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    port: 5173,
    // Proxy API calls to Go controller in development
    // so CORS is not needed during local dev
    proxy: {
      '/graphql': 'http://localhost:8080',
      '/auth':    'http://localhost:8080',
    },
  },
})
```

The proxy is critical for local development. Without it, the browser
blocks requests to `localhost:8080` from `localhost:5173` due to CORS.
The proxy makes all API calls appear same-origin to the browser.

---

## Step 5: `admin/codegen.yml`

**Path:** `admin/codegen.yml`

```yaml
schema: '../controller/graph/schema.graphqls'
documents: 'src/graphql/**/*.graphql'
generates:
  src/generated/graphql.ts:
    preset: client
    config:
      scalars:
        ID: string
```

The schema path `'../controller/graph/schema.graphqls'` points directly
at Member 4's file. When Member 4 changes the schema, Member 1 runs:

```bash
npx graphql-codegen
```

TypeScript compiler immediately shows what broke. No manual syncing.

Add to `package.json` scripts:

```json
{
  "scripts": {
    "dev": "vite",
    "build": "tsc && vite build",
    "codegen": "graphql-codegen",
    "codegen:watch": "graphql-codegen --watch"
  }
}
```

---

## Step 6: Write GraphQL Operation Files

These are the only GraphQL files Member 1 writes.
Everything else is generated.

**Path:** `admin/src/graphql/mutations.graphql`

```graphql
mutation InitiateAuth($provider: String!) {
  initiateAuth(provider: $provider) {
    redirectUrl
    state
  }
}
```

**Path:** `admin/src/graphql/queries.graphql`

```graphql
query Me {
  me {
    id
    email
    role
    provider
    createdAt
  }
}

query GetWorkspace {
  workspace {
    id
    slug
    name
    status
    createdAt
  }
}
```

Run codegen now. This generates `src/generated/graphql.ts` with:
- `InitiateAuthMutation` type
- `InitiateAuthDocument` (the typed query document)
- `MeQuery` type
- `GetWorkspaceQuery` type
- All enum types: `Role`, `WorkspaceStatus`
- React hooks: `useInitiateAuthMutation`, `useMeQuery`, `useGetWorkspaceQuery`

Member 1 imports these hooks directly. Never writes raw `gql``` strings.

---

## Step 7: `admin/src/lib/utils.ts`

**Path:** `admin/src/lib/utils.ts`

shadcn/ui utility function for class merging:

```typescript
import { type ClassValue, clsx } from 'clsx'
import { twMerge } from 'tailwind-merge'

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs))
}
```

---

## Hard Dependency Note

**`controller/graph/schema.graphqls` must exist before running codegen.**
This is Member 4's Phase 1 deliverable.
Steps 1–5 and Step 7 can proceed without it.
Step 6 (writing `.graphql` files) can be written without it.
But `npx graphql-codegen` will fail until the schema file is committed.

---

## Verification Checklist

```
[x] npm run dev starts without errors on localhost:5173
[x] codegen.yml points at ../controller/graph/schema.graphqls
[x] npm run codegen generates src/generated/ (graphql.ts, gql.ts, fragment-masking.ts, index.ts)
[x] Generated file contains: InitiateAuthDocument, MeDocument, GetWorkspaceDocument,
    Role enum, WorkspaceStatus enum, all TypeScript types
[x] Vite proxy forwards /graphql and /auth to localhost:8080
[x] shadcn/ui components installed: button, card, badge, avatar,
    dropdown-menu, separator, skeleton, toast, alert
[x] @ alias resolves correctly in imports
[x] lib/utils.ts cn() utility function created
[x] src/index.css has shadcn/ui CSS variables (--background, --foreground, --primary, etc.)
[x] Toaster component created (useToast hook + toast system)
[x] Alert + AlertTitle + AlertDescription components created
```
