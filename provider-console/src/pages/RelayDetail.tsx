import type { ReactNode } from 'react'
import { ArrowLeft, RefreshCw } from 'lucide-react'
import { Link, Navigate, useParams } from 'react-router-dom'
import { getRelay } from '@/api/reads'
import type { RelayDetail as RelayDetailDTO } from '@/api/types'
import { CapacityTier } from '@/components/Capacity'
import { ExpiryBadge } from '@/components/ExpiryBadge'
import { StatusBadge } from '@/components/StatusBadge'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatRelative } from '@/lib/expiry'
import { formatDateTime, formatScope } from '@/lib/format'
import { useDetailQuery } from '@/lib/useDetailQuery'

// Relay detail (Phase P, relay.read). Read-only. One request when the page
// opens; Refresh and Retry are the only other requests. Certificate state
// comes from the API alone: "Current" is is_current, revocation is
// revoked_at/revocation_reason; only the expiry badge is derived (bucketFor).

function Section({ title, description, children, flush }: { title: string; description?: string; children: ReactNode; flush?: boolean }) {
  return (
    <Card>
      <div className="flex flex-col gap-0.5 px-5 py-4">
        <h2 className="font-semibold">{title}</h2>
        {description && <p className="text-[13px] text-muted-foreground">{description}</p>}
      </div>
      <div className={flush ? undefined : 'px-5 pb-5'}>{children}</div>
    </Card>
  )
}

function TruncatedNote({ show }: { show: boolean }) {
  if (!show) return null
  return <p className="px-5 py-3 text-xs text-muted-foreground">Showing the first 200 entries.</p>
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex min-w-36 flex-[1_1_150px] flex-col gap-1">
      <dt className="text-xs font-medium tracking-wide text-muted-foreground">{label}</dt>
      <dd className="text-sm">{children}</dd>
    </div>
  )
}

function Tags({ label, items }: { label: string; items: string[] }) {
  if (items.length === 0) return <p className="text-sm text-muted-foreground">None</p>
  return (
    <ul aria-label={label} className="flex flex-wrap gap-2">
      {items.map((v) => (
        <li key={v} className="inline-flex min-h-7 items-center rounded-lg border border-input bg-secondary px-2.5 font-mono text-[12.5px] text-secondary-foreground">
          {v}
        </li>
      ))}
    </ul>
  )
}

/** A ring of connection_count over max_connections; amber from 85% used. */
function ConnectionsRing({ count, max }: { count: number; max: number }) {
  const r = 50
  const c = 2 * Math.PI * r
  const pct = max > 0 ? Math.min(1, count / max) : 0
  return (
    <svg width="112" height="112" viewBox="0 0 120 120" role="img" aria-label={max > 0 ? `${count} of ${max} connections used` : 'Connections not reported'}>
      <circle cx="60" cy="60" r={r} fill="none" stroke="var(--color-border)" strokeWidth="12" />
      {pct > 0 && (
        <circle
          cx="60"
          cy="60"
          r={r}
          fill="none"
          stroke={pct >= 0.85 ? 'var(--color-warning)' : 'var(--color-secure)'}
          strokeWidth="12"
          strokeLinecap="round"
          strokeDasharray={`${pct * c} ${c}`}
          transform="rotate(-90 60 60)"
        />
      )}
      <text x="60" y="58" textAnchor="middle" fill="var(--color-foreground)" fontSize="22" fontWeight="600">
        {count.toLocaleString()}
      </text>
      <text x="60" y="78" textAnchor="middle" fill="var(--color-muted-foreground)" fontSize="12">
        {max > 0 ? `of ${max.toLocaleString()}` : 'no max'}
      </text>
    </svg>
  )
}

