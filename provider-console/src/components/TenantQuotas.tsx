import { useRef, useState, type FormEvent } from 'react'
import { ApiError } from '@/api/client'
import { inheritTenantQuota, setTenantQuota } from '@/api/quotas'
import { ErrorCode, type TenantDetail, type TenantQuota } from '@/api/types'
import { hasRole } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { ReasonField } from '@/components/ReasonField'
import type { RelayActionNotice } from '@/components/RelayDangerZone'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatLimit, PLAN_LIMIT_LABELS } from '@/lib/plan-format'
import { reasonValid } from '@/lib/pool-format'

const SUPER_ADMIN = ['super-admin'] as const

const SOURCE_LABELS: Record<TenantQuota['source'], string> = {
  plan: 'Plan',
  override: 'Override',
  unlimited: 'Plan (unlimited)',
}

// Tenant detail's "Quotas" section (Sprint 23 S3, D-47). It shows what the
// audited tenant read returned: each key's effective limit, where it comes
// from, the usage the controller counts and an "Over quota" badge when a
// lowered limit is below usage (nothing is deleted; the tenant can't create
// more). Setting an override is super-admin only (decide()); hiding the
// button is UX only. Each save sends one request and reloads the page.
export function TenantQuotasSection({ tenant, onDone }: { tenant: TenantDetail; onDone: (notice: RelayActionNotice) => void }) {
  const role = useSessionStore((s) => s.me?.role)
  const canChange = hasRole(role, SUPER_ADMIN)
  const [editing, setEditing] = useState<TenantQuota | null>(null)

  return (
    <Card role="region" aria-labelledby="tenant-quotas-title" className="px-5 py-5">
      <h2 id="tenant-quotas-title" className="font-semibold">
        Quotas
      </h2>
      <p className="mt-1 text-sm text-muted-foreground">
        Hard limits, enforced when something is created. Lowering a limit never deletes anything.
      </p>
      <Table aria-label="Quotas" className="mt-3">
        <TableHeader>
          <TableRow className="hover:bg-transparent">
            <TableHead>Resource</TableHead>
            <TableHead className="text-right">Used</TableHead>
            <TableHead className="text-right">Limit</TableHead>
            <TableHead>Source</TableHead>
            {canChange && <TableHead className="sr-only">Actions</TableHead>}
          </TableRow>
        </TableHeader>
        <TableBody>
          {tenant.quotas.map((q) => (
            <TableRow key={q.key} aria-label={PLAN_LIMIT_LABELS[q.key]} data-quota={q.key}>
              <TableCell className="font-medium">
                <span className="inline-flex items-center gap-2">
                  {PLAN_LIMIT_LABELS[q.key]}
                  {q.over && (
                    <Badge variant="destructive" data-over>
                      Over quota
                    </Badge>
                  )}
                </span>
              </TableCell>
              <TableCell className="text-right font-mono tabular-nums">{q.used.toLocaleString()}</TableCell>
              <TableCell className="text-right font-mono tabular-nums">{formatLimit(q.limit)}</TableCell>
              <TableCell>{SOURCE_LABELS[q.source]}</TableCell>
              {canChange && (
                <TableCell className="text-right">
                  <Button variant="outline" size="sm" onClick={() => setEditing(q)} aria-label={`Change ${PLAN_LIMIT_LABELS[q.key]} limit`}>
                    Change
                  </Button>
                </TableCell>
              )}
            </TableRow>
          ))}
        </TableBody>
      </Table>
      {canChange && editing && (
        <QuotaDialog
          tenant={tenant}
          quota={editing}
          onCancel={() => setEditing(null)}
          onDone={(n) => {
            setEditing(null)
            onDone(n)
          }}
        />
      )}
    </Card>
  )
}

type Mode = 'plan' | 'limit' | 'unlimited'

