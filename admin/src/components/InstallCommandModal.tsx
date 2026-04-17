import { useState } from 'react'
import { useMutation, useQuery } from '@apollo/client/react'
import {
  GenerateConnectorTokenDocument,
  GetRemoteNetworksDocument,
} from '@/generated/graphql'
import type { GenerateConnectorTokenMutationVariables } from '@/generated/graphql'
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from '@/components/ui/dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  AlertTriangle,
  Copy,
  Check,
  Loader2,
} from 'lucide-react'

interface InstallCommandModalProps {
  // If provided, skips network selection and uses this network directly
  remoteNetworkId?: string
  open: boolean
  onClose: () => void
}

export function InstallCommandModal({ remoteNetworkId: fixedNetworkId, open, onClose }: InstallCommandModalProps) {
  const [connectorName, setConnectorName] = useState('')
  const [selectedNetworkId, setSelectedNetworkId] = useState('')
  const [installCommand, setInstallCommand] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const { data: networksData, loading: networksLoading } = useQuery(GetRemoteNetworksDocument, {
    skip: !!fixedNetworkId,
    fetchPolicy: 'cache-and-network',
  })

  const [generateToken, { loading }] = useMutation(GenerateConnectorTokenDocument)

  const networkId = fixedNetworkId ?? selectedNetworkId
  const networks = networksData?.remoteNetworks ?? []

  async function handleGenerate() {
    if (!connectorName.trim() || !networkId) return
    setError(null)
    try {
      const result = await generateToken({
        variables: {
          remoteNetworkId: networkId,
          connectorName: connectorName.trim(),
        } as GenerateConnectorTokenMutationVariables,
        refetchQueries: [{ query: GetRemoteNetworksDocument }],
      })
      if (result.data?.generateConnectorToken.installCommand) {
        setInstallCommand(result.data.generateConnectorToken.installCommand)
      }
    } catch (e: unknown) {
      setError(e instanceof Error ? e.message : 'Failed to generate token')
    }
  }

  function handleCopy() {
    if (!installCommand) return
    navigator.clipboard.writeText(installCommand)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  function handleClose() {
    setConnectorName('')
    setSelectedNetworkId('')
    setInstallCommand(null)
    setCopied(false)
    setError(null)
    onClose()
  }

  const canSubmit = !!connectorName.trim() && !!networkId && !loading

  return (
    <Dialog open={open} onOpenChange={(o) => !o && handleClose()}>
      <DialogContent className="sm:max-w-[500px]">
        <DialogHeader>
          <DialogTitle>
            {installCommand ? 'Install Connector' : 'Add Connector'}
          </DialogTitle>
          <DialogDescription>
            {installCommand
              ? 'Copy and run the command below on your server.'
              : 'Register a connector and assign it to a remote network.'}
          </DialogDescription>
        </DialogHeader>

        {!installCommand && (
          <div className="grid gap-4 py-2">
            <div className="grid gap-2">
              <Label htmlFor="connectorName">Connector Name</Label>
              <Input
                id="connectorName"
                placeholder="e.g. prod-connector-01"
                value={connectorName}
                onChange={(e) => setConnectorName(e.target.value)}
                onKeyDown={(e) => e.key === 'Enter' && canSubmit && handleGenerate()}
                className="font-mono text-sm"
                autoFocus
              />
            </div>

            {!fixedNetworkId && (
              <div className="grid gap-2">
                <Label>Remote Network</Label>
                <select
                  value={selectedNetworkId}
                  onChange={(e) => setSelectedNetworkId(e.target.value)}
                  disabled={networksLoading || networks.length === 0}
                  className="flex h-10 w-full rounded-lg border border-border bg-background px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-primary/30 disabled:cursor-not-allowed disabled:opacity-50"
                >
                  <option value="" disabled>
                    {networksLoading
                      ? 'Loading networks...'
                      : networks.length === 0
                        ? 'No remote networks found'
                        : 'Select a remote network'}
                  </option>
                  {networks.map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.name}
                    </option>
                  ))}
                </select>
              </div>
            )}

            {error && (
              <p className="text-sm text-destructive">{error}</p>
            )}
          </div>
        )}

        {installCommand && (
          <div className="space-y-4 py-2">
            <div className="flex items-start gap-3 rounded-lg border border-amber-500/20 bg-amber-400/5 p-3">
              <AlertTriangle className="w-4 h-4 text-amber-600 shrink-0 mt-0.5" />
              <p className="text-xs text-amber-700 leading-relaxed">
                This token expires in{' '}
                <span className="font-semibold text-amber-800">24 hours</span> and works only once.
                Save the install command now.
              </p>
            </div>

            <div className="rounded-lg border border-border/50 bg-muted/40 overflow-hidden">
              <div className="flex items-center justify-between px-3 py-2 border-b border-border/30 bg-muted/20">
                <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">
                  Install Command
                </span>
                <Button variant="ghost" size="sm" className="h-6 px-2 text-[10px] gap-1" onClick={handleCopy}>
                  {copied ? (
                    <>
                      <Check className="w-3 h-3 text-emerald-500" />
                      <span className="text-emerald-600">Copied</span>
                    </>
                  ) : (
                    <>
                      <Copy className="w-3 h-3" />
                      Copy
                    </>
                  )}
                </Button>
              </div>
              <pre className="p-4 text-xs font-mono text-foreground/90 overflow-x-auto whitespace-pre-wrap break-all leading-relaxed">
                {installCommand}
              </pre>
            </div>
          </div>
        )}

        <DialogFooter>
          {!installCommand ? (
            <>
              <Button variant="outline" onClick={handleClose} disabled={loading}>
                Cancel
              </Button>
              <Button onClick={handleGenerate} disabled={!canSubmit}>
                {loading && <Loader2 className="w-4 h-4 animate-spin mr-2" />}
                {loading ? 'Generating...' : 'Add Connector'}
              </Button>
            </>
          ) : (
            <Button onClick={handleClose}>Done</Button>
          )}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
