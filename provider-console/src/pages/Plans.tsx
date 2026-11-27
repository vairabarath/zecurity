import { useState } from 'react'
import { Plus, RefreshCw } from 'lucide-react'
import { Link, Navigate } from 'react-router-dom'
import { createPlanCard, getBillingSettings, listPlanCards, updatePlanCard } from '@/api/billing'
import type { PlanCardSummary } from '@/api/types'
import { ConfirmDangerDialog } from '@/components/ConfirmDangerDialog'
import { PageHeader } from '@/components/PageHeader'
import { PlanCard } from '@/components/PlanCard'
import { PlanCardDialog } from '@/components/PlanCardDialog'
import { DeletePlanDialog, MakeDefaultPlanDialog } from '@/components/PlanDialogs'
import { PlanHistoryDialog } from '@/components/PlanHistoryDialog'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { planErrorMessage } from '@/lib/plan-format'
import { useDetailQuery } from '@/lib/useDetailQuery'

// Plans as cards (Sprint 23 B1 + Sprint 26 P4, D-43, D-63; super-admin only).
// A card is a plan's limits plus an optional price in the billing currency.
// Cards are created and edited at any time; every change is a new revision.
// A month is billed at the revision in force on its 1st (D-63, changed
// 2026-11-24): edits bill from the next month.

const fetchCards = () => listPlanCards()
const fetchSettings = () => getBillingSettings()

type Action =
  | { kind: 'edit' | 'duplicate' | 'history' | 'default' | 'delete' | 'remove-price'; plan: PlanCardSummary }
  | { kind: 'create' }

export function Plans() {
  const cards = useDetailQuery('plan-cards', fetchCards)
  const settings = useDetailQuery('billing-settings', fetchSettings)
  const [action, setAction] = useState<Action | null>(null)
  const [notice, setNotice] = useState<string | null>(null)

  const reload = () => {
    cards.reload()
    settings.reload()
  }
  const done = (text: string) => {
    setAction(null)
    setNotice(text)
    cards.reload()
  }

  if (cards.state.status === 'session_ended') return null
  if (cards.state.status === 'forbidden') return <Navigate to="/forbidden" replace />
  // A card's price is always in the settings currency (D-64); without the
  // settings a new card can't be priced, so creating waits for them.
  const currency = settings.state.status === 'ready' ? settings.state.data.currency : null

  let body
  switch (cards.state.status) {
    case 'loading':
      body = (
        <div aria-busy="true" aria-label="Loading plans" className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          <Skeleton className="h-72 w-full" />
          <Skeleton className="h-72 w-full" />
        </div>
      )
      break
    case 'not_found':
    case 'error':
      body = (
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
            <span>Couldn’t load plans.</span>
            <Button variant="outline" size="sm" onClick={reload}>
              Retry
            </Button>
          </AlertDescription>
        </Alert>
      )
      break
    case 'ready': {
      const items = cards.state.data.items
      body =
        items.length === 0 ? (
          <p className="py-6 text-sm text-muted-foreground">No plans.</p>
        ) : (
          <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3" role="list" aria-label="Plan cards">
            {items.map((p) => (
              <div role="listitem" key={p.id} className="flex">
                <PlanCard
                  plan={p}
                  onEdit={currency ? () => setAction({ kind: 'edit', plan: p }) : undefined}
                  onDuplicate={currency ? () => setAction({ kind: 'duplicate', plan: p }) : undefined}
                  onHistory={() => setAction({ kind: 'history', plan: p })}
                  onRemovePrice={() => setAction({ kind: 'remove-price', plan: p })}
                  onMakeDefault={() => setAction({ kind: 'default', plan: p })}
                  onDelete={() => setAction({ kind: 'delete', plan: p })}
                />
              </div>
            ))}
          </div>
        )
      break
    }
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Plans"
        description="Each tenant is on one plan card: its limits and, when priced, what the tenant pays each month. Edits bill from the next month; this month is billed at the card as it was on the 1st."
        actions={
          <>
            <Button
              variant="outline"
              onClick={() => {
                setNotice(null)
                reload()
              }}
              aria-label="Refresh plans"
            >
              <RefreshCw aria-hidden />
              Refresh
            </Button>
            <Button onClick={() => setAction({ kind: 'create' })} disabled={!currency}>
              <Plus aria-hidden />
              New card
            </Button>
          </>
        }
      />
      {settings.state.status !== 'ready' && settings.state.status !== 'loading' && (
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
            <span>Couldn’t load the billing settings, so cards can’t be created or edited.</span>
            <Button variant="outline" size="sm" onClick={settings.reload}>
              Retry
            </Button>
          </AlertDescription>
        </Alert>
      )}
      {currency && (
        <p className="text-[13px] text-muted-foreground">
          Prices are in {currency}.{' '}
          <Link to="/billing/settings" className="text-primary hover:underline">
            Change the billing currency
          </Link>
        </p>
      )}
      {notice && (
        <Alert role="status">
          <AlertDescription>{notice}</AlertDescription>
        </Alert>
      )}
      {body}
      {action && <ActionDialog action={action} currency={currency} onCancel={() => setAction(null)} onDone={done} />}
    </div>
  )
}

