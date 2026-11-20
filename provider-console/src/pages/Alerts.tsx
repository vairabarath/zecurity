import { useState } from 'react'
import { Plus, RefreshCw } from 'lucide-react'
import { Link, Navigate, useSearchParams } from 'react-router-dom'
import {
  listActiveAlerts,
  listAlertChannels,
  listAlertDeliveries,
  listAlertEvents,
  listAlertRules,
  type ActiveAlert,
  type AlertChannel,
  type AlertEvent,
  type AlertRule,
  type ChannelTestResult,
} from '@/api/alerts'
import { hasRole } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import {
  CreateChannelDialog,
  DeleteChannelDialog,
  EditChannelDialog,
  EditRuleDialog,
  RotateSecretDialog,
  SecretDialog,
  TestChannelDialog,
} from '@/components/AlertDialogs'
import { FilterChips } from '@/components/FilterChips'
import { PageHeader } from '@/components/PageHeader'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatSeconds, kindLabel, SCOPE_FILTER_LABELS, SEVERITY_LABELS, severityVariant } from '@/lib/alert-format'
import { formatRelative } from '@/lib/expiry'
import { formatDateTime } from '@/lib/format'
import { useDetailQuery, type DetailState } from '@/lib/useDetailQuery'

// Alerts (Sprint 24 A3, F26; D-50, D-52, D-53). Four tabs:
//   Active and History — both roles; the controller gives relay-ops relay and
//     pool alerts only (no tenant alerts, no tenant names);
//   Rules and Channels — super-admin only (every change with a reason).
// Requests are made once per view and again only on Refresh or after a write
// (useDetailQuery): no polling.

type Tab = 'active' | 'history' | 'rules' | 'channels'

const TABS_ALL = [
  { value: 'active', label: 'Active' },
  { value: 'history', label: 'History' },
  { value: 'rules', label: 'Rules' },
  { value: 'channels', label: 'Channels' },
] as const
const TABS_RELAY_OPS = TABS_ALL.slice(0, 2)

const SCOPE_CHIPS = [
  { value: '', label: 'All' },
  { value: 'platform', label: 'Platform' },
  { value: 'tenant', label: 'Tenant' },
] as const

export function Alerts() {
  const me = useSessionStore((s) => s.me)
  const isSuperAdmin = hasRole(me?.role, ['super-admin'])
  const [sp, setSp] = useSearchParams()
  // console_url in webhook and email payloads is /alerts?event=<id> (A2):
  // open History on that event.
  const eventLink = sp.get('event') ?? ''
  const rawTab = sp.get('tab') ?? (eventLink ? 'history' : 'active')
  const known: Tab[] = isSuperAdmin ? ['active', 'history', 'rules', 'channels'] : ['active', 'history']
  const tab: Tab = (known as string[]).includes(rawTab) ? (rawTab as Tab) : 'active'
  const rawScope = sp.get('scope') ?? ''
  const scope = isSuperAdmin && (rawScope === 'platform' || rawScope === 'tenant') ? rawScope : ''
  const [round, setRound] = useState(0)
  const [notice, setNotice] = useState<string | null>(null)
  const refresh = () => {
    setNotice(null)
    setRound((n) => n + 1)
  }
  const set = (k: string, v: string) => {
    const next = new URLSearchParams(sp)
    if (v) next.set(k, v)
    else next.delete(k)
    next.delete('event')
    setSp(next, { replace: true })
    setNotice(null)
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Alerts"
        description={
          isSuperAdmin
            ? 'Platform alerts (relays, pools, control plane, certificates) and tenant alerts (their connectors, shields, relay path, quotas), and where they are sent.'
            : 'Relay and pool alerts.'
        }
        actions={
          <Button variant="outline" onClick={refresh} aria-label="Refresh alerts">
            <RefreshCw aria-hidden />
            Refresh
          </Button>
        }
      />
      <div className="flex flex-wrap items-center justify-between gap-3">
        <FilterChips label="View" options={isSuperAdmin ? TABS_ALL : TABS_RELAY_OPS} value={tab} onChange={(v) => set('tab', v)} />
        {isSuperAdmin && (tab === 'active' || tab === 'history') && (
          <FilterChips label="Scope" options={SCOPE_CHIPS} value={scope} onChange={(v) => set('scope', v)} />
        )}
      </div>
      {notice && (
        <Alert role="status">
          <AlertDescription>{notice}</AlertDescription>
        </Alert>
      )}
      {tab === 'active' && <ActiveTab round={round} scope={scope} showTenant={isSuperAdmin} />}
      {tab === 'history' && (
        // keyed by scope: a new scope starts at its newest page (the cursor
        // stack belongs to one scope).
        <HistoryTab
          key={scope}
          round={round}
          scope={scope}
          showTenant={isSuperAdmin}
          showDeliveries={isSuperAdmin}
          focusEvent={eventLink}
        />
      )}
      {tab === 'rules' && isSuperAdmin && (
        <RulesTab
          round={round}
          onDone={(t) => {
            setNotice(t)
            setRound((n) => n + 1)
          }}
        />
      )}
      {tab === 'channels' && isSuperAdmin && <ChannelsTab round={round} onNotice={setNotice} onChanged={() => setRound((n) => n + 1)} />}
    </div>
  )
}

