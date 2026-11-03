import { useState } from 'react'
import { useMutation, useQuery } from '@apollo/client/react'
import {
  UpdateResourceDocument,
  GetAllResourcesDocument,
  GetRemoteNetworksDocument,
  type GetRemoteNetworksQuery,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { AlertTriangle, Loader2, X, Server } from 'lucide-react'
import { toast } from 'sonner'

interface EditResourceModalProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  resource: {
    id: string
    name: string
    description?: string | null
    host: string
    status: string
    protocol: string
    portFrom: number
    portTo: number
    remoteNetwork?: { id: string; name: string } | null
    shield?: { name: string; status: string } | null
  } | null
  onSuccess?: () => void
}

export function EditResourceModal({ open, onOpenChange, resource, onSuccess }: EditResourceModalProps) {
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [host, setHost] = useState('')
  const [protocol, setProtocol] = useState('tcp')
  const [portFrom, setPortFrom] = useState('')
  const [portTo, setPortTo] = useState('')
  const [remoteNetworkId, setRemoteNetworkId] = useState('')
  const [error, setError] = useState<string | null>(null)
  // A protected resource's rule changes only after the admin confirms (Fix 18).
  const [confirming, setConfirming] = useState(false)

  const { data: networksData, loading: networksLoading } = useQuery(GetRemoteNetworksDocument, {
    fetchPolicy: 'cache-and-network',
    skip: !open,
  })

  // Load the form when the dialog opens (or shows another resource), not on
  // every new `resource` object: the detail page polls every 3–10 s, and
  // reloading on each poll threw away whatever the admin had typed.
  const formFor = open && resource ? resource.id : null
  const [loadedFor, setLoadedFor] = useState<string | null>(null)
  if (formFor !== loadedFor) {
    setLoadedFor(formFor)
    if (resource && formFor) {
      setName(resource.name)
      setDescription(resource.description ?? '')
      setHost(resource.host)
      setProtocol(resource.protocol)
      setPortFrom(resource.portFrom.toString())
      setPortTo(resource.portTo.toString())
      setRemoteNetworkId(resource.remoteNetwork?.id ?? '')
      setError(null)
      setConfirming(false)
    }
  }

  const [updateResource, { loading }] = useMutation(UpdateResourceDocument, {
    onCompleted: (data) => {
      if (shieldOffline && confirming) {
        toast.success(`Resource "${data.updateResource.name}" saved — applied when the shield reconnects`)
      } else {
        toast.success(`Resource "${data.updateResource.name}" updated`)
      }
      onSuccess?.()
      onOpenChange(false)
    },
    onError: (err) => setError(err.message),
    refetchQueries: [{ query: GetAllResourcesDocument }],
  })

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError(null)

    if (!name.trim() || !host.trim() || !portFrom || !remoteNetworkId) {
      setError('Name, Host, Remote Network and Port From are required')
      return
    }

    const pFrom = parseInt(portFrom, 10)
    const pTo = portTo ? parseInt(portTo, 10) : pFrom

    if (pFrom < 1 || pFrom > 65535) {
      setError('Port must be between 1 and 65535')
      return
    }
    if (pTo < pFrom) {
      setError('Port To must be >= Port From')
      return
    }

    const ruleChanged =
      host.trim() !== resource!.host ||
      remoteNetworkId !== (resource!.remoteNetwork?.id ?? '') ||
      protocol !== resource!.protocol ||
      pFrom !== resource!.portFrom ||
      pTo !== resource!.portTo
    if (shieldBound && ruleChanged && !confirming) {
      setConfirming(true)
      return
    }

    await updateResource({
      variables: {
        id: resource!.id,
        input: {
          remoteNetworkId,
          name: name.trim(),
          description: description.trim() || null,
          host: host.trim(),
          protocol,
          portFrom: pFrom,
          portTo: pTo,
        },
      },
    })
  }

  function handleClose() {
    setName('')
    setDescription('')
    setHost('')
    setProtocol('tcp')
    setPortFrom('')
    setPortTo('')
    setRemoteNetworkId('')
    setError(null)
    setConfirming(false)
    onOpenChange(false)
  }

  const shieldBound = !!resource && ['protected', 'failed', 'protecting'].includes(resource.status.toLowerCase())
  const shieldName = resource?.shield?.name ?? 'its shield'
  const shieldOffline = shieldBound && resource?.shield?.status !== 'ACTIVE'
  const hostChanged = !!resource && host.trim() !== resource.host

  if (!open || !resource) return null

  const networks = (networksData?.remoteNetworks ?? []) as GetRemoteNetworksQuery['remoteNetworks']

  return (
    <div className="fixed inset-0 z-50 flex">
      <div
        className="absolute inset-0 bg-black/50 backdrop-blur-sm"
        onClick={handleClose}
      />
      <div className="absolute right-0 top-0 h-full w-full max-w-md app-panel animate-slide-in">
        <div className="flex h-full flex-col">
          <div className="flex items-center gap-4 border-b border-border p-5">
            <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-[oklch(0.78_0.10_235/0.16)] text-[oklch(0.78_0.10_235)]">
              <Server className="h-5 w-5" />
            </div>
            <div className="flex-1">
              <h2 className="text-lg font-semibold">Edit Resource</h2>
              <p className="text-sm text-muted-foreground">
                Update resource details.
              </p>
            </div>
            <button
              onClick={handleClose}
              className="flex h-9 w-9 items-center justify-center rounded-lg border border-border text-muted-foreground transition hover:bg-secondary hover:text-foreground"
            >
              <X className="h-4 w-4" />
            </button>
          </div>

          <div className="flex-1 overflow-y-auto p-5">
            <form onSubmit={handleSubmit} className="space-y-5">
              <div className="space-y-2">
                <Label className="text-sm font-semibold">
                  Remote Network <span className="text-destructive">*</span>
                </Label>
                <select
                  value={remoteNetworkId}
                  onChange={(e) => setRemoteNetworkId(e.target.value)}
                  disabled={networksLoading}
                  className="flex h-11 w-full rounded-lg border border-border bg-secondary px-3 py-2 text-sm font-medium focus:outline-none focus:ring-2 focus:ring-primary/30 disabled:cursor-not-allowed disabled:opacity-50"
                >
                  <option value="" disabled>
                    {networksLoading ? 'Loading...' : 'Select network'}
                  </option>
                  {networks.map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.name}
                    </option>
                  ))}
                </select>
              </div>

              <div className="space-y-2">
                <Label className="text-sm font-semibold">
                  Name <span className="text-destructive">*</span>
                </Label>
                <Input
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="e.g. prod-web-01"
                  className="h-11 font-medium"
                />
              </div>

              <div className="space-y-2">
                <Label className="text-sm font-semibold">
                  Host <span className="text-destructive">*</span>
                </Label>
                <Input
                  value={host}
                  onChange={(e) => setHost(e.target.value)}
                  placeholder="e.g. 192.168.1.10"
                  className="h-11 font-mono text-sm"
                />
                {shieldBound && host.trim() !== resource.host && (
                  <p className="text-xs text-muted-foreground">
                    This resource is protected: the new host must be the LAN IP of an active shield in its
                    remote network. The resource moves to that shield.
                  </p>
                )}
              </div>

              <div className="space-y-2">
                <Label className="text-sm font-semibold">Description</Label>
                <Input
                  value={description}
                  onChange={(e) => setDescription(e.target.value)}
                  placeholder="Optional description"
                  className="h-11 font-medium"
                />
              </div>

              <div className="grid grid-cols-3 gap-3">
                <div className="space-y-2">
                  <Label className="text-sm font-semibold">Protocol</Label>
                  <select
                    value={protocol}
                    onChange={(e) => setProtocol(e.target.value)}
                    className="flex h-11 w-full rounded-lg border border-border bg-secondary px-3 py-2 text-sm font-medium focus:outline-none focus:ring-2 focus:ring-primary/30"
                  >
                    <option value="tcp">TCP</option>
                    <option value="udp">UDP</option>
                    <option value="any">ANY</option>
                  </select>
                </div>

                <div className="space-y-2">
                  <Label className="text-sm font-semibold">
                    Port From <span className="text-destructive">*</span>
                  </Label>
                  <Input
                    type="number"
                    min={1}
                    max={65535}
                    value={portFrom}
                    onChange={(e) => setPortFrom(e.target.value)}
                    placeholder="80"
                    className="h-11 font-mono text-sm"
                  />
                </div>

                <div className="space-y-2">
                  <Label className="text-sm font-semibold">Port To</Label>
                  <Input
                    type="number"
                    min={1}
                    max={65535}
                    value={portTo}
                    onChange={(e) => setPortTo(e.target.value)}
                    placeholder="Same"
                    className="h-11 font-mono text-sm"
                  />
                </div>
              </div>

              {confirming && (
                <div className="space-y-2 rounded-lg border border-amber-500/30 bg-amber-500/10 p-3 text-sm">
                  <p className="font-semibold">This resource is protected by {shieldName}.</p>
                  <p>
                    Saving removes its current firewall rule and protects it again with the new settings
                    {hostChanged ? `, on the shield installed on ${host.trim()}` : ''}. The shield swaps the rules
                    in one step, so the resource is never left unprotected; it shows Protecting until the shield
                    confirms.
                  </p>
                  {shieldOffline && (
                    <p className="font-medium text-amber-600 dark:text-amber-400">
                      {shieldName} is offline. The change is saved now and applied when the shield reconnects.
                      Until then the old rule stays in place and the new settings are NOT protected by the shield.
                    </p>
                  )}
                </div>
              )}

              {error && (
                <div className="flex items-start gap-2 rounded-lg border border-destructive/30 bg-destructive/10 p-3 text-sm text-destructive">
                  <AlertTriangle className="h-4 w-4 mt-0.5 shrink-0" />
                  <span>{error}</span>
                </div>
              )}
            </form>
          </div>

          <div className="flex items-center justify-between gap-3 border-t border-border p-5">
            <Button
              type="button"
              variant="outline"
              onClick={confirming ? () => setConfirming(false) : handleClose}
              className="h-11 flex-1"
            >
              {confirming ? 'Back' : 'Cancel'}
            </Button>
            <Button
              onClick={handleSubmit}
              disabled={loading}
              className="h-11 flex-1 gap-2"
            >
              {loading ? (
                <>
                  <Loader2 className="h-4 w-4 animate-spin" />
                  Saving...
                </>
              ) : confirming ? (
                'Unprotect & re-protect'
              ) : (
                'Save Changes'
              )}
            </Button>
          </div>
        </div>
      </div>

      <style>{`
        @keyframes slide-in {
          from {
            transform: translateX(100%);
          }
          to {
            transform: translateX(0);
          }
        }
        .animate-slide-in {
          animation: slide-in 0.3s ease-out;
        }
      `}</style>
    </div>
  )
}