function ActionDialog({
  action,
  currency,
  onCancel,
  onDone,
}: {
  action: Action
  currency: string | null
  onCancel: () => void
  onDone: (text: string) => void
}) {
  switch (action.kind) {
    case 'create':
      return currency ? (
        <PlanCardDialog mode="create" currency={currency} onCreate={async (c, r) => void (await createPlanCard(c, r))} onCancel={onCancel} onDone={onDone} />
      ) : null
    case 'duplicate':
      return currency ? (
        <PlanCardDialog
          mode="duplicate"
          plan={action.plan}
          currency={currency}
          onCreate={async (c, r) => void (await createPlanCard(c, r))}
          onCancel={onCancel}
          onDone={onDone}
        />
      ) : null
    case 'edit':
      return currency ? (
        <PlanCardDialog
          mode="edit"
          plan={action.plan}
          currency={currency}
          onUpdate={(change, r) => updatePlanCard(action.plan.id, change, r)}
          onCancel={onCancel}
          onDone={onDone}
        />
      ) : null
    case 'history':
      return <PlanHistoryDialog plan={action.plan} onClose={onCancel} />
    case 'default':
      return <MakeDefaultPlanDialog plan={action.plan} onCancel={onCancel} onDone={onDone} />
    case 'delete':
      return <DeletePlanDialog plan={action.plan} onCancel={onCancel} onDone={onDone} />
    case 'remove-price':
      return <RemovePriceDialog plan={action.plan} onCancel={onCancel} onDone={onDone} />
  }
}

function RemovePriceDialog({ plan, onCancel, onDone }: { plan: PlanCardSummary; onCancel: () => void; onDone: (text: string) => void }) {
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  return (
    <ConfirmDangerDialog
      title={`Remove the price of ${plan.name}?`}
      description={
        plan.tenants > 0
          ? `${plan.tenants} tenant${plan.tenants === 1 ? '' : 's'} use this card. From next month they get no invoice; this month is billed at the card as it was on the 1st.`
          : 'The card becomes not billable. Its limits stay.'
      }
      confirmText={plan.name}
      requireConfirmText={false}
      confirmLabel="Remove price"
      pendingLabel="Removing…"
      pending={pending}
      error={error ? { message: error } : undefined}
      onCancel={onCancel}
      onConfirm={async (reason) => {
        setPending(true)
        setError(null)
        try {
          await updatePlanCard(plan.id, { price: null }, reason)
          onDone(`${plan.name} is no longer billable from next month.`)
        } catch (err) {
          setError(planErrorMessage(err))
        } finally {
          setPending(false)
        }
      }}
    />
  )
}
