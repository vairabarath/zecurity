import { Routes, Route, Navigate } from 'react-router-dom'
import Login from '@/pages/Login'
import AuthCallback from '@/pages/AuthCallback'
import Dashboard from '@/pages/Dashboard'
import Settings from '@/pages/Settings'
import { AppShell } from '@/components/layout/AppShell'
import { useRequireAuth } from '@/hooks/useRequireAuth'

// ProtectedLayout wraps routes that require authentication.
// useRequireAuth redirects to /login if no token in store.
function ProtectedLayout() {
  const { isReady } = useRequireAuth()
  if (!isReady) return null // or a loading spinner
  return <AppShell />
}

export default function App() {
  return (
    <Routes>
      {/* Public routes */}
      <Route path="/login"         element={<Login />} />
      <Route path="/auth/callback" element={<AuthCallback />} />

      {/* Protected routes */}
      <Route element={<ProtectedLayout />}>
        <Route path="/"          element={<Navigate to="/dashboard" replace />} />
        <Route path="/dashboard" element={<Dashboard />} />
        <Route path="/settings"  element={<Settings />} />
      </Route>
    </Routes>
  )
}
