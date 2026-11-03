import { useState, type ReactNode } from 'react'
import { ChevronRight, RefreshCw } from 'lucide-react'
import { Link } from 'react-router-dom'
import { getCertificates, listRelays, queryAudit } from '@/api/reads'
import type { AuditEntry, CertificateEntry, CertificateExpiryResult, Page, RelaySummary } from '@/api/types'
import { hasRole } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { ExpiryBadge } from '@/components/ExpiryBadge'
import { PageHeader } from '@/components/PageHeader'
import { RoleBadge } from '@/components/RoleBadge'
import { SessionCountdown } from '@/components/SessionCountdown'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { bucketFor, formatRelative } from '@/lib/expiry'
import { formatDateTime } from '@/lib/format'
import { useDetailQuery, type DetailState } from '@/lib/useDetailQuery'
import { cn } from '@/lib/utils'

// Overview: the landing page for both roles. Built only from existing read
// APIs, each requested once per visit (useDetailQuery: StrictMode-safe, no
// polling, no focus refetch) and again only on Refresh:
//   - GET /provider/relays, every page (relay.read: both roles);
//   - super-admin only: GET /provider/certificates?within=168h (summary and
//     the soonest expiries) and GET /provider/audit?limit=6.
// relay-ops never requests certificates or audit (it would be refused).
// Nothing here opens a tenant, so this page writes no audit entry.

const RELAY_PAGE_LIMIT = 200
const MAX_RELAY_PAGES = 10
const HIGH_USE_PCT = 85

interface Fleet {
  relays: RelaySummary[]
  /** More relays exist than were read (MAX_RELAY_PAGES reached). */
  truncated: boolean
}

async function fetchFleet(): Promise<Fleet> {
  const relays: RelaySummary[] = []
  let cursor: string | undefined
  for (let i = 0; i < MAX_RELAY_PAGES; i++) {
    const page: Page<RelaySummary> = await listRelays({ limit: RELAY_PAGE_LIMIT, cursor })
    relays.push(...page.items)
    if (!page.next_cursor) return { relays, truncated: false }
    cursor = page.next_cursor
  }
  return { relays, truncated: true }
}

const fetchSoonCertificates = () => getCertificates({ within: '168h', limit: 6 })
const fetchRecentAudit = () => queryAudit({ limit: 6 })
const skip = () => Promise.resolve(null)

function greeting(hour: number): string {
  if (hour < 12) return 'Good morning'
  if (hour < 18) return 'Good afternoon'
  return 'Good evening'
}

/** "barath.v@…" → "Barath": a friendly name from the email, nothing more. */
function firstName(email: string): string {
  const part = email.split('@')[0].split(/[._-]/)[0]
  return part ? part[0].toUpperCase() + part.slice(1) : email
}

export function Home() {
  const me = useSessionStore((s) => s.me)
  const expiresAt = useSessionStore((s) => s.expiresAt)
  const [round, setRound] = useState(0)
  const isSuperAdmin = hasRole(me?.role, ['super-admin'])

  const fleet = useDetailQuery(`relays|${round}`, fetchFleet)
  const certs = useDetailQuery<CertificateExpiryResult | null>(`certs|${round}|${isSuperAdmin}`, isSuperAdmin ? fetchSoonCertificates : skip)
  const audit = useDetailQuery<Page<AuditEntry> | null>(`audit|${round}|${isSuperAdmin}`, isSuperAdmin ? fetchRecentAudit : skip)

  if (!me) return null
  const refresh = () => setRound((n) => n + 1)

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title={`${greeting(new Date().getHours())}, ${firstName(me.email)}`}
        description="The state of the relay fleet and what needs attention."
        actions={
          <Button variant="outline" onClick={refresh} aria-label="Refresh overview">
            <RefreshCw aria-hidden />
            Refresh
          </Button>
        }
      />

      <FleetTiles state={fleet.state} onRetry={refresh}>
        {isSuperAdmin && <CertificateTile state={certs.state} />}
      </FleetTiles>

      <div className="flex flex-wrap items-start gap-4">
        <Card className="min-w-0 flex-[3_1_520px]" aria-labelledby="attention-heading">
          <div className="flex items-center justify-between border-b border-border px-5 py-4">
            <h2 id="attention-heading" className="font-semibold">
              Needs attention
            </h2>
          </div>
          <Attention fleet={fleet.state} certs={isSuperAdmin ? certs.state : undefined} onRetry={refresh} />
        </Card>

        <div className="flex min-w-0 flex-[2_1_360px] flex-col gap-4">
          {isSuperAdmin ? (
            <RecentActivity state={audit.state} onRetry={refresh} />
          ) : (
            <Card className="flex flex-col gap-2 p-5">
              <h2 className="font-semibold">Your access</h2>
              <p className="text-sm leading-relaxed text-subtle-foreground">
                As relay ops you can see the relay fleet. Tenants, certificates, the audit log and operators are for super-admins.
              </p>
              <Button asChild variant="outline" className="mt-1.5 self-start">
                <Link to="/relays">Open relays</Link>
              </Button>
            </Card>
          )}

          <Card role="region" aria-labelledby="session-heading">
            <div className="border-b border-border px-5 py-4">
              <h2 id="session-heading" className="font-semibold">
                Your session
              </h2>
              <p className="text-[13px] text-muted-foreground">There is no automatic renewal; sign in again when it expires.</p>
            </div>
            <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-3 px-5 py-4 text-sm">
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
          </Card>
        </div>
      </div>
    </div>
  )
}

