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