const QUOTA_ERRORS: Record<string, string> = {
  [ErrorCode.reasonRequired]: 'Enter a reason. It is recorded in the provider audit log.',
  [ErrorCode.reasonTooLong]: 'Keep the reason to 500 characters or fewer.',
  [ErrorCode.invalidLimit]: 'Enter a whole number of 0 or more.',
  [ErrorCode.limitRequired]: 'Enter a limit, or choose unlimited.',
  [ErrorCode.unknownKey]: 'This quota no longer exists. Reload the page.',
  [ErrorCode.forbidden]: 'Only super-admins can change quotas.',
  [ErrorCode.notFound]: 'This tenant no longer exists.',
}

function QuotaDialog({
  tenant,
  quota,
  onCancel,
  onDone,
}: {
  tenant: TenantDetail
  quota: TenantQuota
  onCancel: () => void
  onDone: (n: RelayActionNotice) => void
}) {
  const label = PLAN_LIMIT_LABELS[quota.key]
  const planValue = tenant.plan.limits[quota.key]
  const [mode, setMode] = useState<Mode>(quota.source === 'override' ? (quota.limit === null ? 'unlimited' : 'limit') : 'plan')
  const [value, setValue] = useState(quota.source === 'override' && quota.limit !== null ? String(quota.limit) : '')
  const [reason, setReason] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const inFlight = useRef(false)

  const number = /^\d{1,15}$/.test(value.trim()) ? Number(value.trim()) : null
  const valid = reasonValid(reason) && (mode !== 'limit' || number !== null)
  const lowering = mode === 'limit' && number !== null && number < quota.used

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (!valid || pending || inFlight.current) return
    inFlight.current = true
    setPending(true)
    setError(null)
    try {
      if (mode === 'plan') await inheritTenantQuota(tenant.id, quota.key, reason.trim())
      else await setTenantQuota(tenant.id, quota.key, mode === 'unlimited' ? null : number, reason.trim())
      const now = mode === 'plan' ? `the plan’s value (${formatLimit(planValue)})` : mode === 'unlimited' ? 'unlimited' : number!.toLocaleString()
      onDone({ tone: 'success', text: `${tenant.name}: the ${label.toLowerCase()} limit is now ${now}.` })
    } catch (err) {
      if (err instanceof ApiError && err.sessionEnded) return
      setError(err instanceof ApiError ? (QUOTA_ERRORS[err.code] ?? 'The change didn’t go through. Try again.') : 'Can’t reach the controller. Try again.')
    } finally {
      inFlight.current = false
      setPending(false)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && !pending && onCancel()}>
      <DialogContent>
        <form onSubmit={submit} className="grid gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>{`Change ${label.toLowerCase()} limit`}</DialogTitle>
            <DialogDescription>
              <strong className="text-foreground">{tenant.name}</strong> uses {quota.used.toLocaleString()}. The plan allows{' '}
              {formatLimit(planValue)}. The new limit applies at once to new creations; nothing is deleted.
            </DialogDescription>
          </DialogHeader>
          {error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          <fieldset className="grid gap-2">
            <legend className="sr-only">Limit choice</legend>
            {(
              [
                ['plan', `Use the plan’s value (${formatLimit(planValue)})`],
                ['limit', 'Set a limit for this tenant'],
                ['unlimited', 'Unlimited for this tenant'],
              ] as const
            ).map(([m, text]) => (
              <label key={m} className="flex items-center gap-2 text-sm">
                <input type="radio" name="quota-mode" value={m} checked={mode === m} onChange={() => setMode(m)} />
                {text}
              </label>
            ))}
          </fieldset>
          {mode === 'limit' && (
            <div className="grid gap-2">
              <Label htmlFor="quota-limit">New limit</Label>
              <Input id="quota-limit" inputMode="numeric" autoComplete="off" value={value} onChange={(e) => setValue(e.target.value)} />
              {lowering && (
                <p className="text-xs text-warning-text" role="note">
                  Below current usage ({quota.used.toLocaleString()}): the tenant will be over quota and can’t create more until
                  it is below the limit. Nothing is deleted.
                </p>
              )}
            </div>
          )}
          <ReasonField id="quota-reason" value={reason} onChange={setReason} />
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onCancel} disabled={pending}>
              Cancel
            </Button>
            <Button type="submit" disabled={!valid || pending}>
              {pending ? 'Saving…' : 'Save limit'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