function Tile({ label, children, className }: { label: string; children: ReactNode; className?: string }) {
  return (
    <Card className={cn('flex min-w-0 flex-[1_1_200px] flex-col gap-2 px-5 py-[18px]', className)}>
      <span className="text-xs font-medium tracking-wide text-muted-foreground">{label}</span>
      {children}
    </Card>
  )
}

function Big({ children }: { children: ReactNode }) {
  return <span className="text-[30px] font-semibold leading-tight tracking-[-0.02em]">{children}</span>
}

function Sub({ children }: { children: ReactNode }) {
  return <span className="text-[12.5px] text-muted-foreground">{children}</span>
}

function LoadError({ what, onRetry }: { what: string; onRetry: () => void }) {
  return (
    <span className="flex flex-wrap items-center gap-2 text-[13px] text-critical-text">
      Couldn’t load {what}.
      <Button variant="link" size="sm" className="h-auto p-0" onClick={onRetry}>
        Retry
      </Button>
    </span>
  )
}

const STATUS_DOT: Record<string, string> = { active: 'bg-secure', inactive: 'bg-warning', pending: 'bg-info', revoked: 'bg-critical' }

function FleetTiles({ state, onRetry, children }: { state: DetailState<Fleet>; onRetry: () => void; children?: ReactNode }) {
  if (state.status !== 'ready') {
    return (
      <div className="flex flex-wrap gap-4">
        {['Relays', 'Relay connections', 'Connectors attached to relays'].map((label) => (
          <Tile key={label} label={label}>
            {state.status === 'loading' ? <Skeleton className="h-9 w-24" /> : <LoadError what="relays" onRetry={onRetry} />}
          </Tile>
        ))}
        {children}
      </div>
    )
  }

  const { relays, truncated } = state.data
  const byStatus = new Map<string, number>()
  for (const r of relays) byStatus.set(r.status, (byStatus.get(r.status) ?? 0) + 1)
  const active = relays.filter((r) => r.status === 'active')
  const used = active.reduce((n, r) => n + r.connection_count, 0)
  const max = active.reduce((n, r) => n + r.max_connections, 0)
  const pct = max > 0 ? Math.round((used / max) * 100) : 0
  const low = active.filter((r) => r.capacity_label === 'low').length
  const attached = active.reduce((n, r) => n + r.attached_connectors, 0)
  const order = ['active', 'inactive', 'pending', 'revoked'].filter((s) => byStatus.get(s))

  return (
    <div className="flex flex-wrap gap-4">
      <Tile label="Relays">
        <Big>
          {active.length} <span className="text-base font-medium text-muted-foreground">of {relays.length} active</span>
        </Big>
        {relays.length > 0 && (
          <div className="flex h-2 gap-0.5 overflow-hidden rounded-full bg-border" aria-hidden>
            {order.map((s) => (
              <span key={s} className={STATUS_DOT[s]} style={{ flex: byStatus.get(s) }} />
            ))}
          </div>
        )}
        <span className="flex flex-wrap gap-x-3 gap-y-1 text-[12.5px] text-muted-foreground">
          {order.map((s) => (
            <span key={s} className="inline-flex items-center gap-1.5">
              <span aria-hidden className={cn('size-1.5 rounded-full', STATUS_DOT[s])} />
              {byStatus.get(s)} {s}
            </span>
          ))}
          {truncated && <span>(first {RELAY_PAGE_LIMIT * MAX_RELAY_PAGES} only)</span>}
        </span>
      </Tile>
      <Tile label="Relay connections">
        <Big>
          {used.toLocaleString()} <span className="text-base font-medium text-muted-foreground">/ {max.toLocaleString()}</span>
        </Big>
        <div className="h-2 rounded-full bg-border" aria-hidden>
          <div className={cn('h-2 rounded-full', pct >= HIGH_USE_PCT ? 'bg-warning' : 'bg-primary')} style={{ width: `${pct}%` }} />
        </div>
        <Sub>
          Active relays · {pct}% of reported max{low > 0 ? ` · ${low} in the low capacity tier` : ''}
        </Sub>
      </Tile>
      <Tile label="Connectors attached to relays">
        <Big>{attached.toLocaleString()}</Big>
        <Sub>Sum across active relays</Sub>
      </Tile>
      {children}
    </div>
  )
}

