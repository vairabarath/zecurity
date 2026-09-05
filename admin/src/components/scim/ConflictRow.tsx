import { useState } from 'react'
import { useMutation } from '@apollo/client/react'
import { toast } from 'sonner'
import {
  AcceptScimConflictDocument,
  RejectScimConflictDocument,
  ReopenScimConflictDocument,
  type ScimConflict,
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
import { Alert, AlertDescription, AlertTitle } from '@/components/ui/alert'
import { StatusPill } from '@/lib/console'
import { asConflictError, conflictGuidance, type ConflictError } from '@/lib/conflictError'

function statusTone(status: string): 'ok' | 'warn' | 'muted' {
  switch (status) {
    case 'pending':
      return 'warn'
    case 'approved':
      return 'ok'
    default:
      return 'muted'
  }
}

// Human-readable label for a conflict. The directory-claimed snapshots are
// CONTEXT only (never the identity key). When both snapshots are null (e.g.
// deprovision/reactivate-raised conflicts, or rows written before capture),
// fall back to the canonicalKey so the admin still sees something meaningful.
function describeConflict(conflict: ScimConflict): string {
  const who = conflict.scimUsernameSnapshot || conflict.scimEmailSnapshot
  if (who) return who
  return conflict.canonicalKey
}

type ResolveKind = 'accept' | 'reject' | 'reopen'

const ACTION_LABEL: Record<ResolveKind, string> = {
  accept: 'Accept link',
  reject: 'Reject',
  reopen: 'Reopen',
}

export function ConflictRow({
  conflict,
  connectionId,
  onResolved,
}: {
  conflict: ScimConflict
  connectionId: string
  onResolved: () => void
}) {
  const [dialog, setDialog] = useState<ResolveKind | null>(null)
  const [reason, setReason] = useState('')
  const [error, setError] = useState<ConflictError | null>(null)

  const [accept, acceptingResult] = useMutation(AcceptScimConflictDocument)
  const [reject, rejectingResult] = useMutation(RejectScimConflictDocument)
  const [reopen, reopeningResult] = useMutation(ReopenScimConflictDocument)

  // A pending conflict accepts a link, rejects, or reopens a previously
  // resolved one. Resolved rows (approved/rejected/expired) carry their
  // resolutionReason but take no further action here.
  const canAct = conflict.status === 'pending'
  const busy =
    acceptingResult.loading || rejectingResult.loading || reopeningResult.loading

  async function submit() {
    if (!dialog || !reason.trim()) return
    setError(null)
    const kind = dialog
    try {
      if (kind === 'accept') {
        await accept({
          variables: {
            connectionId,
            canonicalKey: conflict.canonicalKey,
            reason: reason.trim(),
          },
        })
      } else if (kind === 'reject') {
        await reject({
          variables: {
            connectionId,
            canonicalKey: conflict.canonicalKey,
            reason: reason.trim(),
          },
        })
      } else {
        await reopen({
          variables: {
            connectionId,
            canonicalKey: conflict.canonicalKey,
            reason: reason.trim(),
          },
        })
      }
      toast.success(
        kind === 'accept'
          ? 'Directory link accepted.'
          : kind === 'reject'
            ? 'Conflict rejected.'
            : 'Conflict reopened.',
      )
      setDialog(null)
      setReason('')
      onResolved()
    } catch (err) {
      const parsed = asConflictError(err)
      setError(parsed)
      // FORBIDDEN means this admin lacks identity.mapping.break_glass — keep the
      // dialog open so they can read the guidance; everything else also stays
      // open for retry. Never string-match the message.
      if (parsed.code !== 'FORBIDDEN') {
        toast.error(parsed.message)
      }
    }
  }

  const guidance = error ? conflictGuidance(error) : null

  return (
    <div className="rounded-lg border border-border bg-card p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <div className="flex items-center gap-2">
            <StatusPill label={conflict.status} tone={statusTone(conflict.status)} />
            <span className="text-sm font-medium">{describeConflict(conflict)}</span>
          </div>
          <p className="text-xs text-muted-foreground">
            canonical key: {conflict.canonicalKey}
            {conflict.scimExternalId ? ` · external id: ${conflict.scimExternalId}` : ''}
          </p>
          {conflict.resolutionReason ? (
            <p className="text-xs text-muted-foreground">
              reason: {conflict.resolutionReason}
            </p>
          ) : null}
        </div>

        {canAct ? (
          <div className="flex shrink-0 items-center gap-2">
            <Button
              size="sm"
              variant="default"
              disabled={busy}
              onClick={() => {
                setError(null)
                setReason('')
                setDialog('accept')
              }}
            >
              Accept link
            </Button>
            <Button
              size="sm"
              variant="outline"
              disabled={busy}
              onClick={() => {
                setError(null)
                setReason('')
                setDialog('reject')
              }}
            >
              Reject
            </Button>
            {/* Reopen is only meaningful for a previously resolved row. */}
          </div>
        ) : conflict.status === 'rejected' ? (
          <Button
            size="sm"
            variant="outline"
            disabled={busy}
            onClick={() => {
              setError(null)
              setReason('')
              setDialog('reopen')
            }}
          >
            Reopen
          </Button>
        ) : null}
      </div>

      <Dialog
        open={dialog !== null}
        onOpenChange={(next) => {
          if (!next) {
            setDialog(null)
            setError(null)
            setReason('')
          }
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {dialog ? ACTION_LABEL[dialog] : ''} provisioning conflict
            </DialogTitle>
            <DialogDescription>
              A reason is required and will be recorded in the audit log.
            </DialogDescription>
          </DialogHeader>

          <div className="space-y-3 pt-1">
            <label className="text-sm font-medium" htmlFor="conflict-reason">
              Reason
            </label>
            <textarea
              id="conflict-reason"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              rows={3}
              className="w-full rounded-md border border-input bg-background px-3 py-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/30"
              placeholder="Why is this conflict being resolved this way?"
            />
            {guidance ? (
              <Alert variant={guidance.title === 'Permission required' ? 'destructive' : 'default'}>
                <AlertTitle>{guidance.title}</AlertTitle>
                <AlertDescription>{guidance.body}</AlertDescription>
              </Alert>
            ) : null}
          </div>

          <DialogFooter>
            <Button
              variant="outline"
              disabled={busy}
              onClick={() => {
                setDialog(null)
                setError(null)
                setReason('')
              }}
            >
              Cancel
            </Button>
            <Button
              variant={dialog === 'reject' ? 'destructive' : 'default'}
              disabled={busy || !reason.trim()}
              onClick={() => void submit()}
            >
              {busy
                ? 'Working…'
                : dialog
                  ? ACTION_LABEL[dialog]
                  : 'Confirm'}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  )
}
