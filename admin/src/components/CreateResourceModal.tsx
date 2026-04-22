import { useState } from 'react'
import { useMutation, useQuery } from '@apollo/client/react'
import {
  CreateResourceDocument,
  GetRemoteNetworksDocument,
  GetAllResourcesDocument,
} from '@/generated/graphql'
import {
  type CreateResourceMutationVariables,
  type GetRemoteNetworksQuery,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { AlertTriangle, Loader2, Info } from 'lucide-react'
import { toast } from 'sonner'

interface CreateResourceModalProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  onSuccess?: () => void
}

export function CreateResourceModal({
  open,
  onOpenChange,
  onSuccess,
}: CreateResourceModalProps) {
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [host, setHost] = useState('')
  const [protocol, setProtocol] = useState('tcp')
  const [portFrom, setPortFrom] = useState('')
  const [portTo, setPortTo] = useState('')
  const [remoteNetworkId, setRemoteNetworkId] = useState('')
  const [error, setError] = useState<string | null>(null)

  const { data: networksData, loading: networksLoading } = useQuery(GetRemoteNetworksDocument, {
    fetchPolicy: 'cache-and-network',
  })

  const [createResource, { loading: creating }] = useMutation(CreateResourceDocument, {
    onCompleted: (data) => {
      toast.success(`Resource "${data.createResource.name}" created`)
      resetForm()
      onSuccess?.()
    },
    onError: (err) => {
      const msg = err.message
      if (msg.includes('no shield') || msg.includes('no shield installed')) {
        setError('No shield found on this host. Make sure a shield is enrolled on the machine at ' + host)
      } else {
        setError(msg)
      }
    },
  })

  function resetForm() {
    setName('')
    setDescription('')
    setHost('')
    setProtocol('tcp')
    setPortFrom('')
    setPortTo('')
    setRemoteNetworkId('')
    setError(null)
  }

  const handleClose = (isOpen: boolean) => {
    if (!isOpen) resetForm()
    onOpenChange(isOpen)
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError(null)

    if (!name.trim() || !host.trim() || !portFrom || !remoteNetworkId) {
      setError('Please fill in all required fields')
      return
    }

    const pFrom = parseInt(portFrom, 10)
    const pTo = portTo ? parseInt(portTo, 10) : pFrom

    if (pFrom < 1 || pFrom > 65535) {
      setError('Port must be between 1 and 65535')
      return
    }

    if (pTo < pFrom) {
      setError('Port To must be greater than or equal to Port From')
      return
    }

    await createResource({
      variables: {
        input: {
          remoteNetworkId,
          name: name.trim(),
          description: description.trim() || undefined,
          host: host.trim(),
          protocol,
          portFrom: pFrom,
          portTo: pTo,
        },
      } as CreateResourceMutationVariables,
      refetchQueries: [{ query: GetAllResourcesDocument }],
    } as any)
  }

  const networks = networksData?.remoteNetworks ?? []
  const isValid = name.trim() && host.trim() && portFrom && remoteNetworkId

  return (
    <div>
      {open && (
        <div className="fixed inset-0 z-50 flex items-center justify-center">
          <div
            className="absolute inset-0 bg-black/50"
            onClick={() => handleClose(false)}
          />
          <div className="relative z-10 w-full max-w-md rounded-lg bg-background p-6 shadow-lg">
            <h2 className="text-lg font-semibold">Add Resource</h2>
            <p className="text-sm text-muted-foreground mb-4">
              Define a resource to protect. A shield on this host will automatically match.
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
                  {(networks as GetRemoteNetworksQuery['remoteNetworks']).map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.name}
                    </option>
                  ))}
                </select>
              </div>

              <div className="space-y-2">
                <Label>Name *</Label>
                <Input
                  value={name}
                  onChange={(e) => setName(e.target.value)}
                  placeholder="e.g. prod-web-01"
                />
              </div>

              <div className="space-y-2">
                <Label>Description</Label>
                <Input
                  value={description}
                  onChange={(e) => setDescription(e.target.value)}
                  placeholder="Optional description"
                />
              </div>

              <div className="space-y-2">
                <Label>
                  Host IP *
                  <span className="ml-1 text-muted-foreground text-xs font-normal">
                    (must match a shield's LAN IP)
                  </span>
                </Label>
                <Input
                  value={host}
                  onChange={(e) => setHost(e.target.value)}
                  placeholder="e.g. 192.168.1.100"
                />
                <div className="flex items-center gap-1 text-[10px] text-muted-foreground">
                  <Info className="h-3 w-3" />
                  <span>A shield must be installed on this machine.</span>
                </div>
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
                <Button
                  type="button"
                  variant="outline"
                  onClick={() => handleClose(false)}
                >
                  Cancel
                </Button>
                <Button
                  type="submit"
                  disabled={!isValid || creating}
                >
                  {creating ? (
                    <>
                      <Loader2 className="mr-2 h-4 w-4 animate-spin" />
                      Creating...
                    </>
                  ) : (
                    'Add Resource'
                  )}
                </Button>
              </div>
            </form>
          </div>
        </div>
      )}
    </div>
  )
}