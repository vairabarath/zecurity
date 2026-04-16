import { Routes, Route, Navigate } from 'react-router-dom'
import Login from '@/pages/Login'
import AuthCallback from '@/pages/AuthCallback'
import Dashboard from '@/pages/Dashboard'
import RemoteNetworks from '@/pages/RemoteNetworks'
import Connectors from '@/pages/Connectors'
import AllConnectors from '@/pages/AllConnectors'
import ConnectorDetail from '@/pages/ConnectorDetail'
import Settings from '@/pages/Settings'
import Step1Email from '@/pages/signup/Step1Email'
import Step2Workspace from '@/pages/signup/Step2Workspace'
import Step3Auth from '@/pages/signup/Step3Auth'
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

      {/* Signup wizard routes */}
      <Route path="/signup"             element={<Step1Email />} />
      <Route path="/signup/workspace"   element={<Step2Workspace />} />
      <Route path="/signup/auth"        element={<Step3Auth />} />

      {/* Protected routes */}
      <Route element={<ProtectedLayout />}>
        <Route path="/"          element={<Navigate to="/dashboard" replace />} />
        <Route path="/dashboard" element={<Dashboard />} />
        <Route path="/remote-networks" element={<RemoteNetworks />} />
        <Route path="/remote-networks/:id/connectors" element={<Connectors />} />
        <Route path="/connectors/:connectorId" element={<ConnectorDetail />} />
        <Route path="/connectors" element={<AllConnectors />} />
        <Route path="/settings"  element={<Settings />} />
      </Route>
    </Routes>
  )
}
