import { useEffect, useState } from 'react'
import { Clock } from 'lucide-react'
import { useSessionStore } from '@/auth/session'
import { formatRemaining } from '@/lib/format'
import { cn } from '@/lib/utils'

const WARN_BELOW_MS = 2 * 60 * 1000

// Time left on the current session. Display only: the session store's own
// timer ends the session at expiry and the guards then show Login. There is
// no refresh token (D-28), so the operator signs in again.
export function SessionCountdown({ className }: { className?: string }) {
  const expiresAt = useSessionStore((s) => s.expiresAt)
  const [now, setNow] = useState(() => Date.now())

  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [])

  if (expiresAt === undefined) return null
  const remaining = expiresAt - now
  const warn = remaining < WARN_BELOW_MS

  return (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 font-mono text-xs tabular-nums',
        warn ? 'text-warning' : 'text-muted-foreground',
        className,
      )}
      title="Time until this session expires. Sign in again when it does."
      data-warning={warn || undefined}
    >
      <Clock className="size-3.5" aria-hidden />
      <span aria-label="Session time remaining">{formatRemaining(remaining)}</span>
    </span>
  )
}
