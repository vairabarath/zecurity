import { useState } from 'react'
import { useMutation } from '@apollo/client/react'
import {
  GenerateConnectorTokenDocument,
} from '@/generated/graphql'
import type {
  GenerateConnectorTokenMutationVariables,
} from '@/generated/graphql'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  AlertTriangle,
  Copy,
  Check,
  Terminal,
  X,
  Loader2,
} from 'lucide-react'

interface InstallCommandModalProps {
  remoteNetworkId: string
  open: boolean
  onClose: () => void
}

export function InstallCommandModal({ remoteNetworkId, open, onClose }: InstallCommandModalProps) {
  const [connectorName, setConnectorName] = useState('')
  const [installCommand, setInstallCommand] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)

  const [generateToken, { loading }] = useMutation(GenerateConnectorTokenDocument)

  async function handleGenerate() {
    if (!connectorName.trim()) return
    const result = await generateToken({
      variables: {
        remoteNetworkId,
        connectorName: connectorName.trim(),
      } as GenerateConnectorTokenMutationVariables,
    })
    if (result.data?.generateConnectorToken.installCommand) {
      setInstallCommand(result.data.generateConnectorToken.installCommand)
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
    setInstallCommand(null)
    setCopied(false)
    onClose()
  }

  if (!open) return null

  return (
    <Card className="border-primary/20 bg-card/80 backdrop-blur-sm animate-fade-up overflow-hidden">
      <CardHeader className="flex flex-row items-center justify-between pb-3">
        <div className="flex items-center gap-2">
          <Terminal className="w-4 h-4 text-primary" />
          <CardTitle className="text-base font-display">
            {installCommand ? 'Install Connector' : 'New Connector'}
          </CardTitle>
        </div>
        <Button variant="ghost" size="sm" className="h-7 w-7 p-0" onClick={handleClose}>
          <X className="w-4 h-4" />
        </Button>
      </CardHeader>
      <CardContent className="space-y-4">
        {/* Step 1: Name + Generate */}
        {!installCommand && (
          <div className="space-y-4">
            <div className="space-y-1.5">
              <label className="text-xs text-muted-foreground font-mono uppercase tracking-wider">
                Connector Name
              </label>
              <Input
                placeholder="e.g. prod-connector-01"
                value={connectorName}
                onChange={(e) => setConnectorName(e.target.value)}
                onKeyDown={(e) => e.key === 'Enter' && handleGenerate()}
                className="bg-background/50 font-mono text-sm"
              />
            </div>
            <Button
              onClick={handleGenerate}
              disabled={!connectorName.trim() || loading}
              className="gap-2"
            >
              {loading && <Loader2 className="w-4 h-4 animate-spin" />}
              {loading ? 'Generating...' : 'Generate Token'}
            </Button>
          </div>
        )}

        {/* Step 2: Install Command */}
        {installCommand && (
          <div className="space-y-4">
            {/* Warning Banner */}
            <div className="flex items-start gap-3 rounded-lg border border-amber-400/20 bg-amber-400/5 p-3">
              <AlertTriangle className="w-4 h-4 text-amber-400 shrink-0 mt-0.5" />
              <div className="text-xs text-amber-200/80 leading-relaxed">
                This token expires in <span className="font-semibold text-amber-300">24 hours</span> and works only once.
                Save the install command now.
              </div>
            </div>

            {/* Command Block */}
            <div className="relative group">
              <div className="rounded-lg border border-border/50 bg-background/80 overflow-hidden">
                <div className="flex items-center justify-between px-3 py-2 border-b border-border/30 bg-muted/20">
                  <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">
                    Install Command
                  </span>
                  <Button
                    variant="ghost"
                    size="sm"
                    className="h-6 px-2 text-[10px] gap-1"
                    onClick={handleCopy}
                  >
                    {copied ? (
                      <>
                        <Check className="w-3 h-3 text-emerald-400" />
                        <span className="text-emerald-400">Copied</span>
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

            {/* Done */}
            <div className="flex justify-end">
              <Button onClick={handleClose} variant="outline" className="gap-2">
                Done
              </Button>
            </div>
          </div>
        )}
      </CardContent>
    </Card>
  )
}
