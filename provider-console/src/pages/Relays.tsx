import { useCallback } from 'react'
import { RefreshCw } from 'lucide-react'
import { Link } from 'react-router-dom'
import { listRelays } from '@/api/reads'
import { RELAY_STATUSES, type Page, type RelayStatus, type RelaySummary } from '@/api/types'
import { DataState } from '@/components/DataState'
import { ExpiryBadge } from '@/components/ExpiryBadge'
import { Pager } from '@/components/Pager'
import { StatusBadge } from '@/components/StatusBadge'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatRelative } from '@/lib/expiry'
import { formatDateTime } from '@/lib/format'
import { usePagedQuery, type Filters } from '@/lib/usePagedQuery'

// Relays (Phase P, relay.read: super-admin and relay-ops). Read-only: the
// only controls are the status filter, paging, Refresh and Retry.

const FILTER_KEYS = ['status'] as const
const ALL = 'all' // Select sentinel for "no status filter" (every status except deleted)

function validStatus(f: Filters): boolean {
  return !f.status || (RELAY_STATUSES as readonly string[]).includes(f.status)
}

function fetchRelays(f: Filters, cursor: string | undefined): Promise<Page<RelaySummary>> {
  return listRelays({ status: f.status as RelayStatus | undefined, cursor })
}

export function Relays() {
  const q = usePagedQuery<Page<RelaySummary>>({ filterKeys: FILTER_KEYS, validate: validStatus, fetchPage: fetchRelays })
  const onStatus = useCallback((v: string) => q.setFilters(v === ALL ? {} : { status: v }), [q])

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="font-display text-2xl font-semibold tracking-tight">Relays</h1>
          <p className="text-sm text-muted-foreground">Provider relays, their liveness, capacity and certificates.</p>
        </div>
        <div className="flex items-end gap-2">
          <div className="grid gap-1.5">
            <Label htmlFor="relay-status">Status</Label>
            <Select value={q.filters.status ?? ALL} onValueChange={onStatus}>
              <SelectTrigger id="relay-status" aria-label="Status filter" className="w-44">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={ALL}>All (except deleted)</SelectItem>
                {RELAY_STATUSES.map((s) => (
                  <SelectItem key={s} value={s} className="capitalize">
                    {s}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <Button variant="outline" onClick={q.retry} aria-label="Refresh relays">
            <RefreshCw aria-hidden />
            Refresh
          </Button>
        </div>
      </div>

      <DataState
        state={q.state}
        what="relays"
        emptyMessage={q.filters.status ? `No ${q.filters.status} relays.` : 'No relays yet.'}
        isEmpty={(d) => d.items.length === 0}
        onRetry={q.retry}
        onReset={q.resetFilters}
      >
        {(page) => (
          <>
            <Table aria-label="Relays">
              <TableHeader>
                <TableRow>
                  <TableHead>Status</TableHead>
                  <TableHead>Name</TableHead>
                  <TableHead>Version</TableHead>
                  <TableHead>Hostname</TableHead>
                  <TableHead>Public address</TableHead>
                  <TableHead>Capacity</TableHead>
                  <TableHead>Last heartbeat</TableHead>
                  <TableHead>Cert expiry</TableHead>
                  <TableHead className="text-right">Connectors</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {page.items.map((r) => (
                  <TableRow key={r.id} aria-label={r.name}>
                    <TableCell>
                      <StatusBadge status={r.status} />
                    </TableCell>
                    <TableCell className="font-medium">
                      <Link to={`/relays/${encodeURIComponent(r.id)}`} className="text-primary underline-offset-4 hover:underline">
                        {r.name}
                      </Link>
                    </TableCell>
                    <TableCell>{r.version ?? '—'}</TableCell>
                    <TableCell>{r.hostname ?? '—'}</TableCell>
                    <TableCell className="font-mono text-xs">{r.public_addr ?? '—'}</TableCell>
                    <TableCell className="whitespace-nowrap">
                      {r.capacity_label} · {r.connection_count} / {r.max_connections}
                    </TableCell>
                    <TableCell className="whitespace-nowrap" title={formatDateTime(r.last_heartbeat_at)}>
                      {formatRelative(r.last_heartbeat_at)}
                    </TableCell>
                    <TableCell>
                      <ExpiryBadge notAfter={r.cert_not_after} />
                    </TableCell>
                    <TableCell className="text-right tabular-nums">{r.attached_connectors}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            <Pager page={q.page} hasPrev={q.hasPrev} hasNext={q.hasNext} onPrev={q.prev} onNext={q.next} />
          </>
        )}
      </DataState>
    </div>
  )
}
