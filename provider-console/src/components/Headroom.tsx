import { useState, type FormEvent } from 'react'
import { ApiError } from '@/api/client'
import { setRelayLinkCapacity } from '@/api/relays'
import type { RelaySummary } from '@/api/types'
import type { RelayActionNotice } from '@/components/RelayDangerZone'
import { ReasonField } from '@/components/ReasonField'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { formatLinkSpeed, formatUtilization, linkSpeedInput, NEAR_CAPACITY, parseLinkSpeed, type LinkUnit } from '@/lib/capacity'
import { formatRate } from '@/lib/format'
import { reasonValid } from '@/lib/pool-format'

// Relay headroom (Sprint 23 B2, F5; D-42 refined 2026-11-18). Shown as the
// API reports it: the operator-set link capacity and the headroom the
// controller derives (capacity − (to clients + to connectors): both flows
// share the relay's link). Nothing is computed here. No capacity reads
// "Not configured", never zero or unlimited; headroom is an estimate over the
// bandwidth window.

type Relay = Pick<RelaySummary, 'link_capacity_bps' | 'headroom' | 'bandwidth' | 'status'>

export const HEADROOM_NOTE =
  'An estimate: the link capacity an operator entered minus the average traffic over the bandwidth window (both directions share the link). Short bursts can exceed it.'

/** Compact cell for the relay list. */
export function HeadroomCell({ relay }: { relay: Relay }) {
  if (relay.link_capacity_bps === null) return <span className="text-muted-foreground">Not configured</span>
  const h = relay.headroom
  if (!h) {
    return (
      <span className="text-muted-foreground" title={`Link ${formatLinkSpeed(relay.link_capacity_bps)}; no measured rate yet`}>
        {formatLinkSpeed(relay.link_capacity_bps)} link
      </span>
    )
  }
  return (
    <div className="flex flex-col whitespace-nowrap font-mono text-xs tabular-nums" aria-label="Headroom" title={HEADROOM_NOTE}>
      <span className="inline-flex items-center gap-1.5">
        {formatRate(h.headroom_bytes_per_sec)} free
        {h.over_capacity ? (
          <Badge variant="destructive">Over capacity</Badge>
        ) : (
          h.utilization >= NEAR_CAPACITY && <Badge variant="warning">{formatUtilization(h.utilization)}</Badge>
        )}
      </span>
      <span className="font-sans text-muted-foreground">of {formatLinkSpeed(relay.link_capacity_bps)}</span>
    </div>
  )
}

/** Detail-page block: rate, capacity and headroom, each on its own. */
export function HeadroomDetail({ relay }: { relay: Relay }) {
  if (relay.link_capacity_bps === null) {
    return (
      <p className="text-[13px] text-muted-foreground">
        Not configured. Enter the relay’s usable link speed to see how much more traffic it can carry.
      </p>
    )
  }
  const h = relay.headroom
  return (
    <div className="flex flex-col gap-2.5">
      <dl className="grid grid-cols-2 gap-3 text-[13px] sm:grid-cols-4">
        <div className="flex flex-col gap-0.5">
          <dt className="text-xs text-muted-foreground">Link capacity</dt>
          <dd className="font-mono tabular-nums" data-capacity>
            {formatLinkSpeed(relay.link_capacity_bps)}
          </dd>
        </div>
        <div className="flex flex-col gap-0.5">
          <dt className="text-xs text-muted-foreground">In use (both directions)</dt>
          <dd className="font-mono tabular-nums">{h ? formatRate(h.used_bytes_per_sec) : '—'}</dd>
        </div>
        <div className="flex flex-col gap-0.5">
          <dt className="text-xs text-muted-foreground">Headroom</dt>
          <dd className="font-mono tabular-nums" data-headroom>
            {h ? formatRate(h.headroom_bytes_per_sec) : '—'}
          </dd>
        </div>
        <div className="flex flex-col gap-0.5">
          <dt className="text-xs text-muted-foreground">Utilization</dt>
          <dd className="inline-flex items-center gap-2 font-mono tabular-nums">
            {h ? formatUtilization(h.utilization) : '—'}
            {h?.over_capacity && <Badge variant="destructive">Over capacity</Badge>}
          </dd>
        </div>
      </dl>
      <p className="text-[13px] text-subtle-foreground">
        {h ? HEADROOM_NOTE : 'No measured rate yet (not reported, or measuring), so no headroom is shown.'}
      </p>
    </div>
  )
}

const ERRORS: Record<string, string> = {
  invalid_capacity: 'Enter a link speed above 0 and up to 400 Gbps.',
  reason_required: 'Enter a reason.',
  reason_too_long: 'Keep the reason to 500 characters or fewer.',
  already_revoked: 'This relay is revoked.',
  already_deleted: 'This relay is deleted.',
  forbidden: 'Only relay operators and super-admins can set the link capacity.',
  not_found: 'This relay no longer exists.',
}

