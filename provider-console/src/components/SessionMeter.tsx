import { useEffect, useState } from 'react'
import { useSessionStore } from '@/auth/session'
import { formatRemaining } from '@/lib/format'
import { cn } from '@/lib/utils'

const WARN_BELOW_MS = 2 * 60 * 1000
// The controller's full session lifetime (ProviderSessionTTL). Only scales
// the bar; the countdown itself comes from the session's own expiry.
const SESSION_MS = 15 * 60 * 1000

// Time left on the session as text plus a bar, for the sidebar. Display only,
// like SessionCountdown: the session store ends the session at expiry.
export function SessionMeter({ className }: { className?: string }) {
  const expiresAt = useSessionStore((s) => s.expiresAt)
  const [now, setNow] = useState(() => Date.now())

  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [])

  if (expiresAt === undefined) return null
  const remaining = Math.max(0, expiresAt - now)
  const warn = remaining < WARN_BELOW_MS
  const pct = Math.min(100, (remaining / SESSION_MS) * 100)

  return (
    <div className={cn('flex flex-col gap-1.5', className)} title="Time until this session expires. Sign in again when it does.">
      <div className="flex justify-between text-xs text-muted-foreground">
        <span>Session</span>
        <span className={cn('font-mono tabular-nums', warn && 'text-warning-text')} data-warning={warn || undefined}>
          <span aria-label="Session time remaining">{formatRemaining(remaining)}</span> left
        </span>
      </div>
      <div className="h-1 rounded-full bg-border" aria-hidden>
        <div className={cn('h-1 rounded-full', warn ? 'bg-warning' : 'bg-primary')} style={{ width: `${pct}%` }} />
      </div>
    </div>
  )
}
