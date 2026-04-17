import { useState } from 'react'
import { useNavigate, useLocation } from 'react-router-dom'
import { useAuthStore } from '@/store/auth'
import { apolloClient } from '@/apollo/client'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { 
  LogOut, 
  ChevronDown, 
  Search,
  Bell,
  Settings,
  Shield,
} from 'lucide-react'

export function Header() {
  const navigate = useNavigate()
  const location = useLocation()
  const { user, clearAuth } = useAuthStore()
  const [searchFocused, setSearchFocused] = useState(false)

  async function handleSignOut() {
    await apolloClient.clearStore()
    clearAuth()
    navigate('/login', { replace: true })
  }

  const initials = user?.email?.slice(0, 2).toUpperCase() ?? '??'
  const pageName = location.pathname.split('/')[1] || 'Dashboard'

  return (
    <header className="relative h-14 flex items-center justify-between px-6 bg-card/80 border-b border-border">
      <div className="flex items-center gap-3">
        <span className="text-sm font-semibold text-foreground capitalize">
          {pageName.replace('-', ' ')}
        </span>
      </div>

      {!searchFocused && (
        <button
          onClick={() => setSearchFocused(true)}
          className="absolute left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2 flex items-center gap-2 px-3 py-1.5 rounded-lg border border-border bg-muted text-muted-foreground hover:text-foreground hover:bg-muted/80 transition-all text-sm"
        >
          <Search className="w-4 h-4" />
          Search...
          <kbd className="ml-1 px-1.5 py-0.5 text-xs font-mono bg-background rounded border border-border">
            ⌘K
          </kbd>
        </button>
      )}

      {searchFocused && (
        <div className="absolute left-1/2 top-1/2 -translate-x-1/2 -translate-y-1/2">
          <input
            placeholder="Search..."
            className="w-64 px-3 py-1.5 rounded-lg border border-primary bg-white text-sm outline-none focus:ring-2 focus:ring-primary/30"
            autoFocus
            onBlur={() => setSearchFocused(false)}
          />
        </div>
      )}

      <div className="flex items-center gap-3">
        <button className="relative flex items-center justify-center w-9 h-9 rounded-lg hover:bg-muted transition-colors">
          <Bell className="w-4 h-4 text-muted-foreground" />
          <span className="absolute top-1.5 right-1.5 w-2 h-2 rounded-full bg-primary" />
        </button>

        <button 
          onClick={() => navigate('/settings')}
          className="flex items-center justify-center w-9 h-9 rounded-lg hover:bg-muted transition-colors"
        >
          <Settings className="w-4 h-4 text-muted-foreground" />
        </button>

        <div className="h-6 w-px bg-border" />

        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <button className="flex items-center gap-2 rounded-lg px-1.5 py-1 hover:bg-muted transition-colors outline-none">
              <Avatar className="h-7 w-7 border border-border">
                <AvatarFallback className="text-xs font-medium bg-primary/10 text-primary">
                  {initials}
                </AvatarFallback>
              </Avatar>
              <ChevronDown className="w-4 h-4 text-muted-foreground" />
            </button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-56">
            <div className="px-3 py-2.5 border-b border-border">
              <div className="flex items-center gap-2">
                <Shield className="w-4 h-4 text-primary" />
                <p className="text-sm font-medium text-foreground">{user?.email}</p>
              </div>
              <p className="text-xs text-muted-foreground mt-0.5 ml-6">{user?.role}</p>
            </div>
            <DropdownMenuItem 
              onClick={() => navigate('/settings')}
              className="cursor-pointer"
            >
              <Settings className="w-4 h-4 mr-2" />
              Settings
            </DropdownMenuItem>
            <DropdownMenuItem 
              onClick={handleSignOut} 
              className="text-destructive cursor-pointer"
            >
              <LogOut className="w-4 h-4 mr-2" />
              Sign out
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </header>
  )
}