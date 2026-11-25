import { useState } from 'react'
import { ChevronLeft, ChevronRight, Download, RefreshCw } from 'lucide-react'
import { Link, useSearchParams } from 'react-router-dom'
import { HOURLY_METERS, listFleetUsage, USAGE_METERS, type TenantMonthUsage, type UsageMeter } from '@/api/usage'
import { download } from '@/lib/auditExport'
import { FilterChips } from '@/components/FilterChips'
import { PageHeader } from '@/components/PageHeader'
import { Pager } from '@/components/Pager'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { addMonths, fleetUsageCSV, fleetUsageJSON, formatMeter, METER_LABELS, monthKey, monthLabel, monthStart } from '@/lib/usage-format'
import { useDetailQuery } from '@/lib/useDetailQuery'

// Fleet usage (Sprint 25 U3, F33; D-57, D-59, D-60): every tenant's usage for
// one UTC month, largest first by the chosen meter. Super-admin only (the
// controller refuses relay-ops: tenant.usage.read). Each page read is audited
// (usage.fleet.read), so the page reads once per view, month, sort or page,
// and on Refresh; no polling. Deleted tenants keep their name (D-58) and
// have no link (their page is gone).
//
// URL: ?month=YYYY-MM (default: this month) &sort=<meter>.

const SORT_CHIPS = USAGE_METERS.map((m) => ({ value: m, label: METER_LABELS[m] }))
const EXPORT_MAX = 10_000
const PAGE_SIZE = 50

export function Usage() {
  const [sp, setSp] = useSearchParams()
  const [now] = useState(() => new Date())
  const current = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1))
  const parsed = monthStart(sp.get('month') ?? '')
  const month = parsed && parsed.getTime() <= current.getTime() ? parsed : current
  const rawSort = sp.get('sort') ?? ''
  const sort: UsageMeter = (USAGE_METERS as readonly string[]).includes(rawSort) ? (rawSort as UsageMeter) : 'relay_bytes_to_clients'
  const [round, setRound] = useState(0)

  const set = (key: string, value: string | null) => {
    const p = new URLSearchParams(sp)
    if (value) p.set(key, value)
    else p.delete(key)
    setSp(p, { replace: true })
  }
  const key = `${monthKey(month)}|${sort}|${round}`
  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Usage"
        description="What each tenant used in a UTC month: relayed bytes, session, connector and shield hours, and active users. Counted, never estimated; only relayed traffic is metered."
        actions={
          <Button variant="outline" onClick={() => setRound((n) => n + 1)} aria-label="Refresh usage (records an audit entry)">
            <RefreshCw aria-hidden />
            Refresh
          </Button>
        }
      />
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm" aria-label="Previous month" onClick={() => set('month', monthKey(addMonths(month, -1)))}>
            <ChevronLeft aria-hidden />
          </Button>
          <h2 className="text-sm font-medium" aria-live="polite">
            {monthLabel(month)}
          </h2>
          <Button
            variant="outline"
            size="sm"
            aria-label="Next month"
            disabled={month.getTime() >= current.getTime()}
            onClick={() => set('month', monthKey(addMonths(month, 1)))}
          >
            <ChevronRight aria-hidden />
          </Button>
        </div>
        <ExportButtons month={monthKey(month)} sort={sort} />
      </div>
      <FilterChips label="Sort by" options={SORT_CHIPS} value={sort} onChange={(v) => set('sort', v === 'relay_bytes_to_clients' ? null : v)} />
      {/* keyed: a new month, sort or Refresh starts at the first page. */}
      <FleetTable key={key} month={monthKey(month)} sort={sort} round={round} />
    </div>
  )
}

