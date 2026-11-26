import { useState, type FormEvent } from 'react'
import { BILLING_METERS, type BillingMeter, type PlanCardSummary, type PlanLimits, type PlanPrice } from '@/api/types'
import { LimitsFields, PlanTextFields } from '@/components/PlanDialogs'
import { ReasonField } from '@/components/ReasonField'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { isByteMeter, METER_LABELS, unitOptions } from '@/lib/billing-money'
import { limitInputs, parseLimitInputs, sameLimits, type LimitInputs } from '@/lib/plan-format'
import { parsePriceInputs, priceInputs, pricePayload, type PriceErrors, type PriceInputs } from '@/lib/plan-price-form'
import { reasonValid } from '@/lib/pool-format'
import { useWrite } from '@/lib/useWrite'
import { cn } from '@/lib/utils'

// Plan card editor (Sprint 26, D-63): create a card, edit one, or duplicate
// one as a starting point for a custom deal. A card is the plan (name,
// description, six limits) plus an optional price. Every save is one request
// with a reason; the API calls are passed in (the P1b endpoints wire them).

export interface NewPlanCard {
  name: string
  description: string
  limits: PlanLimits
  price: PlanPrice | null
}

/** Only the changed fields; `price: null` removes the price, a missing `price` leaves it. */
export interface PlanCardChange {
  name?: string
  description?: string
  limits?: PlanLimits
  price?: PlanPrice | null
}

type Props =
  | { mode: 'create'; currency: string; onCreate: (card: NewPlanCard, reason: string) => Promise<void>; onCancel: () => void; onDone: (text: string) => void }
  | { mode: 'duplicate'; plan: PlanCardSummary; currency: string; onCreate: (card: NewPlanCard, reason: string) => Promise<void>; onCancel: () => void; onDone: (text: string) => void }
  | { mode: 'edit'; plan: PlanCardSummary; currency: string; onUpdate: (change: PlanCardChange, reason: string) => Promise<void>; onCancel: () => void; onDone: (text: string) => void }

const plural = (n: number, one: string) => `${n.toLocaleString()} ${one}${n === 1 ? '' : 's'}`

