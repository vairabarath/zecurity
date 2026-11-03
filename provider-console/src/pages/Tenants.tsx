import { useCallback } from 'react'
import { RefreshCw } from 'lucide-react'
import { Link } from 'react-router-dom'
import { listTenants } from '@/api/reads'
import { TENANT_STATUSES, type AgentCounts, type Page, type TenantStatus, type TenantSummary } from '@/api/types'
import { DataState } from '@/components/DataState'
import { ExpiryBadge } from '@/components/ExpiryBadge'
import { FilterChips } from '@/components/FilterChips'
import { PageHeader } from '@/components/PageHeader'
import { Pager } from '@/components/Pager'
import { StatusBadge } from '@/components/StatusBadge'
import { TenantAvatar } from '@/components/TenantAvatar'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { agentSummary } from '@/lib/format'
import { usePagedQuery, type Filters } from '@/lib/usePagedQuery'

// Tenants (Phase P, tenant.read: super-admin only). Read-only. The list is
// not audited; opening a tenant's detail is (OQ-1), so rows link to detail
// only through an ordinary click: no prefetch, no hover requests.
// Users appear only as counts (OQ-2).

const FILTER_KEYS = ['status'] as const
const ALL = 'all' // chip value for "no status filter" (every status except deleted)

function validStatus(f: Filters): boolean {
  return !f.status || (TENANT_STATUSES as readonly string[]).includes(f.status)
}

function fetchTenants(f: Filters, cursor: string | undefined): Promise<Page<TenantSummary>> {
  return listTenants({ status: f.status as TenantStatus | undefined, cursor })
}

/**
 * agentSummary that wraps only between statuses: spaces inside a part are
 * non-breaking, so "1 disconnected" never splits across lines.
 */
function WrappingSummary({ counts }: { counts: AgentCounts }) {
  return agentSummary(counts)
    .split(' · ')
    .map((part) => part.replace(/ /g, '\u00a0'))
    .join(' · ')
}

const STATUS_OPTIONS = [
  { value: ALL, label: 'All except deleted' },
  { value: 'active', label: 'Active' },
  { value: 'provisioning', label: 'Provisioning' },
  { value: 'suspended', label: 'Suspended' },
  { value: 'deleted', label: 'Deleted' },
] satisfies { value: typeof ALL | TenantStatus; label: string }[]

export function Tenants() {
  const q = usePagedQuery<Page<TenantSummary>>({ filterKeys: FILTER_KEYS, validate: validStatus, fetchPage: fetchTenants })
  const onStatus = useCallback((v: string) => q.setFilters(v === ALL ? {} : { status: v }), [q])

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Tenants"
        description="Every workspace and its footprint, as counts. The list isn't audited; opening a tenant is."
        actions={
          <Button variant="outline" onClick={q.retry} aria-label="Refresh tenants">
            <RefreshCw aria-hidden />
            Refresh
          </Button>
        }
      />

      <FilterChips label="Status filter" options={STATUS_OPTIONS} value={q.filters.status ?? ALL} onChange={onStatus} />

      <DataState
        state={q.state}
        what="tenants"
        emptyMessage={q.filters.status ? `No ${q.filters.status} tenants.` : 'No tenants yet.'}
        isEmpty={(d) => d.items.length === 0}
        onRetry={q.retry}
        onReset={q.resetFilters}
      >
        {(page) => (
          <Card>
            <Table aria-label="Tenants">
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>Tenant</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Connectors</TableHead>
                  <TableHead>Shields</TableHead>
                  <TableHead className="text-right">Networks</TableHead>
                  <TableHead className="text-right">Users</TableHead>
                  <TableHead className="text-right">Devices</TableHead>
                  <TableHead>Workspace CA</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {page.items.map((t) => (
                  <TableRow key={t.id} aria-label={t.name}>
                    <TableCell>
                      <div className="flex items-center gap-3">
                        <TenantAvatar name={t.name} />
                        <div className="flex min-w-0 flex-col">
                          <Link to={`/tenants/${encodeURIComponent(t.id)}`} className="font-medium text-foreground hover:text-primary">
                            {t.name}
                          </Link>
                          <span className="flex flex-col whitespace-nowrap font-mono text-xs text-muted-foreground">
                            <span>{t.slug}</span>
                            <span className="max-w-48 truncate" title={t.trust_domain}>
                              {t.trust_domain}
                            </span>
                          </span>
                        </div>
                      </div>
                    </TableCell>
                    <TableCell>
                      <StatusBadge status={t.status} />
                    </TableCell>
                    <TableCell className="min-w-28 max-w-36 text-[13px] leading-snug text-subtle-foreground">
                      <WrappingSummary counts={t.counts.connectors} />
                    </TableCell>
                    <TableCell className="min-w-28 max-w-36 text-[13px] leading-snug text-subtle-foreground">
                      <WrappingSummary counts={t.counts.shields} />
                    </TableCell>
                    <TableCell className="text-right font-mono text-[13px] tabular-nums">{t.counts.remote_networks}</TableCell>
                    <TableCell className="text-right font-mono text-[13px] tabular-nums" title="Active users (counts only)">
                      {t.counts.users.active.toLocaleString()}
                    </TableCell>
                    <TableCell className="text-right font-mono text-[13px] tabular-nums" title="Active devices">
                      {t.counts.client_devices.active.toLocaleString()}
                    </TableCell>
                    <TableCell>
                      <ExpiryBadge notAfter={t.ca_not_after} />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            <Pager page={q.page} hasPrev={q.hasPrev} hasNext={q.hasNext} onPrev={q.prev} onNext={q.next} />
          </Card>
        )}
      </DataState>
    </div>
  )
}
