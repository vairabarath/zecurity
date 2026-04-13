import { Outlet } from 'react-router-dom'
import { Sidebar } from './Sidebar'
import { Header } from './Header'

export function AppShell() {
  return (
    <div className="relative flex h-screen overflow-hidden bg-background">
      {/* Subtle grid */}
      <div
        className="pointer-events-none absolute inset-0 opacity-[0.015]"
        style={{
          backgroundImage: `
            linear-gradient(oklch(0.55 0.18 250) 1px, transparent 1px),
            linear-gradient(90deg, oklch(0.55 0.18 250) 1px, transparent 1px)
          `,
          backgroundSize: '32px 32px',
        }}
      />

      {/* Gradient glows - subtle for light theme */}
      <div
        className="pointer-events-none absolute -top-[20%] -left-[10%] w-[50%] h-[40%] rounded-full blur-[100px]"
        style={{ background: 'radial-gradient(circle, oklch(0.55 0.18 250 / 0.06) 0%, transparent 70%)' }}
      />
      <div
        className="pointer-events-none absolute -bottom-[10%] -right-[10%] w-[40%] h-[30%] rounded-full blur-[80px]"
        style={{ background: 'radial-gradient(circle, oklch(0.55 0.18 250 / 0.04) 0%, transparent 70%)' }}
      />

      <Sidebar />
      <div className="relative flex flex-col flex-1 overflow-hidden">
        <Header />
        <main className="relative flex-1 overflow-y-auto p-6">
          <div className="relative">
            <Outlet />
          </div>
        </main>
      </div>
    </div>
  )
}