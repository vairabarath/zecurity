import { useState } from 'react'
import { RefreshCw, X } from 'lucide-react'
import { Link, Navigate, useSearchParams } from 'react-router-dom'
import { listTimeline, type TimelineItem } from '@/api/alerts'
import { can } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { FilterChips } from '@/components/FilterChips'
import { PageHeader } from '@/components/PageHeader'
import { Pager } from '@/components/Pager'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { actionLabel, kindLabel, SEVERITY_LABELS, severityVariant } from '@/lib/alert-format'
import { formatDateTime } from '@/lib/format'
import { useDetailQuery } from '@/lib/useDetailQuery'

// Incident timeline (Sprint 24 T1, F27). Read-only: alert events (A1) and
// provider audit rows (F17: operator actions, pool failover and failback) in
// one list, newest first. It stores and writes nothing.
//
// Visibility is the controller's (decide()): super-admins get everything;
// relay-ops get relay and pool alert rows and the audit rows of relay and
// pool operations (actor and reason; D-55), never tenant rows or names. The
// console also hides tenant focus and links from relay-ops; that is UX only.
//
// Relay status (active / inactive) is kept as current state, not history, so
// a relay going offline shows here as its relay.offline alert, not as a
// separate state change.
//
// Focus: ?relay_id= | ?pool_id= | ?tenant_id= (one at a time; pool_id also
// matches the relays now in the pool). ?focus_name= is the label only.

type FocusKey = 'relay_id' | 'pool_id' | 'tenant_id'
const FOCUS_KEYS: FocusKey[] = ['relay_id', 'pool_id', 'tenant_id']
const FOCUS_LABELS: Record<FocusKey, string> = { relay_id: 'Relay', pool_id: 'Pool', tenant_id: 'Tenant' }

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

const SOURCE_CHIPS = [
  { value: '', label: 'All' },
  { value: 'alert', label: 'Alerts' },
  { value: 'audit', label: 'Operator actions' },
] as const

const TRANSITION_LABELS: Record<string, string> = { fired: 'Fired', renotified: 'Reminder', resolved: 'Resolved' }

// Sprint 27 O2 (D-73): links and tenant focus follow the role's reads.
interface Perms {
  tenants: boolean
  rules: boolean
  channels: boolean
  /** The full view (audit rows of every kind): super-admin and auditor. */
  full: boolean
}

function focusFor(it: TimelineItem, perms: Perms): { key: FocusKey; id: string } | null {
  switch (it.target_type) {
    case 'relay':
      return { key: 'relay_id', id: it.target_id }
    case 'pool':
    case 'relay_pool':
      return { key: 'pool_id', id: it.target_id }
  }
  if (perms.tenants && it.tenant_id) return { key: 'tenant_id', id: it.tenant_id }
  return null
}

function targetHref(it: TimelineItem, perms: Perms): string | null {
  const id = encodeURIComponent(it.target_id)
  switch (it.target_type) {
    case 'relay':
      return `/relays/${id}`
    case 'pool':
    case 'relay_pool':
      return `/relay-pools/${id}`
    case 'tenant':
    case 'workspace':
      return perms.tenants ? `/tenants/${id}` : null
    case 'alert_rule':
      return perms.rules ? '/alerts?tab=rules' : null
    case 'alert_channel':
      return perms.channels ? '/alerts?tab=channels' : null
  }
  return null
}