export function PlanCardDialog(props: Props) {
  const plan = props.mode === 'create' ? undefined : props.plan
  const { currency } = props
  const [name, setName] = useState(props.mode === 'duplicate' ? `${plan!.name} (copy)`.slice(0, 64) : (plan?.name ?? ''))
  const [description, setDescription] = useState(plan?.description ?? '')
  const [limits, setLimits] = useState<LimitInputs>(limitInputs(plan?.limits))
  const [price, setPrice] = useState<PriceInputs>(priceInputs(plan?.price ?? null))
  const [reason, setReason] = useState('')
  const { pending, error, run } = useWrite()

  const parsedLimits = parseLimitInputs(limits)
  const parsedPrice = parsePriceInputs(price, currency)
  const nameOk = name.trim().length > 0 && name.trim().length <= 64
  const formOk = nameOk && description.length <= 500 && parsedLimits !== null && parsedPrice.ok && reasonValid(reason)

  const change: PlanCardChange = {}
  if (props.mode === 'edit' && parsedLimits && parsedPrice.ok) {
    if (name.trim() !== plan!.name) change.name = name.trim()
    if (description.trim() !== plan!.description) change.description = description.trim()
    if (!sameLimits(parsedLimits, plan!.limits)) change.limits = parsedLimits
    const p = pricePayload(plan!.price, parsedPrice.price)
    if (p !== undefined) change.price = p
  }
  const changed = props.mode !== 'edit' || Object.keys(change).length > 0
  const valid = formOk && changed

  const title = props.mode === 'create' ? 'New plan card' : props.mode === 'duplicate' ? 'Duplicate plan card' : 'Edit plan card'
  const submitLabel = props.mode === 'edit' ? 'Save card' : 'Create card'

  function submit(e: FormEvent) {
    e.preventDefault()
    if (!valid || pending || !parsedLimits || !parsedPrice.ok) return
    const r = reason.trim()
    void run(async () => {
      if (props.mode === 'edit') {
        await props.onUpdate(change, r)
        props.onDone(`Plan card ${name.trim()} saved.`)
      } else {
        await props.onCreate({ name: name.trim(), description: description.trim(), limits: parsedLimits, price: parsedPrice.price }, r)
        props.onDone(`Plan card ${name.trim()} created. Assign it to tenants from their tenant page.`)
      }
    })
  }

  return (
    <Dialog open onOpenChange={(o) => !o && !pending && props.onCancel()}>
      <DialogContent className="max-h-[90vh] overflow-y-auto overscroll-contain sm:max-w-2xl">
        <form onSubmit={submit} className="grid gap-5" noValidate>
          <DialogHeader>
            <DialogTitle>{title}</DialogTitle>
            <DialogDescription>
              {props.mode === 'edit' ? (
                <EditImpact plan={plan!} />
              ) : props.mode === 'duplicate' ? (
                <>A new card that starts as a copy of {plan!.name}. Change what this tenant’s deal needs, then assign it from their tenant page.</>
              ) : (
                <>What a tenant on this card may create and what it pays each month. Leave billing off for a card that sends no invoice.</>
              )}
            </DialogDescription>
          </DialogHeader>
          {error && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error}</AlertDescription>
            </Alert>
          )}
          <PlanTextFields name={name} description={description} onName={setName} onDescription={setDescription} />
          <LimitsFields value={limits} onChange={setLimits} />
          <PriceFields value={price} currency={currency} errors={parsedPrice.ok ? {} : parsedPrice.errors} onChange={setPrice} />
          <ReasonField id="plan-card-reason" value={reason} onChange={setReason} />
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={props.onCancel} disabled={pending}>
              Cancel
            </Button>
            <Button type="submit" disabled={!valid || pending}>
              {pending ? 'Saving…' : submitLabel}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function EditImpact({ plan }: { plan: PlanCardSummary }) {
  if (plan.tenants === 0) return <>No tenant uses this card yet.</>
  return (
    <>
      {plural(plan.tenants, 'tenant')} use{plan.tenants === 1 ? 's' : ''} this card. Limits change at once. Price changes apply from next
      month: this month is billed at the card as it was on the 1st (D-63). Issued invoices don’t change.
    </>
  )
}

const fieldClass =
  'flex h-9 w-full rounded-lg border border-input bg-field px-3 text-sm text-foreground transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/30 aria-invalid:border-destructive'

/** The price part of a card: billing on or off, the monthly fee, and each priced meter. */
export function PriceFields({
  value,
  currency,
  errors,
  onChange,
}: {
  value: PriceInputs
  currency: string
  errors: PriceErrors
  onChange: (v: PriceInputs) => void
}) {
  const setMeter = (m: BillingMeter, patch: Partial<PriceInputs['meters'][BillingMeter]>) =>
    onChange({ ...value, meters: { ...value.meters, [m]: { ...value.meters[m], ...patch } } })
  return (
    <fieldset className="grid gap-3">
      <legend className="mb-1 text-sm font-medium">Price</legend>
      <label className="inline-flex items-center gap-2 text-sm">
        <input type="checkbox" checked={value.billable} onChange={(e) => onChange({ ...value, billable: e.target.checked })} />
        Bill tenants on this card
      </label>
      {!value.billable ? (
        <p className="text-xs text-muted-foreground">Not billable: tenants on this card get no invoice.</p>
      ) : (
        <>
          <div className="grid gap-2">
            <Label htmlFor="price-base-fee">Monthly fee ({currency})</Label>
            <Input
              id="price-base-fee"
              name="base_fee"
              inputMode="decimal"
              autoComplete="off"
              placeholder="19.99…"
              value={value.baseFee}
              aria-invalid={errors.baseFee !== undefined}
              aria-describedby={errors.baseFee ? 'price-base-fee-error' : undefined}
              onChange={(e) => onChange({ ...value, baseFee: e.target.value })}
              className="max-w-48"
            />
            {errors.baseFee && (
              <p id="price-base-fee-error" className="text-xs text-destructive" aria-live="polite">
                {errors.baseFee}
              </p>
            )}
          </div>
          <p className="text-xs text-muted-foreground">
            Usage is billed above what’s included, rounded up to whole units. Leave a meter off to not bill it.
          </p>
          <ul className="grid gap-3">
            {BILLING_METERS.map((m) => (
              <MeterRow key={m} meter={m} value={value.meters[m]} currency={currency} errors={errors} onChange={(p) => setMeter(m, p)} />
            ))}
          </ul>
        </>
      )}
    </fieldset>
  )
}

