import { Outlet } from 'react-router-dom'

// Policies is a shell, not a page: the header belongs to every section beneath
// it, and which section shows is decided by the route so the sidebar can mark
// it active and a section can be linked to directly.
//
// There is deliberately no Sign In Policy section. The Sprint 19 plan's tree
// lists one, but no backend for it exists anywhere in the controller — no
// schema, no resolver, no store — so it would be a control that cannot do
// anything.
export default function Policies() {
  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Policies</h2>
          <p className="page-subtitle">Manage access and posture policies.</p>
        </div>
      </div>

      <Outlet />
    </div>
  )
}