export function Timeline() {
  const me = useSessionStore((s) => s.me)
  const role = me?.role
  const perms: Perms = {
    tenants: can(role, 'tenant.read'),
    rules: can(role, 'alert.rule.read'),
    channels: can(role, 'alert.channel.read'),
    full: can(role, 'audit.view'),
  }
  const [sp, setSp] = useSearchParams()
  const [round, setRound] = useState(0)

  // One valid focus from the URL; anything else is ignored (no request with
  // a filter the controller would refuse). relay-ops have no tenant focus.
  const focus = (() => {
    for (const key of FOCUS_KEYS) {
      const v = sp.get(key) ?? ''
      if (!v || !UUID_RE.test(v)) continue
      if (key === 'tenant_id' && !perms.tenants) continue
      return { key, id: v }
    }
    return null
  })()
  const focusName = sp.get('focus_name') ?? ''
  const rawSource = sp.get('source') ?? ''
  const source = rawSource === 'alert' || rawSource === 'audit' ? rawSource : ''

  const setFocus = (next: { key: FocusKey; id: string; name: string } | null) => {
    const p = new URLSearchParams(sp)
    for (const k of FOCUS_KEYS) p.delete(k)
    p.delete('focus_name')
    if (next) {
      p.set(next.key, next.id)
      if (next.name) p.set('focus_name', next.name)
    }
    setSp(p, { replace: true })
  }
  const setSource = (v: string) => {
    const p = new URLSearchParams(sp)
    if (v) p.set('source', v)
    else p.delete('source')
    setSp(p, { replace: true })
  }

  const filterKey = `${focus?.key ?? ''}|${focus?.id ?? ''}|${source}`
  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Timeline"
        description={
          perms.full
            ? 'What happened, in order: alerts fired and resolved, and what operators and the system did to relays, pools, tenants and alert rules.'
            : perms.tenants
              ? 'Tenant alerts, in order: when they fired and resolved.'
              : 'What happened to relays and pools, in order: alerts fired and resolved, and what operators and the system did.'
        }
        actions={
          <Button variant="outline" onClick={() => setRound((n) => n + 1)} aria-label="Refresh timeline">
            <RefreshCw aria-hidden />
            Refresh
          </Button>
        }
      />
      <div className="flex flex-wrap items-center justify-between gap-3">
        <FilterChips label="Show" options={SOURCE_CHIPS} value={source} onChange={setSource} />
        {focus && (
          <span className="inline-flex items-center gap-2 rounded-md border border-border px-3 py-1 text-sm" role="status">
            <span className="text-muted-foreground">{FOCUS_LABELS[focus.key]}:</span>
            <span className="font-medium">{focusName || focus.id}</span>
            <Button variant="ghost" size="sm" onClick={() => setFocus(null)} aria-label="Clear focus">
              <X aria-hidden />
            </Button>
          </span>
        )}
      </div>
      {/* keyed by filter: a new filter starts at its newest page. */}
      <TimelineTable
        key={filterKey}
        round={round}
        focus={focus}
        source={source}
        perms={perms}
        onFocus={(f) => setFocus(f)}
      />
    </div>
  )
}

function TimelineTable({
  round,
  focus,
  source,
  perms,
  onFocus,
}: {
  round: number
  focus: { key: FocusKey; id: string } | null
  source: '' | 'alert' | 'audit'
  perms: Perms
  onFocus: (f: { key: FocusKey; id: string; name: string }) => void
}) {
  const [cursors, setCursors] = useState<string[]>([''])
  const cursor = cursors[cursors.length - 1]
  const { state } = useDetailQuery(`timeline|${round}|${focus?.key}|${focus?.id}|${source}|${cursor}`, () =>
    listTimeline({ ...(focus ? { [focus.key]: focus.id } : {}), source, cursor, limit: 50 }),
  )
  switch (state.status) {
    case 'loading':
      return (
        <div aria-busy="true" aria-label="Loading timeline" className="flex flex-col gap-2">
          <Skeleton className="h-8 w-full" />
          <Skeleton className="h-8 w-2/3" />
        </div>
      )
    case 'session_ended':
      return null
    case 'forbidden':
      return <Navigate to="/forbidden" replace />
    case 'not_found':
    case 'error':
      return (
        <Alert variant="destructive">
          <AlertDescription>Couldn’t load the timeline. Use Refresh to try again.</AlertDescription>
        </Alert>
      )
  }
  const items = state.data.items
  if (items.length === 0 && cursors.length === 1) {
    return <p className="py-6 text-sm text-muted-foreground">Nothing recorded{focus ? ' for this filter' : ' yet'}.</p>
  }
  return (
    <Card>
      <Table aria-label="Timeline">
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead>Time</TableHead>
            <TableHead>Event</TableHead>
            <TableHead>What happened</TableHead>
            <TableHead>Target</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {items.map((it) => (
            <TimelineRow key={`${it.source}:${it.id}`} it={it} perms={perms} focused={focus} onFocus={onFocus} />
          ))}
        </TableBody>
      </Table>
      <Pager
        page={cursors.length}
        hasPrev={cursors.length > 1}
        hasNext={!!state.data.next_cursor}
        onPrev={() => setCursors((c) => c.slice(0, -1))}
        onNext={() => state.data.next_cursor && setCursors((c) => [...c, state.data.next_cursor!])}
      />
    </Card>
  )
}

