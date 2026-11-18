import { useEffect, useState, type FormEvent } from 'react'
import { ApiError } from '@/api/client'
import { assignTenantPlan } from '@/api/plans'
import { listPlans } from '@/api/reads'
import { PLAN_LIMIT_KEYS, type PlanSummary, type TenantDetail } from '@/api/types'
import { hasRole } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { ReasonField } from '@/components/ReasonField'
import type { RelayActionNotice } from '@/components/RelayDangerZone'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { formatLimit, PLAN_LIMIT_LABELS, planErrorMessage } from '@/lib/plan-format'
import { reasonValid } from '@/lib/pool-format'

const SUPER_ADMIN = ['super-admin'] as const

// Tenant detail's "Plan" section (Sprint 23 B1, D-43). It shows what the
// audited tenant read returned; the Change plan dialog loads the plan list
// when it opens. Assigning a plan is super-admin only (decide()); hiding the
// button is UX only. Limits are enforced by quotas (S3).
export function TenantPlanSection({ tenant, onDone }: { tenant: TenantDetail; onDone: (notice: RelayActionNotice) => void }) {
  const role = useSessionStore((s) => s.me?.role)
  const canChange = hasRole(role, SUPER_ADMIN)
  const [changing, setChanging] = useState(false)
  const plan = tenant.plan

  return (
    <Card role="region" aria-labelledby="tenant-plan-title" className="px-5 py-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 id="tenant-plan-title" className="font-semibold">
          Plan
        </h2>
        {canChange && (
          <Button variant="outline" onClick={() => setChanging(true)}>
            Change plan
          </Button>
        )}
      </div>
      <p className="mt-3 inline-flex flex-wrap items-center gap-2 text-sm">
        <span className="font-medium" data-plan-name>
          {plan.name}
        </span>
        {plan.is_default && <Badge variant="secondary">Default</Badge>}
      </p>
      <dl className="mt-4 grid grid-cols-2 gap-x-8 gap-y-3 text-sm sm:grid-cols-3 lg:grid-cols-6" aria-label="Plan limits">
        {PLAN_LIMIT_KEYS.map((k) => (
          <div key={k} className="flex flex-col gap-1">
            <dt className="text-xs font-medium tracking-wide text-muted-foreground">{PLAN_LIMIT_LABELS[k]}</dt>
            <dd className="font-mono" data-limit={k}>
              {formatLimit(plan.limits[k])}
            </dd>
          </div>
        ))}
      </dl>
      {canChange && changing && (
        <ChangePlanDialog
          tenant={tenant}
          onCancel={() => setChanging(false)}
          onDone={(n) => {
            setChanging(false)
            onDone(n)
          }}
        />
      )}
    </Card>
  )
}

type PlansState = { status: 'loading' } | { status: 'ready'; items: PlanSummary[] } | { status: 'error' }

function ChangePlanDialog({ tenant, onCancel, onDone }: { tenant: TenantDetail; onCancel: () => void; onDone: (n: RelayActionNotice) => void }) {
  const [plans, setPlans] = useState<PlansState>({ status: 'loading' })
  const [planId, setPlanId] = useState(tenant.plan.id)
  const [reason, setReason] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let live = true
    listPlans().then(
      (res) => live && setPlans({ status: 'ready', items: res.items }),
      (err: unknown) => live && !(err instanceof ApiError && err.sessionEnded) && setPlans({ status: 'error' }),
    )
    return () => {
      live = false
    }
  }, [])

  const chosen = plans.status === 'ready' ? plans.items.find((p) => p.id === planId) : undefined
  const valid = planId !== tenant.plan.id && chosen !== undefined && reasonValid(reason)

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (!valid || pending || !chosen) return
    setPending(true)
    setError(null)
    try {
      await assignTenantPlan(tenant.id, planId, reason.trim())
      onDone({ tone: 'success', text: `${tenant.name} is now on the ${chosen.name} plan. Its new limits apply at once.` })
    } catch (err) {
      setError(planErrorMessage(err))
    } finally {
      setPending(false)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && !pending && onCancel()}>
      <DialogContent>
        <form onSubmit={submit} className="grid gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>Change plan</DialogTitle>
            <DialogDescription>
              The new plan’s limits apply to <strong className="text-foreground">{tenant.name}</strong> at once. Nothing is
              deleted if the new limits are lower; the tenant can’t create more until it’s below them.
            </DialogDescription>
          </DialogHeader>
          {error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          {plans.status === 'error' && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>Couldn’t load plans. Close and try again.</AlertDescription>
            </Alert>
          )}
          <div className="grid gap-2">
            <Label htmlFor="tenant-plan-select">Plan</Label>
            <Select value={planId} onValueChange={setPlanId} disabled={plans.status !== 'ready'}>
              <SelectTrigger id="tenant-plan-select" aria-label="Plan">
                <SelectValue placeholder={plans.status === 'loading' ? 'Loading plans…' : 'Choose a plan'} />
              </SelectTrigger>
              <SelectContent>
                {plans.status === 'ready' &&
                  plans.items.map((p) => (
                    <SelectItem key={p.id} value={p.id}>
                      {p.name}
                      {p.is_default ? ' (default)' : ''}
                    </SelectItem>
                  ))}
              </SelectContent>
            </Select>
            {chosen && (
              <p className="text-xs text-muted-foreground" data-chosen-limits>
                {PLAN_LIMIT_KEYS.map((k) => `${PLAN_LIMIT_LABELS[k]} ${formatLimit(chosen.limits[k])}`).join(' · ')}
              </p>
            )}
          </div>
          <ReasonField id="tenant-plan-reason" value={reason} onChange={setReason} />
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onCancel} disabled={pending}>
              Cancel
            </Button>
            <Button type="submit" disabled={!valid || pending}>
              {pending ? 'Saving…' : 'Change plan'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
