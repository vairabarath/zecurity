import { Outlet } from 'react-router-dom'
import { Sidebar } from './Sidebar'
import { Header } from './Header'

export function AppShell() {
  return (
    <div className="admin-shell">
      <Sidebar />
      <Header />
      <main className="app-panel relative col-start-2 row-start-2 min-h-0 overflow-y-auto p-6 max-[980px]:col-start-1 max-[980px]:row-start-3">
        <Outlet />
      </main>
    </div>
  )
}