function CertificateTile({ state }: { state: DetailState<CertificateExpiryResult | null> }) {
  if (state.status !== 'ready' || !state.data) {
    return (
      <Tile label="Certificates needing action">
        {state.status === 'loading' ? <Skeleton className="h-9 w-16" /> : <Sub>Couldn’t load certificates.</Sub>}
      </Tile>
    )
  }
  const s = state.data.summary
  const urgent = s.expired + s.lt_24h + s.lt_7d
  return (
    <Link to="/certificates" className="flex min-w-0 flex-[1_1_200px] rounded-xl focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
      <Tile label="Certificates needing action" className="transition-colors hover:border-border-strong">
        <Big>
          <span className={urgent > 0 ? 'text-critical-text' : undefined}>{urgent}</span>
        </Big>
        <Sub>
          {s.expired} expired · {s.lt_24h} within 24 hours · {s.lt_7d} within 7 days
        </Sub>
      </Tile>
    </Link>
  )
}

interface AttentionItem {
  key: string
  to: string
  tag: ReactNode
  title: string
  note: string
}

const KIND_LABELS: Record<string, string> = {
  client_device: 'Client device',
  connector: 'Connector',
  controller_grpc: 'Controller',
  intermediate: 'Intermediate CA',
  relay: 'Relay',
  root: 'Root CA',
  shield: 'Shield',
  workspace_ca: 'Workspace CA',
}

const DRAIN_NOTES: Record<NonNullable<RelaySummary['drain_state']>, string> = {
  migrating: 'Connectors still attached',
  streams_open: 'Connections still open',
  unverified: 'Not confirmed yet',
  drained: 'Drained, as of the last heartbeat',
}

function drainNote(r: RelaySummary): string {
  const note = r.drain_state ? DRAIN_NOTES[r.drain_state] : 'Draining'
  return r.drain_state === 'migrating' ? `${note}: ${r.attached_connectors.toLocaleString()}` : note
}

function relayItems(relays: RelaySummary[]): AttentionItem[] {
  const items: AttentionItem[] = []
  for (const r of relays) {
    const to = `/relays/${encodeURIComponent(r.id)}`
    if (r.status === 'inactive') {
      items.push({ key: `${r.id}:inactive`, to, tag: <Badge variant="warning">Inactive</Badge>, title: `${r.name} is inactive`, note: `Last heartbeat ${formatRelative(r.last_heartbeat_at)}` })
    }
    if (r.draining_since) {
      items.push({ key: `${r.id}:draining`, to, tag: <Badge variant="warning">Draining</Badge>, title: `${r.name} is draining`, note: drainNote(r) })
    }
    if (r.status === 'active' && r.capacity_label === 'low') {
      items.push({
        key: `${r.id}:low`,
        to,
        tag: <Badge variant="destructive">Low tier</Badge>,
        title: `${r.name} is in the low capacity tier`,
        note: `${r.connection_count.toLocaleString()} / ${r.max_connections.toLocaleString()} connections`,
      })
    }
    const b = r.cert_not_after && r.status !== 'revoked' ? bucketFor(r.cert_not_after) : null
    if (b === 'expired' || b === 'lt_24h' || b === 'lt_7d') {
      items.push({ key: `${r.id}:cert`, to, tag: <ExpiryBadge notAfter={r.cert_not_after} />, title: `${r.name} certificate`, note: `Expires ${formatDateTime(r.cert_not_after)}` })
    }
  }
  return items
}

