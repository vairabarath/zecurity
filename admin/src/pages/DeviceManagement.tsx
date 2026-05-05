import { useState } from 'react'
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
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogFooter,
} from '@/components/ui/dialog'
import { EmptyState, StatusPill, relativeTime } from '@/lib/console'

type Device = GetClientDevicesQuery['clientDevices'][number]

function shortId(id: string): string {
  return id.length > 10 ? `${id.slice(0, 8)}…` : id
}

function DeviceRow({
  device,
  onRevoke,
}: {
  device: Device
  onRevoke: (id: string, name: string) => void
}) {
  const isRevoked = !!device.revokedAt
  return (
    <div className={`admin-table-row grid-cols-[1fr_80px_1fr_130px_130px_130px_100px_80px] ${isRevoked ? 'opacity-60' : ''}`}>
      <span className="font-mono text-[12px] truncate" title={device.id}>
        {shortId(device.id)}
      </span>
      <span className="text-[12.5px] text-muted-foreground capitalize">{device.os}</span>
      <span className="truncate text-[13px]">{device.commonName}</span>
      <span className="text-[12.5px] text-muted-foreground">{relativeTime(device.createdAt)}</span>
      <span className="text-[12.5px] text-muted-foreground">{relativeTime(device.lastSeenAt)}</span>
      <span className="text-[12.5px] text-muted-foreground">
        {device.certNotAfter ? new Date(device.certNotAfter).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' }) : '—'}
      </span>
      <StatusPill label={isRevoked ? 'Revoked' : 'Active'} tone={isRevoked ? 'danger' : 'ok'} />
      <div>
        {!isRevoked && (
          <button
            onClick={() => onRevoke(device.id, device.commonName)}
            className="grid h-8 w-8 place-items-center rounded-xl border border-border text-muted-foreground transition-colors hover:border-destructive hover:text-destructive"
            title="Revoke device"
          >
            <ShieldOff className="h-3.5 w-3.5" />
          </button>
        )}
      </div>
    </div>
  )
}

export default function DeviceManagement() {
  const { data, loading, refetch } = useQuery(GetClientDevicesDocument, {
    fetchPolicy: 'cache-and-network',
    pollInterval: 30_000,
  })
  const [revokeDevice, { loading: revoking }] = useMutation(RevokeDeviceDocument)

  const [confirmId, setConfirmId] = useState<string | null>(null)
  const [confirmName, setConfirmName] = useState('')

  const devices = data?.clientDevices ?? []
  const activeCount = devices.filter((d) => !d.revokedAt).length

  function openConfirm(id: string, name: string) {
    setConfirmId(id)
    setConfirmName(name)
  }

  function closeConfirm() {
    setConfirmId(null)
    setConfirmName('')
  }

  async function handleRevoke() {
    if (!confirmId) return
    try {
      await revokeDevice({ variables: { deviceId: confirmId } })
      toast('Device revoked.')
      await refetch()
    } catch {
      toast('Failed to revoke device.')
    } finally {
      closeConfirm()
    }
  }

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div className="flex items-center gap-3">
          <div className="grid h-10 w-10 place-items-center rounded-xl bg-primary/10 text-primary">
            <Laptop className="h-5 w-5" />
          </div>
          <div>
            <h1 className="text-[18px] font-semibold tracking-[-0.01em]">Devices</h1>
            <p className="text-[13px] text-muted-foreground">Enrolled client devices across your workspace</p>
          </div>
        </div>
        {devices.length > 0 && (
          <StatusPill label={`${activeCount} active`} tone="ok" />
        )}
      </div>

      <div className="table-shell">
        <div className="table-scroll">
          <div className="grid min-w-[900px] grid-cols-[1fr_80px_1fr_130px_130px_130px_100px_80px] gap-4 table-head">
            <span>Device ID</span>
            <span>OS</span>
            <span>Common Name</span>
            <span>Enrolled</span>
            <span>Last Seen</span>
            <span>Cert Expires</span>
            <span>Status</span>
            <span></span>
          </div>

          {loading && devices.length === 0 && (
            <div className="flex flex-col gap-2 p-3">
              {Array.from({ length: 5 }).map((_, i) => (
                <Skeleton key={i} className="h-12 rounded-2xl" />
              ))}
            </div>
          )}

          {!loading && devices.length === 0 && (
            <EmptyState
              icon={<Laptop className="h-6 w-6" />}
              title="No enrolled devices."
              description="Devices appear here after users run zecurity login."
            />
          )}

          {devices.map((device) => (
            <DeviceRow key={device.id} device={device} onRevoke={openConfirm} />
          ))}
        </div>
      </div>

      <Dialog open={!!confirmId} onOpenChange={(open) => { if (!open) closeConfirm() }}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Revoke device?</DialogTitle>
          </DialogHeader>
          <p className="text-[13.5px] text-muted-foreground">
            Revoke <span className="font-semibold text-foreground">{confirmName}</span>? The device will be blocked on its next tunnel attempt once the Connector refreshes the CRL (up to 5 minutes).
          </p>
          <DialogFooter>
            <Button variant="outline" onClick={closeConfirm} disabled={revoking}>
              Cancel
            </Button>
            <Button variant="destructive" onClick={handleRevoke} disabled={revoking}>
              {revoking ? 'Revoking…' : 'Revoke'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
