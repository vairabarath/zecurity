import { useState, type FormEvent, type ReactNode } from 'react'
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
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { cn } from '@/lib/utils'

/** Matches the controller's reason limit (relay admin maxReasonRunes). */
export const MAX_DANGER_REASON = 500

export interface DangerError {
  message: string
  /** Put the message on this field; otherwise it is shown as an alert. */
  field?: 'reason' | 'confirm'
}

/** Characters as the server counts them (Go utf8.RuneCountInString). */
function codePoints(s: string): number {
  return Array.from(s).length
}

// A destructive action's confirmation (Sprint 22 Phase X, D-17; reused by
// Phase D for drain): a required reason, recorded in the provider audit log,
// and the target's exact name typed to confirm. The reason is trimmed and
// limited to 500 characters like the server; the name is compared exactly
// (no trimming, case matters), like the server. The server checks both again.
// Drain is reversible and not D-17, so it turns both off (`reasonRequired`,
// `requireConfirmText`); the defaults keep revoke and delete as they are.
// `warning` is advisory text shown above the fields; it never disables
// confirming. The caller runs the request and passes back `pending` and `error`.
export function ConfirmDangerDialog({
  title,
  description,
  confirmText,
  confirmLabel,
  pendingLabel,
  pending,
  error,
  onConfirm,
  onCancel,
  reasonRequired = true,
  requireConfirmText = true,
  warning,
}: {
  title: string
  description: ReactNode
  /** What the operator must type, exactly (ignored when requireConfirmText is false). */
  confirmText: string
  confirmLabel: string
  pendingLabel: string
  pending: boolean
  error?: DangerError
  /** The trimmed reason and the name exactly as typed (equal to confirmText). */
  onConfirm: (reason: string, typedName: string) => void
  onCancel: () => void
  /** False: an empty reason is allowed (it is then not sent). */
  reasonRequired?: boolean
  /** False: no typed name; onConfirm gets an empty name. */
  requireConfirmText?: boolean
  warning?: ReactNode
}) {
  const [reason, setReason] = useState('')
  const [typed, setTyped] = useState('')
  const trimmed = reason.trim()
  const length = codePoints(trimmed)
  const tooLong = length > MAX_DANGER_REASON
  const valid = (!reasonRequired || trimmed !== '') && !tooLong && (!requireConfirmText || typed === confirmText)

  const reasonError = error?.field === 'reason' ? error.message : tooLong ? 'Keep the reason to 500 characters or fewer.' : undefined
  const confirmError = error?.field === 'confirm' ? error.message : undefined
  const generalError = error && !error.field ? error.message : undefined

  function submit(e: FormEvent) {
    e.preventDefault()
    if (valid && !pending) onConfirm(trimmed, requireConfirmText ? typed : '')
  }

  return (
    <Dialog open onOpenChange={(o) => !o && !pending && onCancel()}>
      <DialogContent>
        <form onSubmit={submit} className="grid gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
            <DialogDescription>{description}</DialogDescription>
          </DialogHeader>
          {warning && (
            <Alert role="note">
              <AlertDescription>{warning}</AlertDescription>
            </Alert>
          )}
          {generalError && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{generalError}</AlertDescription>
            </Alert>
          )}
          <div className="grid gap-2">
            <Label htmlFor="danger-reason">Reason</Label>
            <textarea
              id="danger-reason"
              value={reason}
              rows={3}
              autoComplete="off"
              aria-invalid={reasonError !== undefined}
              aria-describedby="danger-reason-hint"
              onChange={(e) => setReason(e.target.value)}
              className="flex w-full rounded-lg border border-input bg-field px-3 py-2 text-sm transition-colors placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/30 aria-invalid:border-destructive"
            />
            <p id="danger-reason-hint" className="flex justify-between gap-3 text-xs">
              <span className={cn(reasonError ? 'text-destructive' : 'text-muted-foreground')}>
                {reasonError ?? (reasonRequired ? 'Required. Recorded in the provider audit log.' : 'Optional. Recorded in the provider audit log.')}
              </span>
              <span className={cn('tabular-nums', tooLong ? 'text-destructive' : 'text-muted-foreground')}>
                {length} / {MAX_DANGER_REASON}
              </span>
            </p>
          </div>
          {requireConfirmText && (
            <div className="grid gap-2">
              <Label htmlFor="danger-confirm">
                Type <span className="font-mono font-semibold text-foreground">{confirmText}</span> to confirm
              </Label>
              <Input
                id="danger-confirm"
                value={typed}
                autoComplete="off"
                spellCheck={false}
                aria-invalid={confirmError !== undefined}
                aria-describedby={confirmError ? 'danger-confirm-error' : undefined}
                onChange={(e) => setTyped(e.target.value)}
              />
              {confirmError && (
                <p id="danger-confirm-error" className="text-xs text-destructive">
                  {confirmError}
                </p>
              )}
            </div>
          )}
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onCancel} disabled={pending}>
              Cancel
            </Button>
            <Button type="submit" variant="destructive" disabled={!valid || pending}>
              {pending ? pendingLabel : confirmLabel}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
