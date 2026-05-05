import { useMemo } from 'react'
import { useQuery, useMutation } from '@apollo/client/react'
import { Laptop, ShieldOff } from 'lucide-react'
import { toast } from 'sonner'
import {
  GetClientDevicesDocument,
  RevokeDeviceDocument,
  type GetClientDevicesQuery,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState, StatusPill, relativeTime } from '@/lib/console'

type Device = GetClientDevicesQuery['clientDevices'][number]

function shortId(id: string): string {
  return id.length > 10 ? `${id.slice(0, 8)}…` : id
}

function formatExpiry(notAfter: string | null | undefined): string {
  if (!notAfter) return '—'
  const d = new Date(notAfter)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' })
}

export default function DeviceManagement() {
  const { data, loading } = useQuery(GetClientDevicesDocument, {
    fetchPolicy: 'cache-and-network',
    pollInterval: 30000,
  })

  const [revokeDevice, { loading: revoking }] = useMutation(RevokeDeviceDocument, {
    refetchQueries: [{ query: GetClientDevicesDocument }],
    awaitRefetchQueries: true,
  })

  const devices = useMemo<Device[]>(() => data?.clientDevices ?? [], [data])
  const activeCount = devices.filter((d) => !d.revokedAt).length

  async function handleRevoke(device: Device) {
    if (!window.confirm(`Revoke device "${device.name}"? The client will be rejected on its next connection.`)) return
    try {
      await revokeDevice({ variables: { deviceId: device.id } })
      toast.success('Device revoked.')
    } catch (err) {
      const msg = err instanceof Error ? err.message : 'Failed to revoke device.'
      toast.error(msg)
    }
  }

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Devices</h2>
          <p className="page-subtitle">Enrolled client devices in this workspace.</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="status-pill border-[oklch(0.82_0.12_160/0.28)] bg-[oklch(0.82_0.12_160/0.12)] text-[oklch(0.82_0.12_160)]">
            <span className="status-pill-dot bg-[oklch(0.82_0.12_160)]" />
            <span className="font-bold">{activeCount}</span> active
          </span>
          <span className="status-pill border-border bg-secondary text-muted-foreground">
            <span className="font-bold text-foreground">{devices.length}</span> total
          </span>
        </div>
      </div>

      <div className="table-shell">
        <div className="table-scroll">
          <div className="table-head grid min-w-[1200px] items-center grid-cols-[1.4fr_120px_1.5fr_160px_140px_120px_120px] gap-4 px-5 py-4">
            {['Device', 'OS', 'SPIFFE ID', 'Cert Expires', 'Last Seen', 'Status', 'Actions'].map((label, index) => (
              <div key={label} className={`table-head-label ${index === 6 ? 'text-right' : ''}`}>{label}</div>
            ))}
          </div>

          {loading && !data ? (
            <div className="min-w-[1200px] p-5 space-y-3">
              {Array.from({ length: 5 }).map((_, index) => (
                <Skeleton key={index} className="h-16 rounded-2xl bg-secondary" />
              ))}
            </div>
          ) : devices.length === 0 ? (
            <EmptyState
              icon={<Laptop className="h-6 w-6" />}
              title="No enrolled devices"
              description="Devices appear here as users enroll through the client."
            />
          ) : (
            <div className="min-w-[1200px]">
              {devices.map((device) => {
                const revoked = !!device.revokedAt
                return (
                  <div key={device.id} className="admin-table-row grid items-center grid-cols-[1.4fr_120px_1.5fr_160px_140px_120px_120px] gap-4 px-5 py-4">
                    <div className="flex min-w-0 items-center gap-3">
                      <span className="grid h-9 w-9 place-items-center rounded-xl border border-[oklch(0.78_0.09_310/0.25)] bg-[oklch(0.78_0.09_310/0.14)] text-[oklch(0.78_0.09_310)]">
                        <Laptop className="h-4 w-4" />
                      </span>
                      <div className="min-w-0">
                        <div className="truncate text-[15px] font-bold leading-tight">{device.name}</div>
                        <div className="truncate font-mono text-[10.5px] tracking-tight text-muted-foreground/70">
                          {shortId(device.id)}
                        </div>
                      </div>
                    </div>

                    <div className="truncate text-sm text-muted-foreground">{device.os || '—'}</div>

                    <div className="truncate font-mono text-[12px] text-muted-foreground/80" title={device.spiffeId ?? ''}>
                      {device.spiffeId ?? '—'}
                    </div>

                    <div className="font-mono text-[12.5px] text-muted-foreground/80">
                      {formatExpiry(device.certNotAfter)}
                    </div>

                    <div className="font-mono text-[12.5px] text-muted-foreground/80">
                      {relativeTime(device.lastSeenAt)}
                    </div>

                    <div>
                      <StatusPill label={revoked ? 'revoked' : 'active'} tone={revoked ? 'danger' : 'ok'} />
                    </div>

                    <div className="text-right">
                      {revoked ? (
                        <span className="text-[12.5px] text-muted-foreground/60">—</span>
                      ) : (
                        <Button
                          variant="ghost"
                          size="sm"
                          disabled={revoking}
                          onClick={() => handleRevoke(device)}
                          className="gap-1.5 text-[oklch(0.75_0.16_25)] hover:bg-[oklch(0.75_0.16_25/0.1)]"
                        >
                          <ShieldOff className="h-3.5 w-3.5" />
                          Revoke
                        </Button>
                      )}
                    </div>
                  </div>
                )
              })}
            </div>
          )}
        </div>
      </div>
    </div>
  )
}
