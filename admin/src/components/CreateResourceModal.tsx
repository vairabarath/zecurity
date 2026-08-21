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
import { AlertTriangle, Info, Loader2, X, Server, Globe } from 'lucide-react'
import { toast } from 'sonner'
import {
  type Addressing,
  allowedLocalTargets,
  emptyAddressing,
  isAddressingValid,
  toCreateAddressingInput,
} from '@/lib/resourceAddressing'

interface CreateResourceDefaults {
  remoteNetworkId?: string
  name?: string
  host?: string
  protocol?: string
  portFrom?: number
  portTo?: number
}

interface CreateResourceModalProps {
  open: boolean
  onOpenChange: (open: boolean) => void
  onSuccess?: () => void
  defaults?: CreateResourceDefaults | null
}

export function CreateResourceModal({
  open,
  onOpenChange,
  onSuccess,
  defaults,
}: CreateResourceModalProps) {
  const [name, setName] = useState(() => defaults?.name ?? '')
  const [description, setDescription] = useState('')
  // One value, not two optional fields: `host` and `hostname` live in different
  // variants, so "both" is unrepresentable rather than rejected on submit.
  const [addressing, setAddressing] = useState<Addressing>(() =>
    emptyAddressing(defaults?.host ?? ''),
  )
  const [protocol, setProtocol] = useState(() => defaults?.protocol ?? 'tcp')
  const [portFrom, setPortFrom] = useState(() => defaults?.portFrom ? String(defaults.portFrom) : '')
  const [portTo, setPortTo] = useState(() => defaults?.portTo ? String(defaults.portTo) : '')
  const [remoteNetworkId, setRemoteNetworkId] = useState(() => defaults?.remoteNetworkId ?? '')
  const [error, setError] = useState<string | null>(null)

  const { data: networksData, loading: networksLoading } = useQuery(GetRemoteNetworksDocument, {
    fetchPolicy: 'cache-and-network',
  })

  const [createResource, { loading: creating }] = useMutation(CreateResourceDocument, {
    onCompleted: (data) => {
      toast.success(`Resource "${data.createResource.name}" created`)
      onSuccess?.()
      handleClose(false)
    },
    onError: (err) => {
      const msg = err.message
      if (msg.includes('no shield') || msg.includes('no shield installed')) {
        setError(
          'No shield found on this host. Make sure a shield is enrolled on the machine at ' +
            (addressing.mode === 'ip' ? addressing.host : ''),
        )
      } else {
        setError(msg)
      }
    },
  })

  const handleClose = (isOpen: boolean) => {
    onOpenChange(isOpen)
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    setError(null)

    if (!name.trim() || !isAddressingValid(addressing) || !portFrom || !remoteNetworkId) {
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
          ...toCreateAddressingInput(addressing),
          protocol,
          portFrom: pFrom,
          portTo: pTo,
        },
      } as CreateResourceMutationVariables,
      refetchQueries: [{ query: GetAllResourcesDocument }],
    } as any)
  }

  const networks = networksData?.remoteNetworks ?? []
  const isValid = name.trim() && isAddressingValid(addressing) && portFrom && remoteNetworkId

  if (!open) return null

  return (
    <div className="fixed inset-0 z-50">
      <div
        className="absolute inset-0 bg-black/50 backdrop-blur-sm"
        onClick={() => handleClose(false)}
      />
      <div className="absolute right-0 top-0 h-full w-full max-w-md app-panel animate-slide-in">
        <div className="flex h-full flex-col">
          <div className="flex items-center gap-4 border-b border-border p-5">
            <div className="flex h-10 w-10 items-center justify-center rounded-lg bg-[oklch(0.78_0.10_235/0.16)] text-[oklch(0.78_0.10_235)]">
              <Server className="h-5 w-5" />
            </div>
            <div className="flex-1">
              <h2 className="text-lg font-semibold">Add Resource</h2>
              <p className="text-sm text-muted-foreground">
                Register a resource. Enable protection manually from its detail page.
              </p>
            </div>
            <button
              onClick={() => handleClose(false)}
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
                  {(networks as GetRemoteNetworksQuery['remoteNetworks']).map((n) => (
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

              {/* Addressing is a CHOICE. Only the chosen mode's inputs render, so
                  a resource carrying both an IP and a hostname cannot be built. */}
              <div className="space-y-2">
                <Label className="text-sm font-semibold">
                  Addressing <span className="text-destructive">*</span>
                </Label>
                <div className="grid grid-cols-2 gap-2 rounded-lg border border-border bg-secondary p-1">
                  <button
                    type="button"
                    onClick={() => setAddressing(emptyAddressing())}
                    className={`flex items-center justify-center gap-2 rounded-md px-3 py-2 text-sm font-medium transition ${
                      addressing.mode === 'ip'
                        ? 'bg-background text-foreground shadow-sm'
                        : 'text-muted-foreground hover:text-foreground'
                    }`}
                  >
                    <Server className="h-3.5 w-3.5" />
                    IP address
                  </button>
                  <button
                    type="button"
                    onClick={() =>
                      setAddressing({
                        mode: 'hostname',
                        hostname: '',
                        resolver: { type: 'dns', name: '' },
                      })
                    }
                    className={`flex items-center justify-center gap-2 rounded-md px-3 py-2 text-sm font-medium transition ${
                      addressing.mode === 'hostname'
                        ? 'bg-background text-foreground shadow-sm'
                        : 'text-muted-foreground hover:text-foreground'
                    }`}
                  >
                    <Globe className="h-3.5 w-3.5" />
                    Hostname
                  </button>
                </div>
              </div>

              {addressing.mode === 'ip' ? (
                <>
                  <div className="space-y-2">
                    <Label className="flex items-center gap-2 text-sm font-semibold">
                      Host IP <span className="text-destructive">*</span>
                      <span className="text-xs font-normal text-muted-foreground">
                        (must match a shield's LAN IP)
                      </span>
                    </Label>
                    <Input
                      value={addressing.host}
                      onChange={(e) =>
                        setAddressing({ ...addressing, host: e.target.value })
                      }
                      placeholder="e.g. 192.168.1.100"
                      className="h-11 font-mono text-sm"
                    />
                    <div className="flex items-center gap-1 text-xs text-muted-foreground">
                      <Info className="h-3 w-3" />
                      <span>A shield must be installed on this machine.</span>
                    </div>
                  </div>

                  {/* The shield accepts exactly two dial targets — loopback, or its
                      own LAN IP. Offering only those keeps a value the shield would
                      reject with status:"failed" out of the form entirely. */}
                  <div className="space-y-2">
                    <Label className="flex items-center gap-2 text-sm font-semibold">
                      Shield dials
                      <span className="text-xs font-normal text-muted-foreground">
                        (once protected)
                      </span>
                    </Label>
                    <div className="space-y-1.5">
                      {allowedLocalTargets(addressing.host).map((t) => (
                        <label
                          key={t}
                          className="flex cursor-pointer items-center gap-2 rounded-lg border border-border bg-secondary px-3 py-2 text-sm"
                        >
                          <input
                            type="radio"
                            name="localTarget"
                            checked={addressing.localTarget === t}
                            onChange={() => setAddressing({ ...addressing, localTarget: t })}
                          />
                          <span className="font-mono text-xs">{t}</span>
                          <span className="text-xs text-muted-foreground">
                            {t === '127.0.0.1'
                              ? '— loopback-only services'
                              : "— the shield's own LAN IP"}
                          </span>
                        </label>
                      ))}
                    </div>
                  </div>
                </>
              ) : (
                <>
                  <div className="space-y-2">
                    <Label className="flex items-center gap-2 text-sm font-semibold">
                      Hostname <span className="text-destructive">*</span>
                      <span className="text-xs font-normal text-muted-foreground">
                        (what apps connect to)
                      </span>
                    </Label>
                    <Input
                      value={addressing.hostname}
                      onChange={(e) =>
                        setAddressing({ ...addressing, hostname: e.target.value })
                      }
                      placeholder="e.g. db.internal"
                      className="h-11 font-mono text-sm"
                    />
                    <div className="flex items-center gap-1 text-xs text-muted-foreground">
                      <Info className="h-3 w-3" />
                      <span>
                        The connector resolves this per connection — the backend IP can change
                        freely.
                      </span>
                    </div>
                  </div>

                  {/* Resolver renders ONLY in hostname mode: it is meaningless for a
                      pinned IP, and offering it there invites the misconception that
                      a resolver chooses delivery. It does not — route_type does. */}
                  <div className="space-y-2">
                    <Label className="text-sm font-semibold">Resolver</Label>
                    <select
                      value={addressing.resolver.type}
                      onChange={(e) => {
                        const t = e.target.value
                        setAddressing({
                          ...addressing,
                          resolver:
                            t === 'static'
                              ? { type: 'static', address: '' }
                              : t === 'raw'
                                ? { type: 'raw', json: '' }
                                : { type: 'dns', name: '' },
                        })
                      }}
                      className="flex h-11 w-full rounded-lg border border-border bg-secondary px-3 py-2 text-sm font-medium focus:outline-none focus:ring-2 focus:ring-primary/30"
                    >
                      {/* NO `shield` option — delivery is derived server-side from
                          (status, shield_id); resolver.type only answers HOW the
                          connector finds the endpoint. Listing it here would encode
                          a conflation the data plane explicitly rejects. */}
                      <option value="dns">DNS — resolve the name at dial time</option>
                      <option value="static">Static — a fixed backend address</option>
                      <option value="raw">Advanced — raw JSON</option>
                    </select>

                    {addressing.resolver.type === 'dns' && (
                      <Input
                        value={addressing.resolver.name}
                        onChange={(e) =>
                          setAddressing({
                            ...addressing,
                            resolver: { type: 'dns', name: e.target.value },
                          })
                        }
                        placeholder={`Backend name (optional) — blank resolves ${
                          addressing.hostname || 'the hostname'
                        }`}
                        className="h-11 font-mono text-xs"
                      />
                    )}
                    {addressing.resolver.type === 'static' && (
                      <Input
                        value={addressing.resolver.address}
                        onChange={(e) =>
                          setAddressing({
                            ...addressing,
                            resolver: { type: 'static', address: e.target.value },
                          })
                        }
                        placeholder="e.g. 10.0.3.7"
                        className="h-11 font-mono text-xs"
                      />
                    )}
                    {addressing.resolver.type === 'raw' && (
                      <Input
                        value={addressing.resolver.json}
                        onChange={(e) =>
                          setAddressing({
                            ...addressing,
                            resolver: { type: 'raw', json: e.target.value },
                          })
                        }
                        placeholder={'{"type":"dns","config":{"name":"backend.svc.internal"}}'}
                        className="h-11 font-mono text-xs"
                      />
                    )}
                  </div>
                </>
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
              onClick={() => handleClose(false)}
              className="h-11 flex-1"
            >
              Cancel
            </Button>
            <Button
              onClick={handleSubmit}
              disabled={!isValid || creating}
              className="h-11 flex-1 gap-2"
            >
              {creating ? (
                <span className="flex items-center gap-2">
                  <Loader2 className="h-4 w-4 animate-spin" />
                  Creating...
                </span>
              ) : (
                <span className="flex items-center gap-2">
                  <Server className="h-4 w-4" />
                  Add Resource
                </span>
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
