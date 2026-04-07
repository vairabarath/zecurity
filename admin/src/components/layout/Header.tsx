import { useNavigate } from 'react-router-dom'
import { useAuthStore } from '@/store/auth'
import { apolloClient } from '@/apollo/client'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Badge } from '@/components/ui/badge'

export function Header() {
  const navigate = useNavigate()
  const { user, clearAuth } = useAuthStore()

  async function handleSignOut() {
    // Clear Apollo cache so no stale data persists
    await apolloClient.clearStore()
    clearAuth()
    navigate('/login', { replace: true })
    // Note: this does NOT call a sign-out endpoint.
    // The refresh cookie will expire on its own (7 days).
    // A sign-out endpoint that deletes the Redis refresh key
    // can be added later for immediate invalidation.
  }

  const initials = user?.email?.slice(0, 2).toUpperCase() ?? '??'

  return (
    <header className="h-14 border-b flex items-center justify-between px-6">
      <div />
      <div className="flex items-center gap-3">
        {user && (
          <Badge variant="outline" className="text-xs">
            {user.role}
          </Badge>
        )}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Avatar className="cursor-pointer h-8 w-8">
              <AvatarFallback className="text-xs">{initials}</AvatarFallback>
            </Avatar>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <div className="px-2 py-1.5 text-xs text-muted-foreground">
              {user?.email}
            </div>
            <DropdownMenuItem onClick={handleSignOut}>
              Sign out
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </header>
  )
}
