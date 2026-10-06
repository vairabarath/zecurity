import type { ReactNode } from 'react'
import { ArrowLeft, RefreshCw } from 'lucide-react'
import { Link, Navigate, useParams } from 'react-router-dom'
import { getRelay } from '@/api/reads'
import type { RelayDetail as RelayDetailDTO } from '@/api/types'
import { ExpiryBadge } from '@/components/ExpiryBadge'
import { StatusBadge } from '@/components/StatusBadge'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatRelative } from '@/lib/expiry'
import { formatDateTime } from '@/lib/format'
import { useDetailQuery } from '@/lib/useDetailQuery'

// Relay detail (Phase P, relay.read). Read-only. One request when the page
// opens; Refresh and Retry are the only other requests. Certificate state
// comes from the API alone: "Current" is is_current, revocation is
// revoked_at/revocation_reason; only the expiry badge is derived (bucketFor).

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

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd>{children}</dd>
    </>
  )
}

export function RelayDetail() {
  const { id = '' } = useParams()
  const { state, reload } = useDetailQuery(id, getRelay)

  const back = (
    <Link to="/relays" className="inline-flex items-center gap-1 text-sm text-primary underline-offset-4 hover:underline">
      <ArrowLeft className="size-4" aria-hidden />
      Back to relays
    </Link>
  )

  switch (state.status) {
    case 'loading':
      return (
        <div aria-busy="true" aria-label="Loading relay" className="flex flex-col gap-3">
          <Skeleton className="h-10 w-64" />
          <Skeleton className="h-40 w-full" />
        </div>
      )
    case 'session_ended':
      return null
    case 'forbidden':
      return <Navigate to="/forbidden" replace />
    case 'not_found':
      return (
        <div className="flex flex-col items-start gap-3 py-10">
          <h1 className="font-display text-xl font-semibold">Relay not found</h1>
          <p className="text-sm text-muted-foreground">It may have been removed, or the link is wrong.</p>
          {back}
        </div>
      )
    case 'error':
      return (
        <div className="flex flex-col gap-4">
          {back}
          <Alert variant="destructive">
            <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
              <span>Couldn’t load this relay.</span>
              <Button variant="outline" size="sm" onClick={reload}>
                Retry
              </Button>
            </AlertDescription>
          </Alert>
        </div>
      )
    case 'ready':
      return <RelayView relay={state.data} back={back} onRefresh={reload} />
  }
}

