import { useSessionStore } from '@/auth/session'
import { RoleBadge } from '@/components/RoleBadge'
import { SessionCountdown } from '@/components/SessionCountdown'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { formatDateTime } from '@/lib/format'

// Landing page (C4): who is signed in, with which role, since when, and when
// the session ends. relay-ops lands here until the Relays page (Phase P).
export function Home() {
  const me = useSessionStore((s) => s.me)
  const expiresAt = useSessionStore((s) => s.expiresAt)
  if (!me) return null

  return (
    <div className="flex flex-col gap-6">
      <div>
        <h1 className="font-display text-2xl font-semibold tracking-tight">Provider Console</h1>
        <p className="text-sm text-muted-foreground">Signed in to the Zecurity provider plane.</p>
      </div>
      <Card className="max-w-xl" role="region" aria-labelledby="session-heading">
        <CardHeader>
          <CardTitle>
            <h2 id="session-heading">Your session</h2>
          </CardTitle>
          <CardDescription>There is no automatic renewal; sign in again when it expires.</CardDescription>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-3 text-sm">
            <dt className="text-muted-foreground">Email</dt>
            <dd className="truncate">{me.email}</dd>
            <dt className="text-muted-foreground">Role</dt>
            <dd>
              <RoleBadge role={me.role} />
            </dd>
            {/* last_login_at is stamped by the sign-in that started this session. */}
            <dt className="text-muted-foreground">Signed in at</dt>
            <dd>{formatDateTime(me.last_login_at)}</dd>
            <dt className="text-muted-foreground">Session expires</dt>
            <dd className="flex flex-wrap items-center gap-2">
              <span>{formatDateTime(expiresAt)}</span>
              <SessionCountdown />
            </dd>
          </dl>
        </CardContent>
      </Card>
    </div>
  )
}
