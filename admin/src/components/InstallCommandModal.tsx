import { useMemo, useState } from 'react'
import { useMutation, useQuery } from '@apollo/client/react'
import { useNavigate } from 'react-router-dom'
import {
  GenerateConnectorTokenDocument,
  GenerateShieldTokenDocument,
  GetRemoteNetworksDocument,
} from '@/generated/graphql'
import {
  type GenerateConnectorTokenMutationVariables,
  type GenerateShieldTokenMutationVariables,
  GetShieldsDocument,
  type GetShieldsQueryVariables,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { AlertTriangle, ChevronDown, HardDriveDownload, Plus, Server, Shield, X } from 'lucide-react'

interface InstallCommandModalProps {
  remoteNetworkId?: string
  variant?: 'connector' | 'shield'
  open: boolean
  onClose: () => void
}

export function InstallCommandModal({
  remoteNetworkId: fixedNetworkId,
  variant = 'connector',
  open,
  onClose,
}: InstallCommandModalProps) {
  const navigate = useNavigate()
  const [agentName, setAgentName] = useState('')
  const [selectedNetworkId, setSelectedNetworkId] = useState('')
  const [platform, setPlatform] = useState<'linux' | 'docker'>('linux')
  const [error, setError] = useState<string | null>(null)

  const isShield = variant === 'shield'
  const networkId = fixedNetworkId ?? selectedNetworkId

  const { data: networksData, loading: networksLoading } = useQuery(GetRemoteNetworksDocument, {
    skip: !!fixedNetworkId,
    fetchPolicy: 'cache-and-network',
  })

  const networks = networksData?.remoteNetworks ?? []
  const selectedNetwork = useMemo(
    () => networks.find((network) => network.id === networkId),
    [networkId, networks],
  )

  const [generateConnectorToken, connectorState] = useMutation(GenerateConnectorTokenDocument)
  const [generateShieldToken, shieldState] = useMutation(GenerateShieldTokenDocument)
  const loading = isShield ? shieldState.loading : connectorState.loading

  function resetState() {
    setAgentName('')
    setSelectedNetworkId('')
    setPlatform('linux')
    setError(null)
  }

  function handleClose() {
    resetState()
    onClose()
  }

  async function handleSubmit() {
    if (!agentName.trim() || !networkId) return
    setError(null)

    try {
      if (isShield) {
        const result = await generateShieldToken({
          variables: {
            remoteNetworkId: networkId,
            shieldName: agentName.trim(),
          } as GenerateShieldTokenMutationVariables,
          refetchQueries: [
            { query: GetRemoteNetworksDocument },
            { query: GetShieldsDocument, variables: { remoteNetworkId: networkId } as GetShieldsQueryVariables },
          ],
          awaitRefetchQueries: true,
        })
        const shieldId = result.data?.generateShieldToken.shieldId
        handleClose()
        if (shieldId) {
          navigate(`/shields/${shieldId}`)
        }
        return
      }

      const result = await generateConnectorToken({
        variables: {
          remoteNetworkId: networkId,
          connectorName: agentName.trim(),
        } as GenerateConnectorTokenMutationVariables,
        refetchQueries: [{ query: GetRemoteNetworksDocument }],
        awaitRefetchQueries: true,
      })

      const connectorId = result.data?.generateConnectorToken.connectorId
      handleClose()
      if (connectorId) {
        navigate(`/connectors/${connectorId}`)
      }
    } catch (mutationError: unknown) {
      setError(mutationError instanceof Error ? mutationError.message : 'Failed to create agent')
    }
  }

  const canSubmit = !!agentName.trim() && !!networkId && !loading

  if (isShield) {
    return (
      <Dialog open={open} onOpenChange={(isOpen) => !isOpen && handleClose()}>
        <DialogContent className="left-auto right-0 top-0 h-screen max-w-[500px] translate-x-0 translate-y-0 rounded-none border-l border-border bg-card p-0 text-card-foreground shadow-[0_30px_80px_oklch(0.10_0.02_250/0.45)] data-[state=closed]:slide-out-to-right data-[state=open]:slide-in-from-right [&>button]:hidden">
          <div className="flex h-full flex-col">
            <div className="flex items-start justify-between border-b border-border px-6 py-5">
              <div className="flex items-start gap-4">
                <div className="grid h-12 w-12 place-items-center rounded-2xl bg-[oklch(0.78_0.10_235/0.14)] text-[oklch(0.78_0.10_235)]">
                  <Shield className="h-6 w-6" />
                </div>
                <div>
                  <DialogTitle className="text-2xl font-bold tracking-[-0.02em]">Add Shield</DialogTitle>
                  <DialogDescription className="mt-1 text-sm text-muted-foreground">
                    Register a shield and assign it to a remote network.
                  </DialogDescription>
                </div>
              </div>
              <button
                onClick={handleClose}
                className="grid h-9 w-9 place-items-center rounded-xl border border-border bg-secondary text-muted-foreground transition hover:text-foreground"
              >
                <X className="h-4 w-4" />
              </button>
            </div>

            <div className="flex-1 overflow-y-auto px-6 py-6">
              <div className="space-y-6">
                <div>
                  <label htmlFor="shieldName" className="mb-2 block text-sm font-medium">Shield Name *</label>
                  <Input
                    id="shieldName"
                    value={agentName}
                    onChange={(event) => setAgentName(event.target.value)}
                    placeholder="e.g. prod-shield-01"
                    className="h-11 rounded-xl border-border bg-secondary px-4"
                    autoFocus
                  />
                  <p className="mt-2 text-sm text-muted-foreground">
                    Used to identify this shield in the console and CLI.
                  </p>
                </div>

                {!fixedNetworkId ? (
                  <div>
                    <label className="mb-2 block text-sm font-medium">Remote Network *</label>
                    <div className="relative">
                      <select
                        value={selectedNetworkId}
                        onChange={(event) => setSelectedNetworkId(event.target.value)}
                        disabled={networksLoading || networks.length === 0}
                        className="flex h-11 w-full appearance-none rounded-xl border border-border bg-secondary px-4 text-sm outline-none"
                      >
                        <option value="" disabled>
                          {networksLoading
                            ? 'Loading networks...'
                            : networks.length === 0
                              ? 'No remote networks found'
                              : 'Select a remote network'}
                        </option>
                        {networks.map((network) => (
                          <option key={network.id} value={network.id}>{network.name}</option>
                        ))}
                      </select>
                      <ChevronDown className="pointer-events-none absolute right-4 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
                    </div>
                    <p className="mt-2 text-sm text-muted-foreground">
                      The shield will bind to a connector in this network at install time.
                    </p>
                  </div>
                ) : (
                  <div className="rounded-2xl border border-border bg-secondary px-4 py-3">
                    <div className="text-xs font-semibold uppercase tracking-[0.08em] text-muted-foreground">Remote Network</div>
                    <div className="mt-1 text-sm font-semibold">{selectedNetwork?.name ?? 'Selected network'}</div>
                  </div>
                )}

                <div>
                  <label className="mb-2 block text-sm font-medium">Platform</label>
                  <div className="grid grid-cols-2 gap-2 rounded-2xl bg-secondary p-1">
                    <button
                      type="button"
                      onClick={() => setPlatform('linux')}
                      className={`flex h-10 items-center justify-center gap-2 rounded-xl text-sm font-semibold transition ${platform === 'linux' ? 'bg-card text-foreground' : 'text-muted-foreground'}`}
                    >
                      <HardDriveDownload className="h-4 w-4" />
                      Linux
                    </button>
                    <button
                      type="button"
                      onClick={() => setPlatform('docker')}
                      className={`flex h-10 items-center justify-center gap-2 rounded-xl text-sm font-semibold transition ${platform === 'docker' ? 'bg-card text-foreground' : 'text-muted-foreground'}`}
                    >
                      <Server className="h-4 w-4" />
                      Docker
                    </button>
                  </div>
                  <p className="mt-2 text-sm text-muted-foreground">Windows & macOS are not currently supported.</p>
                </div>

                <div className="rounded-2xl border border-dashed border-border bg-background/20 px-4 py-4">
                  <div className="flex flex-wrap items-center gap-x-4 gap-y-2 text-sm">
                    <span className="text-xs font-semibold uppercase tracking-[0.08em] text-muted-foreground">Preview</span>
                    <span className="font-semibold text-primary">{agentName || 'shield-name'}</span>
                    <span className="text-xs font-semibold uppercase tracking-[0.08em] text-muted-foreground">Platform</span>
                    <span className="font-semibold">{platform}</span>
                  </div>
                </div>

                {error ? <p className="text-sm text-destructive">{error}</p> : null}
              </div>
            </div>

            <div className="flex items-center justify-end gap-3 border-t border-border px-6 py-4">
              <Button variant="ghost" onClick={handleClose} disabled={loading}>Cancel</Button>
              <Button onClick={handleSubmit} disabled={!canSubmit} className="gap-2">
                {loading ? 'Creating...' : 'Add Shield'}
                <Plus className="h-4 w-4" />
              </Button>
            </div>
          </div>
        </DialogContent>
      </Dialog>
    )
  }

  return (
    <Dialog open={open} onOpenChange={(isOpen) => !isOpen && handleClose()}>
      <DialogContent className="left-auto right-0 top-0 h-screen max-w-[500px] translate-x-0 translate-y-0 rounded-none border-l border-border bg-card p-0 text-card-foreground shadow-[0_30px_80px_oklch(0.10_0.02_250/0.45)] data-[state=closed]:slide-out-to-right data-[state=open]:slide-in-from-right [&>button]:hidden">
        <div className="flex h-full flex-col">
          <div className="flex items-start justify-between border-b border-border px-6 py-5">
            <div className="flex items-start gap-4">
              <div className="grid h-12 w-12 place-items-center rounded-2xl bg-primary/14 text-primary">
                <Server className="h-6 w-6" />
              </div>
              <div>
                <DialogTitle className="text-2xl font-bold tracking-[-0.02em]">Add Connector</DialogTitle>
                <DialogDescription className="mt-1 text-sm text-muted-foreground">
                  Register a connector and assign it to a remote network.
                </DialogDescription>
              </div>
            </div>
            <button
              onClick={handleClose}
              className="grid h-9 w-9 place-items-center rounded-xl border border-border bg-secondary text-muted-foreground transition hover:text-foreground"
            >
              <X className="h-4 w-4" />
            </button>
          </div>

          <div className="flex-1 overflow-y-auto px-6 py-6">
            <div className="space-y-6">
              <div>
                <label htmlFor="connectorName" className="mb-2 block text-sm font-medium">Connector Name *</label>
                <Input
                  id="connectorName"
                  value={agentName}
                  onChange={(event) => setAgentName(event.target.value)}
                  placeholder="e.g. prod-connector-01"
                  className="h-11 rounded-xl border-border bg-secondary px-4"
                  autoFocus
                />
                <p className="mt-2 text-sm text-muted-foreground">
                  Used to identify this connector in the console and CLI.
                </p>
              </div>

              {!fixedNetworkId ? (
                <div>
                  <label className="mb-2 block text-sm font-medium">Remote Network *</label>
                  <div className="relative">
                    <select
                      value={selectedNetworkId}
                      onChange={(event) => setSelectedNetworkId(event.target.value)}
                      disabled={networksLoading || networks.length === 0}
                      className="flex h-11 w-full appearance-none rounded-xl border border-border bg-secondary px-4 text-sm outline-none"
                    >
                      <option value="" disabled>
                        {networksLoading
                          ? 'Loading networks...'
                          : networks.length === 0
                            ? 'No remote networks found'
                            : 'Select a remote network'}
                      </option>
                      {networks.map((network) => (
                        <option key={network.id} value={network.id}>{network.name}</option>
                      ))}
                    </select>
                    <ChevronDown className="pointer-events-none absolute right-4 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
                  </div>
                  <p className="mt-2 text-sm text-muted-foreground">
                    The connector will route traffic to and from this network.
                  </p>
                </div>
              ) : (
                <div className="rounded-2xl border border-border bg-secondary px-4 py-3">
                  <div className="text-xs font-semibold uppercase tracking-[0.08em] text-muted-foreground">Remote Network</div>
                  <div className="mt-1 text-sm font-semibold">{selectedNetwork?.name ?? 'Selected network'}</div>
                </div>
              )}

              <div>
                <label className="mb-2 block text-sm font-medium">Platform</label>
                <div className="grid grid-cols-2 gap-2 rounded-2xl bg-secondary p-1">
                  <button
                    type="button"
                    onClick={() => setPlatform('linux')}
                    className={`flex h-10 items-center justify-center gap-2 rounded-xl text-sm font-semibold transition ${platform === 'linux' ? 'bg-card text-foreground' : 'text-muted-foreground'}`}
                  >
                    <HardDriveDownload className="h-4 w-4" />
                    Linux
                  </button>
                  <button
                    type="button"
                    onClick={() => setPlatform('docker')}
                    className={`flex h-10 items-center justify-center gap-2 rounded-xl text-sm font-semibold transition ${platform === 'docker' ? 'bg-card text-foreground' : 'text-muted-foreground'}`}
                  >
                    <Server className="h-4 w-4" />
                    Docker
                  </button>
                </div>
                <p className="mt-2 text-sm text-muted-foreground">Windows & macOS are not currently supported.</p>
              </div>

              <div className="rounded-2xl border border-dashed border-border bg-background/20 px-4 py-4">
                <div className="flex flex-wrap items-center gap-x-4 gap-y-2 text-sm">
                  <span className="text-xs font-semibold uppercase tracking-[0.08em] text-muted-foreground">Preview</span>
                  <span className="font-semibold text-primary">{agentName || 'connector-name'}</span>
                  <span className="text-xs font-semibold uppercase tracking-[0.08em] text-muted-foreground">Platform</span>
                  <span className="font-semibold">{platform}</span>
                  {networkId ? (
                    <>
                      <span className="text-xs font-semibold uppercase tracking-[0.08em] text-muted-foreground">Network</span>
                      <span className="font-semibold">{selectedNetwork?.name ?? 'Assigned network'}</span>
                    </>
                  ) : null}
                </div>
              </div>

              <div className="rounded-2xl border border-[oklch(0.85_0.13_80/0.35)] bg-[oklch(0.85_0.13_80/0.08)] px-4 py-4">
                <div className="flex items-start gap-3">
                  <div className="grid h-10 w-10 shrink-0 place-items-center rounded-xl bg-[oklch(0.85_0.13_80/0.15)] text-[oklch(0.85_0.13_80)]">
                    <AlertTriangle className="h-5 w-5" />
                  </div>
                  <div>
                    <div className="text-base font-semibold">Install command shown on next step</div>
                    <p className="mt-1 text-sm text-muted-foreground">
                      After creating, you'll get a one-line install command on the connector details page. The connector stays in Pending until it checks in.
                    </p>
                  </div>
                </div>
              </div>

              {error ? <p className="text-sm text-destructive">{error}</p> : null}
            </div>
          </div>

          <div className="flex items-center justify-end gap-3 border-t border-border px-6 py-4">
            <Button variant="ghost" onClick={handleClose} disabled={loading}>Cancel</Button>
            <Button onClick={handleSubmit} disabled={!canSubmit} className="gap-2">
              {loading ? 'Creating...' : 'Add Connector'}
            </Button>
          </div>
        </div>
      </DialogContent>
    </Dialog>
  )
}
