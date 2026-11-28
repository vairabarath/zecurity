import { useState } from 'react'
import { RefreshCw } from 'lucide-react'
import { Navigate } from 'react-router-dom'
import { getBillingSettings, listPlanCards, updateBillingSettings } from '@/api/billing'
import { BillingSettingsForm } from '@/components/BillingSettingsForm'
import { can } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { PageHeader } from '@/components/PageHeader'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { BILLING_COUNTRIES } from '@/lib/billing-countries'
import { useDetailQuery } from '@/lib/useDetailQuery'

// Billing settings (Sprint 26 P4; D-64 country and currency, D-67
// retention, D-68 tenant email, D-69 tax). Super-admin only
// (billing.settings.write). A currency change switches every priced card
// with no conversion; the form says how many and needs an acknowledgement.

const fetchSettings = () => getBillingSettings()
const fetchCards = () => listPlanCards()

export function BillingSettings() {
  // Sprint 27 O2: billing and super-admins change the settings; auditor reads them.
  const readOnly = !can(useSessionStore((s) => s.me?.role), 'billing.settings.write')
  const settings = useDetailQuery('billing-settings', fetchSettings)
  const cards = useDetailQuery('plan-cards', fetchCards)
  const [notice, setNotice] = useState<string | null>(null)
  // A new key remounts the form with the saved values after a save.
  const [round, setRound] = useState(0)

  const reload = () => {
    settings.reload()
    cards.reload()
  }

  if (settings.state.status === 'session_ended') return null
  if (settings.state.status === 'forbidden') return <Navigate to="/forbidden" replace />

  let body
  if (settings.state.status === 'loading' || cards.state.status === 'loading') {
    body = <Skeleton className="h-96 w-full" aria-label="Loading billing settings" />
  } else if (settings.state.status !== 'ready' || cards.state.status !== 'ready') {
    body = (
      <Alert variant="destructive">
        <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
          <span>Couldn’t load the billing settings.</span>
          <Button variant="outline" size="sm" onClick={reload}>
            Retry
          </Button>
        </AlertDescription>
      </Alert>
    )
  } else {
    const { countries, ...current } = settings.state.data
    body = (
      <BillingSettingsForm
          key={round}
          settings={current}
          countries={countries && countries.length > 0 ? countries : BILLING_COUNTRIES}
          cardCount={cards.state.data.items.filter((c) => c.price).length}
          readOnly={readOnly}
          onSave={(change, reason) => updateBillingSettings(change, reason)}
          onDone={(text) => {
            setNotice(text)
            setRound((n) => n + 1)
            reload()
          }}
        />
    )
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Billing settings"
        description="The country and currency every plan card is priced in, the tax added to invoices, how long invoices are kept, and whether tenants get their invoices by email."
        actions={
          <Button
            variant="outline"
            onClick={() => {
              setNotice(null)
              reload()
            }}
            aria-label="Refresh billing settings"
          >
            <RefreshCw aria-hidden />
            Refresh
          </Button>
        }
      />
      {notice && (
        <Alert role="status">
          <AlertDescription>{notice}</AlertDescription>
        </Alert>
      )}
      {body}
    </div>
  )
}
