import { useCallback } from 'react'
import { RefreshCw } from 'lucide-react'
import { Link } from 'react-router-dom'
import { listTenants } from '@/api/reads'
import { TENANT_STATUSES, type Page, type TenantStatus, type TenantSummary } from '@/api/types'
import { DataState } from '@/components/DataState'
import { ExpiryBadge } from '@/components/ExpiryBadge'
import { Pager } from '@/components/Pager'
import { StatusBadge } from '@/components/StatusBadge'
import { Button } from '@/components/ui/button'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { agentSummary } from '@/lib/format'
import { usePagedQuery, type Filters } from '@/lib/usePagedQuery'

// Tenants (Phase P, tenant.read: super-admin only). Read-only. The list is
// not audited; opening a tenant's detail is (OQ-1), so rows link to detail
// only through an ordinary click: no prefetch, no hover requests.
// Users appear only as counts (OQ-2).

const FILTER_KEYS = ['status'] as const
const ALL = 'all' // Select sentinel for "no status filter" (every status except deleted)

function validStatus(f: Filters): boolean {
  return !f.status || (TENANT_STATUSES as readonly string[]).includes(f.status)
}

function fetchTenants(f: Filters, cursor: string | undefined): Promise<Page<TenantSummary>> {
  return listTenants({ status: f.status as TenantStatus | undefined, cursor })
}

export function Tenants() {
  const q = usePagedQuery<Page<TenantSummary>>({ filterKeys: FILTER_KEYS, validate: validStatus, fetchPage: fetchTenants })
  const onStatus = useCallback((v: string) => q.setFilters(v === ALL ? {} : { status: v }), [q])

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="font-display text-2xl font-semibold tracking-tight">Tenants</h1>
          <p className="text-sm text-muted-foreground">Customer workspaces and their infrastructure counts.</p>
        </div>
        <div className="flex items-end gap-2">
          <div className="grid gap-1.5">
            <Label htmlFor="tenant-status">Status</Label>
            <Select value={q.filters.status ?? ALL} onValueChange={onStatus}>
              <SelectTrigger id="tenant-status" aria-label="Status filter" className="w-44">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={ALL}>All (except deleted)</SelectItem>
                {TENANT_STATUSES.map((s) => (
                  <SelectItem key={s} value={s} className="capitalize">
                    {s}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <Button variant="outline" onClick={q.retry} aria-label="Refresh tenants">
            <RefreshCw aria-hidden />
            Refresh
          </Button>
        </div>
      </div>

      <DataState
        state={q.state}
        what="tenants"
        emptyMessage={q.filters.status ? `No ${q.filters.status} tenants.` : 'No tenants yet.'}
        isEmpty={(d) => d.items.length === 0}
        onRetry={q.retry}
        onReset={q.resetFilters}
      >
        {(page) => (
          <>
            <Table aria-label="Tenants">
              <TableHeader>
                <TableRow>
                  <TableHead>Status</TableHead>
                  <TableHead>Name</TableHead>
                  <TableHead>Slug</TableHead>
                  <TableHead>Trust domain</TableHead>
                  <TableHead>CA expiry</TableHead>
                  <TableHead>Connectors</TableHead>
                  <TableHead>Shields</TableHead>
                  <TableHead className="text-right">Networks</TableHead>
                  <TableHead className="text-right">Users</TableHead>
                  <TableHead className="text-right">Devices</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {page.items.map((t) => (
                  <TableRow key={t.id} aria-label={t.name}>
                    <TableCell>
                      <StatusBadge status={t.status} />
                    </TableCell>
                    <TableCell className="font-medium">
                      <Link to={`/tenants/${encodeURIComponent(t.id)}`} className="text-primary underline-offset-4 hover:underline">
                        {t.name}
                      </Link>
                    </TableCell>
                    <TableCell className="font-mono text-xs">{t.slug}</TableCell>
                    <TableCell className="font-mono text-xs">{t.trust_domain}</TableCell>
                    <TableCell>
                      <ExpiryBadge notAfter={t.ca_not_after} />
                    </TableCell>
                    <TableCell className="whitespace-nowrap text-xs">{agentSummary(t.counts.connectors)}</TableCell>
                    <TableCell className="whitespace-nowrap text-xs">{agentSummary(t.counts.shields)}</TableCell>
                    <TableCell className="text-right tabular-nums">{t.counts.remote_networks}</TableCell>
                    <TableCell className="text-right tabular-nums" title="Active users (counts only)">
                      {t.counts.users.active}
                    </TableCell>
                    <TableCell className="text-right tabular-nums" title="Active devices">
                      {t.counts.client_devices.active}
                    </TableCell>
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
