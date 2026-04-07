# Phase 6 — Protected Layout + Nav

No backend needed. These are pure UI components.
AppShell wraps all protected routes via React Router `<Outlet />`.

---

## File 1: `admin/src/components/layout/AppShell.tsx`

**Path:** `admin/src/components/layout/AppShell.tsx`

```tsx
import { Outlet } from 'react-router-dom'
import { Sidebar } from './Sidebar'
import { Header } from './Header'

// AppShell wraps all protected pages.
// Outlet renders the active route's page component.
export function AppShell() {
  return (
    <div className="flex h-screen overflow-hidden bg-background">
      <Sidebar />
      <div className="flex flex-col flex-1 overflow-hidden">
        <Header />
        <main className="flex-1 overflow-y-auto p-6">
          <Outlet />
        </main>
      </div>
    </div>
  )
}
```

Layout: fixed sidebar on the left, header at top, scrollable main content area.

---

## File 2: `admin/src/components/layout/Sidebar.tsx`

**Path:** `admin/src/components/layout/Sidebar.tsx`

```tsx
import { NavLink } from 'react-router-dom'
import { LayoutDashboard, Settings } from 'lucide-react'
import { cn } from '@/lib/utils'

const nav = [
  { to: '/dashboard', label: 'Dashboard', icon: LayoutDashboard },
  { to: '/settings',  label: 'Settings',  icon: Settings },
]

export function Sidebar() {
  return (
    <aside className="w-56 border-r flex flex-col bg-card">
      <div className="p-4 border-b">
        <span className="font-semibold text-sm">ZTNA Console</span>
      </div>
      <nav className="flex-1 p-2 space-y-1">
        {nav.map(({ to, label, icon: Icon }) => (
          <NavLink
            key={to}
            to={to}
            className={({ isActive }) =>
              cn(
                'flex items-center gap-2 px-3 py-2 rounded-md text-sm transition-colors',
                isActive
                  ? 'bg-primary text-primary-foreground'
                  : 'text-muted-foreground hover:bg-accent hover:text-accent-foreground'
              )
            }
          >
            <Icon className="w-4 h-4" />
            {label}
          </NavLink>
        ))}
      </nav>
    </aside>
  )
}
```

`NavLink` automatically applies the `isActive` class when the current route matches.
Active state: primary background. Inactive: muted text with hover accent.

---

## File 3: `admin/src/components/layout/Header.tsx`

**Path:** `admin/src/components/layout/Header.tsx`

```tsx
import { useNavigate } from 'react-router-dom'
import { useAuthStore } from '@/store/auth'
import { apolloClient } from '@/apollo/client'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Badge } from '@/components/ui/badge'

export function Header() {
  const navigate = useNavigate()
  const { user, clearAuth } = useAuthStore()

  async function handleSignOut() {
    // Clear Apollo cache so no stale data persists
    await apolloClient.clearStore()
    clearAuth()
    navigate('/login', { replace: true })
    // Note: this does NOT call a sign-out endpoint.
    // The refresh cookie will expire on its own (7 days).
    // A sign-out endpoint that deletes the Redis refresh key
    // can be added later for immediate invalidation.
  }

  const initials = user?.email?.slice(0, 2).toUpperCase() ?? '??'

  return (
    <header className="h-14 border-b flex items-center justify-between px-6">
      <div />
      <div className="flex items-center gap-3">
        {user && (
          <Badge variant="outline" className="text-xs">
            {user.role}
          </Badge>
        )}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Avatar className="cursor-pointer h-8 w-8">
              <AvatarFallback className="text-xs">{initials}</AvatarFallback>
            </Avatar>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <div className="px-2 py-1.5 text-xs text-muted-foreground">
              {user?.email}
            </div>
            <DropdownMenuItem onClick={handleSignOut}>
              Sign out
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </header>
  )
}
```

### Sign Out Behavior

Sign out is client-side only in this sprint:
1. Apollo cache cleared (no stale data for next user)
2. Zustand store cleared (token + user removed from memory)
3. Navigate to `/login`

The httpOnly refresh cookie cannot be deleted from JavaScript.
It will expire on its own (7-day TTL set by Member 2).
A server-side sign-out endpoint (deleting the Redis refresh key) is a future enhancement.

---

## Verification Checklist

```
[x] AppShell renders Sidebar + Header + Outlet
[x] Sidebar NavLink active state highlights current route
[x] Sidebar shows Dashboard and Settings nav items
[x] Header shows user role badge
[x] Header shows avatar with email initials
[x] Dropdown menu shows user email
[x] Sign out clears Apollo cache AND Zustand store
[x] Sign out redirects to /login
[x] No stale data visible after sign out + sign in as different user
```
