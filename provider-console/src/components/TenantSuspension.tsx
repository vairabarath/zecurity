import { useRef, useState, type FormEvent } from 'react'
import { ApiError } from '@/api/client'
import { resumeTenant, suspendTenant } from '@/api/tenants'
import type { TenantDetail } from '@/api/types'
import { can } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { ConfirmDangerDialog, type DangerError } from '@/components/ConfirmDangerDialog'
import { ReasonField } from '@/components/ReasonField'
import type { RelayActionNotice } from '@/components/RelayDangerZone'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { formatDateTime } from '@/lib/format'
import { reasonValid } from '@/lib/pool-format'
import { tenantLifecycleFailure, type TenantLifecycleAction } from '@/lib/tenant-errors'


/** The "Suspended" badge for a tenant suspended by the provider (status stays "active"). */
export function SuspendedBadge() {
  return (
    <Badge variant="destructive" data-suspended>
      Suspended
    </Badge>
  )
}

// Tenant detail's "Suspension" section (Sprint 23 S1, D-45, D-17 extended).
// It shows the suspension the audited tenant read already returned. Suspend
// needs a reason and the tenant's slug typed back; Resume needs a reason.
// Both are super-admin only (decide()); hiding the buttons is UX only. Each
// confirm sends exactly one request; when it ends, onDone reloads the page
// with a notice.
export function TenantSuspensionSection({ tenant, onDone }: { tenant: TenantDetail; onDone: (notice: RelayActionNotice) => void }) {
  const role = useSessionStore((s) => s.me?.role)
  const [action, setAction] = useState<TenantLifecycleAction | null>(null)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<DangerError>()
  // Guards a second submit before the pending re-render lands.
  const inFlight = useRef(false)
  const s = tenant.suspension
  const canAct = can(role, 'tenant.suspend')

  function open(next: TenantLifecycleAction) {
    setError(undefined)
    setAction(next)
  }

  async function run(reason: string, typedSlug: string) {
    if (!action || inFlight.current) return
    inFlight.current = true
    setPending(true)
    setError(undefined)
    try {
      if (action === 'suspend') await suspendTenant(tenant.id, reason, typedSlug)
      else await resumeTenant(tenant.id, reason)
      setAction(null)
      onDone({
        tone: 'success',
        text:
          action === 'suspend'
            ? `${tenant.name} is suspended. Its users can’t sign in and their sessions have ended; its connectors and shields stay connected with a deny-all policy.`
            : `${tenant.name} is resumed. Its connectors and shields get the normal policy back; users sign in again.`,
      })
    } catch (err) {
      // An ended session is handled by the session store (sign-in page).
      if (err instanceof ApiError && err.sessionEnded) return
      const failure = tenantLifecycleFailure(err, action, tenant.name)
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
    <Card role="region" aria-labelledby="tenant-suspension-title" className={s ? 'border-destructive/40 px-5 py-5' : 'px-5 py-5'}>
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 id="tenant-suspension-title" className="flex items-center gap-2 font-semibold">
          Suspension
          {s && <SuspendedBadge />}
        </h2>
        {canAct &&
          (s ? (
            <Button variant="outline" onClick={() => open('resume')}>
              Resume tenant
            </Button>
          ) : (
            <Button variant="outline" className="border-destructive/50 text-destructive" onClick={() => open('suspend')}>
              Suspend tenant
            </Button>
          ))}
      </div>
      {s ? (
        <dl aria-label="Suspension details" className="mt-4 grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1.5 text-sm">
          <dt className="text-muted-foreground">Since</dt>
          <dd>{formatDateTime(s.suspended_at)}</dd>
          <dt className="text-muted-foreground">By</dt>
          <dd>{s.by}</dd>
          <dt className="text-muted-foreground">Reason</dt>
          <dd className="whitespace-pre-wrap break-words">{s.reason}</dd>
        </dl>
      ) : (
        <p className="mt-2 text-sm text-muted-foreground">Not suspended.</p>
      )}

      {action === 'suspend' && (
        <ConfirmDangerDialog
          title="Suspend tenant"
          description={
            <>
              <strong className="text-foreground">{tenant.name}</strong>: admin and client sign-in is refused and every
              session ends now. Connectors and shields stay connected with a deny-all policy. You can resume it later.
            </>
          }
          confirmText={tenant.slug}
          confirmLabel="Suspend tenant"
          pendingLabel="Suspending…"
          pending={pending}
          error={error}
          onConfirm={(reason, typed) => void run(reason, typed)}
          onCancel={() => setAction(null)}
        />
      )}
      {action === 'resume' && (
        <ResumeDialog
          tenantName={tenant.name}
          pending={pending}
          error={error}
          onConfirm={(reason) => void run(reason, '')}
          onCancel={() => setAction(null)}
        />
      )}
    </Card>
  )
}

function ResumeDialog({
  tenantName,
  pending,
  error,
  onConfirm,
  onCancel,
}: {
  tenantName: string
  pending: boolean
  error?: DangerError
  onConfirm: (reason: string) => void
  onCancel: () => void
}) {
  const [reason, setReason] = useState('')
  const valid = reasonValid(reason)
  function submit(e: FormEvent) {
    e.preventDefault()
    if (valid && !pending) onConfirm(reason.trim())
  }
  return (
    <Dialog open onOpenChange={(o) => !o && !pending && onCancel()}>
      <DialogContent>
        <form onSubmit={submit} className="grid gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>Resume tenant</DialogTitle>
            <DialogDescription>
              <strong className="text-foreground">{tenantName}</strong> gets its normal policy and relays back; agents need no
              re-enrollment. Users sign in again.
            </DialogDescription>
          </DialogHeader>
          {error && !error.field && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error.message}</AlertDescription>
            </Alert>
          )}
          <ReasonField id="resume-reason" value={reason} onChange={setReason} error={error?.field === 'reason' ? error.message : undefined} />
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onCancel} disabled={pending}>
              Cancel
            </Button>
            <Button type="submit" disabled={!valid || pending}>
              {pending ? 'Resuming…' : 'Resume tenant'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
