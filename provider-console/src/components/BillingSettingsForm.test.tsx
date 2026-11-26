import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import type { BillingSettings } from '@/api/types'
import { BillingSettingsForm } from '@/components/BillingSettingsForm'
import { BILLING_COUNTRIES } from '@/lib/billing-countries'

const DEFAULTS: BillingSettings = {
  country: 'US',
  currency: 'USD',
  tax_rate_bp: 0,
  tax_label: '',
  invoice_retention_years: 8,
  email_tenants: true,
}
const REASON = 'Moving billing to India'

function setup(settings = DEFAULTS, cardCount = 3) {
  const onSave = vi.fn().mockResolvedValue(undefined)
  const onDone = vi.fn()
  render(<BillingSettingsForm settings={settings} countries={BILLING_COUNTRIES} cardCount={cardCount} onSave={onSave} onDone={onDone} />)
  return { onSave, onDone, save: screen.getByRole('button', { name: 'Save settings' }) }
}

describe('BillingSettingsForm', () => {
  it('shows the defaults: United States / USD, no tax, 8 years, email on', () => {
    setup()
    expect(screen.getByLabelText('Country')).toHaveValue('US')
    expect(screen.getByLabelText('Currency')).toHaveValue('USD')
    expect(screen.getByLabelText('No tax (invoices say “Tax not included”)')).toBeChecked()
    expect(screen.getByLabelText('Keep invoices for (years)')).toHaveValue('8')
    expect(screen.getByRole('checkbox', { name: /Email invoices to tenants/ })).toBeChecked()
    expect(screen.getByRole('button', { name: 'Save settings' })).toBeDisabled()
  })

  it('choosing India fills in INR and needs the card-switch acknowledgement', async () => {
    const { onSave, onDone, save } = setup()
    await userEvent.selectOptions(screen.getByLabelText('Country'), 'IN')
    expect(screen.getByLabelText('Currency')).toHaveValue('INR')
    expect(screen.getByText(/All 3 plan cards switch to INR\. Amounts are not converted/)).toBeInTheDocument()
    await userEvent.type(screen.getByLabelText('Reason'), REASON)
    expect(save).toBeDisabled() // not acknowledged yet
    await userEvent.click(screen.getByLabelText('I’ll review the card prices'))
    await userEvent.click(save)
    expect(onSave).toHaveBeenCalledWith({ country: 'IN', currency: 'INR' }, REASON)
    expect(onDone).toHaveBeenCalledWith('Billing settings saved.')
  })

  it('tax: a percentage becomes basis points with its name', async () => {
    const { onSave, save } = setup()
    await userEvent.click(screen.getByLabelText('Add tax to every invoice'))
    await userEvent.type(screen.getByLabelText('Tax name'), 'GST')
    await userEvent.type(screen.getByLabelText('Rate (%)'), '7.25')
    await userEvent.type(screen.getByLabelText('Reason'), REASON)
    await userEvent.click(save)
    expect(onSave).toHaveBeenCalledWith({ tax_rate_bp: 725, tax_label: 'GST' }, REASON)
  })

  it('tax: refuses a rate with three decimals, a zero rate, or no name', async () => {
    const { save } = setup()
    await userEvent.click(screen.getByLabelText('Add tax to every invoice'))
    await userEvent.type(screen.getByLabelText('Reason'), REASON)
    await userEvent.type(screen.getByLabelText('Rate (%)'), '7.255')
    await userEvent.type(screen.getByLabelText('Tax name'), 'GST')
    expect(save).toBeDisabled()
    await userEvent.clear(screen.getByLabelText('Rate (%)'))
    await userEvent.type(screen.getByLabelText('Rate (%)'), '0')
    expect(save).toBeDisabled()
    await userEvent.clear(screen.getByLabelText('Rate (%)'))
    await userEvent.type(screen.getByLabelText('Rate (%)'), '18')
    await userEvent.clear(screen.getByLabelText('Tax name'))
    expect(save).toBeDisabled()
  })

  it('turning tax off sends rate 0 and an empty name', async () => {
    const { onSave, save } = setup({ ...DEFAULTS, tax_rate_bp: 1800, tax_label: 'GST' })
    expect(screen.getByLabelText('Rate (%)')).toHaveValue('18')
    await userEvent.click(screen.getByLabelText('No tax (invoices say “Tax not included”)'))
    await userEvent.type(screen.getByLabelText('Reason'), REASON)
    await userEvent.click(save)
    expect(onSave).toHaveBeenCalledWith({ tax_rate_bp: 0, tax_label: '' }, REASON)
  })

  it('retention must be 1 to 30 years; email can be turned off', async () => {
    const { onSave, save } = setup()
    const years = screen.getByLabelText('Keep invoices for (years)')
    await userEvent.type(screen.getByLabelText('Reason'), REASON)
    for (const bad of ['0', '31', 'x']) {
      await userEvent.clear(years)
      await userEvent.type(years, bad)
      expect(save).toBeDisabled()
    }
    await userEvent.clear(years)
    await userEvent.type(years, '10')
    await userEvent.click(screen.getByRole('checkbox', { name: /Email invoices to tenants/ }))
    await userEvent.click(save)
    expect(onSave).toHaveBeenCalledWith({ invoice_retention_years: 10, email_tenants: false }, REASON)
  })

  it('a currency change with no priced cards still states that issued invoices keep theirs', async () => {
    setup(DEFAULTS, 0)
    await userEvent.selectOptions(screen.getByLabelText('Currency'), 'EUR')
    expect(screen.getByText(/New prices will be in EUR\. Issued invoices keep their currency\./)).toBeInTheDocument()
  })

  describe('unsaved-changes guard', () => {
    // True when leaving the page would ask "Leave site?".
    const unloadPrompts = () => {
      const e = new Event('beforeunload', { cancelable: true })
      window.dispatchEvent(e)
      return e.defaultPrevented
    }

    it('a clean form doesn’t prompt; a reason alone doesn’t either', async () => {
      setup()
      expect(unloadPrompts()).toBe(false)
      await userEvent.type(screen.getByLabelText('Reason'), REASON)
      expect(unloadPrompts()).toBe(false)
    })

    it('a changed setting prompts on leaving, even while still invalid, and changing it back clears it', async () => {
      setup()
      const years = screen.getByLabelText('Keep invoices for (years)')
      await userEvent.clear(years)
      expect(unloadPrompts()).toBe(true) // empty: invalid, still unsaved
      await userEvent.type(years, '8')
      expect(unloadPrompts()).toBe(false)
    })

    it('after a successful save leaving no longer prompts', async () => {
      const { onSave, save } = setup()
      await userEvent.click(screen.getByRole('checkbox', { name: /Email invoices to tenants/ }))
      expect(unloadPrompts()).toBe(true)
      await userEvent.type(screen.getByLabelText('Reason'), REASON)
      await userEvent.click(save)
      expect(onSave).toHaveBeenCalledWith({ email_tenants: false }, REASON)
      expect(unloadPrompts()).toBe(false)
    })

    it('a failed save keeps the guard on', async () => {
      const { onSave, save } = setup()
      onSave.mockRejectedValueOnce(new Error('boom'))
      await userEvent.click(screen.getByRole('checkbox', { name: /Email invoices to tenants/ }))
      await userEvent.type(screen.getByLabelText('Reason'), REASON)
      await userEvent.click(save)
      expect(await screen.findByRole('alert')).toBeInTheDocument()
      expect(unloadPrompts()).toBe(true)
    })
  })
})
