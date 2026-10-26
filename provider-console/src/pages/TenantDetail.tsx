import type { ReactNode } from 'react'
import { ArrowLeft, Eye, RefreshCw } from 'lucide-react'
import { Link, Navigate, useParams } from 'react-router-dom'
import { getTenant } from '@/api/reads'
import type { TenantDetail as TenantDetailDTO } from '@/api/types'
import { ExpiryBadge } from '@/components/ExpiryBadge'
import { StatusBadge } from '@/components/StatusBadge'
import { TenantAvatar } from '@/components/TenantAvatar'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatRelative } from '@/lib/expiry'
import { agentSummary, formatDateTime } from '@/lib/format'
import { useDetailQuery } from '@/lib/useDetailQuery'

// Tenant detail (Phase P, tenant.read: super-admin only).
//
// AUDITED READ (OQ-1): every successful GET /provider/tenants/{id} writes a
// provider audit row. So this page owns exactly one fetch per visit
// (useDetailQuery: StrictMode-safe, no polling, no focus/visibility
// refetch, no automatic retry, no prefetch); the only other requests are an
// explicit Refresh or Retry. A malformed id is rejected here and never sent.
// Child components only render data already fetched; nothing in them links
// to another tenant detail.
//
// Infrastructure only (OQ-2): no tenant users or identities exist in this
// response; users are counts by status.

const UUID = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/

const back = (
  <Link to="/tenants" className="inline-flex items-center gap-1 self-start text-[13px] text-muted-foreground hover:text-primary">
    <ArrowLeft className="size-4" aria-hidden />
    Back to tenants
  </Link>
)

function NotFound() {
  return (
    <div className="flex flex-col items-start gap-3 py-10">
      <h1 className="font-display text-xl font-semibold">Tenant not found</h1>
      <p className="text-sm text-muted-foreground">It may have been removed, or the link is wrong.</p>
      {back}
    </div>
  )
}

export function TenantDetail() {
  const { id = '' } = useParams()
  // Validated before any request: a malformed id never reaches the server.
  if (!UUID.test(id)) return <NotFound />
  return <TenantDetailFetch key={id} id={id} />
}

/** The page-level owner of the single audited fetch. */
function TenantDetailFetch({ id }: { id: string }) {
  const { state, reload } = useDetailQuery(id, getTenant)

  switch (state.status) {
    case 'loading':
      return (
        <div aria-busy="true" aria-label="Loading tenant" className="flex flex-col gap-3">
          <Skeleton className="h-10 w-64" />
          <Skeleton className="h-40 w-full" />
        </div>
      )
    case 'session_ended':
      return null
    case 'forbidden':
      return <Navigate to="/forbidden" replace />
    case 'not_found':
      return <NotFound />
    case 'error':
      return (
        <div className="flex flex-col gap-4">
          {back}
          <Alert variant="destructive">
            <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
              <span>Couldn’t load this tenant.</span>
              <Button variant="outline" size="sm" onClick={reload}>
                Retry
              </Button>
            </AlertDescription>
          </Alert>
        </div>
      )
    case 'ready':
      return <TenantView tenant={state.data} onRefresh={reload} />
  }
}

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <Card>
      <h2 className="px-5 py-4 font-semibold">{title}</h2>
      {children}
    </Card>
  )
}

function Empty({ children }: { children: ReactNode }) {
  return <p className="px-5 pb-5 text-sm text-muted-foreground">{children}</p>
}

function TruncatedNote({ show }: { show: boolean }) {
  if (!show) return null
  return <p className="px-5 py-3 text-xs text-muted-foreground">Showing the first 200 entries.</p>
}

/** A headline count with the API's own per-status breakdown under it. */
function CountTile({ label, value, children }: { label: string; value: ReactNode; children: ReactNode }) {
  return (
    <Card className="flex min-w-0 flex-col gap-2.5 px-[18px] py-4">
      <span className="text-xs font-medium tracking-wide text-muted-foreground">{label}</span>
      <span className="text-[26px] font-semibold tracking-[-0.02em]">{value}</span>
      <div className="text-[12.5px] leading-relaxed text-muted-foreground">{children}</div>
    </Card>
  )
}