function FleetTable({ month, sort, round }: { month: string; sort: UsageMeter; round: number }) {
  const [cursors, setCursors] = useState<string[]>([''])
  const cursor = cursors[cursors.length - 1]
  const { state } = useDetailQuery(`fleet-usage|${month}|${sort}|${round}|${cursor}`, () => listFleetUsage({ month, sort, cursor, limit: PAGE_SIZE }))
  if (state.status === 'loading') return <Skeleton className="h-40 w-full" aria-label="Loading usage" />
  if (state.status === 'session_ended') return null
  if (state.status !== 'ready') {
    return (
      <Alert variant="destructive">
        <AlertDescription>{state.status === 'forbidden' ? 'Usage is available to super-admins only.' : 'Couldn’t load usage. Use Refresh to try again.'}</AlertDescription>
      </Alert>
    )
  }
  const p = state.data
  return (
    <Card>
      <div className="flex items-center gap-2 px-4 pt-4 text-[13px] text-muted-foreground">
        {p.complete ? (
          <span>Every minute of the month was counted.</span>
        ) : (
          <>
            <Badge variant="warning">Incomplete</Badge>
            <span>
              {Math.round((p.collected_minutes / p.minutes) * 100)}% of the month’s minutes counted so far: figures are at least what was counted.
            </span>
          </>
        )}
      </div>
      {p.items.length === 0 ? (
        <p className="px-4 py-6 text-sm text-muted-foreground">No tenants with usage in this month.</p>
      ) : (
        <div className="overflow-x-auto">
          <Table aria-label="Usage by tenant">
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Tenant</TableHead>
                <TableHead>Plan</TableHead>
                {USAGE_METERS.map((m) => (
                  <TableHead key={m} className="text-right" aria-sort={m === sort ? 'descending' : undefined}>
                    {METER_LABELS[m]}
                  </TableHead>
                ))}
              </TableRow>
            </TableHeader>
            <TableBody>
              {p.items.map((r) => (
                <FleetRow key={r.tenant.id} r={r} />
              ))}
            </TableBody>
          </Table>
        </div>
      )}
      <Pager
        page={cursors.length}
        hasPrev={cursors.length > 1}
        hasNext={!!p.next_cursor}
        onPrev={() => setCursors((c) => c.slice(0, -1))}
        onNext={() => p.next_cursor && setCursors((c) => [...c, p.next_cursor!])}
      />
    </Card>
  )
}

function FleetRow({ r }: { r: TenantMonthUsage }) {
  return (
    <TableRow aria-label={r.tenant.name || r.tenant.id}>
      <TableCell className="whitespace-nowrap">
        {r.tenant.purged ? (
          <span className="inline-flex items-center gap-2">
            <span className="font-medium">{r.tenant.name || r.tenant.id}</span>
            <Badge variant="secondary">Deleted</Badge>
          </span>
        ) : (
          <Link to={`/tenants/${encodeURIComponent(r.tenant.id)}`} className="font-medium text-primary hover:underline">
            {r.tenant.name || r.tenant.id}
          </Link>
        )}
      </TableCell>
      <TableCell className="text-[13px] text-muted-foreground">{r.tenant.plan_name ?? '—'}</TableCell>
      {HOURLY_METERS.map((m) => (
        <TableCell key={m} className="text-right font-mono text-xs tabular-nums">
          {formatMeter(m, r.meters[m] ?? 0)}
        </TableCell>
      ))}
      <TableCell className="text-right font-mono text-xs tabular-nums">{formatMeter('active_users', r.active_users)}</TableCell>
    </TableRow>
  )
}

/** Export every tenant of the month (all pages, at most EXPORT_MAX). Each
 * page read is audited, like viewing it. */
function ExportButtons({ month, sort }: { month: string; sort: UsageMeter }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const run = async (format: 'csv' | 'json') => {
    setBusy(true)
    setError(null)
    try {
      const rows: TenantMonthUsage[] = []
      const seen = new Set<string>()
      let cursor = ''
      let complete = false
      let truncated = false
      for (;;) {
        const page = await listFleetUsage({ month, sort, cursor, limit: 200 })
        complete = page.complete
        for (const r of page.items) {
          if (seen.has(r.tenant.id)) throw new Error('a tenant was returned twice')
          seen.add(r.tenant.id)
          rows.push(r)
        }
        if (!page.next_cursor) break
        if (rows.length >= EXPORT_MAX) {
          truncated = true
          break
        }
        cursor = page.next_cursor
      }
      if (truncated) setError(`Exported the first ${rows.length.toLocaleString()} tenants only.`)
      if (format === 'csv') download(`usage-${month}.csv`, 'text/csv;charset=utf-8', fleetUsageCSV(month, complete, rows, HOURLY_METERS))
      else download(`usage-${month}.json`, 'application/json', fleetUsageJSON(month, complete, rows))
    } catch {
      setError('Export failed. Try again.')
    } finally {
      setBusy(false)
    }
  }
  return (
    <div className="flex items-center gap-2">
      {error && (
        <span className="text-[12.5px] text-muted-foreground" role="status">
          {error}
        </span>
      )}
      <Button variant="outline" size="sm" disabled={busy} onClick={() => void run('csv')}>
        <Download aria-hidden />
        CSV
      </Button>
      <Button variant="outline" size="sm" disabled={busy} onClick={() => void run('json')}>
        <Download aria-hidden />
        JSON
      </Button>
    </div>
  )
}