function Loading({ what }: { what: string }) {
  return (
    <div aria-busy="true" aria-label={`Loading ${what}`} className="flex flex-col gap-2">
      <Skeleton className="h-8 w-full" />
      <Skeleton className="h-8 w-2/3" />
    </div>
  )
}

/** The non-ready states of a read, or null when ready. */
function NotReady<T>({ state, what }: { state: DetailState<T>; what: string }) {
  switch (state.status) {
    case 'loading':
      return <Loading what={what} />
    case 'session_ended':
      return null
    case 'forbidden':
      return <Navigate to="/forbidden" replace />
    case 'not_found':
    case 'error':
      return (
        <Alert variant="destructive">
          <AlertDescription>Couldn’t load {what}. Use Refresh to try again.</AlertDescription>
        </Alert>
      )
  }
  return null
}

function SeverityBadge({ severity }: { severity: string }) {
  return <Badge variant={severityVariant(severity)}>{SEVERITY_LABELS[severity as keyof typeof SEVERITY_LABELS] ?? severity}</Badge>
}

function Target({ type, name, tenantId, showTenant }: { type: string; name: string; tenantId: string | null; showTenant: boolean }) {
  return (
    <span className="inline-flex flex-col gap-0.5">
      <span className="font-medium">{name}</span>
      <span className="text-[12.5px] text-muted-foreground">
        {type}
        {showTenant && tenantId && (
          <>
            {' · '}
            <Link to={`/tenants/${encodeURIComponent(tenantId)}`} className="text-primary hover:underline">
              tenant
            </Link>
          </>
        )}
      </span>
    </span>
  )
}

