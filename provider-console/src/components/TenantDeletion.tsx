import { useRef, useState } from 'react'
import { ApiError } from '@/api/client'
import { cancelTenantDelete, scheduleTenantDelete, setTenantLegalHold } from '@/api/tenants'
import type { PurgeStep, TenantDetail } from '@/api/types'
import { hasRole } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { ConfirmDangerDialog, type DangerError } from '@/components/ConfirmDangerDialog'
import type { RelayActionNotice } from '@/components/RelayDangerZone'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { formatDateTime } from '@/lib/format'
import { tenantLifecycleFailure, type TenantLifecycleAction } from '@/lib/tenant-errors'

const SUPER_ADMIN = ['super-admin'] as const

const PURGE_STEPS: Record<PurgeStep, string> = {
  pending: 'Starting: exporting the tenant’s logs',
  exported: 'Logs exported and verified; revoking access',
  revoked: 'Access revoked; destroying tenant data',
  destroyed: 'Data destroyed; writing the tombstone',
  done: 'Deleted',
}

type Action = 'delete' | 'cancel' | 'hold' | 'release'

function daysUntil(iso: string): number {
  return Math.max(0, Math.ceil((new Date(iso).getTime() - Date.now()) / 86_400_000))
}

// Tenant detail's "Deletion" section (Sprint 23 S4, D-46, D-17 extended).
// Status only: the tenant's exported logs go to the tenant, never to the
// provider. Delete needs a suspended tenant, a reason and the slug typed back;
// cancel and the legal hold need a reason. All super-admin only (decide());
// hiding the buttons is UX only.
export function TenantDeletionSection({ tenant, onDone }: { tenant: TenantDetail; onDone: (notice: RelayActionNotice) => void }) {
  const role = useSessionStore((s) => s.me?.role)
  const canAct = hasRole(role, SUPER_ADMIN)
  const [action, setAction] = useState<Action | null>(null)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<DangerError>()
  const inFlight = useRef(false)
  const d = tenant.deletion
  const scheduled = !!d?.delete_after
  const started = !!d?.purge_step
  const suspended = !!tenant.suspension

  function open(next: Action) {
    setError(undefined)
    setAction(next)
  }

  async function run(reason: string, typedSlug: string) {
    if (!action || inFlight.current) return
    inFlight.current = true
    setPending(true)
    setError(undefined)
    const label: TenantLifecycleAction = action === 'delete' ? 'delete' : action === 'cancel' ? 'cancel the deletion' : 'legal hold change'
    try {
      let text: string
      if (action === 'delete') {
        const res = await scheduleTenantDelete(tenant.id, reason, typedSlug)
        text = `${tenant.name} will be deleted on ${formatDateTime(res.delete_after)}. Its admins were emailed a link to download their logs until then.`
      } else if (action === 'cancel') {
        await cancelTenantDelete(tenant.id, reason)
        text = `${tenant.name}’s deletion is cancelled. It stays suspended; resume it to restore access.`
      } else {
        await setTenantLegalHold(tenant.id, action === 'hold', reason)
        text = action === 'hold' ? `${tenant.name} is on legal hold: its purge is paused.` : `${tenant.name}’s legal hold is released.`
      }
      setAction(null)
      onDone({ tone: 'success', text })
    } catch (err) {
      if (err instanceof ApiError && err.sessionEnded) return
      const failure = tenantLifecycleFailure(err, label, tenant.name)
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
    <Card role="region" aria-labelledby="tenant-deletion-title" className={scheduled ? 'border-destructive/40 px-5 py-5' : 'px-5 py-5'}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 id="tenant-deletion-title" className="flex items-center gap-2 font-semibold">
          Deletion
          {scheduled && <Badge variant="destructive">Scheduled</Badge>}
          {d?.legal_hold && <Badge variant="secondary">Legal hold</Badge>}
        </h2>
        {canAct && (
          <div className="flex flex-wrap gap-2">
            {scheduled && !started && (
              <Button variant="outline" onClick={() => open('cancel')}>
                Cancel deletion
              </Button>
            )}
            {d?.legal_hold ? (
              <Button variant="outline" onClick={() => open('release')}>
                Release legal hold
              </Button>
            ) : (
              <Button variant="outline" onClick={() => open('hold')}>
                Place legal hold
              </Button>
            )}
            {!scheduled && (
              <Button
                variant="outline"
                className="border-destructive/50 text-destructive"
                disabled={!suspended}
                title={suspended ? undefined : 'Only a suspended tenant can be deleted'}
                onClick={() => open('delete')}
              >
                Delete tenant
              </Button>
            )}
          </div>
        )}
      </div>
      {scheduled ? (
        <dl aria-label="Deletion details" className="mt-4 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1.5 text-sm">
          <dt className="text-muted-foreground">Deletes on</dt>
          <dd data-delete-after>
            {formatDateTime(d!.delete_after!)}
            {!started && <span className="text-muted-foreground"> (in {daysUntil(d!.delete_after!)} days)</span>}
          </dd>
          <dt className="text-muted-foreground">Scheduled by</dt>
          <dd>{d!.by}</dd>
          <dt className="text-muted-foreground">Reason</dt>
          <dd className="whitespace-pre-wrap break-words">{d!.reason}</dd>
          {started && (
            <>
              <dt className="text-muted-foreground">Purge</dt>
              <dd data-purge-step={d!.purge_step}>{PURGE_STEPS[d!.purge_step!]}</dd>
            </>
          )}
          {d!.legal_hold && (
            <>
              <dt className="text-muted-foreground">Legal hold</dt>
              <dd className="whitespace-pre-wrap break-words">Purge paused. {d!.legal_hold_reason}</dd>
            </>
          )}
        </dl>
      ) : (
        <p className="mt-2 text-sm text-muted-foreground">
          {suspended ? 'Not scheduled.' : 'Not scheduled. Only a suspended tenant can be deleted.'}
          {d?.legal_hold && ` Legal hold: ${d.legal_hold_reason}`}
        </p>
      )}

      {action === 'delete' && (
        <ConfirmDangerDialog
          title="Delete tenant"
          description={
            <>
              <strong className="text-foreground">{tenant.name}</strong> is deleted after a 30-day grace: its logs are
              exported to the tenant’s own storage, then everything (users, agents, keys and data) is destroyed and can’t be
              recovered. Until then you can cancel, and the tenant’s admins can download their logs.
            </>
          }
          confirmText={tenant.slug}
          confirmLabel="Schedule deletion"
          pendingLabel="Scheduling…"
          pending={pending}
          error={error}
          onConfirm={(reason, typed) => void run(reason, typed)}
          onCancel={() => setAction(null)}
        />
      )}
      {(action === 'cancel' || action === 'hold' || action === 'release') && (
        <ConfirmDangerDialog
          title={action === 'cancel' ? 'Cancel deletion' : action === 'hold' ? 'Place legal hold' : 'Release legal hold'}
          description={
            action === 'cancel' ? (
              <>The deletion of <strong className="text-foreground">{tenant.name}</strong> is cancelled; it stays suspended.</>
            ) : action === 'hold' ? (
              <>The purge of <strong className="text-foreground">{tenant.name}</strong> is paused until the hold is released.</>
            ) : (
              <>The purge of <strong className="text-foreground">{tenant.name}</strong> can continue once its grace has passed.</>
            )
          }
          confirmText=""
          requireConfirmText={false}
          confirmLabel={action === 'cancel' ? 'Cancel deletion' : action === 'hold' ? 'Place legal hold' : 'Release legal hold'}
          pendingLabel="Saving…"
          pending={pending}
          error={error}
          onConfirm={(reason) => void run(reason, '')}
          onCancel={() => setAction(null)}
        />
      )}
    </Card>
  )
}
