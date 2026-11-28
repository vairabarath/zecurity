import { BILLING_METERS, PLAN_LIMIT_KEYS, type PlanCardSummary } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { formatIncluded, formatMoney, METER_LABELS, unitName } from '@/lib/billing-money'
import { formatLimit, PLAN_LIMIT_LABELS } from '@/lib/plan-format'

// One plan card (Sprint 26, D-63): what a tenant on this plan pays and may
// create. The price leads; the limits follow; the actions sit at the foot.
// Actions are optional so the card also works read-only.

export interface PlanCardActions {
  onEdit?: () => void
  onDuplicate?: () => void
  onHistory?: () => void
  /** Shown on a priced card only. */
  onRemovePrice?: () => void
  /** Shown on a card that isn't the default. */
  onMakeDefault?: () => void
  /** Shown on a card that isn't the default; disabled while tenants use it. */
  onDelete?: () => void
}

const plural = (n: number, one: string) => `${n.toLocaleString()} ${one}${n === 1 ? '' : 's'}`

export function PlanCard({ plan, ...actions }: { plan: PlanCardSummary } & PlanCardActions) {
  const titleId = `plan-card-${plan.id}`
  const price = plan.price
  const priced = price ? BILLING_METERS.filter((m) => price.meters[m]) : []
  return (
    <Card role="article" aria-labelledby={titleId} className="flex flex-col">
      <div className="flex flex-col gap-1 px-5 pt-5">
        <div className="flex min-w-0 flex-wrap items-center gap-2">
          <h3 id={titleId} className="min-w-0 break-words font-display text-base font-semibold">
            {plan.name}
          </h3>
          {plan.is_default && (
            <Badge variant="secondary" title="New workspaces start on this plan">
              Default
            </Badge>
          )}
        </div>
        {plan.description && <p className="break-words text-[13px] text-muted-foreground">{plan.description}</p>}
      </div>

      <div className="px-5 pt-4">
        {price ? (
          <p className="flex items-baseline gap-1.5" data-testid="plan-price">
            <span className="font-display text-2xl font-semibold tabular-nums">{formatMoney(price.base_fee_minor, price.currency)}</span>
            <span className="text-sm text-muted-foreground">per month</span>
          </p>
        ) : (
          <p className="text-sm text-muted-foreground" data-testid="plan-price">
            Not billable. Tenants on this plan get no invoice.
          </p>
        )}
        {priced.length > 0 && (
          <ul className="mt-3 flex flex-col gap-1.5 text-sm" aria-label="Usage prices">
            {priced.map((m) => {
              const p = price!.meters[m]!
              return (
                <li key={m} className="flex flex-col">
                  <span>{METER_LABELS[m]}</span>
                  <span className="text-[13px] text-muted-foreground tabular-nums">
                    {formatIncluded(m, p.included, p.unit)} included, then {formatMoney(p.unit_price_minor, price!.currency)} per{' '}
                    {unitName(m, p.unit)}
                  </span>
                </li>
              )
            })}
          </ul>
        )}
      </div>

      <dl className="mx-5 mt-4 grid grid-cols-2 gap-x-4 gap-y-1.5 border-t border-border pt-4 text-[13px]" aria-label="Limits">
        {PLAN_LIMIT_KEYS.map((k) => (
          <div key={k} className="flex justify-between gap-2">
            <dt className="text-muted-foreground">{PLAN_LIMIT_LABELS[k]}</dt>
            <dd className="font-mono tabular-nums" data-limit={k}>
              {formatLimit(plan.limits[k])}
            </dd>
          </div>
        ))}
      </dl>

      {(actions.onRemovePrice || actions.onMakeDefault || actions.onDelete) && (
        <div className="mt-4 flex flex-wrap gap-2 px-5">
          {actions.onMakeDefault && !plan.is_default && (
            <Button variant="ghost" size="sm" onClick={actions.onMakeDefault} aria-label={`Make ${plan.name} the default`}>
              Make default
            </Button>
          )}
          {actions.onRemovePrice && price && (
            <Button variant="ghost" size="sm" onClick={actions.onRemovePrice} aria-label={`Remove the price of ${plan.name}`}>
              Remove price
            </Button>
          )}
          {actions.onDelete && !plan.is_default && (
            <Button
              variant="ghost"
              size="sm"
              onClick={actions.onDelete}
              aria-label={`Delete ${plan.name}`}
              disabled={plan.tenants > 0}
              title={plan.tenants > 0 ? 'Move its tenants to another plan first' : undefined}
            >
              Delete
            </Button>
          )}
        </div>
      )}

      <div className="mt-auto flex flex-wrap items-center justify-between gap-3 px-5 py-4">
        <span className="text-[13px] text-muted-foreground">
          {plural(plan.tenants, 'tenant')} · revision {plan.revision}
        </span>
        <span className="inline-flex flex-wrap gap-2">
          {actions.onHistory && (
            <Button variant="ghost" size="sm" onClick={actions.onHistory} aria-label={`History of ${plan.name}`}>
              History
            </Button>
          )}
          {actions.onDuplicate && (
            <Button variant="outline" size="sm" onClick={actions.onDuplicate} aria-label={`Duplicate ${plan.name}`}>
              Duplicate
            </Button>
          )}
          {actions.onEdit && (
            <Button variant="outline" size="sm" onClick={actions.onEdit} aria-label={`Edit ${plan.name}`}>
              Edit
            </Button>
          )}
        </span>
      </div>
    </Card>
  )
}
