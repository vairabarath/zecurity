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
import { AlertTriangle, Loader2 } from 'lucide-react'
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

  if (!open || !resource) return null

  const networks = (networksData?.remoteNetworks ?? []) as GetRemoteNetworksQuery['remoteNetworks']

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center">
      <div className="absolute inset-0 bg-black/50" onClick={() => onOpenChange(false)} />
      <div className="relative z-10 w-full max-w-md rounded-lg bg-background p-6 shadow-lg">
        <h2 className="text-lg font-semibold">Edit Resource</h2>
        <p className="text-sm text-muted-foreground mb-4">
          Update resource details. Host IP cannot be changed.
        </p>

        <form onSubmit={handleSubmit} className="space-y-4">
          <div className="space-y-2">
            <Label>Remote Network *</Label>
            <select
              value={remoteNetworkId}
              onChange={(e) => setRemoteNetworkId(e.target.value)}
              disabled={networksLoading}
              className="flex h-10 w-full rounded-lg border border-border bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-primary/30 disabled:cursor-not-allowed disabled:opacity-50"
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
            <Label>Name *</Label>
            <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="e.g. prod-web-01" />
          </div>

          <div className="space-y-2">
            <Label>Description</Label>
            <Input value={description} onChange={(e) => setDescription(e.target.value)} placeholder="Optional description" />
          </div>

          <div className="grid grid-cols-3 gap-3">
            <div className="space-y-2">
              <Label>Protocol</Label>
              <select
                value={protocol}
                onChange={(e) => setProtocol(e.target.value)}
                className="flex h-10 w-full rounded-lg border border-border bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-primary/30"
              >
                <option value="tcp">TCP</option>
                <option value="udp">UDP</option>
                <option value="any">ANY</option>
              </select>
            </div>

            <div className="space-y-2">
              <Label>Port From *</Label>
              <Input
                type="number"
                min={1}
                max={65535}
                value={portFrom}
                onChange={(e) => setPortFrom(e.target.value)}
                placeholder="80"
              />
            </div>

            <div className="space-y-2">
              <Label>Port To</Label>
              <Input
                type="number"
                min={1}
                max={65535}
                value={portTo}
                onChange={(e) => setPortTo(e.target.value)}
                placeholder="Same"
              />
            </div>
          </div>

          {error && (
            <div className="flex items-center gap-2 text-sm text-red-500 bg-red-50 p-2 rounded-md">
              <AlertTriangle className="h-4 w-4" />
              <span>{error}</span>
            </div>
          )}

          <div className="flex justify-end gap-2 pt-2">
            <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" disabled={loading}>
              {loading ? (
                <>
                  <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                  Saving...
                </>
              ) : (
                'Save Changes'
              )}
            </Button>
          </div>
        </form>
      </div>
    </div>
  )
}