export function SetLinkCapacityDialog({
  relay,
  onCancel,
  onDone,
}: {
  relay: Pick<RelaySummary, 'id' | 'name' | 'link_capacity_bps'>
  onCancel: () => void
  onDone: (text: string) => void
}) {
  const initial = linkSpeedInput(relay.link_capacity_bps)
  const [value, setValue] = useState(initial.value)
  const [unit, setUnit] = useState<LinkUnit>(initial.unit)
  const [reason, setReason] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const bps = parseLinkSpeed(value, unit)
  const valid = bps !== null && bps !== relay.link_capacity_bps && reasonValid(reason)

  async function send(next: number | null) {
    setPending(true)
    setError(null)
    try {
      await setRelayLinkCapacity(relay.id, next, reason.trim())
      onDone(next === null ? `Link capacity of ${relay.name} cleared.` : `Link capacity of ${relay.name} set to ${formatLinkSpeed(next)}.`)
    } catch (err) {
      setError(err instanceof ApiError ? (ERRORS[err.code] ?? 'Something went wrong. Try again.') : 'Couldn’t reach the controller. Try again.')
    } finally {
      setPending(false)
    }
  }

  function submit(e: FormEvent) {
    e.preventDefault()
    if (valid && !pending) void send(bps)
  }

  return (
    <Dialog open onOpenChange={(o) => !o && !pending && onCancel()}>
      <DialogContent>
        <form onSubmit={submit} className="grid gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>Set link capacity</DialogTitle>
            <DialogDescription>
              The usable speed of <strong className="text-foreground">{relay.name}</strong>’s network link. A relay sends as much as it
              receives, so enter the <strong className="text-foreground">slower</strong> direction (on an asymmetric link, usually the
              upload speed). Headroom is this minus the measured traffic.
            </DialogDescription>
          </DialogHeader>
          {error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          <div className="grid grid-cols-[1fr_7rem] items-end gap-3">
            <div className="grid gap-2">
              <Label htmlFor="link-capacity">Link speed</Label>
              <Input
                id="link-capacity"
                inputMode="decimal"
                value={value}
                autoComplete="off"
                aria-invalid={value.trim() !== '' && bps === null}
                onChange={(e) => setValue(e.target.value)}
              />
            </div>
            <Select value={unit} onValueChange={(u) => setUnit(u as LinkUnit)}>
              <SelectTrigger aria-label="Unit">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="Mbps">Mbps</SelectItem>
                <SelectItem value="Gbps">Gbps</SelectItem>
              </SelectContent>
            </Select>
          </div>
          <ReasonField id="link-capacity-reason" value={reason} onChange={setReason} />
          <DialogFooter>
            {relay.link_capacity_bps !== null && (
              <Button
                type="button"
                variant="ghost"
                className="mr-auto"
                disabled={pending || !reasonValid(reason)}
                onClick={() => void send(null)}
              >
                Clear capacity
              </Button>
            )}
            <Button type="button" variant="ghost" onClick={onCancel} disabled={pending}>
              Cancel
            </Button>
            <Button type="submit" disabled={!valid || pending}>
              {pending ? 'Saving…' : 'Save'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

/**
 * Relay detail card: capacity, use and headroom, and "Set link capacity"
 * (relay-ops and super-admin; the controller decides). Hidden actions for
 * revoked and deleted relays, which the controller refuses.
 */
export function RelayCapacityCard({
  relay,
  onDone,
}: {
  relay: Pick<RelaySummary, 'id' | 'name' | 'status' | 'link_capacity_bps' | 'headroom' | 'bandwidth'>
  onDone: (notice: RelayActionNotice) => void
}) {
  const [editing, setEditing] = useState(false)
  const terminal = relay.status === 'revoked' || relay.status === 'deleted'
  return (
    <Card role="region" className="flex flex-[1_1_300px] flex-col gap-3.5 px-5 py-5" aria-labelledby="relay-capacity">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 id="relay-capacity" className="font-semibold">
          Link capacity
        </h2>
        {!terminal && (
          <Button variant="outline" size="sm" onClick={() => setEditing(true)}>
            Set link capacity
          </Button>
        )}
      </div>
      <HeadroomDetail relay={relay} />
      {editing && (
        <SetLinkCapacityDialog
          relay={relay}
          onCancel={() => setEditing(false)}
          onDone={(text) => {
            setEditing(false)
            onDone({ tone: 'success', text })
          }}
        />
      )}
    </Card>
  )
}