function certificateItems(entries: CertificateEntry[]): AttentionItem[] {
  // Relay certificates are already covered by the relay items above.
  return entries
    .filter((c) => c.kind !== 'relay' && (c.bucket === 'expired' || c.bucket === 'lt_24h' || c.bucket === 'lt_7d'))
    .map((c) => ({
      key: `${c.kind}:${c.entity_id}`,
      to: '/certificates',
      tag: <ExpiryBadge notAfter={c.not_after} bucket={c.bucket} />,
      title: `${KIND_LABELS[c.kind] ?? c.kind} ${c.entity_name ?? ''}`.trim(),
      note: `${c.tenant_id ? `Tenant ${c.tenant_id.slice(0, 8)}…` : 'Platform'} · expires ${formatDateTime(c.not_after)}`,
    }))
}

function Attention({
  fleet,
  certs,
  onRetry,
}: {
  fleet: DetailState<Fleet>
  certs?: DetailState<CertificateExpiryResult | null>
  onRetry: () => void
}) {
  if (fleet.status === 'loading' || certs?.status === 'loading') {
    return (
      <div aria-busy="true" aria-label="Loading attention items" className="flex flex-col gap-2 p-5">
        <Skeleton className="h-8 w-full" />
        <Skeleton className="h-8 w-2/3" />
      </div>
    )
  }
  const items = [
    ...(certs?.status === 'ready' && certs.data ? certificateItems(certs.data.items) : []),
    ...(fleet.status === 'ready' ? relayItems(fleet.data.relays) : []),
  ]
  const failed = fleet.status !== 'ready' || (certs !== undefined && certs.status !== 'ready')

  return (
    <div>
      {failed && (
        <div className="px-5 py-3">
          <LoadError what="everything" onRetry={onRetry} />
        </div>
      )}
      {items.length === 0 && !failed && <p className="px-5 py-6 text-sm text-muted-foreground">Nothing needs attention right now.</p>}
      <ul>
        {items.map((it) => (
          <li key={it.key} className="border-b border-[#1a202b] last:border-0">
            <Link to={it.to} className="flex items-center gap-3 px-5 py-3 text-sm hover:bg-[#141924]">
              <span className="flex min-w-[84px] justify-start">{it.tag}</span>
              <span className="flex min-w-0 flex-1 flex-col gap-0.5">
                <span className="truncate font-medium">{it.title}</span>
                <span className="truncate text-[13px] text-muted-foreground">{it.note}</span>
              </span>
              <ChevronRight className="size-4 shrink-0 text-muted-foreground" aria-hidden />
            </Link>
          </li>
        ))}
      </ul>
    </div>
  )
}

function RecentActivity({ state, onRetry }: { state: DetailState<Page<AuditEntry> | null>; onRetry: () => void }) {
  return (
    <Card aria-labelledby="activity-heading">
      <div className="flex items-center justify-between border-b border-border px-5 py-4">
        <h2 id="activity-heading" className="font-semibold">
          Recent operator activity
        </h2>
        <Link to="/audit" className="text-[13px] text-primary hover:underline">
          Audit log
        </Link>
      </div>
      {state.status === 'loading' && (
        <div className="flex flex-col gap-2 p-5">
          <Skeleton className="h-8 w-full" />
          <Skeleton className="h-8 w-3/4" />
        </div>
      )}
      {state.status !== 'loading' && state.status !== 'ready' && (
        <div className="px-5 py-4">
          <LoadError what="activity" onRetry={onRetry} />
        </div>
      )}
      {state.status === 'ready' && state.data && (
        <ul>
          {state.data.items.length === 0 && <li className="px-5 py-6 text-sm text-muted-foreground">No operator activity yet.</li>}
          {state.data.items.map((e) => (
            <li key={e.id} className="flex items-start gap-3 border-b border-[#1a202b] px-5 py-3 last:border-0">
              <span className="flex min-w-0 flex-1 flex-col gap-0.5">
                <span className="truncate font-mono text-[13px]">{e.action}</span>
                <span className="truncate text-[12.5px] text-muted-foreground">{e.provider_email}</span>
              </span>
              <span className="whitespace-nowrap font-mono text-xs text-muted-foreground" title={formatDateTime(e.created_at)}>
                {formatRelative(e.created_at)}
              </span>
            </li>
          ))}
        </ul>
      )}
    </Card>
  )
}
