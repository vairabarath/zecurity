import { useState } from 'react'
import { useMutation } from '@apollo/client/react'
import { AlertTriangle, Loader2, Server, X } from 'lucide-react'
import { toast } from 'sonner'
import {
  GetAllResourcesDocument,
  PromoteDiscoveredServiceDocument,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'

interface PromoteServiceModalProps {
  shieldId: string
  protocol: string
  port: number
  serviceName: string
  boundIp: string
  onClose: () => void
}

export function PromoteServiceModal({
  shieldId,
  protocol,
  port,
  serviceName,
  boundIp,
  onClose,
}: PromoteServiceModalProps) {
  const [error, setError] = useState<string | null>(null)
  const [promote, { loading }] = useMutation(PromoteDiscoveredServiceDocument, {
    onCompleted: (data) => {
      toast.success(`Resource "${data.promoteDiscoveredService.name}" created`)
      onClose()
    },
    onError: (err) => setError(err.message),
    refetchQueries: [{ query: GetAllResourcesDocument }],
  })

  async function handleConfirm() {
    setError(null)
    await promote({ variables: { shieldId, protocol, port } })
  }

  return (
    <div className="fixed inset-0 z-50">
      <div className="absolute inset-0 bg-black/50 backdrop-blur-sm" onClick={onClose} />
      <div className="absolute left-1/2 top-1/2 w-full max-w-md -translate-x-1/2 -translate-y-1/2 app-panel">
        <div className="flex items-center gap-4 border-b border-border p-5">
          <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-[oklch(0.78_0.10_235/0.16)] text-[oklch(0.78_0.10_235)]">
            <Server className="h-5 w-5" />
          </div>
          <div className="flex-1">
            <h2 className="text-lg font-semibold">Promote to Resource</h2>
            <p className="text-sm text-muted-foreground">
              Promote <span className="font-semibold">{serviceName}</span> (port{' '}
              <span className="font-mono">{port}/{protocol}</span>) to a managed resource?
            </p>
          </div>
          <button
            onClick={onClose}
            className="flex h-9 w-9 items-center justify-center rounded-lg border border-border text-muted-foreground transition hover:bg-secondary hover:text-foreground"
          >
            <X className="h-4 w-4" />
          </button>
        </div>

        <div className="space-y-4 p-5">
          <div className="rounded-lg border border-border bg-secondary/40 p-3 text-sm">
            <div className="flex justify-between py-1">
              <span className="text-muted-foreground">Service</span>
              <span className="font-semibold">{serviceName}</span>
            </div>
            <div className="flex justify-between py-1">
              <span className="text-muted-foreground">Port / Protocol</span>
              <span className="font-mono">{port} / {protocol}</span>
            </div>
            <div className="flex justify-between py-1">
              <span className="text-muted-foreground">Bound IP</span>
              <span className="font-mono">{boundIp}</span>
            </div>
          </div>

          <p className="text-sm text-muted-foreground">
            A new resource will be created on this shield's host, auto-matched by LAN IP.
            Status will be set to <span className="font-semibold">pending</span> — click
            Protect on the resource page to activate.
          </p>

          {error ? (
            <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive">
              <AlertTriangle className="mt-0.5 h-4 w-4 shrink-0" />
              <span>{error}</span>
            </div>
          ) : null}
        </div>

        <div className="flex items-center justify-between gap-3 border-t border-border p-5">
          <Button type="button" variant="outline" onClick={onClose} className="h-11 flex-1">
            Cancel
          </Button>
          <Button onClick={handleConfirm} disabled={loading} className="h-11 flex-1 gap-2">
            {loading ? (
              <span className="flex items-center gap-2">
                <Loader2 className="h-4 w-4 animate-spin" />
                Promoting...
              </span>
            ) : (
              'Promote'
            )}
          </Button>
        </div>
      </div>
    </div>
  )
}
