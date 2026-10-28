import { useCallback, useState } from 'react'
import { Plus, RefreshCw } from 'lucide-react'
import { Link } from 'react-router-dom'
import { listRelays } from '@/api/reads'
import { createRelay } from '@/api/relays'
import { RELAY_STATUSES, type Page, type RelayStatus, type RelaySummary } from '@/api/types'
import { hasRole } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { AddRelayDialog, type NewRelay } from '@/components/AddRelayDialog'
import { CapacityTier, ConnectionsBar } from '@/components/Capacity'
import { DataState } from '@/components/DataState'
import { ExpiryBadge } from '@/components/ExpiryBadge'
import { FilterChips } from '@/components/FilterChips'
import { PageHeader } from '@/components/PageHeader'
import { Pager } from '@/components/Pager'
import { ProvisioningTokenDialog, type IssuedRelay } from '@/components/ProvisioningTokenDialog'
import { StatusBadge } from '@/components/StatusBadge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatRelative } from '@/lib/expiry'
import { formatDateTime, formatScope } from '@/lib/format'
import { relayErrorMessage } from '@/lib/relay-errors'
import { usePagedQuery, type Filters } from '@/lib/usePagedQuery'

// Relays (Phase P, relay.read: super-admin and relay-ops). Besides the
// status filter, paging, Refresh and Retry, the only control is Add relay
// (Sprint 22 Phase E, D-30): super-admin and relay-ops may create relays
// (D-31). Hiding the button is UX only; the controller decides.

const CAN_CREATE = ['super-admin', 'relay-ops'] as const

const FILTER_KEYS = ['status'] as const
const ALL = 'all' // chip value for "no status filter" (every status except deleted)

const STATUS_OPTIONS = [
  { value: ALL, label: 'All except deleted' },
  { value: 'active', label: 'Active' },
  { value: 'inactive', label: 'Inactive' },
  { value: 'pending', label: 'Pending' },
  { value: 'revoked', label: 'Revoked' },
  { value: 'deleted', label: 'Deleted' },
] satisfies { value: typeof ALL | RelayStatus; label: string }[]

function validStatus(f: Filters): boolean {
  return !f.status || (RELAY_STATUSES as readonly string[]).includes(f.status)
}

function fetchRelays(f: Filters, cursor: string | undefined): Promise<Page<RelaySummary>> {
  return listRelays({ status: f.status as RelayStatus | undefined, cursor })
}

export function Relays() {
  const q = usePagedQuery<Page<RelaySummary>>({ filterKeys: FILTER_KEYS, validate: validStatus, fetchPage: fetchRelays })
  const onStatus = useCallback((v: string) => q.setFilters(v === ALL ? {} : { status: v }), [q])
  const canCreate = hasRole(useSessionStore((s) => s.me?.role), CAN_CREATE)
  const [adding, setAdding] = useState(false)
  // Bumped on every opening so the form starts empty (it keeps its own state).
  const [addKey, setAddKey] = useState(0)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string>()
  // The one-time token lives here only while its dialog is open.
  const [issued, setIssued] = useState<IssuedRelay | null>(null)

  async function onCreate(relay: NewRelay) {
    setPending(true)
    setError(undefined)
    try {
      const res = await createRelay(relay.name, relay.dnsAllowlist, relay.ipAllowlist)
      setAdding(false)
      setIssued({
        name: relay.name,
        expiresAt: res.expires_at,
        relayId: res.relay_id,
        provisioningToken: res.provisioning_token,
        caFingerprint: res.ca_fingerprint,
        controllerGrpcAddr: res.controller_grpc_addr,
        controllerHttpAddr: res.controller_http_addr,
        dnsSans: relay.dnsAllowlist,
        ipSans: relay.ipAllowlist,
      })
    } catch (err) {
      setError(relayErrorMessage(err))
    } finally {
      setPending(false)
    }
  }

  function closeIssued() {
    setIssued(null)
    q.retry()
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Relays"
        description="The relay fleet that carries connector and client traffic: liveness, capacity and certificates."
        actions={
          <>
            <Button variant="outline" onClick={q.retry} aria-label="Refresh relays">
              <RefreshCw aria-hidden />
              Refresh
            </Button>
            {canCreate && (
              <Button
                onClick={() => {
                  setError(undefined)
                  setAddKey((k) => k + 1)
                  setAdding(true)
                }}
              >
                <Plus aria-hidden />
                Add relay
              </Button>
            )}
          </>
        }
      />

      <FilterChips label="Status filter" options={STATUS_OPTIONS} value={q.filters.status ?? ALL} onChange={onStatus} />

      <DataState
        state={q.state}
        what="relays"
        emptyMessage={q.filters.status ? `No ${q.filters.status} relays.` : 'No relays yet.'}
        isEmpty={(d) => d.items.length === 0}
        onRetry={q.retry}
        onReset={q.resetFilters}
      >
        {(page) => (
          <Card>
            <Table aria-label="Relays">
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead>Relay</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Version</TableHead>
                  <TableHead>Connections</TableHead>
                  <TableHead>Capacity tier</TableHead>
                  <TableHead className="text-right">Connectors</TableHead>
                  <TableHead>Last heartbeat</TableHead>
                  <TableHead>Certificate</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {page.items.map((r) => (
                  <TableRow key={r.id} aria-label={r.name}>
                    <TableCell>
                      <Link to={`/relays/${encodeURIComponent(r.id)}`} className="font-medium text-foreground hover:text-primary">
                        {r.name}
                      </Link>
                      <div className="flex flex-col whitespace-nowrap font-mono text-xs text-muted-foreground">
                        <span>{r.hostname ?? '—'}</span>
                        <span>
                          <span>{r.public_addr ?? '—'}</span>
                          {r.address_scope && <span className="font-sans"> · {formatScope(r.address_scope)}</span>}
                        </span>
                      </div>
                    </TableCell>
                    <TableCell>
                      <StatusBadge status={r.status} />
                      {r.status === 'pending' && (
                        <div className="mt-1 whitespace-nowrap text-xs text-muted-foreground">Waiting for enrolment</div>
                      )}
                    </TableCell>
                    <TableCell className="font-mono text-[13px] text-subtle-foreground">{r.version ?? '—'}</TableCell>
                    <TableCell className="min-w-36">
                      <ConnectionsBar count={r.connection_count} max={r.max_connections} />
                    </TableCell>
                    <TableCell>
                      <CapacityTier label={r.capacity_label} />
                    </TableCell>
                    <TableCell className="text-right font-mono text-[13px] tabular-nums">{r.attached_connectors}</TableCell>
                    <TableCell className="whitespace-nowrap text-subtle-foreground" title={formatDateTime(r.last_heartbeat_at)}>
                      {formatRelative(r.last_heartbeat_at)}
                    </TableCell>
                    <TableCell>
                      <ExpiryBadge notAfter={r.cert_not_after} />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
            <Pager page={q.page} hasPrev={q.hasPrev} hasNext={q.hasNext} onPrev={q.prev} onNext={q.next} />
          </Card>
        )}
      </DataState>

      {canCreate && (
        <AddRelayDialog
          key={addKey}
          open={adding}
          pending={pending}
          error={error}
          onSubmit={(relay) => void onCreate(relay)}
          onCancel={() => setAdding(false)}
        />
      )}
      <ProvisioningTokenDialog relay={issued} onClose={closeIssued} />
    </div>
  )
}
