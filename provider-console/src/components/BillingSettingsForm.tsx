import { useState, type FormEvent } from 'react'
import type { BillingSettings } from '@/api/types'
import { ReasonField } from '@/components/ReasonField'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { parseTaxPercent, taxPercentInput } from '@/lib/billing-money'
import type { BillingCountry } from '@/lib/billing-countries'
import { reasonValid } from '@/lib/pool-format'
import { useWrite } from '@/lib/useWrite'

// Billing settings (Sprint 26): country and currency (D-64), tax (D-69),
// invoice retention (D-67) and invoice email to tenants (D-68). One save
// sends only what changed, with a reason. Changing the currency switches
// every plan card to it without converting amounts, so it asks for an
// explicit acknowledgement first.

const plural = (n: number, one: string) => `${n.toLocaleString()} ${one}${n === 1 ? '' : 's'}`

const fieldClass =
  'flex h-9 w-full rounded-lg border border-input bg-field px-3 text-sm text-foreground transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/30'

export function BillingSettingsForm({
  settings,
  countries,
  cardCount,
  onSave,
  onDone,
}: {
  settings: BillingSettings
  countries: BillingCountry[]
  /** Plan cards with a price: they switch currency with the setting. */
  cardCount: number
  onSave: (change: Partial<BillingSettings>, reason: string) => Promise<void>
  onDone: (text: string) => void
}) {
  const [country, setCountry] = useState(settings.country)
  const [currency, setCurrency] = useState(settings.currency)
  const [taxOn, setTaxOn] = useState(settings.tax_rate_bp > 0)
  const [taxRate, setTaxRate] = useState(settings.tax_rate_bp > 0 ? taxPercentInput(settings.tax_rate_bp) : '')
  const [taxLabel, setTaxLabel] = useState(settings.tax_label)
  const [retention, setRetention] = useState(String(settings.invoice_retention_years))
  const [email, setEmail] = useState(settings.email_tenants)
  const [ack, setAck] = useState(false)
  const [reason, setReason] = useState('')
  const { pending, error, run } = useWrite()

  const currencies = [...new Set([settings.currency, ...countries.map((c) => c.currency)])].sort()
  const bp = taxOn ? parseTaxPercent(taxRate) : 0
  const taxOk = !taxOn || (bp !== null && bp > 0 && taxLabel.trim().length > 0 && taxLabel.trim().length <= 32)
  const years = /^\d{1,2}$/.test(retention.trim()) ? Number(retention.trim()) : NaN
  const retentionOk = years >= 1 && years <= 30
  const currencyChanged = currency !== settings.currency

  const change: Partial<BillingSettings> = {}
  if (country !== settings.country) change.country = country
  if (currencyChanged) change.currency = currency
  if (taxOk) {
    const newBp = bp ?? 0
    const newLabel = taxOn ? taxLabel.trim() : ''
    if (newBp !== settings.tax_rate_bp) change.tax_rate_bp = newBp
    if (newLabel !== settings.tax_label) change.tax_label = newLabel
  }
  if (retentionOk && years !== settings.invoice_retention_years) change.invoice_retention_years = years
  if (email !== settings.email_tenants) change.email_tenants = email

  const valid = taxOk && retentionOk && Object.keys(change).length > 0 && reasonValid(reason) && (!currencyChanged || ack)

  function pickCountry(code: string) {
    setCountry(code)
    const c = countries.find((x) => x.code === code)
    if (c) setCurrency(c.currency)
  }

  function submit(e: FormEvent) {
    e.preventDefault()
    if (!valid || pending) return
    void run(async () => {
      await onSave(change, reason.trim())
      setReason('')
      setAck(false)
      onDone('Billing settings saved.')
    })
  }

  return (
    <Card role="region" aria-labelledby="billing-settings-title" className="px-5 py-5">
      <h2 id="billing-settings-title" className="font-display text-base font-semibold">
        Billing settings
      </h2>
      <form onSubmit={submit} className="mt-4 grid max-w-2xl gap-5" noValidate>
        {error && (
          <Alert variant="destructive" role="alert">
            <AlertDescription>{error}</AlertDescription>
          </Alert>
        )}

        <div className="grid gap-3 sm:grid-cols-2">
          <div className="grid content-start gap-2">
            <Label htmlFor="billing-country">Country</Label>
            <select id="billing-country" name="country" className={fieldClass} value={country} onChange={(e) => pickCountry(e.target.value)}>
              {!countries.some((c) => c.code === country) && <option value={country}>{country}</option>}
              {countries.map((c) => (
                <option key={c.code} value={c.code}>
                  {c.name}
                </option>
              ))}
            </select>
          </div>
          <div className="grid content-start gap-2">
            <Label htmlFor="billing-currency">Currency</Label>
            <select id="billing-currency" name="currency" className={fieldClass} value={currency} onChange={(e) => setCurrency(e.target.value)}>
              {currencies.map((c) => (
                <option key={c} value={c}>
                  {c}
                </option>
              ))}
            </select>
          </div>
        </div>
        {currencyChanged && (
          <Alert role="status">
            <AlertDescription className="grid gap-2">
              <span>
                {cardCount === 0
                  ? `New prices will be in ${currency}.`
                  : `All ${plural(cardCount, 'plan card')} switch to ${currency}. Amounts are not converted: a fee of 20.00 ${settings.currency} becomes 20.00 ${currency}. Review each card’s prices after saving.`}{' '}
                Issued invoices keep their currency.
              </span>
              <label className="inline-flex items-center gap-2 text-sm">
                <input type="checkbox" checked={ack} onChange={(e) => setAck(e.target.checked)} />
                I’ll review the card prices
              </label>
            </AlertDescription>
          </Alert>
        )}

        <fieldset className="grid gap-3">
          <legend className="mb-1 text-sm font-medium">Tax</legend>
          <label className="inline-flex items-center gap-2 text-sm">
            <input type="radio" name="tax" checked={!taxOn} onChange={() => setTaxOn(false)} />
            No tax (invoices say “Tax not included”)
          </label>
          <label className="inline-flex items-center gap-2 text-sm">
            <input type="radio" name="tax" checked={taxOn} onChange={() => setTaxOn(true)} />
            Add tax to every invoice
          </label>
          {taxOn && (
            <div className="grid gap-3 pl-6 sm:grid-cols-2">
              <div className="grid content-start gap-2">
                <Label htmlFor="billing-tax-label">Tax name</Label>
                <Input
                  id="billing-tax-label"
                  name="tax_label"
                  value={taxLabel}
                  maxLength={32}
                  autoComplete="off"
                  placeholder="GST…"
                  aria-invalid={taxLabel.trim().length === 0}
                  onChange={(e) => setTaxLabel(e.target.value)}
                />
              </div>
              <div className="grid content-start gap-2">
                <Label htmlFor="billing-tax-rate">Rate (%)</Label>
                <Input
                  id="billing-tax-rate"
                  name="tax_rate"
                  inputMode="decimal"
                  autoComplete="off"
                  placeholder="18…"
                  value={taxRate}
                  aria-invalid={bp === null || bp === 0}
                  aria-describedby="billing-tax-rate-hint"
                  onChange={(e) => setTaxRate(e.target.value)}
                />
                <p id="billing-tax-rate-hint" className="text-xs text-muted-foreground">
                  Up to two decimals, like 18 or 7.25. Applies to invoices generated from now on.
                </p>
              </div>
            </div>
          )}
        </fieldset>

        <div className="grid content-start gap-2">
          <Label htmlFor="billing-retention">Keep invoices for (years)</Label>
          <Input
            id="billing-retention"
            name="invoice_retention_years"
            inputMode="numeric"
            autoComplete="off"
            className="max-w-24"
            value={retention}
            aria-invalid={!retentionOk}
            aria-describedby="billing-retention-hint"
            onChange={(e) => setRetention(e.target.value)}
          />
          <p id="billing-retention-hint" className="text-xs text-muted-foreground">
            1 to 30 years. Older invoices are deleted. Deleting a tenant never deletes its invoices.
          </p>
        </div>

        <label className="inline-flex items-start gap-2 text-sm">
          <input type="checkbox" className="mt-0.5" checked={email} onChange={(e) => setEmail(e.target.checked)} />
          <span>
            Email invoices to tenants
            <span className="block text-xs text-muted-foreground">
              When an invoice is issued, its tenant’s admins get it by email, and a notice if it’s voided.
            </span>
          </span>
        </label>

        <ReasonField id="billing-settings-reason" value={reason} onChange={setReason} />
        <div>
          <Button type="submit" disabled={!valid || pending}>
            {pending ? 'Saving…' : 'Save settings'}
          </Button>
        </div>
      </form>
    </Card>
  )
}
