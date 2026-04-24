import { NavLink } from 'react-router-dom'
import {
  Box,
  LayoutDashboard,
  Network,
  Plug,
  Settings,
  Shield,
  ShieldCheck,
  Waypoints,
} from 'lucide-react'
import { useAuthStore } from '@/store/auth'
import { cn } from '@/lib/utils'

const items = [
  { to: '/dashboard', label: 'Dashboard', icon: LayoutDashboard },
  { to: '/topology', label: 'Topology', icon: Waypoints },
  { to: '/remote-networks', label: 'Remote Networks', icon: Network },
  { to: '/connectors', label: 'Connectors', icon: Plug },
  { to: '/shields', label: 'Shields', icon: Shield },
  { to: '/resources', label: 'Resources', icon: Box },
  { to: '/settings', label: 'Settings', icon: Settings },
]

function NavItem({ to, label, icon: Icon }: { to: string; label: string; icon: React.ElementType }) {
  return (
    <NavLink to={to}>
      {({ isActive }) => (
        <div
          className={cn(
            'flex items-center gap-3 rounded-[10px] px-3 py-2.5 text-[13.5px] font-medium transition-colors',
            isActive
              ? 'bg-primary text-primary-foreground shadow-[0_4px_14px_oklch(0.86_0.095_175/0.2)]'
              : 'text-muted-foreground hover:bg-secondary hover:text-foreground',
          )}
        >
          <Icon className="h-4 w-4 shrink-0" />
          <span>{label}</span>
        </div>
      )}
    </NavLink>
  )
}

export function Sidebar() {
  const user = useAuthStore((state) => state.user)
  const initials = user?.email?.slice(0, 2).toUpperCase() ?? 'ZT'

  return (
    <aside className="app-panel col-start-1 row-span-2 flex min-h-0 flex-col p-3 max-[980px]:row-span-1">
      <div className="flex items-center gap-3 px-3 py-2.5">
        <div className="grid h-10 w-10 place-items-center rounded-xl bg-[linear-gradient(135deg,oklch(0.86_0.095_175)_0%,oklch(0.70_0.10_175)_100%)] text-primary-foreground shadow-[0_6px_16px_oklch(0.86_0.095_175/0.22)]">
          <ShieldCheck className="h-5 w-5" />
        </div>
        <div>
          <div className="text-[15px] font-semibold tracking-[-0.01em]">ZECURITY</div>
          <div className="text-[10.5px] text-muted-foreground">Zero Trust</div>
        </div>
      </div>

      <div className="mt-4 px-2 text-[10px] font-semibold uppercase tracking-[0.12em] text-muted-foreground">
        Console
      </div>

      <nav className="mt-2 flex flex-1 flex-col gap-1 px-1">
        {items.map((item) => (
          <NavItem key={item.to} {...item} />
        ))}
      </nav>

      <div className="mx-1 mt-3 rounded-[14px] border border-border bg-secondary/70 p-3">
        <div className="flex items-center gap-2.5">
          <div className="h-8 w-8 shrink-0 rounded-[10px] bg-[linear-gradient(135deg,oklch(0.78_0.09_310),oklch(0.78_0.10_235))] text-center text-[11px] font-bold leading-8 text-primary-foreground">
            {initials}
          </div>
          <div className="min-w-0">
            <div className="truncate text-[12.5px] font-semibold">{user?.email ?? 'Workspace user'}</div>
            <div className="text-[10.5px] capitalize text-muted-foreground">{user?.role ?? 'admin'}</div>
          </div>
        </div>
      </div>
    </aside>
  )
}