function TimelineRow({
  it,
  perms,
  focused,
  onFocus,
}: {
  it: TimelineItem
  perms: Perms
  focused: { key: FocusKey; id: string } | null
  onFocus: (f: { key: FocusKey; id: string; name: string }) => void
}) {
  const isAlert = it.source === 'alert'
  const title = isAlert ? (it.summary ?? '') : actionLabel(it.action ?? '')
  const href = targetHref(it, perms)
  const focus = focusFor(it, perms)
  const canFocus = focus && !(focused && focused.key === focus.key && focused.id === focus.id)
  const focusName = focus?.key === 'tenant_id' && it.target_type !== 'tenant' && it.target_type !== 'workspace' ? '' : it.target_name
  return (
    <TableRow aria-label={title}>
      <TableCell className="whitespace-nowrap font-mono text-xs">{formatDateTime(it.at)}</TableCell>
      <TableCell>
        {isAlert ? (
          <Badge variant={it.transition === 'resolved' ? 'success' : severityVariant(it.severity ?? 'info')}>
            {TRANSITION_LABELS[it.transition ?? ''] ?? it.transition}
          </Badge>
        ) : (
          <Badge variant="secondary">{it.actor?.startsWith('system:') ? 'System' : 'Operator'}</Badge>
        )}
      </TableCell>
      <TableCell>
        <span className="inline-flex flex-col gap-0.5">
          <span>{title}</span>
          <span className="text-[12.5px] text-muted-foreground">
            {isAlert
              ? `${kindLabel(it.kind ?? '')} · ${SEVERITY_LABELS[it.severity ?? 'info'] ?? it.severity}`
              : [it.actor, it.reason ? `Reason: ${it.reason}` : null].filter(Boolean).join(' · ')}
          </span>
        </span>
      </TableCell>
      <TableCell>
        <span className="inline-flex flex-col gap-0.5">
          {href ? (
            <Link to={href} className="font-medium text-primary hover:underline">
              {it.target_name || it.target_type}
            </Link>
          ) : (
            <span className="font-medium">{it.target_name || it.target_type}</span>
          )}
          <span className="text-[12.5px] text-muted-foreground">
            {it.target_type}
            {perms.tenants && it.tenant_id && it.target_type !== 'tenant' && it.target_type !== 'workspace' && (
              <>
                {' · '}
                <Link to={`/tenants/${encodeURIComponent(it.tenant_id)}`} className="text-primary hover:underline">
                  tenant
                </Link>
              </>
            )}
            {canFocus && focus && (
              <>
                {' · '}
                <button
                  type="button"
                  className="text-primary hover:underline"
                  onClick={() => onFocus({ ...focus, name: focusName })}
                  aria-label={`Show only ${FOCUS_LABELS[focus.key].toLowerCase()} ${focusName || focus.id}`}
                >
                  focus
                </button>
              </>
            )}
          </span>
        </span>
      </TableCell>
    </TableRow>
  )
}
