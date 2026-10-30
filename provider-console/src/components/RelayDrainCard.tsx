import { useRef, useState } from 'react'
import { ApiError } from '@/api/client'
import { drainRelay, resumeRelay } from '@/api/relays'
import type { RelayDetail } from '@/api/types'
import { hasRole, PROVIDER_ROLES } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { ConfirmDangerDialog, type DangerError } from '@/components/ConfirmDangerDialog'
import type { RelayActionNotice } from '@/components/RelayDangerZone'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { formatRelative } from '@/lib/expiry'
import { formatDateTime } from '@/lib/format'
import { drainActionFailure, type DrainAction } from '@/lib/relay-errors'

const plural = (n: number, one: string) => `${n.toLocaleString()} ${one}${n === 1 ? '' : 's'}`

/**
 * Drain progress in words (Phase D, P4). The controller decides the state from
 * fresh telemetry only; this never derives one. "Drained" is as of the last
 * heartbeat, not a guarantee.
 */
function drainProgress(r: Pick<RelayDetail, 'status' | 'drain_state' | 'attached_connectors' | 'connection_count'>): string | null {
  switch (r.drain_state) {
    case 'migrating':
      return `${plural(r.attached_connectors, 'connector')} still attached. They move to other relays as soon as they get the new relay list.`
    case 'streams_open':
      return `No connectors attached; ${plural(r.connection_count, 'connection')} still open. They keep running until they end, so a long transfer can keep it here; it reads as drained once they finish.`
    case 'unverified':
      // The controller sends unverified both for a missing or stale count and
      // for one measured inside the 120 s window; an inactive relay is the
      // stale case and won't confirm by waiting.
      return r.status === 'inactive'
        ? 'Not confirmed: the relay is inactive (no recent heartbeat), so it can’t report its connections. It confirms when its heartbeats return.'
        : 'Not confirmed yet: waiting for a heartbeat measured at least 120 seconds after the drain started. Refresh in a minute.'
    case 'drained':
      return 'Drained (as of the last heartbeat): no connectors attached and no open connections.'
    default:
      return null
  }
}

// Relay detail's "Drain" card (Sprint 22 Phase D; D-30, D-31, D-32). Both
// provider roles may drain and resume, on an enrolled relay that isn't revoked
// or deleted. Hiding it is UX only; the controller authorizes every request.
// Drain asks for confirmation (optional reason, no typed name: it is
// reversible); Resume is one click. Each sends exactly one request, then
// onDone reloads the page with a notice.
export function RelayDrainCard({ relay, onDone }: { relay: RelayDetail; onDone: (notice: RelayActionNotice) => void }) {
  const role = useSessionStore((s) => s.me?.role)
  const [confirming, setConfirming] = useState(false)
  const [pending, setPending] = useState<DrainAction | null>(null)
  const [error, setError] = useState<DangerError>()
  const [resumeError, setResumeError] = useState<string>()
  // Guards a second submit before the pending re-render lands.
  const inFlight = useRef(false)

  if (!hasRole(role, PROVIDER_ROLES) || (relay.status !== 'active' && relay.status !== 'inactive')) return null
  const draining = relay.draining_since !== null
  const progress = drainProgress(relay)

  async function run(action: DrainAction, reason = '') {
    if (inFlight.current) return
    inFlight.current = true
    setPending(action)
    setError(undefined)
    setResumeError(undefined)
    try {
      if (action === 'drain') await drainRelay(relay.id, reason)
      else await resumeRelay(relay.id)
      setConfirming(false)
      onDone({
        tone: 'success',
        text:
          action === 'drain'
            ? `${relay.name} is draining: connectors are moving to other relays.`
            : `${relay.name} is back in the relay list connectors use.`,
      })
    } catch (err) {
      // An ended session is handled by the session store (sign-in page).
      if (err instanceof ApiError && err.sessionEnded) return
      const failure = drainActionFailure(err, action, relay.name)
      if (failure.refresh) {
        setConfirming(false)
        onDone({ tone: 'info', text: failure.message })
      } else if (action === 'drain') {
        setError({ message: failure.message, field: failure.field === 'reason' ? 'reason' : undefined })
      } else {
        setResumeError(failure.message)
      }
    } finally {
      inFlight.current = false
      setPending(null)
    }
  }

  return (
    <Card role="region" aria-labelledby="drain-title" className="px-5 py-5">
      <div className="flex flex-wrap items-center gap-3">
        <h2 id="drain-title" className="font-display text-base font-semibold">
          Drain
        </h2>
        {draining && <Badge variant="warning">Draining</Badge>}
      </div>
      {draining ? (
        <div className="mt-4 flex flex-col gap-4">
          <p className="text-sm" data-drain-state={relay.drain_state ?? undefined}>
            {progress}
          </p>
          {relay.drain_state === 'drained' && hasRole(role, ['super-admin']) && (
            <p className="text-sm text-subtle-foreground">It can now be revoked or deleted without cutting live connections.</p>
          )}
          {resumeError && (
            <Alert variant="destructive">
              <AlertDescription>{resumeError}</AlertDescription>
            </Alert>
          )}
          <div className="flex flex-wrap items-center justify-between gap-3">
            <p className="text-[13px] text-muted-foreground" title={formatDateTime(relay.draining_since)}>
              Draining since {formatRelative(relay.draining_since)}. Resume puts it back in the relay list.
            </p>
            <Button variant="outline" disabled={pending !== null} onClick={() => void run('resume')}>
              {pending === 'resume' ? 'Resuming…' : 'Resume relay'}
            </Button>
          </div>
        </div>
      ) : (
        <div className="mt-4 flex flex-wrap items-center justify-between gap-3">
          <p className="max-w-xl text-sm text-subtle-foreground">
            Take it out of the relay list connectors use, before maintenance. Connectors move to other relays at once;
            connections already on it keep running until they end, which for a long transfer can take a while. Fewer
            relays stay available while it drains.
          </p>
          <Button
            variant="outline"
            onClick={() => {
              setError(undefined)
              setConfirming(true)
            }}
          >
            Drain relay
          </Button>
        </div>
      )}

      {confirming && (
        <ConfirmDangerDialog
          title="Drain relay"
          description={
            <>
              <strong className="text-foreground">{relay.name}</strong> leaves the relay list connectors use. Connectors
              move to other relays at once; connections already on it keep running until they end, which for a long
              transfer can take a while. You can resume it at any time.
            </>
          }
          warning="Draining reduces relay availability: until you resume it, connectors can only use the other relays."
          confirmText={relay.name}
          confirmLabel="Drain relay"
          pendingLabel="Draining…"
          reasonRequired={false}
          requireConfirmText={false}
          pending={pending === 'drain'}
          error={error}
          onConfirm={(reason) => void run('drain', reason)}
          onCancel={() => setConfirming(false)}
        />
      )}
    </Card>
  )
}
