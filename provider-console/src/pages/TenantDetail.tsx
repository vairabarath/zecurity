import type { ReactNode } from 'react'
import { ArrowLeft, RefreshCw } from 'lucide-react'
import { Link, Navigate, useParams } from 'react-router-dom'
import { getTenant } from '@/api/reads'
import type { TenantDetail as TenantDetailDTO } from '@/api/types'
import { ExpiryBadge } from '@/components/ExpiryBadge'
import { StatusBadge } from '@/components/StatusBadge'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
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
  <Link to="/tenants" className="inline-flex items-center gap-1 text-sm text-primary underline-offset-4 hover:underline">
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
      <CardHeader>
        <CardTitle>
          <h2>{title}</h2>
        </CardTitle>
      </CardHeader>
      <CardContent>{children}</CardContent>
    </Card>
  )
}

function TruncatedNote({ show }: { show: boolean }) {
  if (!show) return null
  return <p className="pt-2 text-xs text-muted-foreground">Showing the first 200 entries.</p>
}

function TenantView({ tenant: t, onRefresh }: { tenant: TenantDetailDTO; onRefresh: () => void }) {
  // Names for ids inside the already-fetched response (no extra requests).
  const networkName = new Map(t.remote_networks.map((n) => [n.id, n.name]))
  const connectorName = new Map(t.connectors.map((c) => [c.id, c.name]))
  const c = t.counts

  return (
    <div className="flex flex-col gap-6">
      {back}
      <div className="flex flex-wrap items-center justify-between gap-4">
        <div className="flex flex-wrap items-center gap-3">
          <h1 className="font-display text-2xl font-semibold tracking-tight">{t.name}</h1>
          <StatusBadge status={t.status} />
        </div>
        <Button variant="outline" onClick={onRefresh}>
          <RefreshCw aria-hidden />
          Refresh (records an audit entry)
        </Button>
      </div>

      <Section title="Overview">
        <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2 text-sm">
          <dt className="text-muted-foreground">Slug</dt>
          <dd className="font-mono text-xs">{t.slug}</dd>
          <dt className="text-muted-foreground">Trust domain</dt>
          <dd className="font-mono text-xs">{t.trust_domain}</dd>
          <dt className="text-muted-foreground">Workspace CA</dt>
          <dd>
            {t.ca_not_after ? (
              <span className="inline-flex items-center gap-2">
                {formatDateTime(t.ca_not_after)}
                <ExpiryBadge notAfter={t.ca_not_after} />
              </span>
            ) : (
              '—'
            )}
          </dd>
          <dt className="text-muted-foreground">Created</dt>
          <dd>{formatDateTime(t.created_at)}</dd>
          <dt className="text-muted-foreground">Connectors</dt>
          <dd>{agentSummary(c.connectors)}</dd>
          <dt className="text-muted-foreground">Shields</dt>
          <dd>{agentSummary(c.shields)}</dd>
          <dt className="text-muted-foreground">Remote networks</dt>
          <dd>{c.remote_networks} active</dd>
          <dt className="text-muted-foreground">Users</dt>
          <dd aria-label="User counts">
            {c.users.active} active · {c.users.suspended} suspended · {c.users.locked} locked · {c.users.deleted} deleted
          </dd>
          <dt className="text-muted-foreground">Client devices</dt>
          <dd>
            {c.client_devices.active} active · {c.client_devices.re_enroll_required} re-enroll required ·{' '}
            {c.client_devices.renew_pending} renew pending · {c.client_devices.revoked} revoked
          </dd>
        </dl>
      </Section>

      <Section title="Remote networks">
        {t.remote_networks.length === 0 ? (
          <p className="text-sm text-muted-foreground">No remote networks.</p>
        ) : (
          <Table aria-label="Remote networks">
            <TableHeader>
              <TableRow>
                <TableHead>Name</TableHead>
                <TableHead>Status</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {t.remote_networks.map((n) => (
                <TableRow key={n.id} aria-label={n.name}>
                  <TableCell>{n.name}</TableCell>
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
          <p className="text-sm text-muted-foreground">No connectors.</p>
        ) : (
          <Table aria-label="Connectors">
            <TableHeader>
              <TableRow>
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
                  <TableCell>{k.name}</TableCell>
                  <TableCell>
                    <StatusBadge status={k.status} />
                  </TableCell>
                  <TableCell>{networkName.get(k.remote_network_id) ?? <span className="font-mono text-xs">{k.remote_network_id}</span>}</TableCell>
                  <TableCell>{k.version ?? '—'}</TableCell>
                  <TableCell className="whitespace-nowrap" title={formatDateTime(k.last_heartbeat_at)}>
                    {formatRelative(k.last_heartbeat_at)}
                  </TableCell>
                  <TableCell>
                    <ExpiryBadge notAfter={k.cert_not_after} />
                  </TableCell>
                  <TableCell className="whitespace-nowrap">{k.revoked_at ? formatDateTime(k.revoked_at) : '—'}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        <TruncatedNote show={t.connectors_truncated} />
      </Section>

      <Section title="Shields">
        {t.shields.length === 0 ? (
          <p className="text-sm text-muted-foreground">No shields.</p>
        ) : (
          <Table aria-label="Shields">
            <TableHeader>
              <TableRow>
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
                  <TableCell>{s.name}</TableCell>
                  <TableCell>
                    <StatusBadge status={s.status} />
                  </TableCell>
                  <TableCell>{connectorName.get(s.connector_id) ?? <span className="font-mono text-xs">{s.connector_id}</span>}</TableCell>
                  <TableCell>{networkName.get(s.remote_network_id) ?? <span className="font-mono text-xs">{s.remote_network_id}</span>}</TableCell>
                  <TableCell className="whitespace-nowrap" title={formatDateTime(s.last_heartbeat_at)}>
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
