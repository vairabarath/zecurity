import { useState } from 'react'
import { AlertTriangle } from 'lucide-react'
import { Button } from '@/components/ui/button'
import {
  Dialog,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Alert, AlertDescription } from '@/components/ui/alert'

// Break-glass enable dialog (ADR-025 §3.2).
//
// SCIM stays disabled while the identity mapping is unproven — that is the
// fail-closed intent of §3.1 and this dialog does NOT work around it. It invokes
// enableScimBreakGlass, which requires the explicit identity.mapping.break_glass
// permission (ADMIN role alone is NOT sufficient) plus a mandatory reason, and
// is audited as scim.mapping.break_glass_override.
//
// `refusal` carries the server's verbatim refusal message from the normal enable
// attempt, so the admin sees WHY the deliberate override is being offered.
export function BreakGlassDialog({
  open,
  connectionName,
  refusal,
  loading,
  onClose,
  onConfirm,
}: {
  open: boolean
  connectionName: string
  refusal?: string | null
  loading: boolean
  onClose: () => void
  onConfirm: (reason: string) => void
}) {
  const [reason, setReason] = useState('')

  function handleOpenChange(next: boolean) {
    if (!next) {
      setReason('')
      onClose()
    }
  }

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!reason.trim()) return
    onConfirm(reason.trim())
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Enable SCIM via break-glass override</DialogTitle>
        </DialogHeader>

        <div className="space-y-4 pt-1">
          {refusal ? (
            <Alert variant="destructive">
              <AlertTriangle className="h-4 w-4" />
              <AlertDescription>
                <span className="font-medium">The server refused the normal enable path:</span>{' '}
                {refusal}
              </AlertDescription>
            </Alert>
          ) : null}

          <p className="text-sm text-muted-foreground">
            SCIM for <span className="font-semibold text-foreground">{connectionName}</span> is
            disabled because the identity mapping is <span className="font-mono">unproven</span>. A
            wrong mapping silently splits or merges accounts, which is why the normal enable path
            requires a proven mapping.
          </p>
          <p className="text-sm text-muted-foreground">
            This override requires the{' '}
            <span className="font-mono text-foreground">identity.mapping.break_glass</span>{' '}
            permission — your ADMIN role alone is not sufficient — and is audited as{' '}
            <span className="font-mono text-foreground">scim.mapping.break_glass_override</span>.
          </p>

          <form onSubmit={handleSubmit} className="space-y-4">
            <div className="space-y-1.5">
              <Label htmlFor="break-glass-reason">Reason (required)</Label>
              <Input
                id="break-glass-reason"
                placeholder="Why is the override justified?"
                value={reason}
                onChange={(e) => setReason(e.target.value)}
                required
              />
            </div>
            <DialogFooter>
              <Button type="button" variant="outline" onClick={() => handleOpenChange(false)} disabled={loading}>
                Cancel
              </Button>
              <Button type="submit" variant="destructive" disabled={loading || !reason.trim()}>
                {loading ? 'Overriding…' : 'Override and enable SCIM'}
              </Button>
            </DialogFooter>
          </form>
        </div>
      </DialogContent>
    </Dialog>
  )
}
