---
type: phase
status: complete
sprint: 7
member: M1
phase: Phase2-Admin-User-Detection
depends_on:
  - M1-Phase1 (role routing + pages done)
  - M3-Phase1 (myDevices GraphQL resolver live)
tags:
  - frontend
  - react
  - admin
  - sidebar
---

# M1 Phase 2 — Admin User Detection + Install Client Button

---

## What You're Building

For ADMIN users: show an "Install Client" button in the sidebar or header that links to `/client-install`. This allows admins who are also workspace users to download and install the client.

Also use the `myDevices` GraphQL query to show the admin's enrolled devices count (optional, but useful context).

---

## Files to Touch

### 1. Sidebar or Header (find the correct file)

Check which file contains the sidebar navigation — likely `admin/src/components/Sidebar.tsx` or `admin/src/layouts/ProtectedLayout.tsx`. Add the install button:

```tsx
// In sidebar nav items (only shown if role is ADMIN):
{me?.role === 'ADMIN' && (
  <NavLink
    to="/client-install"
    className="flex items-center gap-2 px-4 py-2 rounded-lg hover:bg-gray-100 text-sm"
  >
    <DownloadIcon className="w-4 h-4" />
    Install Client
  </NavLink>
)}
```

The `me` object should already be available in whatever context the sidebar uses. If not, add `useQuery(MeDocument)` to the sidebar.

---

### 2. `admin/src/graphql/queries.graphql` (MODIFY)

Add the `myDevices` query if not already present after codegen:

```graphql
query MyDevices {
  myDevices {
    id
    name
    os
    spiffeId
    certNotAfter
    lastSeenAt
    createdAt
  }
}
```

After adding, run:
```bash
cd admin && npm run codegen
```

---

### 3. `/client-install` page — show enrolled devices for ADMIN (MODIFY `ClientInstall.tsx`)

Add a small section showing the admin's already-enrolled devices:

```tsx
const { data: devicesData } = useQuery(MyDevicesDocument);
const devices = devicesData?.myDevices ?? [];

// In JSX (below download section, only if devices.length > 0):
{devices.length > 0 && (
  <div className="mt-8">
    <h2 className="text-lg font-semibold mb-3">Your Enrolled Devices</h2>
    <table className="w-full text-sm">
      <thead>
        <tr className="text-left text-gray-500 border-b">
          <th className="pb-2">Name</th>
          <th className="pb-2">OS</th>
          <th className="pb-2">Enrolled</th>
        </tr>
      </thead>
      <tbody>
        {devices.map(d => (
          <tr key={d.id} className="border-b last:border-0">
            <td className="py-2">{d.name}</td>
            <td className="py-2">{d.os}</td>
            <td className="py-2">{new Date(d.createdAt).toLocaleDateString()}</td>
          </tr>
        ))}
      </tbody>
    </table>
  </div>
)}
```

---

## Build Check

```bash
cd admin && npm run build
```

---

## Post-Phase Fixes

_None yet._
