import { Route, Routes } from 'react-router-dom'
import { RedirectIfAuthenticated, RequireSession } from '@/auth/guards'
import { Layout } from '@/components/Layout'
import { Toaster } from '@/components/ui/toaster'
import { ChangePassword } from '@/pages/ChangePassword'
import { Forbidden } from '@/pages/Forbidden'
import { Home } from '@/pages/Home'
import { Login } from '@/pages/Login'
import { NotFound } from '@/pages/NotFound'

// Route table. Guards steer navigation only; the controller authorizes every
// request. Sections behind a role use <RequireRole roles={...}> with the
// roles from auth/roles.ts (C-b: /users; Phase P: the fleet pages).
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
