import { useRef, useState } from 'react'
import { ApiError } from '@/api/client'
import { deleteRelay, revokeRelay } from '@/api/relays'
import type { RelayDetail } from '@/api/types'
import { hasRole } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { ConfirmDangerDialog, type DangerError } from '@/components/ConfirmDangerDialog'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { relayActionFailure, type RelayAction } from '@/lib/relay-errors'

export interface RelayActionNotice {
  tone: 'success' | 'info'
  text: string
}

const SUPER_ADMIN = ['super-admin'] as const

// Relay detail's "Danger zone" (Sprint 22 Phase X; D-17, D-20, D-30, D-31).
// Super-admin only, and only for a relay that isn't deleted: Revoke while it
// isn't revoked, Delete until it is deleted (a revoked relay may still be
// removed, and deleting a pending relay is how an expired token is replaced).
// Hiding it is UX only; the controller authorizes every request. Each confirm
// sends exactly one request; when it ends, onDone reloads the page with a
// notice (the dialog is gone after a reload, so the notice lives above it).
export function RelayDangerZone({ relay, onDone }: { relay: RelayDetail; onDone: (notice: RelayActionNotice) => void }) {
  const role = useSessionStore((s) => s.me?.role)
  const [action, setAction] = useState<RelayAction | null>(null)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<DangerError>()
  // Guards a second submit before the pending re-render lands.
  const inFlight = useRef(false)

  if (!hasRole(role, SUPER_ADMIN) || relay.status === 'deleted') return null
  const canRevoke = relay.status !== 'revoked'
  // Phase D (P3): advisory only, as of the page load; never blocks the action.
  const lastListed = relay.in_relay_list && relay.eligible_relay_count === 1

  function open(next: RelayAction) {
    setError(undefined)
    setAction(next)
  }

  async function confirm(reason: string, typedName: string) {
    if (!action || inFlight.current) return
    inFlight.current = true
    setPending(true)
    setError(undefined)
    try {
      if (action === 'revoke') await revokeRelay(relay.id, reason, typedName)
      else await deleteRelay(relay.id, reason, typedName)
      setAction(null)
      onDone({
        tone: 'success',
        text:
          action === 'revoke'
            ? `${relay.name} is revoked. Its certificates are on the CRL and connectors move to other relays.`
            : `${relay.name} is deleted. Its certificates are revoked; add a new relay to replace it.`,
      })
    } catch (err) {
      // An ended session is handled by the session store (sign-in page).
      if (err instanceof ApiError && err.sessionEnded) return
      const failure = relayActionFailure(err, action, relay.name)
      if (failure.refresh) {
        setAction(null)
        onDone({ tone: 'info', text: failure.message })
      } else {
        setError({ message: failure.message, field: failure.field })
      }
    } finally {
      inFlight.current = false
      setPending(false)
    }
  }

  return (
    <Card role="region" aria-labelledby="danger-zone-title" className="border-destructive/40 px-5 py-5">
      <h2 id="danger-zone-title" className="font-display text-base font-semibold text-destructive">
        Danger zone
      </h2>
      <div className="mt-4 flex flex-col gap-4">
        {canRevoke && (
          <div className="flex flex-wrap items-center justify-between gap-3">
            <p className="max-w-xl text-sm text-subtle-foreground">
              <span className="font-medium text-foreground">Revoke.</span> Its certificates go on the CRL and connectors
              move to other relays. The relay stays listed as revoked.
            </p>
            <Button variant="outline" className="border-destructive/50 text-destructive" onClick={() => open('revoke')}>
              Revoke relay
            </Button>
          </div>
        )}
        <div className="flex flex-wrap items-center justify-between gap-3">
          <p className="max-w-xl text-sm text-subtle-foreground">
            <span className="font-medium text-foreground">Delete.</span> Revokes it and removes it from the fleet. To
            replace it, add a new relay.
          </p>
          <Button variant="destructive" onClick={() => open('delete')}>
            Delete relay
          </Button>
        </div>
      </div>

      {action && (
        <ConfirmDangerDialog
          title={action === 'revoke' ? 'Revoke relay' : 'Delete relay'}
          description={
            action === 'revoke' ? (
              <>
                <strong className="text-foreground">{relay.name}</strong>: its certificates go on the CRL and connectors
                move to other relays. This can’t be undone.
              </>
            ) : (
              <>
                <strong className="text-foreground">{relay.name}</strong> is revoked and removed from the fleet. This
                can’t be undone; to replace it, add a new relay.
              </>
            )
          }
          warning={
            lastListed
              ? 'No eligible relays will remain: this is the only relay in the list connectors use. New connections may fail until another relay is eligible.'
              : undefined
          }
          confirmText={relay.name}
          confirmLabel={action === 'revoke' ? 'Revoke relay' : 'Delete relay'}
          pendingLabel={action === 'revoke' ? 'Revoking…' : 'Deleting…'}
          pending={pending}
          error={error}
          onConfirm={(reason, typedName) => void confirm(reason, typedName)}
          onCancel={() => setAction(null)}
        />
      )}
    </Card>
  )
}
