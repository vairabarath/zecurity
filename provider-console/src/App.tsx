import { Route, Routes } from 'react-router-dom'
import { RedirectIfAuthenticated, RequireRole, RequireSession } from '@/auth/guards'
import { rolesFor } from '@/auth/roles'
import { Layout } from '@/components/Layout'
import { Toaster } from '@/components/ui/toaster'
import { Audit } from '@/pages/Audit'
import { Certificates } from '@/pages/Certificates'
import { ChangePassword } from '@/pages/ChangePassword'
import { Forbidden } from '@/pages/Forbidden'
import { Home } from '@/pages/Home'
import { Login } from '@/pages/Login'
import { NotFound } from '@/pages/NotFound'
import { ProviderUsers } from '@/pages/ProviderUsers'
import { RelayDetail } from '@/pages/RelayDetail'
import { Relays } from '@/pages/Relays'
import { TenantDetail } from '@/pages/TenantDetail'
import { Tenants } from '@/pages/Tenants'

// Route table. Guards steer navigation only; the controller authorizes every
// request. Sections behind a role use <RequireRole roles={...}> with the
// roles from auth/roles.ts (/users now; Phase P: the fleet pages).
// ChangePassword checks the session itself (see the comment there).
export function AppRoutes() {
  return (
    <Routes>
      <Route
        path="/login"
        element={
          <RedirectIfAuthenticated>
            <Login />
          </RedirectIfAuthenticated>
        }
      />
      <Route path="/change-password" element={<ChangePassword />} />
      <Route element={<RequireSession />}>
        <Route element={<Layout />}>
          <Route index element={<Home />} />
          <Route element={<RequireRole roles={rolesFor('relays')} />}>
            <Route path="relays" element={<Relays />} />
            <Route path="relays/:id" element={<RelayDetail />} />
          </Route>
          <Route element={<RequireRole roles={rolesFor('tenants')} />}>
            <Route path="tenants" element={<Tenants />} />
            <Route path="tenants/:id" element={<TenantDetail />} />
          </Route>
          <Route element={<RequireRole roles={rolesFor('audit')} />}>
            <Route path="audit" element={<Audit />} />
          </Route>
          <Route element={<RequireRole roles={rolesFor('certificates')} />}>
            <Route path="certificates" element={<Certificates />} />
          </Route>
          <Route element={<RequireRole roles={rolesFor('users')} />}>
            <Route path="users" element={<ProviderUsers />} />
          </Route>
          <Route path="forbidden" element={<Forbidden />} />
        </Route>
      </Route>
      <Route path="*" element={<NotFound />} />
    </Routes>
  )
}

export default function App() {
  return (
    <>
      <AppRoutes />
      <Toaster />
    </>
  )
}