function ActiveTab({ round, scope, showTenant }: { round: number; scope: string; showTenant: boolean }) {
  const { state } = useDetailQuery(`active|${round}|${scope}`, () =>
    listActiveAlerts({ scope: scope as '' | 'platform' | 'tenant' }),
  )
  if (state.status !== 'ready') return <NotReady state={state} what="alerts" />
  const items: ActiveAlert[] = state.data.items
  if (items.length === 0) return <p className="py-6 text-sm text-muted-foreground">No alerts are firing or pending.</p>
  return (
    <Card>
      {state.data.truncated && (
        <p role="status" className="border-b border-border px-5 py-3 text-[13px] text-warning-text">
          Showing the first {items.length.toLocaleString()} alerts; more are open.
        </p>
      )}
      <Table aria-label="Active alerts">
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead>Severity</TableHead>
            <TableHead>State</TableHead>
            <TableHead>Alert</TableHead>
            <TableHead>Target</TableHead>
            <TableHead>Since</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {items.map((a) => (
            <TableRow key={`${a.rule_id}|${a.target_type}|${a.target_id}`} aria-label={a.summary}>
              <TableCell>
                <SeverityBadge severity={a.severity} />
              </TableCell>
              <TableCell>
                {a.status === 'firing' ? <Badge variant="destructive">Firing</Badge> : <Badge variant="secondary">Pending</Badge>}
              </TableCell>
              <TableCell>
                <span className="inline-flex flex-col gap-0.5">
                  <span>{a.summary}</span>
                  <span className="text-[12.5px] text-muted-foreground">
                    {kindLabel(a.kind)} · {a.scope}
                  </span>
                </span>
              </TableCell>
              <TableCell>
                <Target type={a.target_type} name={a.target_name} tenantId={a.tenant_id} showTenant={showTenant} />
              </TableCell>
              <TableCell className="whitespace-nowrap text-[13px]" title={formatDateTime(a.firing_since ?? a.pending_since)}>
                {formatRelative(a.firing_since ?? a.pending_since)}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </Card>
  )
}

const TRANSITION_LABELS: Record<string, string> = { fired: 'Fired', renotified: 'Reminder', resolved: 'Resolved' }

// A linked event that isn't on the first History page: its deliveries are
// still readable by id.
const LINKED_STUB: AlertEvent = {
  id: '', rule_id: '', kind: '', scope: 'platform', tenant_id: null, target_type: '', target_id: '', target_name: '',
  transition: 'fired', status: 'firing', severity: 'info', summary: 'The alert event from your link', started_at: '', occurred_at: '',
}

function HistoryTab({
  round,
  scope,
  showTenant,
  showDeliveries,
  focusEvent,
}: {
  round: number
  scope: string
  showTenant: boolean
  showDeliveries: boolean
  focusEvent: string
}) {
  const [cursors, setCursors] = useState<string[]>([''])
  const cursor = cursors[cursors.length - 1]
  const { state } = useDetailQuery(`events|${round}|${scope}|${cursor}`, () =>
    listAlertEvents({ scope: scope as '' | 'platform' | 'tenant', cursor, limit: 50 }),
  )
  const [deliveriesOf, setDeliveriesOf] = useState<AlertEvent | null>(null)
  // A linked event (console_url): its deliveries open once, for super-admins.
  const [linkOpened, setLinkOpened] = useState(false)
  if (state.status !== 'ready') return <NotReady state={state} what="alert history" />
  const items = state.data.items
  const linked = focusEvent ? items.find((e) => e.id === focusEvent) : undefined
  if (focusEvent && showDeliveries && !linkOpened && !deliveriesOf) {
    setLinkOpened(true)
    setDeliveriesOf(linked ?? { ...LINKED_STUB, id: focusEvent })
  }
  if (items.length === 0 && cursors.length === 1) return <p className="py-6 text-sm text-muted-foreground">No alert history yet.</p>
  return (
    <Card>
      <Table aria-label="Alert history">
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead>Time</TableHead>
            <TableHead>Event</TableHead>
            <TableHead>Alert</TableHead>
            <TableHead>Target</TableHead>
            {showDeliveries && (
              <TableHead className="text-right">
                <span className="sr-only">Deliveries</span>
              </TableHead>
            )}
          </TableRow>
        </TableHeader>
        <TableBody>
          {items.map((e) => (
            <TableRow key={e.id} aria-label={e.summary} aria-current={e.id === focusEvent ? 'true' : undefined} className={e.id === focusEvent ? 'bg-primary-soft' : undefined}>
              <TableCell className="whitespace-nowrap font-mono text-xs">{formatDateTime(e.occurred_at)}</TableCell>
              <TableCell>
                <span className="inline-flex items-center gap-2">
                  <Badge variant={e.transition === 'resolved' ? 'success' : severityVariant(e.severity)}>
                    {TRANSITION_LABELS[e.transition] ?? e.transition}
                  </Badge>
                </span>
              </TableCell>
              <TableCell>
                <span className="inline-flex flex-col gap-0.5">
                  <span>{e.summary}</span>
                  <span className="text-[12.5px] text-muted-foreground">
                    {kindLabel(e.kind)} · {e.scope} · {SEVERITY_LABELS[e.severity]}
                  </span>
                </span>
              </TableCell>
              <TableCell>
                <Target type={e.target_type} name={e.target_name} tenantId={e.tenant_id} showTenant={showTenant} />
              </TableCell>
              {showDeliveries && (
                <TableCell className="text-right">
                  <Button variant="ghost" size="sm" onClick={() => setDeliveriesOf(e)} aria-label={`Deliveries of ${e.summary}`}>
                    Deliveries
                  </Button>
                </TableCell>
              )}
            </TableRow>
          ))}
        </TableBody>
      </Table>
      {(cursors.length > 1 || state.data.next_cursor) && (
        <nav aria-label="Pagination" className="flex items-center justify-end gap-2 border-t border-border px-4 py-3">
          <Button variant="outline" size="sm" disabled={cursors.length === 1} onClick={() => setCursors((c) => c.slice(0, -1))}>
            Newer
          </Button>
          <Button
            variant="outline"
            size="sm"
            disabled={!state.data.next_cursor}
            onClick={() => state.data.next_cursor && setCursors((c) => [...c, state.data.next_cursor!])}
          >
            Older
          </Button>
        </nav>
      )}
      {deliveriesOf && <DeliveriesDialog event={deliveriesOf} onClose={() => setDeliveriesOf(null)} />}
    </Card>
  )
}

function DeliveriesDialog({ event, onClose }: { event: AlertEvent; onClose: () => void }) {
  const { state } = useDetailQuery(`deliveries|${event.id}`, () => listAlertDeliveries(event.id))
  return (
    <Dialog open onOpenChange={(o) => !o && onClose()}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Deliveries</DialogTitle>
          <DialogDescription>{event.summary}</DialogDescription>
        </DialogHeader>
        {state.status !== 'ready' ? (
          <NotReady state={state} what="deliveries" />
        ) : state.data.items.length === 0 ? (
          <p className="text-sm text-muted-foreground">No channel received this event (no matching channel, or still queued).</p>
        ) : (
          <ul className="flex flex-col gap-2 text-sm">
            {state.data.items.map((d) => (
              <li key={`${d.channel_id}|${d.attempt}`} className="flex flex-wrap items-center gap-2">
                <Badge variant={d.status === 'delivered' ? 'success' : d.status === 'failed' ? 'destructive' : 'warning'}>{d.status}</Badge>
                <span className="font-medium">{d.channel_name}</span>
                <span className="text-muted-foreground">
                  attempt {d.attempt}
                  {d.http_status !== null && ` · HTTP ${d.http_status}`} · {formatDateTime(d.at)}
                </span>
                {d.error && <span className="w-full text-[12.5px] text-critical-text">{d.error}</span>}
              </li>
            ))}
          </ul>
        )}
      </DialogContent>
    </Dialog>
  )
}

function RulesTab({ round, onDone }: { round: number; onDone: (text: string) => void }) {
  const { state } = useDetailQuery(`rules|${round}`, () => listAlertRules())
  const [editing, setEditing] = useState<AlertRule | null>(null)
  if (state.status !== 'ready') return <NotReady state={state} what="alert rules" />
  return (
    <Card>
      <Table aria-label="Alert rules">
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead>Rule</TableHead>
            <TableHead>Scope</TableHead>
            <TableHead>Severity</TableHead>
            <TableHead>Fires after</TableHead>
            <TableHead>Reminder</TableHead>
            <TableHead>State</TableHead>
            <TableHead className="text-right">
              <span className="sr-only">Actions</span>
            </TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {state.data.items.map((r) => (
            <TableRow key={r.id} aria-label={r.name}>
              <TableCell>
                <span className="inline-flex flex-col gap-0.5">
                  <span className="font-medium">{r.name}</span>
                  <span className="text-[12.5px] text-muted-foreground">
                    {kindLabel(r.kind)}
                    {r.params.threshold_percent !== undefined && ` · at ${r.params.threshold_percent}%`}
                    {r.params.days !== undefined && ` · within ${r.params.days} days`}
                  </span>
                </span>
              </TableCell>
              <TableCell className="capitalize">{r.scope}</TableCell>
              <TableCell>
                <SeverityBadge severity={r.severity} />
              </TableCell>
              <TableCell className="text-[13px]">{formatSeconds(r.for_seconds)}</TableCell>
              <TableCell className="text-[13px]">{r.renotify_seconds > 0 ? `every ${formatSeconds(r.renotify_seconds)}` : 'never'}</TableCell>
              <TableCell>{r.enabled ? <Badge variant="success">Enabled</Badge> : <Badge variant="secondary">Disabled</Badge>}</TableCell>
              <TableCell className="text-right">
                <Button variant="outline" size="sm" onClick={() => setEditing(r)} aria-label={`Edit ${r.name}`}>
                  Edit
                </Button>
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
      {editing && (
        <EditRuleDialog
          rule={editing}
          onCancel={() => setEditing(null)}
          onDone={(t) => {
            setEditing(null)
            onDone(t)
          }}
        />
      )}
    </Card>
  )
}

type ChannelAction = { kind: 'edit' | 'test' | 'rotate' | 'delete'; channel: AlertChannel }

function ChannelsTab({ round, onNotice, onChanged }: { round: number; onNotice: (t: string) => void; onChanged: () => void }) {
  const { state } = useDetailQuery(`channels|${round}`, () => listAlertChannels())
  const [creating, setCreating] = useState(false)
  const [action, setAction] = useState<ChannelAction | null>(null)
  // The secret is held only while its dialog is open, then dropped.
  const [secret, setSecret] = useState<{ name: string; value: string } | null>(null)
  const done = (t: string) => {
    setAction(null)
    onNotice(t)
    onChanged()
  }
  const testResult = (c: AlertChannel, r: ChannelTestResult) => {
    setAction(null)
    onNotice(
      r.delivered
        ? `Test alert delivered to ${c.name}${r.http_status ? ` (HTTP ${r.http_status})` : ''}.`
        : `Test alert to ${c.name} failed: ${r.error ?? 'unknown error'}.`,
    )
  }
  return (
    <div className="flex flex-col gap-3">
      <div className="flex justify-end">
        <Button onClick={() => setCreating(true)}>
          <Plus aria-hidden />
          Add channel
        </Button>
      </div>
      {state.status !== 'ready' ? (
        <NotReady state={state} what="alert channels" />
      ) : state.data.items.length === 0 ? (
        <p className="py-6 text-sm text-muted-foreground">No channels yet. Alerts are recorded here but not sent anywhere.</p>
      ) : (
        <Card>
          <Table aria-label="Alert channels">
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Channel</TableHead>
                <TableHead>Sends to</TableHead>
                <TableHead>Receives</TableHead>
                <TableHead>Minimum</TableHead>
                <TableHead>State</TableHead>
                <TableHead className="text-right">
                  <span className="sr-only">Actions</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {state.data.items.map((c) => (
                <TableRow key={c.id} aria-label={c.name}>
                  <TableCell>
                    <span className="inline-flex flex-col gap-0.5">
                      <span className="font-medium">{c.name}</span>
                      <span className="text-[12.5px] capitalize text-muted-foreground">{c.type}</span>
                    </span>
                  </TableCell>
                  <TableCell className="max-w-[260px] truncate font-mono text-xs">
                    {c.type === 'webhook' ? `${c.url_host ?? ''}/…` : c.recipients ? c.recipients.join(', ') : 'Every active super-admin'}
                  </TableCell>
                  <TableCell className="text-[13px]">{SCOPE_FILTER_LABELS[c.scope_filter]}</TableCell>
                  <TableCell>
                    <SeverityBadge severity={c.min_severity} />
                  </TableCell>
                  <TableCell>{c.enabled ? <Badge variant="success">Enabled</Badge> : <Badge variant="secondary">Disabled</Badge>}</TableCell>
                  <TableCell className="text-right">
                    <span className="inline-flex flex-wrap justify-end gap-2">
                      <Button variant="outline" size="sm" onClick={() => setAction({ kind: 'test', channel: c })} aria-label={`Test ${c.name}`}>
                        Test
                      </Button>
                      <Button variant="outline" size="sm" onClick={() => setAction({ kind: 'edit', channel: c })} aria-label={`Edit ${c.name}`}>
                        Edit
                      </Button>
                      {c.type === 'webhook' && (
                        <Button variant="outline" size="sm" onClick={() => setAction({ kind: 'rotate', channel: c })} aria-label={`Rotate secret of ${c.name}`}>
                          Rotate secret
                        </Button>
                      )}
                      <Button variant="ghost" size="sm" onClick={() => setAction({ kind: 'delete', channel: c })} aria-label={`Delete ${c.name}`}>
                        Delete
                      </Button>
                    </span>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </Card>
      )}
      {creating && (
        <CreateChannelDialog
          onCancel={() => setCreating(false)}
          onCreated={(ch, s) => {
            setCreating(false)
            onNotice(`Channel ${ch.name} added.`)
            onChanged()
            if (s) setSecret({ name: ch.name, value: s })
          }}
        />
      )}
      {action?.kind === 'edit' && <EditChannelDialog channel={action.channel} onCancel={() => setAction(null)} onDone={done} />}
      {action?.kind === 'delete' && <DeleteChannelDialog channel={action.channel} onCancel={() => setAction(null)} onDone={done} />}
      {action?.kind === 'test' && (
        <TestChannelDialog channel={action.channel} onCancel={() => setAction(null)} onResult={(r) => testResult(action.channel, r)} />
      )}
      {action?.kind === 'rotate' && (
        <RotateSecretDialog
          channel={action.channel}
          onCancel={() => setAction(null)}
          onRotated={(s) => {
            const name = action.channel.name
            setAction(null)
            onNotice(`Secret of ${name} rotated. The old one no longer works.`)
            setSecret({ name, value: s })
          }}
        />
      )}
      {secret && <SecretDialog channelName={secret.name} secret={secret.value} onClose={() => setSecret(null)} />}
    </div>
  )
}
