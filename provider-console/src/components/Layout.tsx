import type { ComponentType, SVGProps } from 'react'
import { Building2, ChevronsUpDown, KeyRound, LayoutGrid, LogOut, RadioTower, ScrollText, ShieldCheck, Users } from 'lucide-react'
import { Link, NavLink, Outlet, useNavigate } from 'react-router-dom'
import { signOut } from '@/auth/flows'
import { ROLE_LABELS, SECTIONS, sectionsFor, type Section } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { ProviderBrand } from '@/components/ProviderBrand'
import { SessionMeter } from '@/components/SessionMeter'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { cn } from '@/lib/utils'

const ICONS: Record<string, ComponentType<SVGProps<SVGSVGElement>>> = {
  home: LayoutGrid,
  relays: RadioTower,
  tenants: Building2,
  certificates: ShieldCheck,
  audit: ScrollText,
  users: Users,
}

/** Initials for the avatar: the first two letters of the email's local part. */
function initials(email: string): string {
  return email.split('@')[0].replace(/[^a-z0-9]/gi, '').slice(0, 2).toUpperCase() || '?'
}

// The signed-in shell: a sidebar on wide screens, a top bar with a sideways-
// scrolling nav on narrow ones. The nav lists only the sections the role may
// open (roles.ts); that is a convenience, the controller authorizes every call.
export function Layout({ sections = SECTIONS }: { sections?: readonly Section[] }) {
  const me = useSessionStore((s) => s.me)
  const navigate = useNavigate()
  const visible = sectionsFor(me?.role, sections)

  return (
    <div className="flex min-h-screen flex-col lg:flex-row">
      <aside className="flex shrink-0 flex-row flex-wrap items-center gap-x-3 gap-y-2 border-b border-[#1a202b] bg-sidebar px-3.5 py-3 lg:sticky lg:top-0 lg:h-screen lg:w-62 lg:flex-col lg:flex-nowrap lg:items-stretch lg:gap-5 lg:overflow-y-auto lg:border-b-0 lg:border-r lg:py-5">
        <div className="order-1 flex items-center px-1 lg:px-2 lg:py-1">
          <Link to="/" aria-label="Provider Console home" className="rounded-lg">
            <ProviderBrand />
          </Link>
        </div>

        <nav aria-label="Sections" className="no-scrollbar order-3 -mx-1 flex w-full gap-1 overflow-x-auto px-1 lg:order-2 lg:mx-0 lg:flex-col lg:gap-0.5 lg:overflow-visible lg:px-0">
          {visible.map((s, i) => {
            const Icon = ICONS[s.key] ?? LayoutGrid
            const heading = s.group && s.group !== visible[i - 1]?.group ? s.group : undefined
            return (
              <div key={s.key} className="contents">
                {heading && (
                  <div aria-hidden className="hidden px-3 pb-1.5 pt-3.5 text-xs font-medium tracking-wide text-muted-foreground lg:block">
                    {heading}
                  </div>
                )}
                <NavLink
                  to={s.path}
                  end={s.path === '/'}
                  className={({ isActive }) =>
                    cn(
                      'flex min-h-10 shrink-0 items-center gap-2.5 whitespace-nowrap rounded-lg px-3 text-sm font-medium transition-colors',
                      isActive ? 'bg-primary-soft text-[#ece9ff]' : 'text-subtle-foreground hover:bg-muted hover:text-foreground',
                    )
                  }
                >
                  <Icon className="size-[18px] shrink-0" strokeWidth={1.8} aria-hidden />
                  {s.label}
                </NavLink>
              </div>
            )
          })}
        </nav>

        {me && (
          <div className="order-2 ml-auto flex flex-col gap-3 lg:order-3 lg:ml-0 lg:mt-auto lg:rounded-xl lg:border lg:border-[#1f2633] lg:bg-card lg:p-3">
            <DropdownMenu>
              <DropdownMenuTrigger
                aria-label="Account menu"
                className="flex items-center gap-2.5 rounded-lg p-1 lg:w-full text-left outline-none hover:bg-muted focus-visible:ring-2 focus-visible:ring-ring"
              >
                <span aria-hidden className="inline-flex size-8 shrink-0 items-center justify-center rounded-full bg-[#2a2550] text-[13px] font-semibold text-[#d6cfff]">
                  {initials(me.email)}
                </span>
                <span className="hidden min-w-0 flex-1 flex-col sm:flex">
                  <span className="truncate text-[13px] font-medium">{me.email}</span>
                  <span className="text-xs text-[#c2b8ff]">{ROLE_LABELS[me.role] ?? me.role}</span>
                </span>
                <ChevronsUpDown className="hidden size-4 shrink-0 text-muted-foreground sm:block" aria-hidden />
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end" side="top" className="w-56">
                <DropdownMenuLabel className="truncate font-normal text-muted-foreground">{me.email}</DropdownMenuLabel>
                <DropdownMenuSeparator />
                <DropdownMenuItem onSelect={() => navigate('/change-password')}>
                  <KeyRound aria-hidden />
                  Change password
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => void signOut()}>
                  <LogOut aria-hidden />
                  Sign out
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
            <SessionMeter className="hidden lg:flex" />
          </div>
        )}
      </aside>

      <main className="min-w-0 flex-1 px-4 pb-14 pt-6 sm:px-6 lg:px-9 lg:pt-7">
        <div className="mx-auto w-full max-w-[1180px]">
          <Outlet />
        </div>
      </main>
    </div>
  )
}
