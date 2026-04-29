---
type: phase
status: complete
sprint: 7
member: M1
phase: Phase1-Role-Routing-Invite-Pages
depends_on:
  - M2-Phase1 (npm run codegen done — myDevices + invitation queries available)
  - M3-Phase2 (GET /api/invitations/:token live)
tags:
  - frontend
  - react
  - routing
  - invitation
---

# M1 Phase 1 — Role-Based Routing + Invite Accept + Client Install Pages

---

## What You're Building

1. **Role-based redirect after login** — ADMIN goes to `/dashboard`, MEMBER/VIEWER goes to `/client-install`.
2. **`/invite/:token` page** — public page shown after user clicks invitation email link. Shows workspace info, "Sign in with Google" button.
3. **`/client-install` page** — shown after invite acceptance or directly for MEMBER users. Download links + install instructions.

---

## Files to Touch

### 1. `admin/src/App.tsx` (MODIFY)

Add two new public routes and a role-aware redirect after login.

**New routes to add:**
```tsx
import InviteAccept  from './pages/InviteAccept';
import ClientInstall from './pages/ClientInstall';

// Inside router:
<Route path="/invite/:token" element={<InviteAccept />} />
<Route path="/client-install" element={<ClientInstall />} />
```

**Role redirect logic** — find where the app redirects after auth (likely in `ProtectedLayout` or the auth callback handler). After fetching the `Me` query:

```tsx
// After Me query resolves:
if (me.role === 'ADMIN') {
  navigate('/dashboard');
} else {
  // MEMBER or VIEWER
  navigate('/client-install');
}
```

Check how `Me` query is currently used (likely in a context or layout component). Add the role-based redirect there rather than duplicating the query.

---

### 2. `admin/src/pages/InviteAccept.tsx` (NEW)

```tsx
import { useParams } from 'react-router-dom';
import { useEffect, useState } from 'react';

interface InvitationInfo {
  email: string;
  status: string;
  expires_at: string;
}

export default function InviteAccept() {
  const { token } = useParams<{ token: string }>();
  const [invitation, setInvitation] = useState<InvitationInfo | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    fetch(`/api/invitations/${token}`)
      .then(r => r.ok ? r.json() : Promise.reject('not found'))
      .then(setInvitation)
      .catch(() => setError('Invitation not found or expired.'));
  }, [token]);

  const handleSignIn = () => {
    // Trigger existing Google OAuth flow.
    // Append invite_token to the OAuth state so the callback can pass it to TokenExchange.
    // How to do this depends on the existing auth initiation — find where /auth/callback
    // is initiated and pass: state = base64({ returnTo: '/client-install', inviteToken: token })
    // or use a query param on the OAuth redirect.
    window.location.href = `/auth/login?invite_token=${token}`;
    // Adjust the URL to match how the existing auth flow is initiated.
  };

  if (error) return <div className="...">{error}</div>;
  if (!invitation) return <div>Loading...</div>;

  return (
    <div className="flex flex-col items-center justify-center min-h-screen gap-6">
      <h1 className="text-2xl font-semibold">You've been invited to Zecurity</h1>
      <p className="text-gray-500">Sign in to accept the invitation for <strong>{invitation.email}</strong></p>
      <button
        onClick={handleSignIn}
        className="bg-blue-600 text-white px-6 py-3 rounded-lg hover:bg-blue-700"
      >
        Sign in with Google
      </button>
      <p className="text-sm text-gray-400">
        Invitation expires: {new Date(invitation.expires_at).toLocaleDateString()}
      </p>
    </div>
  );
}
```

> **Key integration point:** The "Sign in with Google" button needs to pass the `invite_token` through the OAuth flow so the Controller can accept the invitation during `TokenExchange`. Check how the existing auth flow is started (likely a redirect to `/auth/login` or similar). The Controller's existing `/auth/callback` may need a small addition to handle `invite_token` in state — coordinate with M3 if needed.

---

### 3. `admin/src/pages/ClientInstall.tsx` (NEW)

```tsx
import { useNavigate } from 'react-router-dom';
import { useQuery } from '@apollo/client';
import { MeDocument } from '../graphql/generated';

export default function ClientInstall() {
  const navigate = useNavigate();
  const { data } = useQuery(MeDocument);
  const isAdmin = data?.me?.role === 'ADMIN';

  return (
    <div className="max-w-2xl mx-auto py-16 px-4">
      <h1 className="text-3xl font-bold mb-2">Install Zecurity Client</h1>
      <p className="text-gray-500 mb-8">
        Download and install the client to connect to your workspace.
      </p>

      {/* Download links */}
      <div className="grid grid-cols-3 gap-4 mb-10">
        <a href="#" className="border rounded-lg p-4 text-center hover:border-blue-500">
          <div className="text-2xl mb-1">🐧</div>
          <div className="font-medium">Linux</div>
          <div className="text-sm text-gray-400">x86_64</div>
        </a>
        <a href="#" className="border rounded-lg p-4 text-center hover:border-blue-500">
          <div className="text-2xl mb-1">🍎</div>
          <div className="font-medium">macOS</div>
          <div className="text-sm text-gray-400">Apple Silicon / Intel</div>
        </a>
        <a href="#" className="border rounded-lg p-4 text-center hover:border-blue-500">
          <div className="text-2xl mb-1">🪟</div>
          <div className="font-medium">Windows</div>
          <div className="text-sm text-gray-400">x86_64</div>
        </a>
      </div>

      {/* Setup instructions */}
      <h2 className="text-lg font-semibold mb-3">Setup</h2>
      <pre className="bg-gray-900 text-green-400 rounded-lg p-4 text-sm mb-8 overflow-x-auto">
{`zecurity-client setup \\
  --controller controller.example.com:9090 \\
  --workspace myworkspace

zecurity-client login`}
      </pre>

      {/* Admin shortcut */}
      {isAdmin && (
        <div className="border-t pt-6 mt-6">
          <p className="text-sm text-gray-500 mb-3">
            You're an admin. You can also access the admin console.
          </p>
          <button
            onClick={() => navigate('/dashboard')}
            className="text-blue-600 underline text-sm"
          >
            Go to Admin Console →
          </button>
        </div>
      )}
    </div>
  );
}
```

---

## Build Check

```bash
cd admin && npm run build
```

Must pass with no TypeScript errors.

---

## Post-Phase Fixes

_None yet._
