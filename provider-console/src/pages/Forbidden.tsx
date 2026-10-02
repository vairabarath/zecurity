import { ShieldOff } from 'lucide-react'
import { Link } from 'react-router-dom'

// Shown when the role doesn't allow a section (RequireRole) or the server
// answers 403. The session stays: only the requested page is refused.
export function Forbidden() {
  return (
    <div className="flex flex-col items-center gap-3 py-16 text-center">
      <ShieldOff className="size-8 text-muted-foreground" aria-hidden />
      <h1 className="font-display text-xl font-semibold">You don’t have access to this page</h1>
      <p className="max-w-sm text-sm text-muted-foreground">
        Your role doesn’t include this section. Ask a super-admin if you need it.
      </p>
      <Link to="/" className="text-sm text-primary underline-offset-4 hover:underline">
        Back to Home
      </Link>
    </div>
  )
}
