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
import { AlertTriangle, Loader2, X, Server, Globe, Info } from 'lucide-react'
import { toast } from 'sonner'
import {
  type ResolverDraft,
  LOCAL_TARGET_LOOPBACK,
  allowedLocalTargets,
  parseResolverJson,
  toResolverJson,
} from '@/lib/resourceAddressing'

interface EditResourceModalProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  resource: {
    id: string
    name: string
    description?: string | null
    host?: string | null
    hostname?: string | null
    resolver?: string | null
    localTarget?: string | null
    status?: string | null
    protocol: string
    portFrom: number
    portTo: number
    shield?: { id: string } | null
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
  // Addressing MODE is immutable after create: `host` is not a member of
  // UpdateResourceInput, so a pinned resource can never become name-addressed
  // (or vice versa) through this form. Only the mode's own fields are editable.
  const [hostname, setHostname] = useState('')
  const [resolver, setResolver] = useState<ResolverDraft>({ type: 'dns', name: '' })
  const [localTarget, setLocalTarget] = useState(LOCAL_TARGET_LOOPBACK)
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
      setHostname(resource.hostname ?? '')
      setResolver(parseResolverJson(resource.resolver))
      setLocalTarget(resource.localTarget || LOCAL_TARGET_LOOPBACK)
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
          // Send only the fields this resource's addressing mode can own. A
          // pinned resource has no hostname/resolver to edit; a name-addressed
          // one can never be shield-delivered, so it has no localTarget.
          ...(isNameAddressedResource
            ? { hostname: hostname.trim(), resolver: toResolverJson(resolver) ?? null }
            : {}),
          ...(shieldDelivered ? { localTarget: localTarget.trim() || null } : {}),
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

  // Derived from the resource, never from a control: a name-addressed resource
  // has host = "" (never null) on the wire.
  const isNameAddressedResource = !(resource.host ?? '').trim() && !!(resource.hostname ?? '').trim()
  // The plan's literal gate: localTarget is editable only for shield-delivered
  // resources, and in edit context that is unambiguous — a bound shield.
  const shieldDelivered = !!resource.shield

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

              {/* Addressing mode is FIXED after create — `host` is not part of
                  UpdateResourceInput, so this is shown read-only rather than as a
                  control that would silently do nothing. */}
              <div className="space-y-2">
                <Label className="text-sm font-semibold">Addressing</Label>
                <div className="flex items-center gap-2 rounded-lg border border-border bg-secondary/50 px-3 py-2.5">
                  {isNameAddressedResource ? (
                    <>
                      <Globe className="h-3.5 w-3.5 text-muted-foreground" />
                      <span className="text-sm font-medium">Hostname</span>
                    </>
                  ) : (
                    <>
                      <Server className="h-3.5 w-3.5 text-muted-foreground" />
                      <span className="text-sm font-medium">IP address</span>
                      <span className="ml-auto font-mono text-xs text-muted-foreground">
                        {resource.host}
                      </span>
                    </>
                  )}
                </div>
                <div className="flex items-center gap-1 text-xs text-muted-foreground">
                  <Info className="h-3 w-3" />
                  <span>Addressing mode cannot be changed after creation.</span>
                </div>
              </div>

              {isNameAddressedResource && (
                <>
                  <div className="space-y-2">
                    <Label className="text-sm font-semibold">
                      Hostname <span className="text-destructive">*</span>
                    </Label>
                    <Input
                      value={hostname}
                      onChange={(e) => setHostname(e.target.value)}
                      placeholder="e.g. db.internal"
                      className="h-11 font-mono text-sm"
                    />
                  </div>

                  <div className="space-y-2">
                    <Label className="text-sm font-semibold">Resolver</Label>
                    <select
                      value={resolver.type}
                      onChange={(e) => {
                        const t = e.target.value
                        setResolver(
                          t === 'static'
                            ? { type: 'static', address: '' }
                            : t === 'raw'
                              ? { type: 'raw', json: '' }
                              : { type: 'dns', name: '' },
                        )
                      }}
                      className="flex h-11 w-full rounded-lg border border-border bg-secondary px-3 py-2 text-sm font-medium focus:outline-none focus:ring-2 focus:ring-primary/30"
                    >
                      {/* No `shield` option — see resourceAddressing.ts */}
                      <option value="dns">DNS — resolve the name at dial time</option>
                      <option value="static">Static — a fixed backend address</option>
                      <option value="raw">Advanced — raw JSON</option>
                    </select>
                    {resolver.type === 'dns' && (
                      <Input
                        value={resolver.name}
                        onChange={(e) => setResolver({ type: 'dns', name: e.target.value })}
                        placeholder={`Backend name (optional) — blank resolves ${hostname || 'the hostname'}`}
                        className="h-11 font-mono text-xs"
                      />
                    )}
                    {resolver.type === 'static' && (
                      <Input
                        value={resolver.address}
                        onChange={(e) => setResolver({ type: 'static', address: e.target.value })}
                        placeholder="e.g. 10.0.3.7"
                        className="h-11 font-mono text-xs"
                      />
                    )}
                    {resolver.type === 'raw' && (
                      <Input
                        value={resolver.json}
                        onChange={(e) => setResolver({ type: 'raw', json: e.target.value })}
                        placeholder={'{"type":"dns","config":{"name":"backend.svc.internal"}}'}
                        className="h-11 font-mono text-xs"
                      />
                    )}
                    <div className="flex items-center gap-1 text-xs text-muted-foreground">
                      <Info className="h-3 w-3" />
                      <span>Editing the resolver bumps the ACL version — clients resync.</span>
                    </div>
                  </div>
                </>
              )}

              {shieldDelivered && (
                <div className="space-y-2">
                  <Label className="text-sm font-semibold">Shield dials</Label>
                  <div className="space-y-1.5">
                    {allowedLocalTargets(resource.host ?? '').map((t) => (
                      <label
                        key={t}
                        className="flex cursor-pointer items-center gap-2 rounded-lg border border-border bg-secondary px-3 py-2 text-sm"
                      >
                        <input
                          type="radio"
                          name="editLocalTarget"
                          checked={localTarget === t}
                          onChange={() => setLocalTarget(t)}
                        />
                        <span className="font-mono text-xs">{t}</span>
                        <span className="text-xs text-muted-foreground">
                          {t === LOCAL_TARGET_LOOPBACK
                            ? '— loopback-only services'
                            : "— the shield's own LAN IP"}
                        </span>
                      </label>
                    ))}
                  </div>
                  <div className="flex items-center gap-1 text-xs text-muted-foreground">
                    <Info className="h-3 w-3" />
                    <span>
                      Re-applies on the shield without bumping the ACL version — no tunnel restart.
                    </span>
                  </div>
                </div>
              )}

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