import { useEffect, useState } from 'react'
import { Check, Copy } from 'lucide-react'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { formatDateTime } from '@/lib/format'
import { buildInstallCommands, type InstallParams } from '@/lib/relay-install'

export interface IssuedRelay extends InstallParams {
  name: string
  expiresAt: string
}

type CopyTarget = 'installer' | 'direct' | 'token'
type CopyState = { target: CopyTarget; ok: boolean } | null

// Shows a new relay's provisioning token and install commands exactly once
// (Sprint 22 Phase E, D-30).
//
// Like TemporaryPasswordDialog: the token lives only in the parent's React
// state while this dialog is open; closing calls onClose, which must drop
// it. It is never stored, logged or put in the URL, and it is copied only
// when the operator clicks a Copy button. Clicking outside doesn't close
// the dialog; the close button and Escape do.
export function ProvisioningTokenDialog({ relay, onClose }: { relay: IssuedRelay | null; onClose: () => void }) {
  const [copy, setCopy] = useState<CopyState>(null)

  useEffect(() => {
    if (!copy?.ok) return
    const id = setTimeout(() => setCopy(null), 2000)
    return () => clearTimeout(id)
  }, [copy])

  function close() {
    setCopy(null)
    onClose()
  }

  const commands = relay ? buildInstallCommands(relay) : null

  async function onCopy(target: CopyTarget, text: string) {
    try {
      await navigator.clipboard.writeText(text)
      setCopy({ target, ok: true })
    } catch {
      setCopy({ target, ok: false })
    }
  }

  function copyButton(target: CopyTarget, text: string, label: string) {
    const done = copy?.target === target && copy.ok
    return (
      <Button type="button" variant="outline" size="sm" aria-label={done ? 'Copied' : label} onClick={() => void onCopy(target, text)}>
        {done ? <Check aria-hidden /> : <Copy aria-hidden />}
        {done ? 'Copied' : 'Copy'}
      </Button>
    )
  }

  const block = 'select-all overflow-x-auto whitespace-pre rounded-md border border-border bg-muted/40 px-3 py-2 font-mono text-xs'

  return (
    <Dialog open={relay !== null} onOpenChange={(open) => !open && close()}>
      <DialogContent
        className="max-h-[90vh] w-[calc(100%-2rem)] max-w-2xl overflow-y-auto"
        onPointerDownOutside={(e) => e.preventDefault()}
      >
        <DialogHeader>
          <DialogTitle>Install {relay?.name}</DialogTitle>
          <DialogDescription>
            The relay is pending until it enrols with this token. The token is single use and expires in 24 hours
            (at {formatDateTime(relay?.expiresAt)}). It is shown once.
          </DialogDescription>
        </DialogHeader>

        {commands?.placeholders && (
          <Alert>
            <AlertDescription>
              The controller addresses aren’t configured, so the commands show placeholders. Replace{' '}
              <code className="font-mono">&lt;controller-host&gt;</code> with the address the relay host can reach.
            </AlertDescription>
          </Alert>
        )}

        {relay && commands && (
          <>
            <section className="grid gap-2">
              <div className="flex items-center justify-between gap-2">
                <h3 className="text-sm font-medium">Install as a service</h3>
                {copyButton('installer', commands.installer, 'Copy install command')}
              </div>
              <p className="text-xs text-muted-foreground">
                On the relay host, from a checkout of the Zecurity repository with the relay binary at{' '}
                <code className="font-mono">./zecurity-relay</code>:
              </p>
              <pre aria-label="Install command" className={block}>
                {commands.installer}
              </pre>
            </section>

            <section className="grid gap-2">
              <div className="flex items-center justify-between gap-2">
                <h3 className="text-sm font-medium">Or run the binary directly</h3>
                {copyButton('direct', commands.direct, 'Copy direct command')}
              </div>
              <pre aria-label="Direct command" className={block}>
                {commands.direct}
              </pre>
            </section>

            <section className="grid gap-2">
              <div className="flex items-center justify-between gap-2">
                <h3 className="text-sm font-medium">Provisioning token</h3>
                {copyButton('token', relay.provisioningToken, 'Copy token')}
              </div>
              <code aria-label="Provisioning token" className={`${block} block whitespace-normal break-all`}>
                {relay.provisioningToken}
              </code>
            </section>
          </>
        )}

        {copy && !copy.ok && (
          <Alert>
            <AlertDescription>Couldn’t copy automatically. Select the text and copy it manually.</AlertDescription>
          </Alert>
        )}
        <p className="text-xs text-muted-foreground">
          Closing this dialog discards the token. It can’t be shown again: if it expires before the relay enrols,
          delete the relay and add it again.
        </p>
        <DialogFooter>
          <Button type="button" onClick={close}>
            I’ve saved it, close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