function MeterRow({
  meter,
  value,
  currency,
  errors,
  onChange,
}: {
  meter: BillingMeter
  value: PriceInputs['meters'][BillingMeter]
  currency: string
  errors: PriceErrors
  onChange: (p: Partial<PriceInputs['meters'][BillingMeter]>) => void
}) {
  const label = METER_LABELS[meter]
  const units = unitOptions(meter)
  const incErr = errors[`${meter}.included`]
  const priceErr = errors[`${meter}.unitPrice`]
  const id = `meter-${meter}`
  return (
    <li className={cn('grid gap-2 rounded-lg border border-border px-3 py-2.5', !value.billed && 'border-dashed')}>
      <label className="inline-flex items-center gap-2 text-sm">
        <input type="checkbox" checked={value.billed} onChange={(e) => onChange({ billed: e.target.checked })} />
        Bill {label.charAt(0).toLowerCase() + label.slice(1)}
      </label>
      {value.billed && (
        <div className="grid grid-cols-1 gap-2 sm:grid-cols-3">
          <div className="grid content-start gap-1">
            <Label htmlFor={`${id}-included`} className="text-xs">
              Included
            </Label>
            <Input
              id={`${id}-included`}
              name={`${meter}.included`}
              inputMode="numeric"
              autoComplete="off"
              value={value.included}
              aria-invalid={incErr !== undefined}
              onChange={(e) => onChange({ included: e.target.value })}
            />
          </div>
          <div className="grid content-start gap-1">
            <Label htmlFor={`${id}-unit`} className="text-xs">
              Unit
            </Label>
            <select
              id={`${id}-unit`}
              name={`${meter}.unit`}
              className={fieldClass}
              value={value.unitSize}
              disabled={units.length === 1}
              onChange={(e) => onChange({ unitSize: Number(e.target.value) })}
            >
              {units.map((u) => (
                <option key={u.size} value={u.size}>
                  {u.name}
                </option>
              ))}
            </select>
          </div>
          <div className="grid content-start gap-1">
            <Label htmlFor={`${id}-price`} className="text-xs">
              Price per {units.find((u) => u.size === value.unitSize)?.name ?? 'unit'} ({currency})
            </Label>
            <Input
              id={`${id}-price`}
              name={`${meter}.unit_price`}
              inputMode="decimal"
              autoComplete="off"
              placeholder="0.10…"
              value={value.unitPrice}
              aria-invalid={priceErr !== undefined}
              onChange={(e) => onChange({ unitPrice: e.target.value })}
            />
          </div>
          {(incErr || priceErr) && (
            <p className="text-xs text-destructive sm:col-span-3" role="status">
              {[incErr && `Included: ${incErr}`, priceErr && `Price: ${priceErr}`].filter(Boolean).join(' ')}
            </p>
          )}
          {isByteMeter(meter) && <p className="text-xs text-muted-foreground sm:col-span-3">1 GB = 1,000,000,000 bytes.</p>}
        </div>
      )}
    </li>
  )
}
