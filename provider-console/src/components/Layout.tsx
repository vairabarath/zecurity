import { ChevronDown, CircleUser, KeyRound, LogOut } from 'lucide-react'
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
        {/* Below md the nav moves to its own row (and scrolls sideways if it
            ever has to), so the header never pushes the page wider than the
            screen. */}
        <div className="mx-auto flex max-w-6xl flex-wrap items-center gap-x-6 gap-y-1 px-4 py-2 md:h-14 md:flex-nowrap md:py-0">
          <Link to="/" aria-label="Provider Console home" className="shrink-0">
            <ProviderBrand />
          </Link>
          <nav
            aria-label="Sections"
            className="order-last -mx-1 flex w-full items-center gap-1 overflow-x-auto md:order-none md:mx-0 md:w-auto"
          >
            {visible.map((s) => (
              <NavLink
                key={s.key}
                to={s.path}
                end={s.path === '/'}
                className={({ isActive }) =>
                  cn(
                    'whitespace-nowrap rounded-md px-3 py-1.5 text-sm transition-colors',
                    isActive ? 'bg-primary/10 text-primary' : 'text-muted-foreground hover:text-foreground',
                  )
                }
              >
                {s.label}
              </NavLink>
            ))}
          </nav>
          <div className="ml-auto flex shrink-0 items-center gap-3">
            {/* Home also shows the countdown, so phones can do without it here. */}
            <SessionCountdown className="hidden sm:inline-flex" />
            {me && (
              <DropdownMenu>
                <DropdownMenuTrigger asChild>
                  <Button variant="ghost" size="sm" className="gap-2" aria-label="Account menu">
                    <span className="hidden max-w-48 truncate lg:inline">{me.email}</span>
                    <span className="hidden sm:inline-flex">
                      <RoleBadge role={me.role} />
                    </span>
                    <CircleUser className="size-4 sm:hidden" aria-hidden />
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
