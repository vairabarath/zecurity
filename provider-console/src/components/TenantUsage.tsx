import { useState } from 'react'
import { ChevronLeft, ChevronRight, Download } from 'lucide-react'
import { getTenantUsage, HOURLY_METERS, type HourlyMeter, type TenantUsage as TenantUsageData, type UsageBucket } from '@/api/usage'
import { download } from '@/lib/auditExport'
import { FilterChips } from '@/components/FilterChips'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { addMonths, formatMeter, METER_LABELS, monthKey, monthLabel, tenantUsageCSV, tenantUsageJSON } from '@/lib/usage-format'
import { useDetailQuery, type DetailState } from '@/lib/useDetailQuery'
import { cn } from '@/lib/utils'

// Tenant detail "Usage" (Sprint 25 U3, F33; D-57, D-59, D-60): the last 12
// months per meter and one month by day, UTC. Usage is counted, never
// estimated: a period the collector didn't fully count is marked
// incomplete and shows at least what was counted, never 0 for "unknown".
//
// Every usage read is audited by the controller (tenant.usage.read), so the
// section reads once when it opens and once per month change; no polling.
// Super-admin only, like the page.

const METER_CHIPS = HOURLY_METERS.map((m) => ({ value: m, label: METER_LABELS[m] }))

function UsageError({ state }: { state: DetailState<unknown> }) {
  const text = state.status === 'forbidden' ? 'Not available for your role.' : 'Couldn’t load usage. Refresh the page to try again.'
  return <p className="text-[13px] text-muted-foreground" role="alert">{text}</p>
}

export function TenantUsage({ tenantId }: { tenantId: string }) {
  // "Now" is taken once per visit, so every read of the section agrees.
  const [now] = useState(() => new Date())
  const current = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1))
  const [month, setMonth] = useState(() => monthKey(now))
  const [meter, setMeter] = useState<HourlyMeter>('relay_bytes_to_clients')

  const months = useDetailQuery(`usage-months|${tenantId}|${monthKey(current)}`, () =>
    getTenantUsage({ tenant_id: tenantId, granularity: 'month', from: addMonths(current, -11).toISOString(), to: addMonths(current, 1).toISOString() }),
  ).state
  const selected = new Date(`${month}-01T00:00:00Z`)
  const days = useDetailQuery(`usage-days|${tenantId}|${month}`, () =>
    getTenantUsage({ tenant_id: tenantId, granularity: 'day', from: selected.toISOString(), to: addMonths(selected, 1).toISOString() }),
  ).state

  const plan = months.status === 'ready' ? months.data.tenant.plan_name : null
  return (
    <Card role="region" aria-labelledby="tenant-usage" className="flex flex-col gap-5 px-5 py-5">
      <div className="flex flex-col gap-0.5">
        <h2 id="tenant-usage" className="font-semibold">
          Usage
        </h2>
        <p className="text-[13px] text-muted-foreground">
          Counted per UTC month and day, never estimated. A period marked incomplete wasn’t fully counted (e.g. the controller was down) and shows at least
          what was counted. Only relayed traffic is metered.{plan ? ` Plan: ${plan}.` : ''}
        </p>
      </div>

      <section aria-label="Last 12 months" className="flex flex-col gap-2">
        <h3 className="text-sm font-medium">Last 12 months</h3>
        {months.status === 'loading' ? (
          <Skeleton className="h-24 w-full" />
        ) : months.status !== 'ready' ? (
          <UsageError state={months} />
        ) : (
          <MonthsTable data={months.data} selected={month} onSelect={setMonth} />
        )}
      </section>

      <section aria-label="By day" className="flex flex-col gap-3">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="flex items-center gap-2">
            <Button variant="outline" size="sm" aria-label="Previous month" onClick={() => setMonth(monthKey(addMonths(selected, -1)))}>
              <ChevronLeft aria-hidden />
            </Button>
            <h3 className="text-sm font-medium" aria-live="polite">
              {monthLabel(selected)} by day
            </h3>
            <Button
              variant="outline"
              size="sm"
              aria-label="Next month"
              disabled={selected.getTime() >= current.getTime()}
              onClick={() => setMonth(monthKey(addMonths(selected, 1)))}
            >
              <ChevronRight aria-hidden />
            </Button>
          </div>
          {days.status === 'ready' && <ExportButtons data={days.data} />}
        </div>
        <FilterChips label="Meter" options={METER_CHIPS} value={meter} onChange={(v) => setMeter(v as HourlyMeter)} />
        {days.status === 'loading' ? (
          <Skeleton className="h-32 w-full" />
        ) : days.status !== 'ready' ? (
          <UsageError state={days} />
        ) : (
          <DailyBars buckets={days.data.buckets} meter={meter} />
        )}
      </section>
    </Card>
  )
}

