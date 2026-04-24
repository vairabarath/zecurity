import { useState, useEffect } from 'react'
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
    protocol: string
    portFrom: number
    portTo: number
    remoteNetwork?: { id: string; name: string } | null
  } | null
  onSuccess?: () => void
}

export function EditResourceModal({ open, onOpenChange, resource, onSuccess }: EditResourceModalProps) {
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [protocol, setProtocol] = useState('tcp')
  const [portFrom, setPortFrom] = useState('')
  const [portTo, setPortTo] = useState('')
  const [remoteNetworkId, setRemoteNetworkId] = useState('')
  const [error, setError] = useState<string | null>(null)

  const { data: networksData, loading: networksLoading } = useQuery(GetRemoteNetworksDocument, {
    fetchPolicy: 'cache-and-network',
    skip: !open,
  })

  useEffect(() => {
    if (resource) {
      setName(resource.name)
      setDescription(resource.description ?? '')
      setProtocol(resource.protocol)
      setPortFrom(resource.portFrom.toString())
      setPortTo(resource.portTo.toString())
      setRemoteNetworkId(resource.remoteNetwork?.id ?? '')
      setError(null)
    }
  }, [resource])

  const [updateResource, { loading }] = useMutation(UpdateResourceDocument, {
    onCompleted: (data) => {
      toast.success(`Resource "${data.updateResource.name}" updated`)
      onSuccess?.()
      onOpenChange(false)
    },
    onError: (err) => setError(err.message),
    refetchQueries: [{ query: GetAllResourcesDocument }],
  })

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError(null)

    if (!name.trim() || !portFrom || !remoteNetworkId) {
      setError('Name, Remote Network and Port From are required')
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

    await updateResource({
      variables: {
        id: resource!.id,
        input: {
          remoteNetworkId,
          name: name.trim(),
          description: description.trim() || null,
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
    setProtocol('tcp')
    setPortFrom('')
    setPortTo('')
    setRemoteNetworkId('')
    setError(null)
    onOpenChange(false)
  }

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
                Update resource details. Host IP cannot be changed.
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
              onClick={handleClose}
              className="h-11 flex-1"
            >
              Cancel
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