export function RelayDetail() {
  const { id = '' } = useParams()
  const { state, reload } = useDetailQuery(id, getRelay)

  const back = (
    <Link to="/relays" className="inline-flex items-center gap-1 self-start text-[13px] text-muted-foreground hover:text-primary">
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
  const pct = r.max_connections > 0 ? Math.round((r.connection_count / r.max_connections) * 100) : null
  return (
    <div className="flex flex-col gap-6">
      {back}
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div className="flex flex-col gap-2">
          <div className="flex flex-wrap items-center gap-3">
            <h1 className="font-mono text-[26px] font-medium tracking-tight">{r.name}</h1>
            <StatusBadge status={r.status} />
          </div>
          <p className="text-sm text-subtle-foreground">
            {formatScope(r.address_scope)} address · version <span className="font-mono">{r.version ?? '—'}</span> · created {formatDateTime(r.created_at)}
          </p>
        </div>
        <div className="flex items-center gap-3">
          <span className="text-[13px] text-muted-foreground" title={formatDateTime(r.last_heartbeat_at)}>
            Heartbeat {formatRelative(r.last_heartbeat_at)}
          </span>
          <Button variant="outline" onClick={onRefresh} aria-label="Refresh relay">
            <RefreshCw aria-hidden />
            Refresh
          </Button>
        </div>
      </header>

      {r.status === 'pending' && (
        <Alert>
          <AlertDescription>
            Waiting for enrolment: the relay becomes active when it provisions with its one-time token (valid for 24
            hours). If the token has expired, delete this relay and add a new one.
          </AlertDescription>
        </Alert>
      )}

      <div className="flex flex-wrap gap-4">
        <Card className="flex-[2_1_520px] px-5 py-5">
          <h2 className="sr-only">Overview</h2>
          <dl className="flex flex-wrap gap-x-6 gap-y-5">
            <Fact label="Hostname">
              <span className="font-mono">{r.hostname ?? '—'}</span>
            </Fact>
            <Fact label="Public address">
              <span className="font-mono">{r.public_addr ?? '—'}</span>
            </Fact>
            <Fact label="Address scope">{formatScope(r.address_scope)}</Fact>
            <Fact label="Version">
              <span className="font-mono">{r.version ?? '—'}</span>
            </Fact>
            <Fact label="Attached connectors">
              <span className="font-mono">{r.attached_connectors}</span>
            </Fact>
            <Fact label="Last heartbeat">{formatDateTime(r.last_heartbeat_at)}</Fact>
            <Fact label="Current certificate">
              <span className="font-mono">{r.cert_serial ?? '—'}</span>
            </Fact>
            <Fact label="Certificate expires">
              <span className="inline-flex flex-wrap items-center gap-2">
                {r.cert_not_after && <span>{formatDateTime(r.cert_not_after)}</span>}
                <ExpiryBadge notAfter={r.cert_not_after} />
              </span>
            </Fact>
            <Fact label="Relay ID">
              <span className="break-all font-mono text-subtle-foreground">{r.id}</span>
            </Fact>
          </dl>
        </Card>

        <Card className="flex flex-[1_1_300px] flex-col gap-3.5 px-5 py-5" aria-labelledby="relay-connections">
          <div className="flex items-baseline justify-between">
            <h2 id="relay-connections" className="font-semibold">
              Connections
            </h2>
            {pct !== null && (
              <Badge variant={pct >= 85 ? 'warning' : 'secondary'}>{pct}% used</Badge>
            )}
          </div>
          <div className="flex items-center gap-5">
            <ConnectionsRing count={r.connection_count} max={r.max_connections} />
            <dl className="flex flex-col gap-2.5 text-[13px]">
              <div className="flex flex-col gap-0.5">
                <dt className="text-xs text-muted-foreground">Max connections</dt>
                <dd className="font-mono">{r.max_connections > 0 ? r.max_connections.toLocaleString() : 'Not reported'}</dd>
              </div>
              <div className="flex flex-col gap-0.5">
                <dt className="text-xs text-muted-foreground">Capacity tier</dt>
                <dd>
                  <CapacityTier label={r.capacity_label} />
                </dd>
              </div>
            </dl>
          </div>
          <p className="text-[13px] text-subtle-foreground">Connectors choose relays by the published capacity tier, so a low-tier relay receives fewer new placements.</p>
        </Card>
      </div>

      <Section title="Allowlists">
        <div className="grid gap-6 sm:grid-cols-2">
          <div className="flex flex-col gap-3">
            <h3 className="text-sm font-medium">DNS names</h3>
            <Tags label="DNS allowlist" items={r.dns_allowlist} />
          </div>
          <div className="flex flex-col gap-3">
            <h3 className="text-sm font-medium">IP addresses</h3>
            <Tags label="IP allowlist" items={r.ip_allowlist} />
          </div>
        </div>
      </Section>

      <Section title="Certificate history" description="Every certificate issued to this relay. Serials and dates only." flush>
        {r.certificates.length === 0 ? (
          <p className="px-5 pb-5 text-sm text-muted-foreground">No certificates issued yet.</p>
        ) : (
          <Table aria-label="Certificate history">
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Serial</TableHead>
                <TableHead>State</TableHead>
                <TableHead>Issued</TableHead>
                <TableHead>Expires</TableHead>
                <TableHead>Revocation</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {r.certificates.map((c) => (
                <TableRow key={c.serial} aria-label={c.serial} data-current={c.is_current || undefined} className={c.is_current ? 'bg-primary-soft/40' : undefined}>
                  <TableCell className="font-mono text-[13px]">{c.serial}</TableCell>
                  <TableCell>
                    <span className="inline-flex flex-wrap items-center gap-1.5">
                      {c.is_current && <Badge variant="default">Current</Badge>}
                      {c.revoked_at && <Badge variant="destructive">Revoked</Badge>}
                      {!c.is_current && !c.revoked_at && <span className="text-muted-foreground">—</span>}
                    </span>
                  </TableCell>
                  <TableCell className="whitespace-nowrap text-subtle-foreground">{formatDateTime(c.issued_at)}</TableCell>
                  <TableCell className="whitespace-nowrap">
                    <span className="inline-flex items-center gap-2">
                      {formatDateTime(c.not_after)}
                      <ExpiryBadge notAfter={c.not_after} />
                    </span>
                  </TableCell>
                  <TableCell className="text-[13px] text-subtle-foreground">
                    {c.revoked_at ? `${formatDateTime(c.revoked_at)}${c.revocation_reason ? ` · ${c.revocation_reason}` : ''}` : '—'}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        <TruncatedNote show={r.certificates_truncated} />
      </Section>

      <Section title="Attached connectors" description="Connectors this relay carries, by ID." flush>
        {r.attachments.length === 0 ? (
          <p className="px-5 pb-5 text-sm text-muted-foreground">No connectors attached.</p>
        ) : (
          <Table aria-label="Attached connectors">
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Connector ID</TableHead>
                <TableHead>Tenant ID</TableHead>
                <TableHead>Attached</TableHead>
                <TableHead>Last confirmed</TableHead>
                <TableHead>Source</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {r.attachments.map((a) => (
                <TableRow key={a.connector_id}>
                  <TableCell className="font-mono text-[13px]">{a.connector_id}</TableCell>
                  {/* Plain text: tenant detail is an audited, super-admin read, not a casual link. */}
                  <TableCell className="font-mono text-[13px]">{a.tenant_id}</TableCell>
                  <TableCell className="whitespace-nowrap text-subtle-foreground">{formatDateTime(a.attached_at)}</TableCell>
                  <TableCell className="whitespace-nowrap text-subtle-foreground" title={formatDateTime(a.last_confirmed)}>
                    {formatRelative(a.last_confirmed)}
                  </TableCell>
                  <TableCell>
                    <Badge variant={a.source === 'event' ? 'info' : 'secondary'} className="capitalize">
                      {a.source}
                    </Badge>
                  </TableCell>
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
