import { useState } from 'react'
import { useLocation, useNavigate } from 'react-router-dom'
import { Bell, ChevronDown, LogOut, Search, Settings, Shield } from 'lucide-react'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { apolloClient } from '@/apollo/client'
import { useAuthStore } from '@/store/auth'

const titles: Record<string, { title: string; subtitle: string }> = {
  '/dashboard': { title: 'Dashboard', subtitle: 'Live posture across your zero-trust edge.' },
  '/resource-discovery': { title: 'Resource Discovery', subtitle: 'Connector scans and shield-discovered services in one workspace.' },
  '/remote-networks': { title: 'Remote Networks', subtitle: 'Track networks, connectors, and shield coverage.' },
  '/connectors': { title: 'Connectors', subtitle: 'Gateways carrying heartbeat and tunnel traffic.' },
  '/shields': { title: 'Shields', subtitle: 'Resource agents enforcing protected access.' },
  '/resources': { title: 'Resources', subtitle: 'Protected services currently managed by shields.' },
  '/settings': { title: 'Settings', subtitle: 'Workspace and operator controls.' },
}

function getHeaderCopy(pathname: string) {
  const direct = titles[pathname]
  if (direct) return direct
  if (pathname.startsWith('/remote-networks/')) {
    return { title: 'Remote Network', subtitle: 'Topology, connectors, and shields for a single segment.' }
  }
  if (pathname.startsWith('/connectors/')) {
    return { title: 'Connector', subtitle: 'Gateway details, install state, and lifecycle actions.' }
  }
  if (pathname.startsWith('/shields/')) {
    return { title: 'Shield', subtitle: 'Resource host identity, install state, and lifecycle actions.' }
  }
  return { title: 'Zecurity', subtitle: 'Zero Trust Network Access console.' }
}

export function Header() {
  const navigate = useNavigate()
  const location = useLocation()
  const { user, clearAuth } = useAuthStore()
  const [search, setSearch] = useState('')
  const copy = getHeaderCopy(location.pathname)
  const initials = user?.email?.slice(0, 2).toUpperCase() ?? 'ZT'

  async function handleSignOut() {
    await apolloClient.clearStore()
    clearAuth()
    navigate('/login', { replace: true })
  }

  return (
    <header className="app-panel col-start-2 row-start-1 flex items-center gap-4 px-5 max-[980px]:col-start-1 max-[980px]:row-start-2">
      <div className="min-w-0">
        <h1 className="truncate text-[18px] font-bold tracking-[-0.015em]">{copy.title}</h1>
        <p className="truncate text-[12px] text-muted-foreground">{copy.subtitle}</p>
      </div>

      <div className="flex-1" />

      <label className="toolbar-input hidden max-w-[320px] md:flex">
        <Search className="h-4 w-4 shrink-0" />
        <input
          value={search}
          onChange={(event) => setSearch(event.target.value)}
          placeholder="Search networks, connectors, shields..."
        />
        <span className="rounded-md bg-accent px-1.5 py-0.5 text-[10px] font-semibold text-muted-foreground">/</span>
      </label>

      <button className="grid h-9 w-9 place-items-center rounded-[10px] bg-secondary text-muted-foreground transition-colors hover:bg-accent hover:text-foreground">
        <Bell className="h-4 w-4" />
      </button>

      <button
        onClick={() => navigate('/settings')}
        className="grid h-9 w-9 place-items-center rounded-[10px] bg-secondary text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
      >
        <Settings className="h-4 w-4" />
      </button>

      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <button className="flex items-center gap-2 rounded-[12px] bg-secondary px-2 py-1.5 text-left transition-colors hover:bg-accent">
            <Avatar className="h-8 w-8 border border-border">
              <AvatarFallback className="bg-primary/15 text-[11px] font-bold text-primary">{initials}</AvatarFallback>
            </Avatar>
            <div className="hidden min-w-0 md:block">
              <div className="max-w-[160px] truncate text-[12px] font-semibold">{user?.email ?? 'Workspace user'}</div>
              <div className="text-[10.5px] capitalize text-muted-foreground">{user?.role ?? 'admin'}</div>
            </div>
            <ChevronDown className="h-4 w-4 text-muted-foreground" />
          </button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" sideOffset={8} className="w-72">
          <div className="border-b border-border px-3 py-2.5">
            <div className="flex items-center gap-2 text-sm font-semibold">
              <Shield className="h-4 w-4 text-primary" />
              <span className="truncate">{user?.email}</span>
            </div>
            <div className="ml-6 mt-0.5 text-xs capitalize text-muted-foreground">{user?.role}</div>
          </div>
          <DropdownMenuItem onClick={() => navigate('/settings')} className="cursor-pointer">
            <Settings className="mr-2 h-4 w-4" />
            Settings
          </DropdownMenuItem>
          <DropdownMenuItem onClick={handleSignOut} className="cursor-pointer text-destructive">
            <LogOut className="mr-2 h-4 w-4" />
            Sign out
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </header>
  )
}