function TenantView({ tenant: t, onRefresh }: { tenant: TenantDetailDTO; onRefresh: () => void }) {
  // Names for ids inside the already-fetched response (no extra requests).
  const networkName = new Map(t.remote_networks.map((n) => [n.id, n.name]))
  const connectorName = new Map(t.connectors.map((c) => [c.id, c.name]))
  const c = t.counts

  return (
    <div className="flex flex-col gap-6">
      {back}

      <div role="note" className="flex items-center gap-2.5 rounded-lg border border-info/25 bg-info/10 px-3.5 py-2.5 text-[13px] text-[#b7d5fd]">
        <Eye className="size-4 shrink-0" aria-hidden />
        <span>
          Audited view. Each load of this page, including Refresh, writes a <span className="font-mono">tenant.read</span> entry to the provider audit log under your name.
        </span>
      </div>

      <header className="flex flex-wrap items-start justify-between gap-4">
        <div className="flex items-center gap-3.5">
          <TenantAvatar name={t.name} size="lg" />
          <div className="flex flex-col gap-1.5">
            <div className="flex flex-wrap items-center gap-3">
              <h1 className="font-display text-[26px] font-semibold tracking-[-0.02em]">{t.name}</h1>
              <StatusBadge status={t.status} />
            </div>
            <p className="flex flex-wrap gap-x-2 text-sm text-subtle-foreground">
              <span className="font-mono">{t.slug}</span>
              <span aria-hidden>·</span>
              <span>
                trust domain <span className="font-mono">{t.trust_domain}</span>
              </span>
              <span aria-hidden>·</span>
              <span>created {formatDateTime(t.created_at)}</span>
            </p>
          </div>
        </div>
        <Button variant="outline" onClick={onRefresh}>
          <RefreshCw aria-hidden />
          Refresh (records an audit entry)
        </Button>
      </header>

      <section aria-label="Overview" className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-5">
        <CountTile label="Users" value={c.users.active.toLocaleString()}>
          <span aria-label="User counts">
            {c.users.active} active · {c.users.suspended} suspended · {c.users.locked} locked · {c.users.deleted} deleted
          </span>
        </CountTile>
        <CountTile label="Client devices" value={c.client_devices.active.toLocaleString()}>
          {c.client_devices.active} active · {c.client_devices.re_enroll_required} re-enroll required · {c.client_devices.renew_pending} renew pending ·{' '}
          {c.client_devices.revoked} revoked
        </CountTile>
        <CountTile label="Connectors" value={c.connectors.active.toLocaleString()}>
          {agentSummary(c.connectors)}
        </CountTile>
        <CountTile label="Shields" value={c.shields.active.toLocaleString()}>
          {agentSummary(c.shields)}
        </CountTile>
        <CountTile label="Workspace CA" value={<ExpiryBadge notAfter={t.ca_not_after} />}>
          {t.ca_not_after ? `Expires ${formatDateTime(t.ca_not_after)}` : 'No CA issued'} · {c.remote_networks} active remote networks
        </CountTile>
      </section>

      <Section title="Remote networks">
        {t.remote_networks.length === 0 ? (
          <Empty>No remote networks.</Empty>
        ) : (
          <Table aria-label="Remote networks">
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Name</TableHead>
                <TableHead>Status</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {t.remote_networks.map((n) => (
                <TableRow key={n.id} aria-label={n.name}>
                  <TableCell className="font-medium">{n.name}</TableCell>
                  <TableCell>
                    <StatusBadge status={n.status} />
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        <TruncatedNote show={t.remote_networks_truncated} />
      </Section>

      <Section title="Connectors">
        {t.connectors.length === 0 ? (
          <Empty>No connectors.</Empty>
        ) : (
          <Table aria-label="Connectors">
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Name</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Remote network</TableHead>
                <TableHead>Version</TableHead>
                <TableHead>Last heartbeat</TableHead>
                <TableHead>Cert expiry</TableHead>
                <TableHead>Revoked</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {t.connectors.map((k) => (
                <TableRow key={k.id} aria-label={k.name}>
                  <TableCell className="font-mono text-[13px]">{k.name}</TableCell>
                  <TableCell>
                    <StatusBadge status={k.status} />
                  </TableCell>
                  <TableCell>{networkName.get(k.remote_network_id) ?? <span className="font-mono text-xs">{k.remote_network_id}</span>}</TableCell>
                  <TableCell className="font-mono text-[13px] text-subtle-foreground">{k.version ?? '—'}</TableCell>
                  <TableCell className="whitespace-nowrap text-subtle-foreground" title={formatDateTime(k.last_heartbeat_at)}>
                    {formatRelative(k.last_heartbeat_at)}
                  </TableCell>
                  <TableCell>
                    <ExpiryBadge notAfter={k.cert_not_after} />
                  </TableCell>
                  <TableCell className="whitespace-nowrap text-subtle-foreground">{k.revoked_at ? formatDateTime(k.revoked_at) : '—'}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        <TruncatedNote show={t.connectors_truncated} />
      </Section>

      <Section title="Shields">
        {t.shields.length === 0 ? (
          <Empty>No shields.</Empty>
        ) : (
          <Table aria-label="Shields">
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Name</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Connector</TableHead>
                <TableHead>Remote network</TableHead>
                <TableHead>Last heartbeat</TableHead>
                <TableHead>Cert expiry</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {t.shields.map((s) => (
                <TableRow key={s.id} aria-label={s.name}>
                  <TableCell className="font-mono text-[13px]">{s.name}</TableCell>
                  <TableCell>
                    <StatusBadge status={s.status} />
                  </TableCell>
                  <TableCell className="font-mono text-[13px]">{connectorName.get(s.connector_id) ?? <span className="text-xs">{s.connector_id}</span>}</TableCell>
                  <TableCell>{networkName.get(s.remote_network_id) ?? <span className="font-mono text-xs">{s.remote_network_id}</span>}</TableCell>
                  <TableCell className="whitespace-nowrap text-subtle-foreground" title={formatDateTime(s.last_heartbeat_at)}>
                    {formatRelative(s.last_heartbeat_at)}
                  </TableCell>
                  <TableCell>
                    <ExpiryBadge notAfter={s.cert_not_after} />
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        <TruncatedNote show={t.shields_truncated} />
      </Section>
    </div>
  )
}