function MonthsTable({ data, selected, onSelect }: { data: TenantUsageData; selected: string; onSelect: (m: string) => void }) {
  const rows = [...data.buckets].reverse() // newest first
  return (
    <div className="overflow-x-auto">
      <Table aria-label="Usage by month">
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead>Month</TableHead>
            {HOURLY_METERS.map((m) => (
              <TableHead key={m} className="text-right">
                {METER_LABELS[m]}
              </TableHead>
            ))}
            <TableHead className="text-right">{METER_LABELS.active_users}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((b) => {
            const key = b.start.slice(0, 7)
            return (
              <TableRow key={b.start} aria-label={monthLabel(new Date(b.start))} data-selected={key === selected || undefined}>
                <TableCell className="whitespace-nowrap">
                  <button type="button" className={cn('hover:underline', key === selected ? 'font-semibold text-primary' : 'text-primary')} onClick={() => onSelect(key)}>
                    {monthLabel(new Date(b.start))}
                  </button>
                  {b.collected_minutes === 0 ? (
                    // Live finding A: a month with no counted minute (before
                    // metering started, or the controller was down all month)
                    // was never metered — not an incomplete month of zeros.
                    <Badge variant="secondary" className="ml-2">
                      Not metered
                    </Badge>
                  ) : (
                    !b.complete && (
                      <Badge variant="warning" className="ml-2">
                        Incomplete
                      </Badge>
                    )
                  )}
                </TableCell>
                {HOURLY_METERS.map((m) => (
                  <TableCell key={m} className="text-right font-mono text-xs tabular-nums">
                    {b.collected_minutes === 0 ? <NotMetered /> : formatMeter(m, b.meters[m] ?? 0)}
                  </TableCell>
                ))}
                <TableCell className="text-right font-mono text-xs tabular-nums">
                  {b.collected_minutes === 0 ? <NotMetered /> : formatMeter('active_users', b.active_users ?? 0)}
                </TableCell>
              </TableRow>
            )
          })}
        </TableBody>
      </Table>
    </div>
  )
}

/** One bar per UTC day; incomplete days are hatched and say so; days not yet
 * begun (collected nothing) are left empty, never drawn as 0. */
function DailyBars({ buckets, meter }: { buckets: UsageBucket[]; meter: HourlyMeter }) {
  const max = Math.max(1, ...buckets.map((b) => b.meters[meter] ?? 0))
  const total = buckets.reduce((n, b) => n + (b.meters[meter] ?? 0), 0)
  return (
    <figure className="flex flex-col gap-2">
      <ol aria-label={`${METER_LABELS[meter]} by day`} className="flex h-32 items-end gap-[2px]">
        {buckets.map((b) => {
          const v = b.meters[meter] ?? 0
          const day = new Date(b.start)
          const label = `${day.toLocaleDateString(undefined, { month: 'short', day: 'numeric', timeZone: 'UTC' })}: ${
            b.collected_minutes === 0 ? 'not counted' : formatMeter(meter, v)
          }${b.collected_minutes > 0 && !b.complete ? ' (incomplete)' : ''}`
          return (
            <li key={b.start} aria-label={label} title={label} className="flex h-full flex-1 items-end" data-incomplete={!b.complete || undefined}>
              {b.collected_minutes > 0 && (
                <span
                  className={cn('w-full rounded-sm bg-primary', !b.complete && 'bg-primary/40 [background-image:repeating-linear-gradient(45deg,transparent_0_3px,rgba(255,255,255,.25)_3px_6px)]')}
                  style={{ height: `${Math.max(2, (v / max) * 100)}%` }}
                />
              )}
            </li>
          )
        })}
      </ol>
      <figcaption className="text-[12.5px] text-muted-foreground">
        Month total so far: {formatMeter(meter, total)}. Hatched bars are incomplete days; empty days weren’t counted.
      </figcaption>
    </figure>
  )
}

function ExportButtons({ data }: { data: TenantUsageData }) {
  const stem = `usage-${data.tenant.slug}-${data.from.slice(0, 7)}`
  return (
    <div className="flex gap-2">
      <Button variant="outline" size="sm" onClick={() => download(`${stem}.csv`, 'text/csv;charset=utf-8', tenantUsageCSV(data, HOURLY_METERS))}>
        <Download aria-hidden />
        CSV
      </Button>
      <Button variant="outline" size="sm" onClick={() => download(`${stem}.json`, 'application/json', tenantUsageJSON(data))}>
        <Download aria-hidden />
        JSON
      </Button>
    </div>
  )
}

/** A cell of a month that was never metered: no figure, not a zero. */
function NotMetered() {
  return (
    <span className="text-muted-foreground" aria-label="Not metered">
      —
    </span>
  )
}
