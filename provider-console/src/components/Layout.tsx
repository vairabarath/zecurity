import { ChevronDown, KeyRound, LogOut } from 'lucide-react'
import { Link, NavLink, Outlet, useNavigate } from 'react-router-dom'
import { signOut } from '@/auth/flows'
import { SECTIONS, sectionsFor, type Section } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { ProviderBrand } from '@/components/ProviderBrand'
import { RoleBadge } from '@/components/RoleBadge'
import { SessionCountdown } from '@/components/SessionCountdown'
import { Button } from '@/components/ui/button'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { cn } from '@/lib/utils'

// The signed-in shell. The nav lists only the sections the role may open
// (roles.ts); that is a convenience, the controller authorizes every call.
export function Layout({ sections = SECTIONS }: { sections?: readonly Section[] }) {
  const me = useSessionStore((s) => s.me)
  const navigate = useNavigate()
  const visible = sectionsFor(me?.role, sections)

  return (
    <div className="flex min-h-screen flex-col">
      <header className="border-b border-border/40 bg-card/40 backdrop-blur-xl">
        <div className="mx-auto flex h-14 max-w-6xl items-center gap-6 px-4">
          <Link to="/" aria-label="Provider Console home">
            <ProviderBrand />
          </Link>
          <nav aria-label="Sections" className="flex items-center gap-1">
            {visible.map((s) => (
              <NavLink
                key={s.key}
                to={s.path}
                end={s.path === '/'}
                className={({ isActive }) =>
                  cn(
                    'rounded-md px-3 py-1.5 text-sm transition-colors',
                    isActive ? 'bg-primary/10 text-primary' : 'text-muted-foreground hover:text-foreground',
                  )
                }
              >
                {s.label}
              </NavLink>
            ))}
          </nav>
          <div className="ml-auto flex items-center gap-3">
            <SessionCountdown />
            {me && (
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="sm" className="gap-2" aria-label="Account menu">
                    <span className="hidden max-w-48 truncate sm:inline">{me.email}</span>
                    <RoleBadge role={me.role} />
                    <ChevronDown className="size-4" aria-hidden />
                  </Button>
                </DropdownMenuTrigger>
                <DropdownMenuContent align="end" className="w-56">
                  <DropdownMenuLabel className="truncate font-normal text-muted-foreground">{me.email}</DropdownMenuLabel>
                  <DropdownMenuSeparator />
                  <DropdownMenuItem onSelect={() => navigate('/change-password')}>
                    <KeyRound className="size-4" aria-hidden />
                    Change password
                  </DropdownMenuItem>
                  <DropdownMenuItem onSelect={() => void signOut()}>
                    <LogOut className="size-4" aria-hidden />
                    Sign out
                  </DropdownMenuItem>
                </DropdownMenuContent>
              </DropdownMenu>
            )}
          </div>
        </div>
      </header>
      <main className="mx-auto w-full max-w-6xl flex-1 px-4 py-8">
        <Outlet />
      </main>
    </div>
  )
}
