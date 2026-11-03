import { ShieldOff } from 'lucide-react'
import { Link } from 'react-router-dom'

// Shown when the role doesn't allow a section (RequireRole) or the server
// answers 403. The session stays: only the requested page is refused.
export function Forbidden() {
  return (
    <div className="flex flex-col items-center gap-3 py-20 text-center">
      <span className="mb-1 inline-flex size-12 items-center justify-center rounded-xl bg-primary-soft text-[#c2b8ff]">
        <ShieldOff className="size-6" aria-hidden />
      </span>
      <h1 className="font-display text-xl font-semibold">You don’t have access to this page</h1>
      <p className="max-w-sm text-sm text-subtle-foreground">Your role doesn’t include this section. Ask a super-admin if you need it.</p>
      <Link to="/" className="text-sm text-primary hover:underline">
        Back to Overview
      </Link>
    </div>
  )
}