function RelayView({ relay: r, back, onRefresh }: { relay: RelayDetailDTO; back: ReactNode; onRefresh: () => void }) {
  return (
    <div className="flex flex-col gap-6">
      {back}
      <div className="flex flex-wrap items-center justify-between gap-4">
        <div className="flex flex-wrap items-center gap-3">
          <h1 className="font-display text-2xl font-semibold tracking-tight">{r.name}</h1>
          <StatusBadge status={r.status} />
        </div>
        <Button variant="outline" onClick={onRefresh} aria-label="Refresh relay">
          <RefreshCw aria-hidden />
          Refresh
        </Button>
      </div>

      <Section title="Overview">
        <dl className="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-2 text-sm">
          <Fact label="Version">{r.version ?? '—'}</Fact>
          <Fact label="Hostname">{r.hostname ?? '—'}</Fact>
          <Fact label="Public address">
            <span className="font-mono text-xs">{r.public_addr ?? '—'}</span>
            {r.address_scope && <span className="text-muted-foreground"> ({r.address_scope})</span>}
          </Fact>
          <Fact label="Capacity">
            {r.capacity_label} · {r.connection_count} / {r.max_connections}
          </Fact>
          <Fact label="Last heartbeat">
            <span title={formatDateTime(r.last_heartbeat_at)}>{formatRelative(r.last_heartbeat_at)}</span>
          </Fact>
          <Fact label="Certificate">
            <span className="inline-flex flex-wrap items-center gap-2">
              <span className="font-mono text-xs">{r.cert_serial ?? '—'}</span>
              <ExpiryBadge notAfter={r.cert_not_after} />
            </span>
          </Fact>
          <Fact label="Created">{formatDateTime(r.created_at)}</Fact>
        </dl>
      </Section>

      <Section title="Allowlists">
        <div className="grid gap-4 text-sm sm:grid-cols-2">
          <div>
            <h3 className="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground">DNS names</h3>
            {r.dns_allowlist.length === 0 ? (
              <p className="text-muted-foreground">None</p>
            ) : (
              <ul aria-label="DNS allowlist" className="font-mono text-xs">
                {r.dns_allowlist.map((d) => (
                  <li key={d}>{d}</li>
                ))}
              </ul>
            )}
          </div>
          <div>
            <h3 className="mb-1 text-xs font-medium uppercase tracking-wide text-muted-foreground">IP addresses</h3>
            {r.ip_allowlist.length === 0 ? (
              <p className="text-muted-foreground">None</p>
            ) : (
              <ul aria-label="IP allowlist" className="font-mono text-xs">
                {r.ip_allowlist.map((ip) => (
                  <li key={ip}>{ip}</li>
                ))}
              </ul>
            )}
          </div>
        </div>
      </Section>

      <Section title="Certificate history">
        {r.certificates.length === 0 ? (
          <p className="text-sm text-muted-foreground">No certificates issued yet.</p>
        ) : (
          <Table aria-label="Certificate history">
            <TableHeader>
              <TableRow>
                <TableHead>Serial</TableHead>
                <TableHead>Issued</TableHead>
                <TableHead>Expires</TableHead>
                <TableHead>State</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {r.certificates.map((c) => (
                <TableRow key={c.serial} aria-label={c.serial} data-current={c.is_current || undefined} className={c.is_current ? 'bg-primary/5' : undefined}>
                  <TableCell className="font-mono text-xs">{c.serial}</TableCell>
                  <TableCell className="whitespace-nowrap">{formatDateTime(c.issued_at)}</TableCell>
                  <TableCell className="whitespace-nowrap">
                    <span className="inline-flex items-center gap-2">
                      {formatDateTime(c.not_after)}
                      <ExpiryBadge notAfter={c.not_after} />
                    </span>
                  </TableCell>
                  <TableCell>
                    <span className="inline-flex flex-wrap items-center gap-1.5">
                      {c.is_current && <Badge variant="outline" className="border-primary/40 text-primary">Current</Badge>}
                      {c.revoked_at && (
                        <>
                          <Badge variant="destructive">Revoked</Badge>
                          <span className="text-xs text-muted-foreground">
                            {formatDateTime(c.revoked_at)}
                            {c.revocation_reason ? ` · ${c.revocation_reason}` : ''}
                          </span>
                        </>
                      )}
                    </span>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        <TruncatedNote show={r.certificates_truncated} />
      </Section>

      <Section title="Attached connectors">
        {r.attachments.length === 0 ? (
          <p className="text-sm text-muted-foreground">No connectors attached.</p>
        ) : (
          <Table aria-label="Attached connectors">
            <TableHeader>
              <TableRow>
                <TableHead>Connector</TableHead>
                <TableHead>Tenant</TableHead>
                <TableHead>Attached</TableHead>
                <TableHead>Last confirmed</TableHead>
                <TableHead>Source</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {r.attachments.map((a) => (
                <TableRow key={a.connector_id}>
                  <TableCell className="font-mono text-xs">{a.connector_id}</TableCell>
                  {/* Plain text: tenant detail is an audited, super-admin read, not a casual link. */}
                  <TableCell className="font-mono text-xs">{a.tenant_id}</TableCell>
                  <TableCell className="whitespace-nowrap">{formatDateTime(a.attached_at)}</TableCell>
                  <TableCell className="whitespace-nowrap" title={formatDateTime(a.last_confirmed)}>
                    {formatRelative(a.last_confirmed)}
                  </TableCell>
                  <TableCell>{a.source}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        <TruncatedNote show={r.attachments_truncated} />
      </Section>
    </div>
  )
}
