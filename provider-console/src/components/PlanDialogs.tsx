import { useState, type FormEvent, type ReactNode } from 'react'
import { createPlan, deletePlan, setDefaultPlan, updatePlan } from '@/api/plans'
import { PLAN_LIMIT_KEYS, type CreatePlanResponse, type PlanLimits, type PlanSummary } from '@/api/types'
import { ReasonField } from '@/components/ReasonField'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  limitInputs,
  parseLimitInputs,
  PLAN_LIMIT_LABELS,
  sameLimits,
  type LimitInputs,
} from '@/lib/plan-format'
import { reasonValid } from '@/lib/pool-format'
import { useWrite } from '@/lib/useWrite'

// Plan dialogs (Sprint 23 B1, D-43). Super-admin only; the controller
// decides. Every limit is a whole number or "Unlimited" (sent as null): a
// plan always defines all six keys.

function PlanDialog({
  title,
  description,
  children,
  error,
  pending,
  valid,
  submitLabel,
  pendingLabel,
  destructive,
  onSubmit,
  onCancel,
}: {
  title: string
  description: ReactNode
  children?: ReactNode
  error: string | null
  pending: boolean
  valid: boolean
  submitLabel: string
  pendingLabel: string
  destructive?: boolean
  onSubmit: () => void
  onCancel: () => void
}) {
  function submit(e: FormEvent) {
    e.preventDefault()
    if (valid && !pending) onSubmit()
  }
  return (
    <Dialog open onOpenChange={(o) => !o && !pending && onCancel()}>
      <DialogContent>
        <form onSubmit={submit} className="grid gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
            <DialogDescription>{description}</DialogDescription>
          </DialogHeader>
          {error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          {children}
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onCancel} disabled={pending}>
              Cancel
            </Button>
            <Button type="submit" variant={destructive ? 'destructive' : 'default'} disabled={!valid || pending}>
              {pending ? pendingLabel : submitLabel}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

export function LimitsFields({ value, onChange }: { value: LimitInputs; onChange: (v: LimitInputs) => void }) {
  return (
    <fieldset className="grid gap-3">
      <legend className="mb-1 text-sm font-medium">Limits</legend>
      <p className="text-xs text-muted-foreground">
        Tenant-wide. A tenant at a limit can’t create more; lowering a limit never deletes anything.
      </p>
      {PLAN_LIMIT_KEYS.map((k) => {
        const id = `limit-${k}`
        const v = value[k]
        return (
          <div key={k} className="grid grid-cols-[1fr_7rem_auto] items-center gap-3">
            <Label htmlFor={id}>{PLAN_LIMIT_LABELS[k]}</Label>
            <Input
              id={id}
              inputMode="numeric"
              value={v.unlimited ? '' : v.value}
              disabled={v.unlimited}
              placeholder={v.unlimited ? 'Unlimited' : '0'}
              aria-invalid={!v.unlimited && !/^\d{1,9}$/.test(v.value.trim())}
              onChange={(e) => onChange({ ...value, [k]: { unlimited: false, value: e.target.value } })}
            />
            <label className="inline-flex items-center gap-2 text-sm">
              <input
                type="checkbox"
                checked={v.unlimited}
                onChange={(e) => onChange({ ...value, [k]: { unlimited: e.target.checked, value: v.value } })}
                aria-label={`${PLAN_LIMIT_LABELS[k]} unlimited`}
              />
              Unlimited
            </label>
          </div>
        )
      })}
    </fieldset>
  )
}

export function CreatePlanDialog({ onCancel, onCreated }: { onCancel: () => void; onCreated: (p: CreatePlanResponse) => void }) {
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [limits, setLimits] = useState<LimitInputs>(limitInputs())
  const { pending, error, run } = useWrite()
  const parsed = parseLimitInputs(limits)
  const valid = name.trim().length > 0 && name.trim().length <= 64 && description.length <= 500 && parsed !== null
  return (
    <PlanDialog
      title="Create plan"
      description="A new plan with its resource limits. It isn’t the default until you make it so; assign it to tenants from their tenant page."
      error={error}
      pending={pending}
      valid={valid}
      submitLabel="Create plan"
      pendingLabel="Creating…"
      onCancel={onCancel}
      onSubmit={() =>
        void run(async () => {
          onCreated(await createPlan(name.trim(), description.trim(), parsed!))
        })
      }
    >
      <PlanTextFields name={name} description={description} onName={setName} onDescription={setDescription} />
      <LimitsFields value={limits} onChange={setLimits} />
    </PlanDialog>
  )
}

export function EditPlanDialog({ plan, onCancel, onDone }: { plan: PlanSummary; onCancel: () => void; onDone: (text: string) => void }) {
  const [name, setName] = useState(plan.name)
  const [description, setDescription] = useState(plan.description)
  const [limits, setLimits] = useState<LimitInputs>(limitInputs(plan.limits))
  const [reason, setReason] = useState('')
  const { pending, error, run } = useWrite()
  const parsed = parseLimitInputs(limits)
  const changed =
    name.trim() !== plan.name || description.trim() !== plan.description || (parsed !== null && !sameLimits(parsed, plan.limits))
  const valid = name.trim().length > 0 && name.trim().length <= 64 && parsed !== null && changed && reasonValid(reason)
  return (
    <PlanDialog
      title="Edit plan"
      description={
        <>
          Changes to <strong className="text-foreground">{plan.name}</strong> apply at once to its{' '}
          {plan.tenants === 1 ? '1 tenant' : `${plan.tenants.toLocaleString()} tenants`}. Lowering a limit never deletes
          anything; tenants above it can’t create more until they’re below it.
        </>
      }
      error={error}
      pending={pending}
      valid={valid}
      submitLabel="Save plan"
      pendingLabel="Saving…"
      onCancel={onCancel}
      onSubmit={() =>
        void run(async () => {
          const change: { name?: string; description?: string; limits?: PlanLimits } = {}
          if (name.trim() !== plan.name) change.name = name.trim()
          if (description.trim() !== plan.description) change.description = description.trim()
          if (parsed && !sameLimits(parsed, plan.limits)) change.limits = parsed
          await updatePlan(plan.id, change, reason.trim())
          onDone(`Plan ${name.trim()} saved.`)
        })
      }
    >
      <PlanTextFields name={name} description={description} onName={setName} onDescription={setDescription} />
      <LimitsFields value={limits} onChange={setLimits} />
      <ReasonField id="plan-edit-reason" value={reason} onChange={setReason} />
    </PlanDialog>
  )
}

export function MakeDefaultPlanDialog({ plan, onCancel, onDone }: { plan: PlanSummary; onCancel: () => void; onDone: (text: string) => void }) {
  const [reason, setReason] = useState('')
  const { pending, error, run } = useWrite()
  return (
    <PlanDialog
      title="Make default plan"
      description={
        <>
          New workspaces (self-signup and provider-created) will start on <strong className="text-foreground">{plan.name}</strong>.
          Existing tenants keep their plan.
        </>
      }
      error={error}
      pending={pending}
      valid={reasonValid(reason)}
      submitLabel="Make default"
      pendingLabel="Saving…"
      onCancel={onCancel}
      onSubmit={() =>
        void run(async () => {
          await setDefaultPlan(plan.id, reason.trim())
          onDone(`${plan.name} is now the default plan.`)
        })
      }
    >
      <ReasonField id="plan-default-reason" value={reason} onChange={setReason} />
    </PlanDialog>
  )
}

export function DeletePlanDialog({ plan, onCancel, onDone }: { plan: PlanSummary; onCancel: () => void; onDone: (text: string) => void }) {
  const [reason, setReason] = useState('')
  const { pending, error, run } = useWrite()
  return (
    <PlanDialog
      title="Delete plan"
      description={
        <>
          Deletes <strong className="text-foreground">{plan.name}</strong>. Only a plan that no tenant is on and that isn’t the
          default can be deleted.
        </>
      }
      error={error}
      pending={pending}
      valid={reasonValid(reason)}
      destructive
      submitLabel="Delete plan"
      pendingLabel="Deleting…"
      onCancel={onCancel}
      onSubmit={() =>
        void run(async () => {
          await deletePlan(plan.id, reason.trim())
          onDone(`Plan ${plan.name} deleted.`)
        })
      }
    >
      <ReasonField id="plan-delete-reason" value={reason} onChange={setReason} />
    </PlanDialog>
  )
}

export function PlanTextFields({
  name,
  description,
  onName,
  onDescription,
}: {
  name: string
  description: string
  onName: (v: string) => void
  onDescription: (v: string) => void
}) {
  return (
    <>
      <div className="grid gap-2">
        <Label htmlFor="plan-name">Name</Label>
        <Input id="plan-name" value={name} maxLength={64} autoComplete="off" onChange={(e) => onName(e.target.value)} />
      </div>
      <div className="grid gap-2">
        <Label htmlFor="plan-description">Description</Label>
        <Input id="plan-description" value={description} maxLength={500} autoComplete="off" onChange={(e) => onDescription(e.target.value)} />
      </div>
    </>
  )
}
