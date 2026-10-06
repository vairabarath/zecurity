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

export interface TemporaryCredential {
  email: string
  password: string
}

type CopyState = 'idle' | 'copied' | 'failed'

// Shows a temporary password exactly once (Phase U create/reset).
//
// The password lives only in the parent's React state while this dialog is
// open; closing calls onClose, which must drop it. It is never stored,
// logged or put in the URL, and it is copied only when the operator clicks
// Copy (never automatically). The OS clipboard is left alone afterwards.
// Clicking outside doesn't close the dialog, so the password isn't lost by
// accident; Close and Escape do.
export function TemporaryPasswordDialog({
  credential,
  onClose,
}: {
  credential: TemporaryCredential | null
  onClose: () => void
}) {
  const [copy, setCopy] = useState<CopyState>('idle')

  useEffect(() => {
    if (copy !== 'copied') return
    const id = setTimeout(() => setCopy('idle'), 2000)
    return () => clearTimeout(id)
  }, [copy])

  // Credentials only change via close → null → new, so resetting here gives
  // each new credential a fresh Copy button.
  function close() {
    setCopy('idle')
    onClose()
  }

  async function onCopy() {
    if (!credential) return
    try {
      await navigator.clipboard.writeText(credential.password)
      setCopy('copied')
    } catch {
      setCopy('failed')
    }
  }

  return (
    <Dialog open={credential !== null} onOpenChange={(open) => !open && close()}>
      <DialogContent onPointerDownOutside={(e) => e.preventDefault()}>
        <DialogHeader>
          <DialogTitle>Temporary password</DialogTitle>
          <DialogDescription>
            For <span className="font-medium text-foreground">{credential?.email}</span>. Shown once: share it
            securely. They must change it at first sign-in.
          </DialogDescription>
        </DialogHeader>
        <div className="flex items-center gap-2">
          <code
            aria-label="Temporary password"
            className="flex-1 select-all rounded-md border border-border bg-muted/40 px-3 py-2 font-mono text-sm break-all"
          >
            {credential?.password}
          </code>
          <Button type="button" variant="outline" size="sm" onClick={() => void onCopy()}>
            {copy === 'copied' ? <Check aria-hidden /> : <Copy aria-hidden />}
            {copy === 'copied' ? 'Copied' : 'Copy'}
          </Button>
        </div>
        {copy === 'failed' && (
          <Alert>
            <AlertDescription>Couldn’t copy automatically. Select the password and copy it manually.</AlertDescription>
          </Alert>
        )}
        <p className="text-xs text-muted-foreground">
          Closing this dialog discards the password. It can’t be shown again; reset it to issue a new one.
        </p>
        <DialogFooter>
          <Button type="button" onClick={close}>
            I’ve shared it, close
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
